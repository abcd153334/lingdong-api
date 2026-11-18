package payment

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 支付宝官方直连（当面付 precreate）。
//
// **支付宝没有官方 Go SDK**（github.com/alipay/alipay-sdk-go 不存在），
// 本文件按官方 RSA2 规范用标准库实现。
//
// 三套签名规则必须三个独立函数，绝不能「复用着改改」——这是支付宝接入最容易
// 「本地全过、生产全挂」的地方：
//
//	请求签名     剔除 sign + 空值，sign_type **参与**签名
//	异步通知验签 剔除 sign + sign_type + 空值，用解码后的原值重算
//	同步响应验签 对 xxx_response 节点的**原始 JSON 文本**验签，绝不重新 Marshal

// alipayCST 是支付宝要求的时区。
//
// 必须显式指定：`timestamp` 的格式是 `2006-01-02 15:04:05` 且按东八区解释，
// 服务器跑 UTC 时会整体偏 8 小时，表现为「签名通过但报文已过期」。
var alipayCST = time.FixedZone("CST", 8*3600)

// alipayClient 是一个渠道解析后的可调用客户端。
type alipayClient struct {
	AppID     string
	Gateway   string
	PrivKey   *rsa.PrivateKey
	PublicKey *rsa.PublicKey
}

// parseAlipayChannel 解析渠道参数（失败一律 ErrNotConfigured，属配置问题）。
func parseAlipayChannel(ch Channel) (*alipayClient, error) {
	appID := strings.TrimSpace(ch.Params.AppID)
	if appID == "" {
		return nil, fmt.Errorf("%w: 缺少应用 AppID", ErrNotConfigured)
	}
	gateway := strings.TrimSpace(ch.Params.PayAddress)
	if gateway == "" {
		gateway = DefaultAlipayAddress
	}
	priv, err := LoadRSAPrivateKey(ch.Params.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("%w: 应用私钥不可用: %v", ErrNotConfigured, err)
	}
	pub, err := LoadRSAPublicKey(ch.Params.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: 支付宝公钥不可用: %v", ErrNotConfigured, err)
	}
	return &alipayClient{AppID: appID, Gateway: gateway, PrivKey: priv, PublicKey: pub}, nil
}

/* -------------------- 签名与验签（三套规则） -------------------- */

