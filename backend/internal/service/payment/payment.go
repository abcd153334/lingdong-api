// Package payment 封装支付渠道的配置解析、下单（签名收银台 / 网关下单）与回调验签。
//
// 支持两种协议：
//   - epay（易支付）：本地拼签名后的 GET 收银台地址，**零网络依赖**；
//   - xunhupay（虎皮椒）：服务端 POST do.html 换 JSON（url + url_qrcode），可主动查单。
//
// 协议之间的差异全部收敛在本包内：调用方（service/order）只与 CreateRequest /
// CreateResult / Notification 打交道，不感知签名算法、字段名与状态码。
package payment

import (
	"encoding/json"
	"reflect"
	"sync"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/logger"
)

// ChannelParams 是单渠道的商户参数（键集对齐管理台支付渠道表单）。
//
// 键名注意事项：
//   - `app_id` 同时服务于虎皮椒 APPID 与支付宝直连的 AppID —— 两类渠道不会共存于同一条配置，
//     复用一个键可以避免管理台出现「app_id / appid」两个几乎同名的输入框。
//   - `app_secret` 是敏感字段（管理台以密码框呈现、回显脱敏）。
type ChannelParams struct {
	// —— epay ——
	PayAddress string `json:"pay_address"` // 完整端点或根地址（submitEndpoint 归一化）
	PID        string `json:"pid"`
	Key        string `json:"key"`

	// —— xunhupay ——
	QueryAddress  string `json:"query_address"`  // 查单地址；空则从 pay_address 推导
	RefundAddress string `json:"refund_address"` // 退款地址；空则从 query_address / pay_address 推导
	AppSecret     string `json:"app_secret"`     // 敏感
	ReturnURL     string `json:"return_url"`     // 支付成功后浏览器跳回

	// —— 通用 ——
	CallbackURL string `json:"callback_url"`
	AppID       string `json:"app_id"` // 虎皮椒 APPID / 微信 AppID / 支付宝 AppID

	// —— 官方直连（wxpay / alipay）——
	//
	// 这三个键是**历史预留转正**（原注释写「本期不消费」）：改成新键会让存量库里的
	// 旧值变成谁也看不见的死数据，所以复用。
	MchID    string `json:"mch_id"`     // 微信支付商户号
	APIv3Key string `json:"api_v3_key"` // 微信 APIv3 密钥（32 字节，敏感）
	// PrivateKey 是「我方持有的 RSA 私钥」：微信商户私钥 / 支付宝应用私钥。
	// 语义一致所以复用（敏感，PKCS#1 与 PKCS#8 都接受）。
	PrivateKey string `json:"private_key"`

	// CertSerialNo 是微信商户 API 证书序列号（40 位十六进制）。
	// 回调验签与「平台证书模式」的出站签名都要它，与 private_key 必须同批更换。
	CertSerialNo string `json:"cert_serial_no"`
	// PublicKey 是对方的 RSA 公钥：微信支付公钥 / 支付宝公钥（PEM，非敏感：本来就是公开材料）。
	PublicKey string `json:"public_key"`
	// PublicKeyID 是微信支付公钥 ID（`PUB_KEY_ID_` + 32 位数字），公钥模式下必填。
	PublicKeyID string `json:"public_key_id"`
	// PlatformCert 是微信平台证书 PEM（支持多段 bundle，覆盖轮换窗口）。
	PlatformCert string `json:"platform_cert"`

	// —— 多币种 / Stripe ——
	//
	// SettleCurrency 是渠道的**结算币种**（CNY/USD）。空值视为 CNY（存量零迁移）。
	// 它只用于「下单时筛渠道」：目标币种没有可用渠道时切币种会被阻断。
	// **回调不参与判定**（币种以订单为准），否则运营改渠道币种会让历史待支付订单全冲突。
	SettleCurrency string `json:"settle_currency"`
	// SecretKey 是 Stripe 的 secret key（sk_live_/sk_test_，敏感）。
	SecretKey string `json:"secret_key"`
	// WebhookSecret 是 Stripe webhook 签名密钥（whsec_，敏感）。
	WebhookSecret string `json:"webhook_secret"`
}

// Channel 是 system_config.pay_channels 中的单个支付渠道。
type Channel struct {
	// ID 是稳定标识（^[a-z0-9_-]{1,32}$）。订单只认它，改名不影响在途订单的回调验签。
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Protocol string        `json:"protocol"` // epay / xunhupay
	Enabled  bool          `json:"enabled"`
	Methods  []string      `json:"methods"`
	Params   ChannelParams `json:"params"`
}

