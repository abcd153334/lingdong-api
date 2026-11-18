package model

import "testing"

// TestPricingIsFree 表驱动覆盖三种计费方式的「价格是否为 0」判定。
//
// 这是 is_free 的唯一派生判据，必须与运行期计费口径严格一致：
//   - nil（未配置）**不是**免费 —— 它是「没有价格」，计费会返回 ErrPriceMissing；
//   - 0（显式填 0）才是免费。
//
// 把两者混为一谈的最坏后果：运营漏填价格，模型在门户被打上「免费」标签，
// 调用却整批 400，双方都以为对方错了。
func TestPricingIsFree(t *testing.T) {
	i := func(v int64) *int64 { return &v }

	cases := []struct {
		name string
		m    Model
		want bool
	}{
		{"按量：全未配置", Model{BillingMode: BillingModeTokens}, false},
		{"按量：输入价 0", Model{BillingMode: BillingModeTokens, Prices: Prices{Input: i(0)}}, true},
		{"按量：输入 0 输出 0", Model{BillingMode: BillingModeTokens, Prices: Prices{Input: i(0), Output: i(0)}}, true},
		{"按量：输入 0 但输出收费", Model{BillingMode: BillingModeTokens, Prices: Prices{Input: i(0), Output: i(2)}}, false},
		{"按量：正常价", Model{BillingMode: BillingModeTokens, Prices: Prices{Input: i(150000)}}, false},
		{"按次：未配置", Model{BillingMode: BillingModeCall}, false},
		{"按次：0 元", Model{BillingMode: BillingModeCall, Prices: Prices{Call: i(0)}}, true},
		{"按次：正常", Model{BillingMode: BillingModeCall, Prices: Prices{Call: i(1000000)}}, false},
		{"按规格：无价目", Model{BillingMode: BillingModeSpec}, false},
		{"按规格：全 0", Model{BillingMode: BillingModeSpec, SpecPrices: SpecPrices{
			Kind: "per_image", ResolutionFixed: "1K", TierPrice: map[string]int64{"1K": 0}}}, true},
		{"按规格：有价", Model{BillingMode: BillingModeSpec, SpecPrices: SpecPrices{
			Kind: "per_video", SecondPrice: map[string]int64{"720p": 12000}}}, false},
		{"按规格：按任务有价", Model{BillingMode: BillingModeSpec, SpecPrices: SpecPrices{
			Kind: "per_task", PerTaskPrice: i(50000)}}, false},
		{"未知计费方式", Model{BillingMode: "nope", Prices: Prices{Input: i(0)}}, false},
	}
	for _, c := range cases {
		if got := c.m.PricingIsFree(); got != c.want {
			t.Fatalf("%s：PricingIsFree = %v, want %v", c.name, got, c.want)
		}
	}
	// nil 模型保守按免费处理：调用方无法证明它收费。
	var mp *Model
	if !mp.PricingIsFree() {
		t.Fatal("nil 模型应判为免费（无法证明其收费）")
	}
}

// TestEffectiveSpecPricesHidesResidue 守住「按次模型带着旧秒价」这类历史残留。
//
// 场景真实存在：模型曾经是 spec，运营改成 call 之后（或手工改库、老版本行）
// spec_prices 里还留着 per_second 价目。若投影层直接透传，门户会把
// ¥0.002/秒 与「按次 ¥0.01」同时显示出来，用户按前者算账、平台按后者收。
func TestEffectiveSpecPricesHidesResidue(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	residue := SpecPrices{Kind: "per_second", PerSecondPrice: i(2000)}

	call := Model{BillingMode: BillingModeCall, Prices: Prices{Call: i(10000)}, SpecPrices: residue}
	if got := call.EffectiveSpecPrices(); !got.IsZero() {
		t.Fatalf("按次模型的残留价目必须被视为空，实际 %+v", got)
	}
	spec := Model{BillingMode: BillingModeSpec, SpecPrices: residue}
	if got := spec.EffectiveSpecPrices(); got.IsZero() || *got.PerSecondPrice != 2000 {
		t.Fatalf("按规格模型必须原样返回价目，实际 %+v", got)
	}
	// nil 模型不能 panic（审计/回滚路径会传 nil）。
	var nilModel *Model
	if !nilModel.EffectiveSpecPrices().IsZero() {
		t.Fatal("nil 模型的价目应为空")
	}
	// 按值版本与指针版本必须同口径（快照投影用的是前者）。
	if !EffectiveSpecPricesFor(BillingModeTokens, residue).IsZero() {
		t.Fatal("EffectiveSpecPricesFor 在非 spec 模式下也必须返回空")
	}
	// 空串 = 这条查询没取 billing_mode（如能力补齐的 Select），不是"非 spec"：
	// 此时判空会让「按价目推导能力」凭空丢字段。必须原样透传。
	if EffectiveSpecPricesFor("", residue).IsZero() {
		t.Fatal("billing_mode 缺失时必须透传价目，而不是当成非 spec")
	}
}