// alipaySignContent 把参数拼成待签名串：剔除 skip 中的键与空值，key 按 ASCII 升序，末尾无 `&`。
func alipaySignContent(params map[string]string, skip ...string) string {
	skipSet := make(map[string]bool, len(skip))
	for _, k := range skip {
		skipSet[k] = true
	}
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if skipSet[k] || strings.TrimSpace(v) == "" {
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
	return b.String()
}

// alipaySHA256Sign 用 RSA 私钥做 SHA256withRSA 签名并 base64。
func alipaySHA256Sign(content string, key *rsa.PrivateKey) (string, error) {
	if key == nil {
		return "", fmt.Errorf("%w: 私钥为空", ErrNotConfigured)
	}
	sum := sha256.Sum256([]byte(content))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("支付宝签名失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// alipayVerifyRaw 用支付宝公钥验证一段原始文本的签名。
func alipayVerifyRaw(content, sign string, key *rsa.PublicKey) error {
	if key == nil {
		return fmt.Errorf("%w: 公钥为空", ErrNotConfigured)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sign))
	if err != nil {
		return fmt.Errorf("%w: 签名不是合法 base64", ErrSignInvalid)
	}
	sum := sha256.Sum256([]byte(content))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return ErrSignInvalid
	}
	return nil
}

// alipaySignRequest 生成请求签名（规则一：sign_type 参与，剔除 sign 与空值）。
func alipaySignRequest(params map[string]string, key *rsa.PrivateKey) (string, error) {
	return alipaySHA256Sign(alipaySignContent(params, "sign"), key)
}

// alipayVerifyNotify 验证异步通知（规则二：剔除 sign 与 sign_type）。
func alipayVerifyNotify(params map[string]string, key *rsa.PublicKey) error {
	sign := strings.TrimSpace(params["sign"])
	if sign == "" {
		return fmt.Errorf("%w: 通知缺少 sign", ErrSignInvalid)
	}
	// ParseForm 会把未编码的 `+` 解成空格；base64 里的 `+` 必须还原，
	// 否则「签名正确但解码失败」，且报错信息完全指不到原因。
	if strings.Contains(sign, " ") {
		sign = strings.ReplaceAll(sign, " ", "+")
	}
	return alipayVerifyRaw(alipaySignContent(params, "sign", "sign_type"), sign, key)
}

/* -------------------- 请求与响应 -------------------- */

// alipayResponseKey 返回响应节点名（`alipay.trade.precreate` → `alipay_trade_precreate_response`）。
func alipayResponseKey(method string) string {
	return strings.ReplaceAll(method, ".", "_") + "_response"
}

// alipayCall 发一次 OpenAPI 调用，验签后返回 `xxx_response` 的原始字节。
//
// 顺序铁律：**先验签、再解释业务码**。反过来等于允许中间人把 code 改成 10000，
// 我们就会给用户下发一个必然失败的二维码。
func alipayCall(ctx context.Context, c *alipayClient, method string, biz any, notifyURL string, timeout time.Duration) (json.RawMessage, error) {
	bizRaw, err := json.Marshal(biz)
	if err != nil {
		return nil, fmt.Errorf("支付宝请求参数序列化失败: %w", err)
	}

	params := map[string]string{
		"app_id":      c.AppID,
		"method":      method,
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   time.Now().In(alipayCST).Format(alipayTimestampLayout),
		"version":     "1.0",
		"biz_content": string(bizRaw),
	}
	if notifyURL != "" {
		params["notify_url"] = notifyURL
	}
	sign, err := alipaySignRequest(params, c.PrivKey)
	if err != nil {
		return nil, err
	}
	params["sign"] = sign

	body, status, err := postForm(ctx, c.Gateway, params, timeout)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		// 非 200 一般是网关层故障（支付宝自身故障时也会这么返回）。
		return nil, fmt.Errorf("%w: 网关 HTTP %d", ErrGatewayBusy, status)
	}

	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("支付宝响应不是合法 JSON: %w", err)
	}
	nodeKey := alipayResponseKey(method)
	node, ok := wrapper[nodeKey]
	if !ok {
		// 参数/签名错误时支付宝返回 error_response，没有业务节点。
		if e, exists := wrapper["error_response"]; exists {
			return nil, alipayBizError(e, method)
		}
		return nil, fmt.Errorf("%w: 响应缺少 %s 节点", ErrGatewayRejected, nodeKey)
	}

	// 同步响应验签：必须用节点**原始字节**（RawMessage 逐字节保留键序与转义）。
	// 反序列化后再 Marshal 会改变键序/转义，签名必然不匹配。
	//
	// **缺签名必须拒绝（fail-closed）**：以前写成「有 sign 才验，没有就放行」，等于把
	// 「被中间人剥掉签名字段」与「签名有效」当成同一件事。可被改写的字段里最危险的是
	// 下单响应的 `qr_code`（用户会照着它付钱）与 `total_amount`，改掉就是替攻击者收款。
	// 同理，`sign` 为 null / 数字 / 对象时 json.Unmarshal 会失败，那一支也必须拒绝。
	//
	// 不会误杀：`error_response` 分支在上方已 return（参数/签名错误时支付宝不带业务节点），
	// 能走到这里的响应只要请求带了 sign 就必然带 sign。
	// **待验证（真单）**：业务失败节点（如查单 code=40004 acq.trade_not_exist）是否仍带 sign。
	// 若实测发现不带，则在此处改为「仅当 code == 10000 时必须验签，失败节点放行」，
	// 并在注释里写明实测日期与依据 —— 不要凭猜测放宽。
	signBlock, exists := wrapper["sign"]
	if !exists {
		return nil, fmt.Errorf("%w: 支付宝响应缺少 sign 字段", ErrSignInvalid)
	}
	var sig string
	if err := json.Unmarshal(signBlock, &sig); err != nil || strings.TrimSpace(sig) == "" {
		return nil, fmt.Errorf("%w: 支付宝响应 sign 格式非法", ErrSignInvalid)
	}
	if err := alipayVerifyRaw(string(node), sig, c.PublicKey); err != nil {
		return nil, fmt.Errorf("%w: 支付宝响应验签失败", ErrSignInvalid)
	}
	return node, nil
}

// alipayNode 是 `xxx_response` 的通用字段（成功/失败共用）。
type alipayNode struct {
	Code    string `json:"code"`
	Msg     string `json:"msg"`
	SubCode string `json:"sub_code"`
	SubMsg  string `json:"sub_msg"`
}

