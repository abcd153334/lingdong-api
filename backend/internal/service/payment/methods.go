package payment

import "strings"

// Method 是支付方式的稳定枚举。
//
// 为什么不用中文原文：中文既是「展示文案」又是「上游参数」会互相污染 ——
// 易支付的 type 必须是 wxpay/alipay，虎皮椒则完全不认这个参数。
// 把「方式」收敛成 code，展示文案由 Label() 单点提供，上游参数由
// epayType() 单点翻译，三件事各自只有一处实现。
type Method string

const (
	// MethodWechat 微信支付。
	MethodWechat Method = "wechat"
	// MethodAlipay 支付宝。
	MethodAlipay Method = "alipay"
	// MethodAggregate 聚合支付（不区分具体方式）。
	//
	// **只作兜底产物，不允许运营在管理台选择**：虎皮椒在「只有一个 APPID、
	// 无法区分通道」的历史配置下，收银台若一个具体方式都拿不到就会空白，
	// 用它兜底至少还能付款。Options() 只有在没有任何具体方式时才生成它。
	MethodAggregate Method = "aggregate"
	// MethodCard 信用卡（Stripe Checkout）。
	//
	// Stripe 的 Checkout Session 是一张信用卡支付页，没有「微信/支付宝」之分。
	// 单列一个 code 而不是复用 aggregate：aggregate 的语义是「不区分」，
	// 而 card 是明确的支付方式，两者在收银台 UI 与能力判定上不能混。
	MethodCard Method = "card"
)

// methodAliases 是 code 与历史中文值的归一化别名表。
//
// 必须容忍中文：存量配置（pay_channels[].methods）与历史订单（orders.pay_method）
// 里存的是「微信支付/支付宝/聚合支付」，迁移只改配置不改订单快照，
// 展示与重试都要能认出来。
var methodAliases = map[string]Method{
	"wechat":    MethodWechat,
	"wx":        MethodWechat,
	"wxpay":     MethodWechat,
	"weixin":    MethodWechat,
	"微信":        MethodWechat,
	"微信支付":      MethodWechat,
	"alipay":    MethodAlipay,
	"zfb":       MethodAlipay,
	"支付宝":       MethodAlipay,
	"支付宝支付":     MethodAlipay,
	"aggregate": MethodAggregate,
	"聚合":        MethodAggregate,
	"聚合支付":      MethodAggregate,
	"card":      MethodCard,
	"credit":    MethodCard,
	"信用卡":       MethodCard,
}

// ParseMethod 把任意输入归一化为枚举；无法识别时返回 false。
//
// 大小写与首尾空白不敏感（前端历史值可能是 `WXPAY`），但**不做模糊匹配**：
// 认不出来就是认不出来，宁可报错让运营修，也不要把未知方式猜成微信。
func ParseMethod(s string) (Method, bool) {
	key := strings.ToLower(strings.TrimSpace(s))
	if key == "" {
		return "", false
	}
	m, ok := methodAliases[key]
	return m, ok
}

// NormalizeMethods 归一化一组方式：保序、去重、丢弃无法识别的值。
//
// 第二个返回值是被丢弃的原文，供调用方决定「警告」还是「拒绝」：
//   - Options()：警告后跳过（不能因为一条脏配置就让整个收银台报错）；
//   - 配置保存：拒绝并提示具体值（运营必须改成合法值才能保存）。
func NormalizeMethods(in []string) (ok []Method, dropped []string) {
	ok = make([]Method, 0, len(in))
	dropped = make([]string, 0, 0)
	seen := make(map[Method]bool, len(in))
	for _, raw := range in {
		m, found := ParseMethod(raw)
		if !found {
			dropped = append(dropped, raw)
			continue
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		ok = append(ok, m)
	}
	return ok, dropped
}

// Strings 把枚举切片还原为 code 切片（写库用）。
func MethodStrings(ms []Method) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, string(m))
	}
	return out
}

// Specific 判断是否为「具体」支付方式（微信/支付宝/信用卡）。
func (m Method) Specific() bool {
	return m == MethodWechat || m == MethodAlipay || m == MethodCard
}

// Label 返回展示文案。
func (m Method) Label() string {
	switch m {
	case MethodWechat:
		return "微信支付"
	case MethodAlipay:
		return "支付宝"
	case MethodCard:
		return "信用卡"
	case MethodAggregate:
		return "聚合支付"
	}
	return string(m)
}

// MethodLabel 是「任意原始值 → 展示文案」的唯一入口。
//
// 未知值**原样返回**而不是「未知」：历史订单里可能存着任意中文，
// 原样展示至少还能对上账；返回「未知」等于把线索丢了。
func MethodLabel(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	if m, ok := ParseMethod(raw); ok {
		return m.Label()
	}
	return strings.TrimSpace(raw)
}

// SupportsMethod 判断渠道能否承接该支付方式。
//
// 三条来源，优先级从高到低：
//  1. **聚合支付的存量兼容**（虎皮椒）：M7 之前单 APPID 的渠道库里就是 aggregate，
//     Options() 的兜底分支还会为它产出 `渠道:aggregate` 选项。这里一旦返回 false，
//     `Create` 的能力守卫会把这批渠道的下单全部拒掉（对客 = 支付方式暂不可用）。
//     所以 schema 白名单**不能**覆盖这一条。
//  2. 官方直连是写死的单一方式（Native 只收微信、当面付只收支付宝）。
//  3. 其余协议看 schema 的 Methods 白名单（易支付/虎皮椒都是微信+支付宝）。
//
// schema.Methods 的另一个用途是**配置保存期校验**与**前端 chips 过滤**，
// 两处都不该反过来影响这里的判定。
func (ch Channel) SupportsMethod(m Method) bool {
	p := ParseProtocol(ch.Protocol)
	switch p {
	case ProtocolEpay, ProtocolXunhuPay:
		if m == MethodAggregate {
			return p == ProtocolXunhuPay
		}
	case ProtocolWxpay:
		return m == MethodWechat
	case ProtocolAlipay:
		return m == MethodAlipay
	case ProtocolStripe:
		return m == MethodCard
	default:
		return false
	}
	sch, ok := SchemaOf(p)
	if !ok {
		return false
	}
	for _, allowed := range sch.Methods {
		if allowed == string(m) {
			return true
		}
	}
	return false
}

// DisplayName 生成订单的 pay_channel 展示快照（`渠道名-支付方式`）。
//
// 分隔符沿用 `-`：db.splitPayChannelName 靠它拆历史快照，改分隔符会让
// 存量回填全部错位。方式为空时退化为纯渠道名，避免出现「虎皮椒-」这种尾巴。
func DisplayName(ch Channel, m Method) string {
	name := strings.TrimSpace(ch.Name)
	if m == "" {
		return name
	}
	return name + "-" + m.Label()
}
