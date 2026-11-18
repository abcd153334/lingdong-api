package payment

import (
	"bytes"
	"net/http"
)

// NotifyRequest 是一次支付回调的**完整**原始输入。
//
// 为什么不能只有 `map[string]string`：
//   - 微信 APIv3 的回调是 JSON，签名对象是 `timestamp\nnonce\nbody\n` 的
//     **逐字节原文**（多一个空格就验签失败），拿不到 body 就全部拒绝；
//   - 支付宝的回调是 form，验签要用 `ParseForm` 解码后的值；
//   - 两端的幂等与取证都需要原始字节落 `notify_raw`。
//
// 所以 controller 统一「先读原始字节 → 还原 Body → 再解析表单」，把三样一起传下来。
type NotifyRequest struct {
	// Params 是表单/query 参数（form-urlencoded 回调用；JSON 回调为空）。
	Params map[string]string
	// Body 是请求体原始字节（JSON 回调的验签原文）。
	Body []byte
	// Header 是请求头（微信的 Wechatpay-Serial/Signature/Timestamp/Nonce 在这里）。
	Header http.Header
}

// NotifyAck 是回调应答。
//
// 不同协议对「成功」的定义完全不同，不能用同一个 "success"：
//   - 易支付/虎皮椒/支付宝：body 等于 `success`；
//   - 微信 APIv3：HTTP 2xx（推荐 204 无包体）；非 2xx 会重试 15 次。
//
// 且**失败也不能统一**：支付宝要求 HTTP 200 + 非 success（4xx 会被判地址不可用），
// 微信要求 4xx/5xx + JSON 错误体。
type NotifyAck struct {
	Status      int
	ContentType string
	Body        string
}

// ParseNotifyRequest 按协议验签并把回调映射成 Notification。
//
// 调用方（controller/order）只负责「取到原始输入」与「查订单 / 入账」，
// 协议差异全部收敛在这里。
func ParseNotifyRequest(ch Channel, req NotifyRequest) (*Notification, error) {
	switch ParseProtocol(ch.Protocol) {
	case ProtocolEpay:
		return epayParseNotify(ch, req.Params)
	case ProtocolXunhuPay:
		return xunhuParseNotify(ch, req.Params)
	case ProtocolWxpay:
		return wxpayParseNotify(ch, req)
	case ProtocolAlipay:
		return alipayParseNotify(ch, req)
	case ProtocolStripe:
		return stripeParseNotify(ch, req)
	}
	return nil, ErrUnsupported
}

// notifyHTTPRequest 用原始字节与 header 重建 *http.Request（供 SDK 的通知处理器使用）。
//
// 必须是**新的** Request：SDK 会 ReadAll 并回填 Body，直接复用 gin 的 request
// 会在验签失败时留下半读状态；且 SDK 只关心 Header / Body / Method，
// URL 只是占位（回调处理里不会出网）。
func notifyHTTPRequest(req NotifyRequest) *http.Request {
	r, err := http.NewRequest(http.MethodPost, "https://localhost/notify", bytes.NewReader(req.Body))
	if err != nil {
		// bytes.Reader 不会让 NewRequest 失败；这里只做防御。
		r, _ = http.NewRequest(http.MethodPost, "https://localhost/notify", nil)
	}
	if req.Header != nil {
		r.Header = req.Header.Clone()
	}
	return r
}