// alipayBizError 把 error_response 转成错误。
func alipayBizError(raw json.RawMessage, method string) error {
	var n alipayNode
	_ = json.Unmarshal(raw, &n)
	return alipayClassify(n.Code, n.SubCode, alipayDetail(n), method)
}

// alipayDetail 拼「错误码/子码 + 文案」。
//
// sub_code 必须带上：它是排查的唯一线索（`isv.invalid-app-id` 与
// `ACQ.SELLER_BALANCE_NOT_ENOUGH` 完全不是一类问题），只回中文文案等于把线索丢了。
func alipayDetail(n alipayNode) string {
	code := strings.TrimSpace(n.Code)
	sub := strings.TrimSpace(n.SubCode)
	msg := firstNonEmpty(n.SubMsg, n.Msg)
	switch {
	case sub != "" && code != "":
		return fmt.Sprintf("%s/%s %s", code, sub, msg)
	case sub != "":
		return fmt.Sprintf("%s %s", sub, msg)
	case code != "":
		return fmt.Sprintf("%s %s", code, msg)
	}
	return msg
}

// isAlipayTradeNotExist 判断错误是否为「上游没有这笔订单」。
func isAlipayTradeNotExist(err error) bool {
	var notExist *alipayTradeNotExist
	return errors.As(err, &notExist)
}

// alipayClassify 是「上游错误码 → 本包错误集」的唯一映射。
func alipayClassify(code, subCode, detail, method string) error {
	sub := strings.ToLower(strings.TrimSpace(subCode))
	switch {
	case sub == "acq.trade_not_exist":
		// 查单专用：订单在上游还不存在 = 确认未支付，不是错误。
		return &alipayTradeNotExist{detail: detail}
	case sub == "acq.trade_has_success":
		return fmt.Errorf("%w: %s", ErrOrderExists, detail)
	case code == "20000" || sub == "acq.system_error":
		return fmt.Errorf("%w: %s", ErrGatewayBusy, detail)
	case strings.HasPrefix(sub, "isv.invalid-signature"),
		strings.HasPrefix(sub, "isv.invalid-app-id"),
		strings.HasPrefix(sub, "isv.insufficient-isv-permissions"),
		strings.HasPrefix(sub, "isv.missing-"),
		sub == "aop.invalid-app-auth-token":
		return fmt.Errorf("%w: %s", ErrGatewayRejected, detail)
	}
	return fmt.Errorf("%w: %s（%s）", ErrGatewayRejected, detail, method)
}

// alipayTradeNotExist 表示「上游没有这笔订单」（查单时属正常未支付态）。
type alipayTradeNotExist struct{ detail string }

func (e *alipayTradeNotExist) Error() string { return "支付宝订单不存在: " + e.detail }
func (e *alipayTradeNotExist) Unwrap() error { return ErrGatewayRejected }

/* -------------------- 下单 / 查单 -------------------- */

// alipayPrecreateBiz 是当面付预下单的业务参数。
//
// 用具名结构体而不是 map：金额是「元」的字符串，用 float 会写出 `0.1+0.2` 那种值，
// 而上游对 `total_amount` 的小数位是严格校验的。字符串类型让这种错误编译期就不成立。
type alipayPrecreateBiz struct {
	OutTradeNo  string `json:"out_trade_no"`
	TotalAmount string `json:"total_amount"` // 元，两位小数（FormatCentToCNY）
	Subject     string `json:"subject"`
	// QrCodeTimeoutExpress 是二维码有效期（1m–2h）。与订单 TTL 对齐，不做 2 小时。
	QrCodeTimeoutExpress string `json:"qr_code_timeout_express,omitempty"`
}

// alipayCreate 走当面付预下单，返回 qr_code（内容型二维码）。
func alipayCreate(ctx context.Context, ch Channel, req CreateRequest) (*CreateResult, error) {
	c, err := parseAlipayChannel(ch)
	if err != nil {
		return nil, err
	}
	expire := effectiveExpireAt(req.ExpireAt)
	biz := alipayPrecreateBiz{
		OutTradeNo:           req.OutTradeNo,
		TotalAmount:          FormatCentToCNY(req.AmountCent),
		Subject:              req.Subject,
		QrCodeTimeoutExpress: alipayTimeoutExpress(expire),
	}
	node, err := alipayCall(ctx, c, alipayMethodPrecreate, biz, req.NotifyURL, 8*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		alipayNode
		QrCode  string `json:"qr_code"`
		TradeNo string `json:"trade_no"`
	}
	if err := json.Unmarshal(node, &resp); err != nil {
		return nil, fmt.Errorf("支付宝下单响应解析失败: %w", err)
	}
	if resp.Code != "10000" {
		return nil, alipayClassify(resp.Code, resp.SubCode, alipayDetail(resp.alipayNode), alipayMethodPrecreate)
	}
	if strings.TrimSpace(resp.QrCode) == "" {
		return nil, ErrNoPayURL
	}
	// PayURL 必须留空：前端在 qr_ref 分支取不到时会退回 pay_url 按钮，
	// 而支付宝的 qr_code 是「要求打开支付宝扫码」的页面，点开也付不了。
	return &CreateResult{
		QRRef:     resp.QrCode,
		ExpiresAt: expire,
	}, nil
}

