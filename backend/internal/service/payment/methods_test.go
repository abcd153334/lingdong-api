package payment

import (
	"encoding/json"
	"errors"
	"testing"

	"lingdong-api/internal/conf"
)

// setChannels 临时替换 pay_channels 内存配置，用例结束自动还原。
//
// Options()/FindChannel() 读的都是全局 conf 镜像（不是 DB），所以这里必须用
// conf.System().Set 注入；用完还原成「键不存在」，避免污染同包其它用例。
func setChannels(t *testing.T, chs []map[string]any) {
	t.Helper()
	prev, had := conf.System().Get("pay_channels")
	raw, err := json.Marshal(chs)
	if err != nil {
		t.Fatalf("序列化渠道配置失败: %v", err)
	}
	conf.System().Set("pay_channels", raw)
	t.Cleanup(func() {
		if had {
			conf.System().Set("pay_channels", prev)
			return
		}
		conf.System().Set("pay_channels", json.RawMessage("[]"))
	})
}

func TestParseMethod(t *testing.T) {
	cases := []struct {
		in   string
		want Method
		ok   bool
	}{
		{"wechat", MethodWechat, true},
		{"WXPAY", MethodWechat, true}, // 大小写不敏感
		{" 微信支付 ", MethodWechat, true},
		{"微信", MethodWechat, true},
		{"alipay", MethodAlipay, true},
		{"zfb", MethodAlipay, true},
		{"支付宝", MethodAlipay, true},
		{"aggregate", MethodAggregate, true},
		{"聚合支付", MethodAggregate, true},
		{"", "", false},
		// 不猜：认不出来就是认不出来，猜错会把支付宝的订单打到微信通道
		{"微信支付（推荐）", "", false},
		{"alipay_v2", "", false},
	}
	for _, c := range cases {
		got, ok := ParseMethod(c.in)
		if ok != c.ok || got != c.want {
			t.Fatalf("ParseMethod(%q) = (%q,%v)，期望 (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeMethods(t *testing.T) {
	ok, dropped := NormalizeMethods([]string{"支付宝", "wechat", "alipay", "", "乱写"})
	want := []Method{MethodAlipay, MethodWechat}
	if len(ok) != len(want) {
		t.Fatalf("归一化结果 %v，期望 %v", ok, want)
	}
	for i := range want {
		if ok[i] != want[i] {
			t.Fatalf("第 %d 项为 %q，期望 %q（必须保序）", i, ok[i], want[i])
		}
	}
	// 空串也算无法识别：它在配置里意味着「运营没选」，不能当成合法方式存下去
	if len(dropped) != 2 || dropped[0] != "" || dropped[1] != "乱写" {
		t.Fatalf("被丢弃的值不符: %v", dropped)
	}
}

func TestMethodLabel(t *testing.T) {
	cases := map[string]string{
		"wechat":    "微信支付",
		"alipay":    "支付宝",
		"aggregate": "聚合支付",
		"微信支付":      "微信支付", // 历史订单快照
		"随便写的":      "随便写的", // 未知值原样返回，不丢线索
		"":          "",
	}
	for in, want := range cases {
		if got := MethodLabel(in); got != want {
			t.Fatalf("MethodLabel(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestSupportsMethod(t *testing.T) {
	epay := Channel{Protocol: "epay"}
	xunhu := Channel{Protocol: "xunhupay"}
	cases := []struct {
		ch   Channel
		m    Method
		want bool
	}{
		{epay, MethodWechat, true},
		{epay, MethodAlipay, true},
		// 易支付的 type 参数没有聚合值，建单必然失败 → 不支持
		{epay, MethodAggregate, false},
		{xunhu, MethodWechat, true},
		{xunhu, MethodAlipay, true},
		// 虎皮椒不传方式参数，聚合是合法语义
		{xunhu, MethodAggregate, true},
		{Channel{Protocol: ""}, MethodWechat, false},
	}
	for _, c := range cases {
		if got := c.ch.SupportsMethod(c.m); got != c.want {
			t.Fatalf("%s 渠道对 %s 的支持判定 = %v，期望 %v", c.ch.Protocol, c.m, got, c.want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	ch := Channel{Name: "虎皮椒"}
	if got := DisplayName(ch, MethodWechat); got != "虎皮椒-微信支付" {
		t.Fatalf("展示快照不符: %q", got)
	}
	// 方式为空时不能出现「虎皮椒-」这种尾巴（订单列表里像被截断了）
	if got := DisplayName(ch, ""); got != "虎皮椒" {
		t.Fatalf("空方式应退化为纯渠道名，实际 %q", got)
	}
}

func TestEpayType(t *testing.T) {
	if got, err := epayType(MethodWechat); err != nil || got != "wxpay" {
		t.Fatalf("wechat → %q,%v，期望 wxpay", got, err)
	}
	if got, err := epayType(MethodAlipay); err != nil || got != "alipay" {
		t.Fatalf("alipay → %q,%v，期望 alipay", got, err)
	}
	// 聚合/未知必须拒绝：透传中文或空串会被易支付判为非法支付方式
	if _, err := epayType(MethodAggregate); !errors.Is(err, ErrUnsupportedMethod) {
		t.Fatalf("aggregate 应返回 ErrUnsupportedMethod，实际 %v", err)
	}
}

func TestOptionsAggregatesByMethod(t *testing.T) {
	setChannels(t, []map[string]any{
		{
			"id": "xunhupay-wx", "name": "虎皮椒（微信）", "protocol": "xunhupay",
			"enabled": true, "methods": []any{"wechat"},
			"params": map[string]any{
				"pay_address": "https://api.xunhupay.com/payment/do.html",
				"app_id":      "a1", "app_secret": "s1",
			},
		},
		{
			"id": "xunhupay-ali", "name": "虎皮椒（支付宝）", "protocol": "xunhupay",
			"enabled": true, "methods": []any{"alipay"},
			"params": map[string]any{
				"pay_address": "https://api.xunhupay.com/payment/do.html",
				"app_id":      "a2", "app_secret": "s2",
			},
		},
		{
			"id": "epay", "name": "易支付", "protocol": "epay",
			"enabled": true, "methods": []any{"微信支付", "支付宝"},
			"params": map[string]any{"pay_address": "https://pay.example.com", "pid": "1", "key": "k"},
		},
	})

	opts := Options()
	if len(opts) != 2 {
		t.Fatalf("同一种方式只应出一条（易支付被前两条去重），实际 %d 条: %+v", len(opts), opts)
	}
	// 顺序 = 渠道配置顺序：微信在前（xunhupay-wx），支付宝在后（xunhupay-ali）
	if opts[0].Method != string(MethodWechat) || opts[0].ChannelID != "xunhupay-wx" {
		t.Fatalf("微信应路由到配置顺序第一条渠道，实际 %+v", opts[0])
	}
	if opts[1].Method != string(MethodAlipay) || opts[1].ChannelID != "xunhupay-ali" {
		t.Fatalf("支付宝应路由到 xunhupay-ali，实际 %+v", opts[1])
	}
	if opts[0].Label != "微信支付" || opts[1].Label != "支付宝" {
		t.Fatalf("label 应为中文方式名: %q/%q", opts[0].Label, opts[1].Label)
	}
	// 副标题是实际承接渠道：用户看方式，排障看渠道
	if opts[0].Subtitle != "虎皮椒（微信）" {
		t.Fatalf("subtitle 应为渠道名，实际 %q", opts[0].Subtitle)
	}
	// value 仍是 `channel:method`，老前端缓存提交的串格式不变
	if opts[0].Value != JoinOption("xunhupay-wx", "wechat") {
		t.Fatalf("value 格式不符: %q", opts[0].Value)
	}
}

func TestOptionsAggregateFallback(t *testing.T) {
	setChannels(t, []map[string]any{
		{
			// 升级前的历史配置：一个 APPID + 聚合支付
			"id": "xunhupay", "name": "虎皮椒", "protocol": "xunhupay",
			"enabled": true, "methods": []any{"聚合支付"},
			"params": map[string]any{
				"pay_address": "https://api.xunhupay.com/payment/do.html",
				"app_id":      "a1", "app_secret": "s1",
			},
		},
	})
	opts := Options()
	if len(opts) != 1 {
		t.Fatalf("应兜底出一条聚合支付，实际 %d 条", len(opts))
	}
	if opts[0].Method != string(MethodAggregate) || opts[0].Label != "聚合支付" {
		t.Fatalf("兜底项不符: %+v", opts[0])
	}
}

func TestOptionsNoFallbackForEpay(t *testing.T) {
	// 易支付无法表达「聚合」：给它兜底只是造一个必然失败的下单入口
	setChannels(t, []map[string]any{
		{
			"id": "epay", "name": "易支付", "protocol": "epay",
			"enabled": true, "methods": []any{"聚合支付"},
			"params": map[string]any{"pay_address": "https://pay.example.com", "pid": "1", "key": "k"},
		},
	})
	if opts := Options(); len(opts) != 0 {
		t.Fatalf("易支付不应兜底聚合支付，实际 %+v", opts)
	}
}

func TestOptionsSkipsDisabledAndUnknown(t *testing.T) {
	setChannels(t, []map[string]any{
		{
			"id": "epay-off", "name": "停用", "protocol": "epay",
			"enabled": false, "methods": []any{"wechat"},
		},
		{
			"id": "epay-bad", "name": "脏配置", "protocol": "epay",
			"enabled": true, "methods": []any{"乱写的方式"},
		},
	})
	if opts := Options(); len(opts) != 0 {
		t.Fatalf("停用渠道与无法识别的方式都应被跳过，实际 %+v", opts)
	}
}

func TestOptionByMethod(t *testing.T) {
	setChannels(t, []map[string]any{
		{"id": "epay", "name": "易支付", "protocol": "epay", "enabled": true, "methods": []any{"wechat", "alipay"},
			"params": map[string]any{"pay_address": "https://pay.example.com", "pid": "1", "key": "k"}},
	})
	opt, ok := OptionByMethod(MethodAlipay)
	if !ok || opt.ChannelID != "epay" || opt.Method != string(MethodAlipay) {
		t.Fatalf("按方式取选项失败: %+v ok=%v", opt, ok)
	}
	if _, ok := OptionByMethod(MethodAggregate); ok {
		t.Fatal("聚合支付不在选项里，不应命中")
	}
}

// TestDefaultCurrencyOf 锁住「渠道收哪种币种」的唯一判定口径。
//
// 它是三处共用的：收银台过滤（OptionsFor）、下单校验（SettlesIn）、回调缺币种
// 时补齐（order/notify.go）。判定必须来自协议能力，而不是运营手填的参数 ——
// 否则一条 epay 渠道只要写上 usd，就会在美元站出现且被按美元入账。
func TestDefaultCurrencyOf(t *testing.T) {
	// 单币种协议：参数里写什么都是 CNY（含脏数据 usd）。
	for _, cur := range []string{"", "CNY", "cny", "USD", "usd"} {
		ch := Channel{Protocol: string(ProtocolEpay), Params: ChannelParams{SettleCurrency: cur}}
		if got := DefaultCurrencyOf(ch); got != "CNY" {
			t.Fatalf("易支付 settle_currency=%q 应恒为 CNY，实际 %q", cur, got)
		}
	}
	// 多币种协议：读参数并归一大小写。
	cases := map[string]string{"": "CNY", "usd": "USD", "USD": "USD", " CNY ": "CNY"}
	for in, want := range cases {
		ch := Channel{Protocol: string(ProtocolStripe), Params: ChannelParams{SettleCurrency: in}}
		if got := DefaultCurrencyOf(ch); got != want {
			t.Fatalf("Stripe settle_currency=%q 应为 %q，实际 %q", in, want, got)
		}
	}
	// SettlesIn 与它同源：美元订单不能落到易支付渠道上。
	epay := Channel{Protocol: string(ProtocolEpay), Params: ChannelParams{SettleCurrency: "USD"}}
	if epay.SettlesIn("USD") {
		t.Fatal("易支付渠道不应承接美元订单（收银台会展示成人民币金额的美元支付）")
	}
	if !epay.SettlesIn("cny") {
		t.Fatal("币种比对应大小写不敏感")
	}
}
