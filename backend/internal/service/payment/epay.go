package payment

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// epaySign 计算易支付 MD5 签名。
//
// 规则：剔除 sign / sign_type 与空值 → 参数名 ASCII 升序 → 用 `&` 连接成 `a=1&b=2`
// （**末尾不带 `&`**）→ **直接**拼接商户密钥 → MD5 小写。
//
// 末尾那个 `&` 是易支付签名最常见的一处错：写成 `a=1&b=2&` + key 的话，签名串多一个
// 字节，上游一律判签名错误、收银台打不开，且本地回调验签也会把合法回调判成伪造
// （两条路径共用本函数，见 epayVerify）。`xunhupay.go` 的签名是同一类规则，可对照。
func epaySign(params map[string]string, key string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || k == "sign_type" || v == "" {
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
	b.WriteString(key)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// epayVerify 校验回调签名（大小写不敏感、缺签名直接拒绝）。
//
// **密钥为空必须 fail-closed**：`epaySign(params, "")` 就是「参数串联后的裸 MD5」，
// 任何人都能自己算出来 —— 一旦放行，一条「启用但密钥被清空/历史空值」的渠道就等于
// 一个公开的「免费充值」端点（伪造 out_trade_no + money + 自算签名即可入账）。
//
// L1：用 subtle.ConstantTimeCompare 恒定时间比较，避免时序侧信道泄露签名前缀。
// 先比长度再比内容：长度不等时 ConstantTimeCompare 仍会返回 0，但提前短路省一次哈希无关紧要，
// 关键是签名比对本身不能因「逐字节早退」泄露信息（MD5 签名长度恒 32，长度比较无信息量）。
func epayVerify(params map[string]string, key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	got := strings.ToLower(strings.TrimSpace(params["sign"]))
	want := epaySign(params, key)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// Sign 是 epaySign 的导出名（供测试与对账复算使用）。
func Sign(params map[string]string, key string) string { return epaySign(params, key) }

// Verify 是 epayVerify 的导出名。
func Verify(params map[string]string, key string) bool { return epayVerify(params, key) }

// epayCreate 生成本地签名的收银台跳转地址（**零网络依赖**）。
//
// 易支付是 GET 跳转式收银台，页面自适应 PC/移动端，所以只有 PayURL、没有二维码。
func epayCreate(ch Channel, req CreateRequest) (*CreateResult, error) {
	if strings.TrimSpace(ch.Params.PayAddress) == "" ||
		strings.TrimSpace(ch.Params.PID) == "" || strings.TrimSpace(ch.Params.Key) == "" {
		return nil, ErrNotConfigured
	}
	payURL, err := cashierURL(ch, req.Method, req.OutTradeNo, req.AmountCent, req.Subject, req.NotifyURL, req.ReturnURL)
	if err != nil {
		return nil, err
	}
	return &CreateResult{
		PayURL:    payURL,
		ExpiresAt: effectiveExpireAt(req.ExpireAt),
	}, nil
}

// epayParseNotify 校验易支付回调签名并映射成 Notification。
//
// 密钥缺失属于**配置问题**，必须与「验签不通过」区分开：
// 前者要让运维在日志里一眼看见（ERROR + 明确文案），后者只是公开端点的日常噪音（WARN）。
func epayParseNotify(ch Channel, params map[string]string) (*Notification, error) {
	if strings.TrimSpace(ch.Params.Key) == "" {
		return nil, fmt.Errorf("%w: 易支付渠道未配置密钥，拒绝处理回调", ErrNotConfigured)
	}
	if !epayVerify(params, ch.Params.Key) {
		return nil, ErrSignInvalid
	}
	outTradeNo := strings.TrimSpace(params["out_trade_no"])
	if outTradeNo == "" {
		// 走到这里说明**验签已经通过**，所以缺字段是上游报文问题而不是伪造。
		// 不能复用 ErrSignInvalid：那会让上层按「签名被伪造」记 Warn，排障方向跑偏。
		return nil, fmt.Errorf("%w: 易支付回调缺少 out_trade_no", ErrGatewayRejected)
	}
	amount, err := ParseCNYToCent(params["money"])
	if err != nil {
		return nil, fmt.Errorf("%w: 易支付回调金额 %q 不是合法的人民币金额", err, params["money"])
	}
	kind := NotifyIgnored
	switch strings.ToUpper(strings.TrimSpace(params["trade_status"])) {
	case "TRADE_SUCCESS", "SUCCESS":
		// TRADE_SUCCESS 是易支付系（彩虹/聚合等）的标准成功值；SUCCESS 兼容少数自研网关。
		// 只认这两个：其它值（WAIT_BUYER_PAY / TRADE_CLOSED）一律不入账 —— 回调不参与
		// 状态机，到期关单由 ExpirePending 负责，避免与 TTL 抢同一行。
		kind = NotifyPaid
	}
	return &Notification{
		OutTradeNo: outTradeNo,
		TradeNo:    strings.TrimSpace(params["trade_no"]),
		AmountCent: amount,
		MerchantID: strings.TrimSpace(params["pid"]),
		Kind:       kind,
		Raw:        params,
	}, nil
}

// epayType 把支付方式翻译成易支付网关的 `type` 参数。
//
// 官方取值是英文码（alipay/tenpay/qqpay/wxpay），**不是中文**。把中文
// 「微信支付」原样传给上游会被判为非法支付方式而拒单 —— 这是本次修复的核心。
// 白名单外一律拒绝，绝不透传用户输入。
func epayType(m Method) (string, error) {
	switch m {
	case MethodWechat:
		return "wxpay", nil
	case MethodAlipay:
		return "alipay", nil
	}
	return "", ErrUnsupportedMethod
}

// cashierURL 生成易支付收银台地址；notifyURL 为空时回落到渠道配置的 callback_url。
func cashierURL(ch Channel, m Method, outTradeNo string, amountCent int64, subject, notifyURL, returnURL string) (string, error) {
	typ, err := epayType(m)
	if err != nil {
		return "", err
	}
	if notifyURL == "" {
		notifyURL = ch.Params.CallbackURL
	}
	params := map[string]string{
		"pid":          ch.Params.PID,
		"type":         typ,
		"out_trade_no": outTradeNo,
		"notify_url":   notifyURL,
		"return_url":   returnURL,
		"name":         subject,
		"money":        FormatCentToCNY(amountCent),
		"sign_type":    "MD5",
	}
	params["sign"] = epaySign(params, ch.Params.Key)

	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return submitEndpoint(ch.Params.PayAddress) + "?" + q.Encode(), nil
}

// CashierURL 是 cashierURL 的导出名；金额参数是**分**。
func CashierURL(ch Channel, m Method, outTradeNo string, amountCent int64, subject, notifyURL, returnURL string) (string, error) {
	return cashierURL(ch, m, outTradeNo, amountCent, subject, notifyURL, returnURL)
}

// submitEndpoint 归一化网关地址：配置已指向提交端点时原样使用，否则追加 /submit.php。
//
// 判据只看**路径末段**，不能在整个 URL 上找 "submit"：域名里带 submit 的网关
// （https://submit.xxx.com/）会被误判成「已经指向提交端点」，于是下单打到站点根
// 路径 —— 上游返回 404 或一段首页 HTML，而报错信息只说「响应无法解析」。
func submitEndpoint(payAddress string) string {
	base := strings.TrimRight(strings.TrimSpace(payAddress), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(base), ".php") {
		return base
	}
	if u, err := url.Parse(base); err == nil {
		last := u.Path[strings.LastIndex(u.Path, "/")+1:]
		if strings.Contains(strings.ToLower(last), "submit") {
			return base
		}
	}
	return base + "/submit.php"
}
