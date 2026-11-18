package payment

import (
	"context"
	"errors"
	"strings"
	"time"
)

// PendingTTL 是 pending 订单的存活时长。
//
// 前端倒计时、过期关单、主动查单窗口**共用这一个值**：三处各自定义是分叉的起点，
// 而虎皮椒二维码本身就是 5 分钟有效，与业务 TTL 天然对齐。
const PendingTTL = 5 * time.Minute

// effectiveExpireAt 归一化订单失效时间（W1）。
//
// req.ExpireAt 是订单的**绝对**失效时间，由 placeOrder 按协议 TTL 填好。
// 四个旧协议实现此前都**忽略**它、各自写 `time.Now().Add(PendingTTL)`，
// 于是 Stripe 这种「TTL 应该更长」的协议会与本地 expire_at 分叉。
//
//   - req.ExpireAt 非零 → 原样用（不得早于 now+1min，否则上游会以「早于当前」拒绝）；
//   - 零值 → now+PendingTTL（调用方没传，退回默认）。
func effectiveExpireAt(expireAt time.Time) time.Time {
	if expireAt.IsZero() {
		return time.Now().Add(PendingTTL)
	}
	if min := time.Now().Add(time.Minute); expireAt.Before(min) {
		return min
	}
	return expireAt
}

// Protocol 是渠道的支付协议。决定签名算法、下单方式与回调解析走哪套实现。
type Protocol string

const (
	// ProtocolEpay 易支付：本地拼签名后的 GET 收银台地址，**不预调上游**。
	ProtocolEpay Protocol = "epay"
	// ProtocolXunhuPay 虎皮椒：服务端 POST form 换 JSON（含二维码/跳转地址）。
	ProtocolXunhuPay Protocol = "xunhupay"
	// ProtocolWxpay 微信支付官方直连（APIv3 Native，PC 扫码）。
	ProtocolWxpay Protocol = "wxpay"
	// ProtocolAlipay 支付宝官方直连（当面付 precreate，PC 扫码）。
	ProtocolAlipay Protocol = "alipay"
	// ProtocolStripe Stripe 官方直连（Checkout Session hosted 页，支持多币种）。
	ProtocolStripe Protocol = "stripe"
)

// Valid 判断协议是否已实现。空值表示「渠道还没配协议」，下单时按未配置处理。
//
// **必须与 channelSchemas 的 Implemented 一致**（TestProtocolImplementedMatchesValid 锁定）：
// 只加进 Valid 而没写实现，运营就会配出一条「下单必失败」的渠道；
// 只写实现不进 Valid，渠道保存时会被判「协议不支持」。
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolEpay, ProtocolXunhuPay, ProtocolWxpay, ProtocolAlipay, ProtocolStripe:
		return true
	}
	return false
}

// ParseProtocol 归一化协议名（大小写/空白不敏感）；未知值返回空串。
func ParseProtocol(s string) Protocol {
	p := Protocol(strings.ToLower(strings.TrimSpace(s)))
	if p.Valid() {
		return p
	}
	return ""
}

// IsRefundKind 判断这个回调是否与"退款"相关（不产生入账，只推进退款状态）。
//
// 判定必须只有一处：order 层用它决定「比金额还是比退款额」「要不要认领退款单号」，
// 协议层用它决定「金额取 total 还是 refund」。两处各写一遍 switch，
// 新增一种退款类 Kind 时必然漏一边（后果是金额核对静默用错口径）。
func IsRefundKind(k NotifyKind) bool {
	switch k {
	case NotifyRefunded, NotifyRefunding, NotifyRefundUnknown, NotifyRefundFailed:
		return true
	}
	return false
}

// NotifyKind 是归一化后的回调语义（把各渠道的状态码翻译成同一套）。
type NotifyKind string

