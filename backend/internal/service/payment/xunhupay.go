package payment

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/netguard"
)

// 虎皮椒协议常量。
const (
	xunhuVersion = "1.1"
	// xunhuPlugins 是官方约定的「对接程序标识」，会随下单参数上报。
	xunhuPlugins = "lingdong-api"
	// 下单 5s：10s 会让 lockTopup 的 30s 锁被占满，用户重试直接得到 TopupInProgress。
	xunhuCreateTimeout = 5 * time.Second
	// 查单 3s：worker 高频调用，超时只是本轮不动作，不影响下一轮。
	xunhuQueryTimeout = 3 * time.Second
	// 网关响应体上限：正常响应只有几百字节，64KB 足够且能防超大响应打爆内存。
	maxGatewayBody = 64 << 10
)

// outboundPolicy / outboundClient 是**测试注入点**，生产恒为「仅公网 + 禁重定向」。
//
// 单测要访问 httptest 的 127.0.0.1，而 netguard.Policy{} 会拒绝回环；
// 唯一正确的做法是在这里放开，而不是把生产默认值改成允许内网
// （生产若为自建 vLLM 开了 LD_UPSTREAM_ALLOW_PRIVATE，支付出站绝不能跟着放开）。
var (
	outboundPolicy = netguard.Policy{}
	outboundClient = func(timeout time.Duration, p netguard.Policy) *http.Client {
		return netguard.Client(timeout, p)
	}
)