// alipayQuery 查单。
//
// 返回 (nil, nil) 表示确认未支付；(n, nil) 且 Kind==paid 表示已支付。
func alipayQuery(ctx context.Context, ch Channel, outTradeNo string) (*Notification, error) {
	c, err := parseAlipayChannel(ch)
	if err != nil {
		return nil, err
	}
	biz := map[string]string{"out_trade_no": outTradeNo}
	node, err := alipayCall(ctx, c, alipayMethodQuery, biz, "", 5*time.Second)
	if err != nil {
		if isAlipayTradeNotExist(err) {
			return nil, nil // 上游还没有这笔单 = 确认未支付
		}
		return nil, err
	}
	var resp alipayTradeResp
	if err := json.Unmarshal(node, &resp); err != nil {
		return nil, fmt.Errorf("支付宝查单响应解析失败: %w", err)
	}
	if resp.Code != "10000" {
		qerr := alipayClassify(resp.Code, resp.SubCode, alipayDetail(resp.alipayNode), alipayMethodQuery)
		if isAlipayTradeNotExist(qerr) {
			return nil, nil
		}
		return nil, qerr
	}
	if resp.TradeStatus == "" {
		return nil, nil
	}
	amount, aerr := ParseCNYToCent(resp.TotalAmount)
	if aerr != nil {
		return nil, fmt.Errorf("支付宝查单金额不合法: %w", aerr)
	}
	return &Notification{
		OutTradeNo: resp.OutTradeNo,
		TradeNo:    resp.TradeNo,
		AmountCent: amount,
		MerchantID: resp.AppID,
		Kind:       alipayKind(resp.TradeStatus, resp.SendPayDate),
		Raw: map[string]string{
			"trade_status":  resp.TradeStatus,
			"total_amount":  resp.TotalAmount,
			"trade_no":      resp.TradeNo,
			"out_trade_no":  resp.OutTradeNo,
			"app_id":        resp.AppID,
			"send_pay_date": resp.SendPayDate,
			"source":        "query",
		},
	}, nil
}

// alipayTradeResp 是 query / 通知共用的交易字段。
type alipayTradeResp struct {
	alipayNode
	OutTradeNo  string `json:"out_trade_no"`
	TradeNo     string `json:"trade_no"`
	TradeStatus string `json:"trade_status"`
	TotalAmount string `json:"total_amount"`
	AppID       string `json:"app_id"`
	SendPayDate string `json:"send_pay_date"`
	RefundFee   string `json:"refund_fee"`
}

// alipayTimeoutExpress 把剩余时间转成 `Nm`（支付宝最小 1m、最大 2h）。
func alipayTimeoutExpress(expire time.Time) string {
	d := time.Until(expire)
	minutes := int(d.Minutes())
	if d%time.Minute != 0 {
		minutes++
	}
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 120 {
		minutes = 120
	}
	return fmt.Sprintf("%dm", minutes)
}

// alipayKind 把交易状态映射成归一语义。
//
// TRADE_CLOSED 有两种成因，用 send_pay_date 区分：
//   - 有支付时间 → 曾经付过，之后被关闭（全额退款后会这样）→ 按「已退款」处理，
//     让 order.onRefunded 的三分支去决定「终结 refunding」还是「转人工」；
//   - 无支付时间 → 单纯超时关闭 → ignored（绝不当成功）。
func alipayKind(status, sendPayDate string) NotifyKind {
	switch status {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		return NotifyPaid
	case "TRADE_CLOSED":
		if strings.TrimSpace(sendPayDate) != "" {
			return NotifyRefunded
		}
		return NotifyIgnored
	case "WAIT_BUYER_PAY":
		return NotifyIgnored
	}
	return NotifyIgnored
}
