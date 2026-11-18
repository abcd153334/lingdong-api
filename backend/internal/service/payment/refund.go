package payment

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RefundStatus 是**归一后**的退款状态。
//
// 上游（虎皮椒）的退款状态码是 OD/CD/RD/UD，与订单支付状态共用一套字母，
// 但语义完全不同（CD 在查单里是「已取消/已退款」，在退款里是「已退款」）。
// 任何跨协议比较都必须用本类型，不得直接透传上游状态码。
type RefundStatus string

const (
	// RefundAccepted 上游已受理但结果未定（RD 退款中、OD 尚未执行、或网络超时）。
	//
	// **它不等于失败**：此时额度已扣、订单停在 refunding，等待回调/查单/人工确认。
	RefundAccepted RefundStatus = "pending"
	// RefundDone 上游确认退款完成（CD）。
	RefundDone RefundStatus = "success"
	// RefundDeclined 上游确认退款失败（UD 或明确拒绝）。只有它能安全回滚额度。
	RefundDeclined RefundStatus = "failed"
)

// RefundRequest 是协议无关的退款入参。
//
// **本系统只做全额退款**：虎皮椒退款接口没有金额参数（refund_fee 只在响应里回传），
// 部分退款在上游根本无法表达。AmountCent 仅用于校验上游回传金额，
// 不参与签名、也不下发给网关。
type RefundRequest struct {
	OutTradeNo     string // 商户订单号（与 ChannelOrderID 二选一，本系统两个都传）
	ChannelOrderID string // 上游内部单号
	// PaymentID 是**原路退款真正要用的凭据**（目前只有 Stripe 需要：pi_...）。
	// 与 ChannelOrderID 不同 —— Stripe 的 ChannelOrderID 是 checkout session（cs_...），
	// 拿它去调退款接口必被拒。其它协议留空，退款用 ChannelOrderID / OutTradeNo。
	PaymentID  string
	AmountCent int64  // 订单金额（分），用于核对 refund_fee
	Reason     string // 已由 SanitizeReason 清洗
	// Currency 是订单币种（CNY/USD）。空值视为 CNY（存量协议不退美元）。
	// Stripe 退款需要它（退款金额的币种必须与支付一致）。
	Currency string

	// RefundNo 是我方退款单号（微信 out_refund_no / 支付宝 out_request_no）。
	//
	// 为什么必须由调用方预先生成并落库（而不是等上游返回再存）：两端都要求
	// 「同一笔退款重试时必须复用原退款单号」，否则会出现「第一次超时、第二次
	// 又退一遍」。响应丢失时如果本地没有号，连查都没法查，退款会永远停在 refunding。
	//
	// 空值表示由本包用 DeriveRefundNo(OutTradeNo) 派生（纯函数，重试必然同号）。
	RefundNo string
	// NotifyURL 是退款结果通知地址（微信必填才收退款回调；支付宝忽略）。
	NotifyURL string
}

// DeriveRefundNo 从商户订单号派生退款单号。
//
// 纯函数是刻意的：`Refund` 对 refunding 直接拒绝，所以「重试」只会发生在
// 明确失败（refund_status=failed，订单回到 paid）之后；此时再发起必须用**同一个**号，
// 上游才会返回上次的结果而不是又退一笔。
//
// 长度：`LD`+ULID = 28 字符，加 `RF` 前缀 30 字符 —— 微信 out_refund_no(64)
// 与支付宝 out_request_no(64) 都满足。
func DeriveRefundNo(outTradeNo string) string {
	s := strings.TrimSpace(outTradeNo)
	if s == "" {
		return ""
	}
	return "RF" + s
}

// RefundResult 是退款调用/查单的归一结果。
type RefundResult struct {
	Status       RefundStatus
	RefundNo     string // 上游退款单号 out_refund_no
	RefundedCent int64  // 上游回传的退款金额（分），与订单金额不等时不得判定成功
	TradeNo      string // 支付平台交易号
	Message      string // 上游 errmsg（失败原因/处理中说明）
	Raw          map[string]string
}

var (
	// ErrRefundRejected 上游**明确拒绝**退款（参数/签名/订单不存在等）：未产生退款，可安全回滚额度。
	ErrRefundRejected = errors.New("payment: 退款被网关拒绝")
	// ErrRefundUnknown 退款结果未知（超时、连接中断、响应不可解析、返回体验签失败）。
	//
	// **绝不能**当作失败处理：一旦回滚额度，而钱其实已经退出去了，就是双向损失。
	// 正确动作是保持 refunding，等回调、查单或人工到网关后台核对。
	ErrRefundUnknown = errors.New("payment: 退款结果未知")
)