// xunhuSign 计算虎皮椒 HASH。
//
// 规则（官方）：剔除 hash 与**空值** → 参数名 ASCII 升序 → `k=v&...` → 末尾**直接**拼 APPSECRET → MD5 32 位小写。
//
// 与官方 Demo 的差异（Demo 是错的）：
//   - Demo 不剔除空值，示例里还传了 `callback_url: ""`，按官方规则会直接 invalid sign；
//   - Demo 不剔除 hash，调用方误传 hash 会既进签名又重复提交；
//   - Demo 用 strings.Trim(data, "&")，那是 cutset 语义，会误删值末尾的 &，这里逐个拼接不产生尾随 &。
func xunhuSign(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "hash" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	b.WriteString(secret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// xunhuVerify 校验虎皮椒签名（常量时间比较，避免按字节泄露 timing 信息）。
//
// **密钥为空必须 fail-closed**：`xunhuSign(params, "")` 等于「参数串联后的裸 MD5」，
// 攻击者可自行计算 → 伪造回调即可让未付款订单入账。空密钥不是「弱签名」，是「无签名」。
func xunhuVerify(params map[string]string, secret string) bool {
	if strings.TrimSpace(secret) == "" {
		return false
	}
	got := strings.ToLower(strings.TrimSpace(params["hash"]))
	if got == "" {
		return false
	}
	want := xunhuSign(params, secret)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// xunhuCreate 调 do.html 下单，返回移动端跳转地址与 PC 二维码。
//
// 出站策略固定 netguard.Policy{}（只允许公网、禁重定向）：
// 生产环境为自建 vLLM 开了 LD_UPSTREAM_ALLOW_PRIVATE=1，**不能**让支付出站跟着放开。
func xunhuCreate(ctx context.Context, ch Channel, req CreateRequest) (*CreateResult, error) {
	endpoint := xunhuPayEndpoint(ch.Params.PayAddress)
	if endpoint == "" || strings.TrimSpace(ch.Params.AppID) == "" || strings.TrimSpace(ch.Params.AppSecret) == "" {
		return nil, ErrNotConfigured
	}

	fields := map[string]string{
		"version":        xunhuVersion,
		"appid":          ch.Params.AppID,
		"trade_order_id": req.OutTradeNo,
		"total_fee":      FormatCentToCNY(req.AmountCent),
		"title":          req.Subject,
		"time":           strconv.FormatInt(time.Now().Unix(), 10),
		"notify_url":     req.NotifyURL,
		"nonce_str":      randomNonce(32),
		"plugins":        xunhuPlugins,
	}
	if strings.TrimSpace(req.ReturnURL) != "" {
		fields["return_url"] = req.ReturnURL
	}
	if strings.TrimSpace(req.Attach) != "" {
		fields["attach"] = req.Attach
	}
	fields["hash"] = xunhuSign(fields, ch.Params.AppSecret)

	body, status, err := postForm(ctx, endpoint, fields, xunhuCreateTimeout)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: 网关返回 HTTP %d", ErrGatewayRejected, status)
	}

	raw, str, err := decodeJSONObject(body)
	if err != nil {
		return nil, fmt.Errorf("%w: 响应不是合法 JSON", ErrGatewayRejected)
	}
	if err := xunhuClassify(str["errcode"], str["errmsg"]); err != nil {
		return nil, err
	}

	// 成功响应必须带 hash 且验签通过：这个响应里的 url 会直接展示给用户，
	// 不验签等于「谁能改响应谁就能把用户导到钓鱼页」。
	if _, ok := raw["hash"]; !ok {
		return nil, fmt.Errorf("%w: 成功响应缺少 hash", ErrSignInvalid)
	}
	if !xunhuVerify(str, ch.Params.AppSecret) {
		return nil, fmt.Errorf("%w: 返回体签名不一致", ErrSignInvalid)
	}

	payURL := strings.TrimSpace(str["url"])
	qrRef := strings.TrimSpace(str["url_qrcode"])
	if payURL == "" && qrRef == "" {
		return nil, ErrNoPayURL
	}
	return &CreateResult{
		PayURL:         payURL,
		QRRef:          qrRef,
		ChannelOrderID: firstNonEmpty(strings.TrimSpace(str["open_order_id"]), strings.TrimSpace(str["openid"])),
		ExpiresAt:      effectiveExpireAt(req.ExpireAt),
	}, nil
}

// xunhuQuery 主动查单。
//
// 返回值语义见 gateway.Query 的约定：(nil,nil) 表示确认未支付，调用方不得据此关单之外的任何动作。
//
// 验签口径（与回调不同，必须保守）：
//   - 响应**带 hash 且所有字段都是字符串** → 必须验签，验不过按 `ErrSignInvalid` 失败，
//     订单保持 pending（伪造的"已支付"进不来）；
//   - 响应没带 hash、或含数值/嵌套字段 → 无法可靠重算签名（数值重新序列化会变），
//     按未验签处理：记指标 + Warn，**仍然采信结果**。
//     因为「上游不带签名 → 付了钱的用户永远不到账」比「MIDM 伪造到账」发生得更频繁，
//     而且后者还要求攻击者控制出站链路（HTTPS 已经挡了一层）。
func xunhuQuery(ctx context.Context, ch Channel, outTradeNo, channelOrderID string) (*Notification, error) {
	endpoint := xunhuQueryEndpoint(ch)
	if endpoint == "" || strings.TrimSpace(ch.Params.AppID) == "" || strings.TrimSpace(ch.Params.AppSecret) == "" {
		return nil, ErrNotConfigured
	}

	fields := map[string]string{
		"appid":     ch.Params.AppID,
		"time":      strconv.FormatInt(time.Now().Unix(), 10),
		"nonce_str": randomNonce(32),
	}
	// 有上游内部单号时优先用它（比商户单号更精确）；否则用商户单号。
	if strings.TrimSpace(channelOrderID) != "" {
		fields["open_order_id"] = strings.TrimSpace(channelOrderID)
	} else {
		fields["out_trade_order"] = strings.TrimSpace(outTradeNo)
	}
	fields["hash"] = xunhuSign(fields, ch.Params.AppSecret)

	body, status, err := postForm(ctx, endpoint, fields, xunhuQueryTimeout)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: 查单返回 HTTP %d", ErrGatewayRejected, status)
	}
	raw, _, err := decodeJSONObject(body)
	if err != nil {
		return nil, fmt.Errorf("%w: 查单响应不是合法 JSON", ErrGatewayRejected)
	}

	// 兼容两种结构：顶层直接给字段，或包在 data 里（C2 待实测确认）。
	node := pickQueryNode(raw)
	verified, signOK := xunhuQueryVerify(node, ch.Params.AppSecret)
	if !signOK {
		return nil, fmt.Errorf("%w: 查单响应验签失败", ErrSignInvalid)
	}
	if !verified {
		metrics.Inc(metrics.NamePayQueryUnsigned, "channel", ch.Name)
		logger.L().Warn("查单响应未验签，按响应结果处理（建议联系网关确认是否返回 hash）",
			zap.String("channel", ch.Name), zap.String("out_trade_no", outTradeNo))
	}
	statusStr := firstNonEmpty(strOf(node, "status"), strOf(node, "trade_status"))
	if statusStr == "" {
		return nil, fmt.Errorf("%w: 查单响应缺少 status", ErrGatewayRejected)
	}
	kind, ok := xunhuQueryKind(statusStr)
	if !ok {
		return nil, nil // 明确未支付
	}
	amountRaw := firstNonEmpty(strOf(node, "total_amount"), strOf(node, "total_fee"), strOf(node, "amount"))
	amount, err := ParseCNYToCent(amountRaw)
	if err != nil {
		return nil, fmt.Errorf("查单金额无法解析: %w", err)
	}
	return &Notification{
		OutTradeNo:     firstNonEmpty(strOf(node, "out_trade_order"), strOf(node, "trade_order_id"), outTradeNo),
		TradeNo:        firstNonEmpty(strOf(node, "transaction_id"), strOf(node, "trade_no")),
		ChannelOrderID: firstNonEmpty(strOf(node, "open_order_id"), channelOrderID),
		AmountCent:     amount,
		MerchantID:     firstNonEmpty(strOf(node, "appid"), ch.Params.AppID),
		Kind:           kind,
		Raw:            nil,
	}, nil
}

// xunhuParseNotify 校验回调签名并映射成 Notification。
//
// 密钥缺失是**配置问题**（ERROR 级、要运维处理），与「验签不通过」（公开端点的日常噪音）分开。
func xunhuParseNotify(ch Channel, params map[string]string) (*Notification, error) {
	if strings.TrimSpace(ch.Params.AppSecret) == "" {
		return nil, fmt.Errorf("%w: 虎皮椒渠道未配置 AppSecret，拒绝处理回调", ErrNotConfigured)
	}
	if !xunhuVerify(params, ch.Params.AppSecret) {
		return nil, ErrSignInvalid
	}
	outTradeNo := strings.TrimSpace(params["trade_order_id"])
	if outTradeNo == "" {
		return nil, ErrSignInvalid
	}
	kind, ok := xunhuKind(params["status"])
	if !ok {
		// 未知状态不当作成功：把未支付错标成已支付，比漏一笔待人工核对的单更糟。
		kind = NotifyIgnored
	}
	// 金额：支付类回调必须给（否则 pin 不住金额），退款类回调允许缺省（不产生入账）。
	// 缺金额时返回 0，由 order.settle 按「退款类不校验金额」处理。
	var amount int64
	if raw := strings.TrimSpace(params["total_fee"]); raw != "" {
		v, err := ParseCNYToCent(raw)
		if err != nil {
			return nil, err
		}
		amount = v
	} else if kind == NotifyPaid {
		return nil, fmt.Errorf("%w: 支付回调缺少 total_fee", ErrInvalidAmount)
	}
	return &Notification{
		OutTradeNo: outTradeNo,
		// transaction_id 才是支付平台交易号；openid 是虎皮椒自己的订单号，不能混用。
		TradeNo:        strings.TrimSpace(params["transaction_id"]),
		ChannelOrderID: strings.TrimSpace(params["open_order_id"]),
		AmountCent:     amount,
		MerchantID:     strings.TrimSpace(params["appid"]),
		Kind:           kind,
		Raw:            params,
	}, nil
}

// xunhuKind 把回调状态码映射成归一语义。
// OD 已支付 / CD 已退款 / RD 退款中 / UD 退款失败。
func xunhuKind(status string) (NotifyKind, bool) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "OD":
		return NotifyPaid, true
	case "CD":
		return NotifyRefunded, true
	case "RD":
		return NotifyRefunding, true
	case "UD":
		return NotifyRefundFailed, true
	}
	return "", false
}

