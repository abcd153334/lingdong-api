package payment

import (
	"reflect"
	"strings"
	"sync"
)

// ParamGroup 决定字段在管理台卡片里的位置。
//
// **本版本只剩 basic 一个取值**：高级设置折叠区已移除，所有字段平铺。
// 类型与字段都保留（不删），原因是前端独立构建 —— 旧 bundle 用
// `p.group === "basic"` 过滤参数；一旦不下发 group，旧前端会渲染出
// 一整片空白的参数区，而且不会报任何错（静默功能消失）。
type ParamGroup string

const (
	// ParamGroupBasic 是卡片直接平铺的字段。
	ParamGroupBasic ParamGroup = "basic"
)

// MethodMode 表示一条渠道能承接几种支付方式。
//
// 差异根因在协议：虎皮椒的微信/支付宝是两套独立 APPID（do.html 没有方式参数，
// 分流靠用哪套凭证），所以一条渠道只能一种方式；易支付一条商户号通吃，靠 `type` 区分。
const (
	MethodModeSingle = "single"
	MethodModeMulti  = "multi"
)

// ParamSpec 是单个渠道参数的声明（管理台据此渲染表单，不再遍历 params 的键集）。
//
// 关于 Hidden：本版本已没有任何 spec 使用它（原来用它藏 `callback_url`，
// 现在那个键已从 schema 移除并进 ReservedParamKeys）。字段保留是为了
// 兼容「旧后端下发 hidden 项」的滚动发布窗口。
type ParamSpec struct {
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Group       ParamGroup `json:"group"`
	Required    bool       `json:"required"`
	Secret      bool       `json:"secret"` // 密码框 + 「已设置」标记 + 落库脱敏
	Hidden      bool       `json:"hidden"` // 历史兼容：true 表示不渲染
	Multiline   bool       `json:"multiline,omitempty"`
	Placeholder string     `json:"placeholder,omitempty"`
	Tip         string     `json:"tip,omitempty"` // 问号气泡文案（≤20 字；直观可推断的留空）
	Default     string     `json:"default,omitempty"`
	// ShowWhen 让字段只在「某个虚拟开关取到某值」时显示，用于二选一的互斥字段组。
	//
	// **后端不得据此跳过任何校验** —— 信任客户端传来的模式值等于开一个
	// 「传 pubkey 就跳过证书校验」的洞。后端一律按数据本身判定（见 validate.go）。
	ShowWhen *ShowWhen `json:"show_when,omitempty"`
}

// ShowWhen 声明「本字段只在指定虚拟开关取某值时才渲染」。
//
// Field 是**虚拟控件**的 key，不是 ChannelParams 的字段，不会被持久化
// （有 TestSwitchKeysNotPersisted 守着，同名即失败）。
type ShowWhen struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// ParamSwitch 是表单里的虚拟单选控件：不落库，只控制字段可见性。
type ParamSwitch struct {
	Key     string         `json:"key"`
	Label   string         `json:"label"`
	Default string         `json:"default"`
	Options []SwitchOption `json:"options"`
}

// SwitchOption 是虚拟单选的一个选项。
type SwitchOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ChannelSchema 是单个协议的参数声明集合。
type ChannelSchema struct {
	Protocol string `json:"protocol"`
	Label    string `json:"label"`
	// Implemented=false 表示该协议只有占位（无下单实现），前端置灰不允许新增。
	Implemented bool   `json:"implemented"`
	MethodMode  string `json:"method_mode"`
	// Methods 是该协议**允许**的支付方式白名单。
	//
	// method_mode 只能表达「选几个」，表达不了「能选哪些」：
	// 微信 Native 只能收微信、支付宝当面付只能收支付宝，不给白名单就会出现
	// 「渠道配了支付宝，下单却必然失败」。留空表示不限制（兼容旧后端）。
	Methods []string `json:"methods,omitempty"`
	// MethodHelp 是支付方式区的问号文案（≤16 字）。
	MethodHelp string `json:"method_help,omitempty"`
	// Currencies 是该协议**能结算的币种**（上游事实，不是运营可选项）。
	//
	// 只有一个取值 = 固定币种：协议报文里没有币种参数，金额恒按这个币种解释，
	// 所以 schema 也不会声明 settle_currency（运营看不到那个输入框）。
	// 多个取值 = 由 settle_currency 参数选择（目前只有 Stripe 支持 CNY/USD）。
	//
	// **不下发前端**（json:"-"）：前端读 params.settle_currency 就够 ——
	// 固定币种协议根本没有那个键，UI 回落 CNY 与此处的结论一致。
	// 两条口径若分家（比如这里改了 CNY 而前端仍回落 CNY）会变成第二个真相源，
	// 所以刻意只留在服务端，由 TestCurrencyCapabilityMatchesParamDecl 锁住。
	Currencies []string `json:"-"`
	// Switches 是虚拟单选控件（目前只有微信的「验签方式」）。
	Switches []ParamSwitch `json:"switches,omitempty"`
	Params   []ParamSpec   `json:"params"`
}

