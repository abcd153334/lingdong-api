package payment

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// schemaKeySet 汇总所有 schema 声明的键。
func schemaKeySet() map[string]bool {
	out := map[string]bool{}
	for _, s := range Schemas() {
		for _, p := range s.Params {
			out[p.Key] = true
		}
	}
	return out
}

// TestSchemaKeysExistInChannelParams 防漂移：schema 的每个键都必须是 ChannelParams 的真实 json tag。
//
// 少了这条，schema 写错一个键名（`app_secret` → `appsecret`）时前端会渲染出一个
// 永远存不进库的输入框，而 DefaultParams 的 key 守卫只会静默跳过。
func TestSchemaKeysExistInChannelParams(t *testing.T) {
	real := map[string]bool{}
	for _, k := range ParamKeys() {
		real[k] = true
	}
	for _, s := range Schemas() {
		for _, p := range s.Params {
			if !real[p.Key] {
				t.Fatalf("协议 %s 的 schema 键 %q 不在 ChannelParams 的 json tag 中", s.Protocol, p.Key)
			}
		}
	}
}

// TestSchemaCoversAllParams 每个 ChannelParams 字段要么进 schema，要么在保留名单里。
//
// 新增参数时忘了声明 schema，运营就永远看不到那个输入框（且不会报错）。
func TestSchemaCoversAllParams(t *testing.T) {
	covered := schemaKeySet()
	for _, k := range ReservedParamKeys {
		covered[k] = true
	}
	for _, k := range ParamKeys() {
		if !covered[k] {
			t.Fatalf("ChannelParams.%s 既不在任何 schema 中，也不在 ReservedParamKeys 里", k)
		}
	}
}

// TestDefaultParamsMatchesSchema 模板值必须与 schema 声明一致（且未声明的键为空）。
func TestDefaultParamsMatchesSchema(t *testing.T) {
	for _, s := range Schemas() {
		raw, err := json.Marshal(DefaultParams(Protocol(s.Protocol)))
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("反序列化失败: %v", err)
		}
		for _, p := range s.Params {
			v, _ := got[p.Key].(string)
			if v != p.Default {
				t.Fatalf("协议 %s 的 %s 模板应为 %q，实际 %q", s.Protocol, p.Key, p.Default, v)
			}
		}
	}
	// 虎皮椒的默认网关必须仍在（运营删掉默认值会让新建渠道缺网关地址）。
	x := DefaultParams(ProtocolXunhuPay)
	if x.PayAddress != DefaultXunhuPayAddress {
		t.Fatalf("虎皮椒默认网关丢失: %+v", x)
	}
	// query_address / refund_address 已从 schema 移除 → 派生为空，
	// 运行时由 xunhuQueryEndpoint / xunhuRefundEndpoint 从 pay_address 换文件名推导。
	if x.QueryAddress != "" || x.RefundAddress != "" {
		t.Fatalf("query_address / refund_address 不应再有默认值（已改为自动推导）: %+v", x)
	}
	// 易支付没有可用默认值（网关与商户号因服务商而异）。
	if e := DefaultParams(ProtocolEpay); e.PayAddress != "" || e.PID != "" || e.Key != "" {
		t.Fatalf("易支付不应有默认值: %+v", e)
	}
}

// reservedKeys 是「已从管理台移除、仅保留结构体字段」的键。
//
// 它们全部有推导或兜底（见 schema.go 的 ReservedParamKeys 注释），
// 再出现在 schema 里就等于又给运营多出一个「填了也不生效」的框。
var reservedKeys = map[string]bool{
	"query_address":  true,
	"refund_address": true,
	"return_url":     true,
	"callback_url":   true,
}