// RefundSupported 判断该协议是否实现了原路退款。
//
// 易支付各家的退款接口路径与签名都不统一（`act=refund` / `api.php?act=refund` 混用，
// 有的还要求 TLS 双向），未实测前不接入：宁可让运营走「仅回退平台余额」，
// 也不能发一笔我们无法验证结果的退款请求。
//
// 入参是**字符串**而不是 Protocol：历史调用方与单测传的是 `"XunhuPay"` 这类
// 大小写不一的原始值，靠 ParseProtocol 归一化，不能收窄成枚举。
func RefundSupported(protocol string) bool {
	switch ParseProtocol(protocol) {
	case ProtocolXunhuPay, ProtocolWxpay, ProtocolAlipay, ProtocolStripe:
		return true
	}
	return false
}

// Refund 发起原路退款。
//
// 调用方必须先完成「扣额度 + 置 refunding」再调用本函数：
// 反过来的话，退款成功后扣额度失败，用户既拿到钱又保留额度。
func Refund(ctx context.Context, ch Channel, req RefundRequest) (*RefundResult, error) {
	if req.AmountCent <= 0 {
		return nil, ErrInvalidAmount
	}
	if strings.TrimSpace(req.OutTradeNo) == "" && strings.TrimSpace(req.ChannelOrderID) == "" {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(req.RefundNo) == "" {
		req.RefundNo = DeriveRefundNo(req.OutTradeNo)
	}
	switch ParseProtocol(ch.Protocol) {
	case ProtocolXunhuPay:
		return xunhuRefund(ctx, ch, req)
	case ProtocolWxpay:
		return wxpayRefund(ctx, ch, req)
	case ProtocolAlipay:
		return alipayRefund(ctx, ch, req)
	case ProtocolStripe:
		return stripeRefund(ctx, ch, req)
	}
	return nil, ErrUnsupported
}

// RefundQuery 查询一笔退款的上游终态。
//
// 返回值约定（与 Refund 一致）：RefundDone / RefundDeclined 是终态，
// RefundAccepted 表示仍在处理中或金额对不上（**不是失败**，绝不能回滚额度）。
//
// amountCent 是订单金额（分）：两端都要用它核对「退的是不是全额」，
// 对不上只能转人工，不能判成功（会漏掉上游退少的情况）。
func RefundQuery(ctx context.Context, ch Channel, outTradeNo, refundNo string, amountCent int64) (*RefundResult, error) {
	switch ParseProtocol(ch.Protocol) {
	case ProtocolWxpay:
		return wxpayRefundQuery(ctx, ch, refundNo, amountCent)
	case ProtocolAlipay:
		return alipayRefundQuery(ctx, ch, outTradeNo, refundNo, amountCent)
	}
	return nil, ErrUnsupported
}

// refundAmountMismatch 判定「渠道回报的退款金额」与「本次请求的金额」是否不符。
//
// 这是**唯一**一处比较表达式：退款申请（Refund）与退款查询（RefundQuery）两条路径、
// 四条渠道共用。以前四处各写一遍，Stripe 那条干脆没写 —— 同一个判断有四种写法，
// 迟早会出现「一条渠道判成功、另一条判未知」的口径分裂。
//
// 任一侧 <= 0 视为「渠道没报金额」→ 不判不符。不猜：把「上游没回金额」
// 当成不符会把正常退款全部推进人工队列。
func refundAmountMismatch(wantCent, gotCent int64) bool {
	if wantCent <= 0 || gotCent <= 0 {
		return false
	}
	return gotCent != wantCent
}

// SanitizeReason 清洗退款原因。
//
// 三重约束：
//   - 上游 reason 上限 80 个字符，超出会被网关截断（我们算的 hash 与上游重算的不一致 → invalid sign），
//     必须在**签名之前**按 rune 截断；
//   - `&` `=` `#` 是 `k=v&...` 拼接与 form 编码的边界字符，剔除掉避免签名被自身内容破坏；
//   - 控制字符与零宽字符会破坏微信退款消息的排版，一并剔除。
func SanitizeReason(raw string) string {
	const maxReasonRunes = 80
	var b strings.Builder
	n := 0
	for _, r := range raw {
		if n >= maxReasonRunes {
			break
		}
		switch r {
		case '&', '=', '#':
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r > 0xFFFF {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// TruncateUTF8Bytes 按 UTF-8 边界把字符串截断到**字节数**上限。
//
// 为什么 SanitizeReason 之外还需要它：SanitizeReason 限 80 **rune**，
// 而微信退款的 reason 上限是 80 **字节** —— 80 个汉字 = 240 字节，上游直接
// 报参数错误（对客表现是「退款被拒」）。截断必须落在 rune 边界上，
// 否则会切出非法 UTF-8，JSON 编码时变成 U+FFFD。
func TruncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