// ReservedParamKeys 是「结构体保留但任何协议 schema 都不展示」的字段。
//
// 这四项都有推导或兜底，运营不需要也不应该在界面上填：
//   - query_address / refund_address：从 pay_address 换文件名推导
//     （xunhupay.go:328-342 / xunhupay_refund.go:156-197）；
//   - return_url：留空回落 `{site.site_url}/topup`（order.go:580）；
//   - callback_url：仅 epay 在 notifyURL 为空时兜底，生产链路从不读（epay.go:110-118）。
//
// **不能删结构体字段**：ChannelParams 没有 omitempty，保存时 json.Marshal 会
// 全量回写；删字段等于在下一次保存时把存量值永久清空。
//
// 保留这个变量本身还守护一条测试（TestSchemaCoversAllParams）：新增字段要么
// 进 schema，要么必须在这里显式登记，否则运营永远看不到那个输入框且不会报错。
var ReservedParamKeys = []string{"query_address", "refund_address", "return_url", "callback_url"}

// channelSchemas 是参数声明的**唯一真相源**。
//
// 顺序即管理台「新增支付渠道」里的协议顺序，不要靠 map 遍历（顺序不稳定会让
// e2e 与人工核对都对不上）。
var channelSchemas = []ChannelSchema{
	{
		Protocol:    string(ProtocolEpay),
		Label:       "易支付",
		Implemented: true,
		MethodMode:  MethodModeMulti,
		Methods:     []string{string(MethodWechat), string(MethodAlipay)},
		MethodHelp:  "一个商户号可同时收微信与支付宝",
		Currencies:  []string{"CNY"},
		Params: []ParamSpec{
			{
				Key: "pay_address", Label: "网关地址", Group: ParamGroupBasic, Required: true,
				Placeholder: "https://pay.example.com/",
				Tip:         "只填域名会自动补 /submit.php",
			},
			{Key: "pid", Label: "商户 PID", Group: ParamGroupBasic, Required: true, Placeholder: "1000"},
			{
				Key: "key", Label: "商户密钥", Group: ParamGroupBasic, Required: true, Secret: true,
				Tip: "与上游一致；留空即清空",
			},
		},
	},
	{
		Protocol:    string(ProtocolXunhuPay),
		Label:       "虎皮椒",
		Implemented: true,
		MethodMode:  MethodModeSingle,
		Methods:     []string{string(MethodWechat), string(MethodAlipay)},
		MethodHelp:  "微信与支付宝各需一条渠道",
		Currencies:  []string{"CNY"},
		Params: []ParamSpec{
			{
				Key: "pay_address", Label: "网关地址", Group: ParamGroupBasic, Required: true,
				Default: DefaultXunhuPayAddress, Placeholder: DefaultXunhuPayAddress,
			},
			{
				Key: "app_id", Label: "APPID", Group: ParamGroupBasic, Required: true,
				Tip: "一条渠道只能对应一套 APPID",
			},
			{
				Key: "app_secret", Label: "APP SECRET", Group: ParamGroupBasic, Required: true, Secret: true,
				Tip: "与上游一致；留空即清空",
			},
		},
	},
	// —— 以下两个协议**追加在末尾**：前端「新增支付渠道」取 usable[0] 作默认协议，
	//    e2e 也依赖 schemas[0] == epay。插到前面会让默认协议悄悄变成微信/支付宝。
	{
		Protocol:    string(ProtocolWxpay),
		Label:       "微信直连",
		Implemented: true,
		MethodMode:  MethodModeSingle,
		Methods:     []string{string(MethodWechat)},
		MethodHelp:  "仅收微信；多渠道按顺序取第一条",
		Currencies:  []string{"CNY"},
		Switches: []ParamSwitch{{
			Key: "verify_mode", Label: "验签方式", Default: "pubkey",
			Options: []SwitchOption{
				{Value: "pubkey", Label: "微信支付公钥"},
				{Value: "cert", Label: "平台证书"},
			},
		}},
		Params: []ParamSpec{
			{
				Key: "app_id", Label: "AppID", Group: ParamGroupBasic, Required: true,
				Tip: "已绑定该商户号的公众号/小程序",
			},
			{
				Key: "mch_id", Label: "商户号", Group: ParamGroupBasic, Required: true,
				Placeholder: "1600000000",
				Tip:         "商户平台的纯数字商户号",
			},
			{
				Key: "cert_serial_no", Label: "证书序列号", Group: ParamGroupBasic, Required: true,
				Placeholder: "5157F09EFDC096DE15EBE81A47057A7232F1B8E1",
				Tip:         "apiclient_cert.pem 里的 serial",
			},
			{
				Key: "api_v3_key", Label: "APIv3 密钥", Group: ParamGroupBasic, Required: true, Secret: true,
				Tip: "32 位，用于解密回调",
			},
			{
				Key: "private_key", Label: "商户私钥", Group: ParamGroupBasic, Required: true,
				Secret: true, Multiline: true,
				Tip: "整段粘贴；回显 ****** 表示已保存",
			},
			{
				Key: "public_key", Label: "微信支付公钥", Group: ParamGroupBasic, Required: true,
				Multiline: true, ShowWhen: &ShowWhen{Field: "verify_mode", Value: "pubkey"},
				Tip: "与公钥 ID 必须成对填写",
			},
			{
				Key: "public_key_id", Label: "公钥 ID", Group: ParamGroupBasic, Required: true,
				ShowWhen:    &ShowWhen{Field: "verify_mode", Value: "pubkey"},
				Placeholder: "PUB_KEY_ID_0000000000000000000000000000000000",
				Tip:         "PUB_KEY_ID_ 开头加 32 位数字",
			},
			{
				Key: "platform_cert", Label: "平台证书", Group: ParamGroupBasic, Required: true,
				Multiline: true, ShowWhen: &ShowWhen{Field: "verify_mode", Value: "cert"},
				Tip: "换证期间可新旧两张一起粘贴",
			},
		},
	},
	{
		Protocol:    string(ProtocolAlipay),
		Label:       "支付宝直连",
		Implemented: true,
		MethodMode:  MethodModeSingle,
		Methods:     []string{string(MethodAlipay)},
		MethodHelp:  "仅收支付宝；多渠道按顺序取第一条",
		Currencies:  []string{"CNY"},
		Params: []ParamSpec{
			{
				Key: "app_id", Label: "应用 AppID", Group: ParamGroupBasic, Required: true,
				Placeholder: "2021000000000000",
				Tip:         "开放平台的 16 位应用 ID",
			},
			{
				Key: "pay_address", Label: "网关地址", Group: ParamGroupBasic, Required: true,
				Default: DefaultAlipayAddress, Placeholder: DefaultAlipayAddress,
				Tip: "沙箱环境改填沙箱网关",
			},
			{
				Key: "private_key", Label: "应用私钥", Group: ParamGroupBasic, Required: true,
				Secret: true, Multiline: true,
				Tip: "整段粘贴；回显 ****** 表示已保存",
			},
			{
				Key: "public_key", Label: "支付宝公钥", Group: ParamGroupBasic, Required: true,
				Multiline: true,
				Tip:       "是支付宝公钥，不是应用公钥",
			},
		},
	},
	// —— Stripe **追加在末尾**：前端 usable[0] 仍是 epay，e2e 依赖 schemas[0] == epay。
	{
		Protocol:    string(ProtocolStripe),
		Label:       "Stripe",
		Implemented: true,
		MethodMode:  MethodModeSingle,
		Methods:     []string{string(MethodCard)},
		MethodHelp:  "海外信用卡；支持美元收款",
		Currencies:  []string{"USD", "CNY"},
		Params: []ParamSpec{
			{
				Key: "secret_key", Label: "Secret Key", Group: ParamGroupBasic, Required: true, Secret: true,
				Placeholder: "sk_live_…",
				Tip:         "从 Stripe Dashboard 复制",
			},
			{
				Key: "webhook_secret", Label: "Webhook 密钥", Group: ParamGroupBasic, Required: true, Secret: true,
				Placeholder: "whsec_…",
				Tip:         "用于回调验签",
			},
			{
				Key: "settle_currency", Label: "结算币种", Group: ParamGroupBasic, Required: true,
				Default: "USD",
				Tip:     "CNY 或 USD",
			},
		},
	},
}