// TestSchemaFieldRules 声明自洽：分组恒 basic、必填不得隐藏、保留键不得复现。
func TestSchemaFieldRules(t *testing.T) {
	for _, s := range Schemas() {
		keys := map[string]bool{}
		switchKeys := map[string]bool{}
		for _, sw := range s.Switches {
			switchKeys[sw.Key] = true
		}
		for _, p := range s.Params {
			if keys[p.Key] {
				t.Fatalf("协议 %s 重复声明了键 %s", s.Protocol, p.Key)
			}
			keys[p.Key] = true
			if p.Required && p.Hidden {
				t.Fatalf("协议 %s 的 %s 既必填又隐藏，运营永远无法满足", s.Protocol, p.Key)
			}
			if p.Label == "" {
				t.Fatalf("协议 %s 的 %s 缺少标签", s.Protocol, p.Key)
			}
			// 高级设置折叠区已移除：任何字段都不许再声明成别的分组，
			// 否则前端按 group 过滤时会把它们丢掉（静默消失）。
			if p.Group != ParamGroupBasic {
				t.Fatalf("协议 %s 的 %s 分组必须为 basic（高级区已移除），实际 %q", s.Protocol, p.Key, p.Group)
			}
			if reservedKeys[p.Key] {
				t.Fatalf("协议 %s 不应再声明 %s（已改为推导/兜底，见 ReservedParamKeys）", s.Protocol, p.Key)
			}
			// ShowWhen 必须指向本协议真实存在的开关，否则字段永远不显示。
			if p.ShowWhen != nil && !switchKeys[p.ShowWhen.Field] {
				t.Fatalf("协议 %s 的 %s 引用了不存在的开关 %s", s.Protocol, p.Key, p.ShowWhen.Field)
			}
		}
		// 每个开关都必须至少被一个字段引用，否则是纯装饰的死控件。
		for key := range switchKeys {
			used := false
			for _, p := range s.Params {
				if p.ShowWhen != nil && p.ShowWhen.Field == key {
					used = true
					break
				}
			}
			if !used {
				t.Fatalf("协议 %s 的开关 %s 没有被任何字段引用", s.Protocol, key)
			}
		}
	}
}

// TestSwitchKeysNotPersisted 虚拟开关的 key 不得与 ChannelParams 字段同名。
//
// 同名会让「表单状态」和「落库参数」撞在一起：开关值被当作渠道参数写进库，
// 或者反过来参数被当成开关覆盖可见性 —— 两种都极难排查。
func TestSwitchKeysNotPersisted(t *testing.T) {
	real := map[string]bool{}
	for _, k := range ParamKeys() {
		real[k] = true
	}
	for _, s := range Schemas() {
		for _, sw := range s.Switches {
			if real[sw.Key] {
				t.Fatalf("协议 %s 的虚拟开关 %s 与 ChannelParams 字段同名，会被当作落库参数", s.Protocol, sw.Key)
			}
			if sw.Default == "" {
				t.Fatalf("协议 %s 的开关 %s 缺少默认值（新建渠道时无法确定初始可见集）", s.Protocol, sw.Key)
			}
			hasDefault := false
			for _, o := range sw.Options {
				if o.Value == sw.Default {
					hasDefault = true
				}
			}
			if !hasDefault {
				t.Fatalf("协议 %s 的开关 %s 默认值 %q 不在选项里", s.Protocol, sw.Key, sw.Default)
			}
		}
	}
}