// Option 是收银台的单个支付选项（**服务内部结构**，不直接下发给门户）。
//
// 门户只拿到 `order.PortalPayOption{method,label}` 投影（§4.3.0）：承接渠道、
// 协议、`{channel_id}:{method}` 串都属于供应链信息，不出用户端。
// 内部保留这些字段是为了 `resolveChannel` 选路与日志排障。
//
// **按支付方式聚合**：同一个方式即使有多个渠道支持，也只产出一项（取配置顺序第一个）。
// 用户只关心「微信还是支付宝」，不关心走哪家聚合支付。
type Option struct {
	Value string `json:"value"`
	// Label 是支付方式中文（微信支付/支付宝/聚合支付），由后端单点提供。
	Label     string `json:"label"`
	ChannelID string `json:"channel_id"`
	// Method 是支付方式枚举 code（前端直接回传，不必自己拆 Value）。
	Method   string `json:"method"`
	Protocol string `json:"protocol"`
	// Subtitle 是实际承接的渠道名（仅服务内部/管理台排障用，门户不下发）。
	Subtitle string `json:"subtitle"`
}

// 默认端点：管理台「新增渠道」的参数模板与 seed 共用，避免两处各写一份。
const (
	DefaultXunhuPayAddress    = "https://api.xunhupay.com/payment/do.html"
	DefaultXunhuQueryAddress  = "https://api.xunhupay.com/payment/query.html"
	DefaultXunhuRefundAddress = "https://api.xunhupay.com/payment/refund.html"
	// DefaultAlipayAddress 是支付宝开放平台的正式网关（沙箱见 schema 里的 help 文案）。
	DefaultAlipayAddress = "https://openapi.alipay.com/gateway.do"
)

// 官方直连的固定端点与限额。
const (
	// alipayMethodPrecreate 当面付预下单（返回 qr_code）。
	alipayMethodPrecreate = "alipay.trade.precreate"
	// alipayMethodQuery 统一收单线下交易查询。
	alipayMethodQuery = "alipay.trade.query"
	// alipayMethodRefund 统一收单交易退款。
	alipayMethodRefund = "alipay.trade.refund"
	// alipayMethodRefundQuery 退款查询（fund_change=N 时的兜底证据）。
	alipayMethodRefundQuery = "alipay.trade.fastpay.refund.query"

	// alipayTimestampLayout 是支付宝要求的格式（**东八区**，不是 RFC3339）。
	alipayTimestampLayout = "2006-01-02 15:04:05"
)

// DefaultParams 返回某协议的参数模板（值留空的地方必须由运营填）。
//
// 默认值的真相源是 `payment/schema.go` 的声明，这里只做反射取值：
// 两处各写一份时，改默认端点必然漏改一处，表现为「模板显示 A、新建出来是 B」。
func DefaultParams(protocol Protocol) ChannelParams {
	var out ChannelParams
	sch, ok := SchemaOf(protocol)
	if !ok {
		return out
	}
	v := reflect.ValueOf(&out).Elem()
	idx := paramIndex()
	for _, spec := range sch.Params {
		if spec.Default == "" {
			continue
		}
		// key 守卫：paramIndex 缺键时若直接用零值下标，会静默写到 0 号字段（PayAddress），
		// 那是「配置写错但没人发现」的最坏形态。
		i, found := idx[spec.Key]
		if !found {
			logger.L().Warn("支付参数模板：schema 键在 ChannelParams 中不存在，已跳过",
				zap.String("protocol", string(protocol)), zap.String("key", spec.Key))
			continue
		}
		field := v.Field(i)
		if field.Kind() != reflect.String || !field.CanSet() {
			continue
		}
		field.SetString(spec.Default)
	}
	return out
}

// warnedUnknownMethod 记录已经告警过的「渠道 + 未知方式」，避免每次渲染收银台都刷日志。
var warnedUnknownMethod sync.Map

// warnedUnusableChannel 记录已经告警过的「启用但必填不全」渠道（同上，只报一次）。
var warnedUnusableChannel sync.Map

// Options 把「启用渠道 × 支付方式」聚合成收银台选项。
//
// 两遍扫描的顺序不能换：
//  1. 先收具体方式（微信/支付宝），同方式只保留**配置顺序第一个**渠道；
//  2. 只有在第一遍完全为空时，才用虎皮椒的「聚合支付」兜底。
//
// 反过来的话，只要配置里还留着一条 aggregate 渠道，收银台就会同时出现
// 「微信支付」和「聚合支付」两个按钮 —— 用户选聚合、进去却能改选，是投诉源头。
func Options() []Option {
	return OptionsFor(conf.SiteCurrency())
}