// Schemas 返回全部协议的参数声明（稳定顺序，可直接下发前端）。
func Schemas() []ChannelSchema {
	out := make([]ChannelSchema, len(channelSchemas))
	for i, s := range channelSchemas {
		// 深拷贝切片：Params 与 Switches 都是引用类型，只拷结构体会让调用方
		// 改到全局声明（一次误改会影响所有后续请求）。
		params := make([]ParamSpec, len(s.Params))
		copy(params, s.Params)
		s.Params = params
		if len(s.Switches) > 0 {
			switches := make([]ParamSwitch, len(s.Switches))
			copy(switches, s.Switches)
			for j := range switches {
				opts := make([]SwitchOption, len(switches[j].Options))
				copy(opts, switches[j].Options)
				switches[j].Options = opts
			}
			s.Switches = switches
		}
		out[i] = s
	}
	return out
}

// SchemaOf 按协议取参数声明。
func SchemaOf(p Protocol) (ChannelSchema, bool) {
	for _, s := range channelSchemas {
		if s.Protocol == string(p) {
			return s, true
		}
	}
	return ChannelSchema{}, false
}

// normCurrency 归一币种代码：大小写与首尾空白不敏感。
//
// 唯一实现。表单、旧库、上游回调三处都可能给出 "usd" / " USD "，只要有一处忘了
// 归一，就是「按美元展示、按人民币入账」那类账实不符 —— 币种比较绝不能各写一遍。
func normCurrency(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// FixedCurrency 返回协议**固定**的结算币种；ok=false 表示该协议支持多币种、
// 由渠道参数 settle_currency 选择。
//
// 这是「渠道收哪种币种」的唯一判定入口：收银台过滤（OptionsFor）、下单校验
// （SettlesIn）、回调币种补齐（DefaultCurrencyOf）都经它，而不是各自去读
// 那个参数。差异是实打实的：读了参数就等于把「渠道收什么币种」交给运营手填，
// 一条 epay 渠道填上 usd 就会在美元站出现「微信支付」按钮，用户付的是人民币
// 金额、系统按美元记额度。
func FixedCurrency(p Protocol) (string, bool) {
	if s, ok := SchemaOf(p); ok && len(s.Currencies) == 1 {
		return s.Currencies[0], true
	}
	return "", false
}

// CurrencyAllowed 判断协议能否结算目标币种（大小写不敏感）。
func CurrencyAllowed(p Protocol, currency string) bool {
	for _, c := range CurrenciesOf(p) {
		if c == normCurrency(currency) {
			return true
		}
	}
	return false
}

// CurrenciesOf 返回协议支持的币种（升序即声明顺序，用于错误文案）。
func CurrenciesOf(p Protocol) []string {
	if s, ok := SchemaOf(p); ok {
		return s.Currencies
	}
	return nil
}

var (
	paramIndexOnce  sync.Once
	paramIndexByKey map[string]int
)

// paramIndex 返回 ChannelParams 的 `json 键 → 字段下标` 映射（init 期建一次）。
func paramIndex() map[string]int {
	paramIndexOnce.Do(func() {
		t := reflect.TypeOf(ChannelParams{})
		m := make(map[string]int, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			m[name] = i
		}
		paramIndexByKey = m
	})
	return paramIndexByKey
}

// ParamKeys 返回 ChannelParams 的全部 json 键（schema 防漂移测试与前端兜底用）。
func ParamKeys() []string {
	t := reflect.TypeOf(ChannelParams{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}