// xunhuQueryKind 把查单状态映射成归一语义。
// 查单接口的状态取值集与回调不同（可能是 paid/unpaid 而非 OD/CD），两套都要认。
func xunhuQueryKind(status string) (NotifyKind, bool) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "OD", "PAID", "SUCCESS", "TRADE_SUCCESS":
		return NotifyPaid, true
	case "CD", "REFUNDED":
		return NotifyRefunded, true
	case "RD", "REFUNDING":
		return NotifyRefunding, true
	case "UD", "REFUND_FAILED":
		return NotifyRefundFailed, true
	}
	return "", false
}

// xunhuClassify 把 errcode/errmsg 分类成可据以决策的错误。
//
// C4（「订单已存在」的确切码值）尚未实测，目前按 errmsg 文案启发式判断，
// 并在日志里带上原始 errcode，便于确认后改成精确匹配。
// 分类的意义很实际：参数错误要告警给管理员（是配置问题），订单已存在要转查单（不是失败），
// 一律当成「网关异常」会让配置错误无声无息，也会让用户重试永远失败。
func xunhuClassify(code, msg string) error {
	code = strings.TrimSpace(code)
	switch code {
	case "0":
		return nil
	case "":
		return fmt.Errorf("%w: 响应缺少 errcode（msg=%s）", ErrGatewayRejected, msg)
	}
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "exist", "exists", "already", "duplicate", "repeat", "重复", "已存在"):
		return fmt.Errorf("%w: %s（errcode=%s）", ErrOrderExists, msg, code)
	case containsAny(m, "busy", "frequent", "limit", "rate", "频繁", "限流", "繁忙", "稍后"):
		return fmt.Errorf("%w: %s（errcode=%s）", ErrGatewayBusy, msg, code)
	}
	return fmt.Errorf("%w: %s（errcode=%s）", ErrGatewayRejected, msg, code)
}