// OptionsFor 按结算币种过滤收银台选项（P2）。
//
// 渠道的 settle_currency 决定它收哪种币种；空 settle_currency 视为 CNY（存量零迁移）。
// 切换币种时，收不到目标币种的渠道会被过滤掉 —— 这正是 P3 切币种前置检查
// 「目标币种无可用渠道即阻断」的数据基础。
func OptionsFor(currency string) []Option {
	cur := normCurrency(currency)
	if cur == "" {
		cur = "CNY"
	}
	out := make([]Option, 0, 4)
	seen := make(map[Method]bool, 4)
	for _, ch := range EnabledChannels() {
		if !ParseProtocol(ch.Protocol).Valid() {
			continue
		}
		// 必填不全（典型是新加渠道忘了填密钥）：**不能出现在收银台**。
		// 回调侧是 fail-closed 的（空密钥无法验签），让用户付钱等于制造卡单。
		if !ChannelUsable(ch) {
			warnUnusableOnce(ch)
			continue
		}
		// 币种过滤：渠道声明的结算币种必须与当前一致（空视为 CNY）。
		if !channelSettlesIn(ch, cur) {
			continue
		}
		ms, dropped := NormalizeMethods(ch.Methods)
		for _, raw := range dropped {
			// 未知值只警告不阻断：一条脏配置不该让整个收银台报错。
			key := ch.ID + "\x00" + raw
			if _, loaded := warnedUnknownMethod.LoadOrStore(key, struct{}{}); !loaded {
				logger.L().Warn("支付渠道配置了无法识别的支付方式，已跳过",
					zap.String("channel", ch.ID), zap.String("method", raw))
			}
		}
		for _, m := range ms {
			if !m.Specific() || !ch.SupportsMethod(m) {
				// aggregate 走兜底分支；epay 配了 aggregate 也在这里被挡掉
				// （它的 type 参数没有对应值，建单必然失败）。
				continue
			}
			if seen[m] {
				continue // 同一方式多渠道路由：配置顺序第一个胜出
			}
			seen[m] = true
			out = append(out, Option{
				Value:     JoinOption(ch.ID, string(m)),
				Label:     m.Label(),
				ChannelID: ch.ID,
				Method:    string(m),
				Protocol:  ch.Protocol,
				Subtitle:  ch.Name,
			})
		}
	}
	if len(out) > 0 {
		return out
	}
	// 兜底：一条具体方式都没有（典型是升级前的「聚合支付」配置）。
	// 只在 xunhupay 上兜底 —— 它的方式由 APPID 决定，至少还能付款。
	for _, ch := range EnabledChannels() {
		if ParseProtocol(ch.Protocol) != ProtocolXunhuPay {
			continue
		}
		if !channelSettlesIn(ch, cur) {
			continue
		}
		if !ChannelUsable(ch) {
			warnUnusableOnce(ch)
			continue
		}
		return []Option{{
			Value:     JoinOption(ch.ID, string(MethodAggregate)),
			Label:     MethodAggregate.Label(),
			ChannelID: ch.ID,
			Method:    string(MethodAggregate),
			Protocol:  ch.Protocol,
			Subtitle:  ch.Name,
		}}
	}
	return nil
}

// warnUnusableOnce 对「启用但必填不全」的渠道每条只告警一次。
//
// 只在配置热更新前后各出现一次：OptionsFor 是每次读配置都会调用的热路径，
// 不去重会把日志刷满（而这条警告的价值恰恰是「让人第一眼看到」）。
func warnUnusableOnce(ch Channel) {
	if _, loaded := warnedUnusableChannel.LoadOrStore(ch.ID, struct{}{}); !loaded {
		logger.L().Error("支付渠道已启用但必填项不全，已从收银台隐藏（回调会被拒绝）",
			zap.String("channel", ch.ID), zap.String("protocol", ch.Protocol),
			zap.Error(ValidateChannelRequired(ch)))
	}
}

// channelSettlesIn 判断渠道能否结算目标币种（口径唯一：DefaultCurrencyOf）。
//
// 不要在这里重新解析 settle_currency：口径分家时「收银台展示的币种」与
// 「入账比对的币种」会不一致，而这种不一致在页面上完全看不出来。
func channelSettlesIn(ch Channel, currency string) bool {
	return DefaultCurrencyOf(ch) == normCurrency(currency)
}

// SettlesIn 判断渠道是否收指定币种（导出给 order 侧 resolveChannel 校验）。
func (ch Channel) SettlesIn(currency string) bool {
	return channelSettlesIn(ch, currency)
}

// OptionByMethod 按支付方式取收银台选项（同方式取配置顺序第一个）。
func OptionByMethod(m Method) (Option, bool) {
	for _, o := range Options() {
		if o.Method == string(m) {
			return o, true
		}
	}
	return Option{}, false
}

// Channels 返回全部支付渠道（含未启用）；配置缺失或非法时返回 nil。
func Channels() []Channel {
	raw, ok := conf.System().Get("pay_channels")
	if !ok {
		return nil
	}
	var list []Channel
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}