const (
	NotifyPaid      NotifyKind = "paid"
	NotifyRefunded  NotifyKind = "refunded"
	NotifyRefunding NotifyKind = "refunding"
	// NotifyRefundUnknown 表示「上游已受理退款，但当前状态不是成功也不是终态失败」。
	//
	// 典型来源是微信的 REFUND.ABNORMAL：原路退回银行卡失败（卡注销/冻结），**资金已经
	// 从商户账户划出、停在微信的退款中间账户**，等商户在商户平台二选一处理（退用户
	// 银行卡 / 退回结算账户），处理成功后会流转成 SUCCESS —— 它**不是终态**。
	//
	// 处置必须与「退款失败」相反：保持 refunding（巡检继续查）、**绝不回滚额度**。
	// 回滚就是双向损失：用户既拿回平台额度，又收到上游那笔真钱。
	NotifyRefundUnknown NotifyKind = "refund_unknown"
	// NotifyRefundFailed 表示「上游确认退款没发生」—— 资金仍留在商户，可以安全回滚额度。
	// 微信的 REFUND.CLOSED（退款关闭）属于这一类。
	NotifyRefundFailed NotifyKind = "refund_failed"
	// NotifyIgnored 表示「回调到达但既不是支付成功、也不是可识别的退款态」。
	// 调用方只记录 + 回 success，不改订单状态。
	//
	// 未知状态**不当作成功**：把未支付订单错标成已支付，比漏一笔需要人工核对的单更糟。
	NotifyIgnored NotifyKind = "ignored"
)

// CreateRequest 是协议无关的下单入参。
//
// AmountCent 是**分**，不是元：调用方负责把用户输入换成整数，
// 协议实现负责把它格式化成上游要的字符串（见 FormatCentToCNY）。
type CreateRequest struct {
	OutTradeNo string // ≤32，[0-9A-Za-z_-*]
	AmountCent int64  // > 0
	// Currency 是订单币种（CNY/USD）。空值视为 CNY（存量协议不收美元）。
	//
	// 只有 Stripe 真正用它（它按 currency 收）；微信/支付宝/易支付/虎皮椒
	// 固定收人民币，忽略本字段。多币种下「¥72 与 $72」必须能区分。
	Currency string
	// Method 是收银台选的支付方式枚举（wechat / alipay / aggregate）。
	//
	// 它**不是展示文案**：易支付会经 epayType 翻译成 `type=wxpay|alipay`；
	// 虎皮椒的 do.html 没有这个参数，方式由用哪套 APPID 决定，方法仅用于能力校验。
	// 类型用 Method 而不是 string，是为了让「把中文传上游」在编译期就不可能发生。
	Method    Method
	Subject   string // 已由 SanitizeTitle 清洗
	NotifyURL string // 绝对 URL（http/https）
	ReturnURL string
	ClientIP  string
	// Attach 是原样回传的备注。放 order_id（纯数字），回调时用于交叉验证。
	Attach string
	// ExpireAt 是订单的**绝对**失效时间（零值 = now+PendingTTL）。
	//
	// 官方直连会把它写成上游的 time_expire / qr_code_timeout_express：
	// 必须传绝对时间，否则重试时按 now+TTL 算会让上游比本地多活一段，
	// 出现「本地已关单、二维码还能付」。
	ExpireAt time.Time
}

// CreateResult 是协议无关的下单产物。
type CreateResult struct {
	PayURL         string    // 移动端跳转地址
	QRRef          string    // 上游 url_qrcode 原值（可能是图片 URL，也可能是二维码内容）
	ChannelOrderID string    // 上游内部单号
	ExpiresAt      time.Time // 支付链接/二维码的失效时间
}

// Notification 是协议无关的回调/查单结果。
type Notification struct {
	OutTradeNo string
	// TradeNo 是**支付平台交易号**（虎皮椒 transaction_id、易支付 trade_no）。
	// 注意不要取虎皮椒返回里的 openid —— 那是它自己的订单号，不是交易号。
	TradeNo string
	// ChannelOrderID 是上游内部订单号（虎皮椒 open_order_id / openid），供精确查单。
	ChannelOrderID string
	// PaymentID 是**原路退款要用的支付凭据**，与 ChannelOrderID 可能不同。
	// 只有 Stripe 会填（checkout session 的 payment_intent=pi_...）：它的退款接口
	// 认 pi_ 不认 cs_，把 session id 传过去必然被拒。其它协议为空，退款用 ChannelOrderID。
	PaymentID  string
	AmountCent int64
	// Currency 是回调/查单上报的币种（大写）。空值表示该协议单币种，由调用方按渠道补齐。
	// 微信取 amount.currency、Stripe 取 currency、epay/xunhupay/alipay 固定 CNY。
	Currency   string
	MerchantID string
	// OutRefundNo 是上游的退款单号（微信 out_refund_no）。
	//
	// 有了它，退款回调才能**认领到具体哪一笔退款**：同一订单未来出现第二笔部分退款时，
	// 只按 out_trade_no 认领会把两笔混成一处（第二笔会去改第一笔的结论）。
	// 目前只有微信上报；其它协议留空 = 不做单号认领（保持原口径）。
	OutRefundNo string
	Kind        NotifyKind
	Raw         map[string]string
	// EventID 是上游**事件级**幂等锚（仅 Stripe 有，evt_...）。Stripe 会重放同一 event
	// 而 trade_no（session id）不变，需要 event 级别去重（见 payment_events 表）。
	// 其它协议为空。
	EventID string
}