// TestDecodeSpecPricesStrict 严格解码：旧键名必须报错，而不是被静默丢成空价目。
//
// 静默丢键的后果是「保存成功但计费读到 nil」→ ErrPriceMissing → 该模型整批 400，
// 而管理台上价格一栏看起来是填好的。旧键名（带 `_quota` 后缀）尤其危险：
// 灰度期正是新旧键共存的时候，按微解释旧值会造成 100 倍错账。
func TestDecodeSpecPricesStrict(t *testing.T) {
	ok := []string{
		``,
		`null`,
		`{}`,
		`{"kind":"per_image","tier_price":{"1K":28000}}`,
		`{"kind":"per_video","second_price":{"720p":12000},"long_after_seconds":30,"long_video_multiplier":1500000}`,
		`{"kind":"per_task","per_task_price":50000}`,
		`{"kind":"per_video","ref_image_free":0}`,
	}
	for _, raw := range ok {
		if _, err := DecodeSpecPrices([]byte(raw)); err != nil {
			t.Fatalf("合法价目应解码成功: %q → %v", raw, err)
		}
	}

	bad := []string{
		`{"kind":"per_image","default_price_quota":1}`,         // 旧键名
		`{"kind":"per_video","second_price_quota":{"720p":1}}`, // 旧键名
		`{"kind":"per_video","free_ref_images":2}`,             // 旧键名
		`{"kind":"per_image","tier_price":{"1K":"28000"}}`,     // 类型错：字符串
		`{"kind":"per_image","tier_price":{"1K":28.5}}`,        // 类型错：小数
		`{"kind":"per_image","unknown_key":1}`,                 // 未知键
	}
	for _, raw := range bad {
		if _, err := DecodeSpecPrices([]byte(raw)); err == nil {
			t.Fatalf("非法价目必须被拒绝: %q", raw)
		}
	}
}

// TestSpecPricesValueRoundTrip 空价目落 NULL、非空价目落 JSON 且能读回。
//
// NULL 与 JSONB 'null' 在 `IS NULL` 判断里不等价：写成后者会让
// 「未配置价目」和「配了 null」分成两种状态，而计费侧只认其中一种。
func TestSpecPricesValueRoundTrip(t *testing.T) {
	var empty SpecPrices
	v, err := empty.Value()
	if err != nil || v != nil {
		t.Fatalf("空价目应落 NULL，实际 %v（err=%v）", v, err)
	}

	src := SpecPrices{
		Kind:                "per_video",
		SecondPrice:         map[string]int64{"480p": 8000, "1080p": 20000},
		LongAfterSeconds:    i64p(30),
		LongVideoMultiplier: i64p(1500000),
	}
	v, err = src.Value()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var dst SpecPrices
	if err := dst.Scan(v); err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if dst.Kind != src.Kind || dst.SecondPrice["1080p"] != 20000 ||
		dst.LongVideoMultiplier == nil || *dst.LongVideoMultiplier != 1500000 {
		t.Fatalf("往返后价目失真: %+v", dst)
	}
}

func i64p(v int64) *int64 { return &v }