// xunhuPayEndpoint 归一化支付网关地址。
// 配置给了完整 .html 端点就原样用；给了根地址则按约定推导。
func xunhuPayEndpoint(configured string) string {
	base := strings.TrimRight(strings.TrimSpace(configured), "/")
	if base == "" {
		return ""
	}
	low := strings.ToLower(base)
	if strings.HasSuffix(low, ".html") || strings.Contains(low, "/payment/") {
		return base
	}
	return base + "/payment/do.html"
}

// xunhuQueryEndpoint 推导查单地址：优先用 query_address，否则把 do.html 换成 query.html。
//
// query_address 已从管理台 schema 移除（只剩 pay_address），存量值可能是 http。
// 这里**只接受 https**：查单是能改订单状态的入口，明文链路上的响应可被任意篡改
// （改一个 status 就是白拿额度），回落 pay_address 推导出来的恒为 https。
func xunhuQueryEndpoint(ch Channel) string {
	if q := strings.TrimSpace(ch.Params.QueryAddress); isHTTPSURL(q) {
		return q
	}
	p := strings.TrimRight(strings.TrimSpace(ch.Params.PayAddress), "/")
	if p == "" {
		return ""
	}
	base := xunhuPayEndpoint(p) // 归一化成 .../payment/do.html
	if strings.HasSuffix(strings.ToLower(base), "do.html") {
		return base[:len(base)-len("do.html")] + "query.html"
	}
	return base
}

// isHTTPSURL 判断地址是否 https（大小写不敏感，只认前缀）。
func isHTTPSURL(v string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "https://")
}

// xunhuQueryVerify 校验查单响应签名，返回 (是否验签通过, 是否应当采信)。
//
// 只在「全部字段都是字符串」时才尝试验签：签名是对**原始表单值**做的 md5，
// 数值字段重新序列化后可能与原文不同（1.00 → 1），重算必然误判 ——
// 而误判的后果是「查单永远失败」，用户付了钱永远不到账，比不验签更危险。
func xunhuQueryVerify(node map[string]any, secret string) (verified, signOK bool) {
	params := make(map[string]string, len(node))
	for k, v := range node {
		s, isStr := v.(string)
		if !isStr {
			return false, true // 含数值/嵌套：无法可靠重算，按未验签采信
		}
		params[k] = s
	}
	if strings.TrimSpace(params["hash"]) == "" || strings.TrimSpace(secret) == "" {
		return false, true
	}
	if !xunhuVerify(params, secret) {
		return false, false // 有签名但对不上：不是网关发的（或密钥不对）
	}
	return true, true
}

// postForm 以 application/x-www-form-urlencoded 提交表单并读回响应体。
func postForm(ctx context.Context, endpoint string, fields map[string]string, timeout time.Duration) ([]byte, int, error) {
	// 字面量校验（拒非 http(s)、内嵌凭据、单标签主机）；IP 级校验由 netguard.Client 的 Dial 完成。
	if err := netguard.ValidateURL(endpoint, outboundPolicy); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrNotConfigured, err)
	}
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: 构造请求失败: %v", ErrGatewayRejected, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "lingdong-api/payment")

	resp, err := outboundClient(timeout, outboundPolicy).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("支付网关请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取支付网关响应失败: %w", err)
	}
	return body, resp.StatusCode, nil
}

// decodeJSONObject 解析 JSON 对象，同时给出「原始值的字符串形式」用于验签。
//
// 必须用 UseNumber：签名是对**字符串值**拼接，若把数字转成 float64 再格式化，
// `1e3` 会变成 `1000`，与上游签名时的字面量不一致，验签必然失败。
func decodeJSONObject(body []byte) (map[string]any, map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, nil, err
	}
	str := make(map[string]string, len(raw))
	for k, v := range raw {
		str[k] = jsonScalar(v)
	}
	return raw, str, nil
}

// jsonScalar 把 JSON 标量转成字符串；嵌套对象/数组保留为紧凑 JSON（正常响应里不会出现）。
func jsonScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// pickQueryNode 兼容「字段在顶层」与「字段在 data 里」两种查单响应。
func pickQueryNode(raw map[string]any) map[string]any {
	if d, ok := raw["data"].(map[string]any); ok {
		if _, has := d["status"]; has {
			return d
		}
	}
	return raw
}

// strOf 读取节点里的字符串字段（数字/布尔按原始字面量转换）。
func strOf(node map[string]any, key string) string {
	if node == nil {
		return ""
	}
	return jsonScalar(node[key])
}

// randomNonce 生成 n 位随机串（字母数字）。
//
// 不能用时间戳：官方 Demo 直接复用 `time` 作为 nonce_str，同秒请求完全重复，
// 防重放形同虚设。这里用 crypto/rand，取模带来的偏差对 nonce 无影响（只需不可预测）。
func randomNonce(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 不可用极罕见；退化仍要保证「不是固定值」，否则等于没有 nonce。
		seed := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(seed >> (uint(i%8) * 8))
		}
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// containsAny 判断 s 是否包含任一副串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