var (
	// ErrInvalidAmount 金额格式不合法（非数字、超两位小数、超大）。
	ErrInvalidAmount = errors.New("payment: 金额格式不合法")
	// ErrUnsupported 该协议不支持此操作（如易支付没有实现主动查单）。
	ErrUnsupported = errors.New("payment: 该渠道协议不支持此操作")
	// ErrSignInvalid 签名校验失败。
	ErrSignInvalid = errors.New("payment: 签名校验失败")
	// ErrGatewayRejected 网关明确拒绝（参数/签名/配置错误）——**不可重试**。
	ErrGatewayRejected = errors.New("payment: 支付网关拒绝请求")
	// ErrGatewayBusy 网关繁忙/限频——可重试，但只能转查单确认，不能重复建单。
	ErrGatewayBusy = errors.New("payment: 支付网关繁忙")
	// ErrOrderExists 上游已有同号订单（不是失败）——应转查单取支付信息。
	ErrOrderExists = errors.New("payment: 上游订单已存在")
	// ErrNoPayURL 上游返回成功但没有可用支付地址。
	ErrNoPayURL = errors.New("payment: 上游未返回支付地址")
	// ErrNotConfigured 渠道必填参数缺失（app_id / app_secret / pay_address 等）。
	ErrNotConfigured = errors.New("payment: 支付渠道未正确配置")
	// ErrNotifyStatus 回调/查单返回了无法识别的状态码。
	ErrNotifyStatus = errors.New("payment: 无法识别的支付状态")
	// ErrUnsupportedMethod 渠道不支持所选支付方式。
	// 典型：epay 配了 aggregate（type 没有对应取值）、渠道已删/改了方式后旧页面重试。
	ErrUnsupportedMethod = errors.New("payment: 该渠道不支持所选支付方式")
)

// Create 按协议创建支付单。
//
// 只有虎皮椒会真正出网（POST do.html）；易支付仍是本地拼串，保持零网络依赖。
// 调用方**必须先落库再调用本函数**：反过来的话，上游建单成功而本地写库失败，
// 用户拿到二维码并付款后本地没有订单，无法入账也无从对账。
func Create(ctx context.Context, ch Channel, req CreateRequest) (*CreateResult, error) {
	if req.AmountCent <= 0 {
		return nil, ErrInvalidAmount
	}
	if strings.TrimSpace(req.OutTradeNo) == "" || strings.TrimSpace(req.NotifyURL) == "" {
		return nil, ErrNotConfigured
	}
	// 能力守卫：防绕过订单侧 resolveChannel 的调用（如 RetryPay 传入历史中文值）。
	// 静默放行会让上游收到它不认识的 type，用户拿到一个必然失败的收银台。
	if !ch.SupportsMethod(req.Method) {
		return nil, ErrUnsupportedMethod
	}
	switch ParseProtocol(ch.Protocol) {
	case ProtocolEpay:
		return epayCreate(ch, req)
	case ProtocolXunhuPay:
		return xunhuCreate(ctx, ch, req)
	case ProtocolWxpay:
		return wxpayCreate(ctx, ch, req)
	case ProtocolAlipay:
		return alipayCreate(ctx, ch, req)
	case ProtocolStripe:
		return stripeCreate(ctx, ch, req)
	}
	return nil, ErrUnsupported
}