// TestSchemaSnapshot 锁定 schema 形状。
//
// 前端 `web/src/lib/payChannelSchema.ts` 的 FALLBACK_SCHEMAS 必须与此同步；
// 改 schema 时同时改这里与前端兜底，否则后端不可用（滚动发布）时前端会渲染出不同的字段集。
//
// 行格式：`协议|名称|方式模式|已实现|方式白名单`，参数行 `键:分组:标志:默认值`。
// `implemented` 也必须进快照：它决定「运营能不能新增该协议」，
// 之前没被锁住，翻 true 时没有任何守卫。
func TestSchemaSnapshot(t *testing.T) {
	lines := make([]string, 0, 32)
	for _, s := range Schemas() {
		impl := "false"
		if s.Implemented {
			impl = "true"
		}
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%s|%s",
			s.Protocol, s.Label, s.MethodMode, impl, strings.Join(s.Methods, ",")))
		for _, sw := range s.Switches {
			opts := make([]string, 0, len(sw.Options))
			for _, o := range sw.Options {
				opts = append(opts, o.Value+"="+o.Label)
			}
			lines = append(lines, "  @switch "+sw.Key+":"+sw.Default+":"+strings.Join(opts, ","))
		}
		for _, p := range s.Params {
			flags := make([]string, 0, 4)
			if p.Required {
				flags = append(flags, "req")
			}
			if p.Secret {
				flags = append(flags, "secret")
			}
			if p.Hidden {
				flags = append(flags, "hidden")
			}
			if p.Multiline {
				flags = append(flags, "multiline")
			}
			row := "  " + p.Key + ":" + string(p.Group) + ":" + strings.Join(flags, ",") + ":" + p.Default
			if p.ShowWhen != nil {
				row += ":when=" + p.ShowWhen.Field + ":" + p.ShowWhen.Value
			}
			lines = append(lines, row)
		}
	}
	want := strings.Join([]string{
		"epay|易支付|multi|true|wechat,alipay",
		"  pay_address:basic:req:",
		"  pid:basic:req:",
		"  key:basic:req,secret:",
		"xunhupay|虎皮椒|single|true|wechat,alipay",
		"  pay_address:basic:req:" + DefaultXunhuPayAddress,
		"  app_id:basic:req:",
		"  app_secret:basic:req,secret:",
		"wxpay|微信直连|single|true|wechat",
		"  @switch verify_mode:pubkey:pubkey=微信支付公钥,cert=平台证书",
		"  app_id:basic:req:",
		"  mch_id:basic:req:",
		"  cert_serial_no:basic:req:",
		"  api_v3_key:basic:req,secret:",
		"  private_key:basic:req,secret,multiline:",
		"  public_key:basic:req,multiline::when=verify_mode:pubkey",
		"  public_key_id:basic:req::when=verify_mode:pubkey",
		"  platform_cert:basic:req,multiline::when=verify_mode:cert",
		"alipay|支付宝直连|single|true|alipay",
		"  app_id:basic:req:",
		"  pay_address:basic:req:" + DefaultAlipayAddress,
		"  private_key:basic:req,secret,multiline:",
		"  public_key:basic:req,multiline:",
		"stripe|Stripe|single|true|card",
		"  secret_key:basic:req,secret:",
		"  webhook_secret:basic:req,secret:",
		"  settle_currency:basic:req:USD",
	}, "\n")
	if got := strings.Join(lines, "\n"); got != want {
		t.Fatalf("schema 形状已变化：\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestProtocolImplementedMatchesValid 锁定「可选」与「可用」一致。
//
// 只加进 Valid 不写实现 → 运营能配出一条下单必失败的渠道；
// 只写实现不进 Valid → 渠道保存时被判「协议不支持」。
func TestProtocolImplementedMatchesValid(t *testing.T) {
	for _, p := range Protocols() {
		sch, ok := SchemaOf(p)
		if !ok {
			t.Fatalf("协议 %s 没有 schema 声明", p)
		}
		if sch.Implemented != p.Valid() {
			t.Fatalf("协议 %s 的 implemented=%v 与 Valid()=%v 不一致", p, sch.Implemented, p.Valid())
		}
	}
	// Valid 集合必须与 Schemas 集合完全相同（多一个 = 有协议不可配；少一个 = 有协议配不了）。
	if len(Protocols()) != len(Schemas()) {
		t.Fatalf("Protocols()=%d 与 Schemas()=%d 数量不一致", len(Protocols()), len(Schemas()))
	}
}

// TestSchemaMethodsMatchSupportsMethod schema 白名单与能力判定必须一致（只比具体方式）。
func TestSchemaMethodsMatchSupportsMethod(t *testing.T) {
	for _, p := range Protocols() {
		sch, _ := SchemaOf(p)
		for _, m := range []Method{MethodWechat, MethodAlipay} {
			ch := Channel{Protocol: string(p)}
			allowed := false
			for _, a := range sch.Methods {
				if a == string(m) {
					allowed = true
				}
			}
			if got := ch.SupportsMethod(m); got != allowed {
				t.Fatalf("协议 %s 对方式 %s：SupportsMethod=%v，schema 白名单=%v", p, m, got, allowed)
			}
		}
	}
}

// TestSupportsMethodKeepsAggregateCompat 存量聚合渠道必须仍可下单。
//
// 破坏它 = M7 之前配置的单 APPID 虎皮椒渠道全部「支付方式暂不可用」，
// 而 payment.Create 的能力守卫会直接把下单拒掉。
func TestSupportsMethodKeepsAggregateCompat(t *testing.T) {
	xunhu := Channel{Protocol: string(ProtocolXunhuPay)}
	if !xunhu.SupportsMethod(MethodAggregate) {
		t.Fatal("虎皮椒必须兼容 aggregate（存量渠道靠它兜底）")
	}
	epay := Channel{Protocol: string(ProtocolEpay)}
	if epay.SupportsMethod(MethodAggregate) {
		t.Fatal("易支付不能承接 aggregate（type 没有对应取值）")
	}
	wx := Channel{Protocol: string(ProtocolWxpay)}
	if wx.SupportsMethod(MethodAggregate) || !wx.SupportsMethod(MethodWechat) || wx.SupportsMethod(MethodAlipay) {
		t.Fatal("微信直连应只接受微信支付")
	}
	ali := Channel{Protocol: string(ProtocolAlipay)}
	if ali.SupportsMethod(MethodAggregate) || !ali.SupportsMethod(MethodAlipay) || ali.SupportsMethod(MethodWechat) {
		t.Fatal("支付宝直连应只接受支付宝")
	}
}

// TestReservedKeysDisjoint 保留键与 schema 键必须无交集（否则同一个键会出现在两处，语义分叉）。
func TestReservedKeysDisjoint(t *testing.T) {
	covered := schemaKeySet()
	for _, k := range ReservedParamKeys {
		if covered[k] {
			t.Fatalf("保留键 %s 同时出现在 schema 里", k)
		}
	}
}

// TestCurrencyCapabilityMatchesParamDecl 币种能力与 settle_currency 的声明必须同向。
//
// 两条规则互相推导，任一条单独改动都会让「运营能填的」与「系统真正支持的」分家：
//   - 单币种协议（Currencies 一个取值）不该出现 settle_currency 输入框 ——
//     填了也不生效，是纯粹的误导；
//   - 多币种协议必须声明它，否则运营无法表达收哪种币种（Stripe 美元站全废）。
//
// 顺带锁住「除 Stripe 外全部固定人民币」这条上游事实：易支付/虎皮椒/微信/支付宝
// 的报文里没有币种参数，改错一个字母就等于允许在美元站用人民币网关收款。
func TestCurrencyCapabilityMatchesParamDecl(t *testing.T) {
	for _, s := range Schemas() {
		hasParam := false
		for _, p := range s.Params {
			if p.Key == "settle_currency" {
				hasParam = true
			}
		}
		if len(s.Currencies) == 0 {
			t.Fatalf("协议 %s 没有声明 Currencies（渠道收哪种币种将无从判定）", s.Protocol)
		}
		if multi := len(s.Currencies) > 1; multi != hasParam {
			t.Fatalf("协议 %s：Currencies=%v 与 settle_currency 参数声明（%v）不一致",
				s.Protocol, s.Currencies, hasParam)
		}
	}
	for _, p := range []Protocol{ProtocolEpay, ProtocolXunhuPay, ProtocolWxpay, ProtocolAlipay} {
		fixed, ok := FixedCurrency(p)
		if !ok || fixed != "CNY" {
			t.Fatalf("协议 %s 必须固定人民币结算，实际 fixed=%q ok=%v", p, fixed, ok)
		}
		if CurrencyAllowed(p, "USD") {
			t.Fatalf("协议 %s 不应支持美元", p)
		}
	}
	// Stripe 是唯一的多币种协议：美元与人民币都收，其余币种一律拒绝。
	if _, fixed := FixedCurrency(ProtocolStripe); fixed {
		t.Fatal("Stripe 不该被当成固定币种协议")
	}
	for _, cur := range []string{"USD", "usd", " CNY "} {
		if !CurrencyAllowed(ProtocolStripe, cur) {
			t.Fatalf("Stripe 应支持 %q", cur)
		}
	}
	if CurrencyAllowed(ProtocolStripe, "EUR") {
		t.Fatal("Stripe 目前只支持 CNY/USD，EUR 应被拒")
	}
}