// ParseNotify 按协议验签并把原始参数映射成 Notification（不含 raw body 的旧入口）。
//
// 只做「验签 + 字段映射」，**不查库、不判业务、不改状态**：
// 状态流转与入账统一由 service/order 的 settle 处理，
// 否则回调与主动查单会各写一套，迟早不一致。
//
// 微信 APIv3 的回调是 JSON + 头签名，必须拿到**逐字节原文**，所以新代码走
// ParseNotifyRequest（controller 侧已改为保留 raw body）。
func ParseNotify(ch Channel, params map[string]string) (*Notification, error) {
	return ParseNotifyRequest(ch, NotifyRequest{Params: params})
}

// Query 主动查单。
//
// 返回值约定（调用方必须按这三种情况分支）：
//   - (n, nil) 且 n.Kind == paid  → 已支付，走 settlePaid
//   - (nil, nil)                  → 确认未支付，不动作
//   - (nil, err)                  → 查询失败，**不改订单状态**（关单不能靠「查不到」）
func Query(ctx context.Context, ch Channel, outTradeNo, channelOrderID string) (*Notification, error) {
	switch ParseProtocol(ch.Protocol) {
	case ProtocolEpay:
		// 易支付有 act=order 查询接口，但本期不实现（订单按 expire_at 直接关单）。
		return nil, ErrUnsupported
	case ProtocolXunhuPay:
		return xunhuQuery(ctx, ch, outTradeNo, channelOrderID)
	case ProtocolWxpay:
		return wxpayQuery(ctx, ch, outTradeNo)
	case ProtocolAlipay:
		return alipayQuery(ctx, ch, outTradeNo)
	case ProtocolStripe:
		return stripeQuery(ctx, ch, outTradeNo, channelOrderID)
	}
	return nil, ErrUnsupported
}

// EnabledChannels 返回收银台可见的渠道（enabled=true）。
func EnabledChannels() []Channel {
	all := Channels()
	out := make([]Channel, 0, len(all))
	for _, ch := range all {
		if ch.Enabled {
			out = append(out, ch)
		}
	}
	return out
}

// DefaultCurrencyOf 返回渠道**实际结算**的币种。
//
// 单一判定：先看协议是否固定币种（易支付/虎皮椒/微信/支付宝恒为 CNY，见
// schema 的 Currencies），固定就返回它 —— 参数里写了什么都不算数；只有多币种
// 协议（Stripe）才读 settle_currency，空值回落 CNY。
//
// 供三处共用：收银台过滤（OptionsFor）、下单校验（SettlesIn）、回调缺币种时
// 补齐（order/notify.go）。它们必须是同一口径：任一处读了「运营手填的参数」，
// 就会出现「收银台按美元展示、入账按人民币」这类账实不符。
func DefaultCurrencyOf(ch Channel) string {
	if fixed, ok := FixedCurrency(ParseProtocol(ch.Protocol)); ok {
		return fixed
	}
	if declared := normCurrency(ch.Params.SettleCurrency); declared != "" {
		return declared
	}
	return "CNY"
}

// FindChannel 按稳定 ID 找渠道，**不看 enabled**。
//
// 为什么不能复用 EnabledChannels：enabled 的语义是「是否出现在收银台」，
// 不是「能否验签」。管理员停用渠道后，在途 pending 订单的回调仍必须能验签入账——
// 否则钱收了、额度不到账，而且网关重试 6 次后会放弃，连补单线索都没有。
func FindChannel(id string) (Channel, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Channel{}, false
	}
	for _, ch := range Channels() {
		if ch.ID == id {
			return ch, true
		}
	}
	return Channel{}, false
}

// JoinOption 拼出选项 value `{channel_id}:{method}`（渠道未配置 methods 时就是渠道 ID）。
//
// **只有生成方向，没有解析方向**：选路由是订单侧 `resolveChannel(channelID, method)`，
// 前端回传的是结构化字段，从不把 value 反解成渠道。曾经的 SplitOption 已删除 ——
// 留着它就是给「前端自填 channel_id 挑渠道」留入口，两套口径必然分叉。
func JoinOption(channelID, method string) string {
	if method == "" {
		return channelID
	}
	return channelID + ":" + method
}
