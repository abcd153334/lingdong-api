package billing

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// TestEffectiveMode 守「配置写错也能出图」的第一条：billing_mode 落在列默认值上。
//
// 线上真实症状：运营把 tier_price 配得完全正确，但 `models.billing_mode` 仍是建表
// 默认值 `tokens` → 网关冻结前直接 400「模型未配置价格」，用户以为模型坏了。
// 纠偏方向必须**只有** 空/tokens → spec：反向降级会让图片请求的 token 数为 0（白送）。
func TestEffectiveMode(t *testing.T) {
	withPrice := model.SpecPrices{Kind: SpecKindPerImage, TierPrice: map[string]int64{"1K": 1000}}
	empty := model.SpecPrices{Kind: SpecKindPerImage}

	cases := []struct {
		name string
		in   string
		sp   model.SpecPrices
		want string
	}{
		{"tokens + 有价目 → spec", model.BillingModeTokens, withPrice, model.BillingModeSpec},
		{"空 + 有价目 → spec", "", withPrice, model.BillingModeSpec},
		{"tokens + 无价目 → tokens（没什么可纠的）", model.BillingModeTokens, empty, model.BillingModeTokens},
		// 运营显式选的按次/按规格一律尊重，不纠正。
		{"call 不纠偏", model.BillingModeCall, withPrice, model.BillingModeCall},
		{"spec 保持", model.BillingModeSpec, withPrice, model.BillingModeSpec},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveMode(c.in, c.sp); got != c.want {
				t.Fatalf("EffectiveMode(%q)=%q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// TestEffectiveKind 守「kind 缺失/不符按价目形状推断」。
//
// 必须先尊重自洽的声明：一个视频模型若残留 tier_price，无条件按形状会把它判成
// 图片，于是视频请求按张计价 —— 比"不纠正"更糟。
func TestEffectiveKind(t *testing.T) {
	cases := []struct {
		name string
		sp   model.SpecPrices
		want string
	}{
		{"kind 自洽保持", model.SpecPrices{Kind: SpecKindPerVideo, SecondPrice: map[string]int64{"720p": 100}}, SpecKindPerVideo},
		{"视频残留 tier_price 仍按视频", model.SpecPrices{
			Kind:        SpecKindPerVideo,
			SecondPrice: map[string]int64{"720p": 100},
			TierPrice:   map[string]int64{"1K": 50},
		}, SpecKindPerVideo},
		{"kind 空按 tier 推断", model.SpecPrices{TierPrice: map[string]int64{"1K": 100}}, SpecKindPerImage},
		{"kind 空按 second 推断", model.SpecPrices{SecondPrice: map[string]int64{"720p": 100}}, SpecKindPerVideo},
		{"kind=per_image 但只有秒价 → 按形状纠成视频", model.SpecPrices{
			Kind:        SpecKindPerImage,
			SecondPrice: map[string]int64{"720p": 100},
		}, SpecKindPerVideo},
		{"全空推断不出", model.SpecPrices{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveKind(c.sp); got != c.want {
				t.Fatalf("EffectiveKind=%q，期望 %q", got, c.want)
			}
		})
	}
}

// TestEffectiveSnapshotCarriesFlag 守纠偏必须留痕且真的改了快照。
//
// 不留痕的话，"这一单是按纠偏后的口径收的"在账单上完全看不出来，
// 运营永远不会去修配置。
func TestEffectiveSnapshotCarriesFlag(t *testing.T) {
	snap := Snapshot{
		BillingMode: model.BillingModeTokens,
		SpecPrice:   model.SpecPrices{TierPrice: map[string]int64{"1K": 1000}},
	}
	got, flags := Effective(snap, "image")
	if got.BillingMode != model.BillingModeSpec {
		t.Fatalf("计费方式应纠为 spec，实际 %q", got.BillingMode)
	}
	if got.SpecPrice.Kind != SpecKindPerImage {
		t.Fatalf("kind 应纠为 per_image，实际 %q", got.SpecPrice.Kind)
	}
	for _, want := range []string{model.AnomalyBillingModeCoerced, model.AnomalySpecKindInferred} {
		if !strings.Contains(flags, want) {
			t.Fatalf("必须留 %s，实际 %q", want, flags)
		}
	}

	// 不改入参：快照是账单凭证，调用方显式替换。
	if snap.BillingMode != model.BillingModeTokens || snap.SpecPrice.Kind != "" {
		t.Fatalf("Effective 不得修改入参：%+v", snap)
	}

	// 纠偏后的快照必须能真的算出钱（不是"能过校验但仍 400"）。
	amount, err := Charge(Snapshot{BillingMode: got.BillingMode, SpecPrice: got.SpecPrice},
		Usage{ImageSpec: "1K", ImageN: 2})
	if err != nil || amount != 2000 {
		t.Fatalf("纠偏后应能计费 2000，实际 %d/%v", amount, err)
	}
}

// TestEffectiveRespectsModality 守「模态允许的 kind 是集合，且冲突时不许把模型搞成不可用」。
//
// 两条现实约束同时成立：
//   - 视频可以用 per_video（按秒）**也可以** per_task（按条）—— 只认一个值会把
//     合法的按条视频模型判成"不符"并改成 per_video，而 second_price 为空 → 直接不可用；
//   - 真的不符时（视频端点却只有图片档位价），保留原口径让它还能跑 + 留痕，
//     而不是报"本端点无可用价目" —— 后者对一个原本能出结果的模型是更糟的破坏。
func TestEffectiveRespectsModality(t *testing.T) {
	// 依据 1：视频 + per_task 是合法的，不得报冲突。
	perTask := int64(1000)
	vidTask := model.SpecPrices{Kind: SpecKindPerTask, PerTaskPrice: &perTask}
	got, flags := Effective(Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: vidTask}, "video")
	if got.SpecPrice.Kind != SpecKindPerTask {
		t.Fatalf("按条的视频模型应保持 per_task，实际 %q", got.SpecPrice.Kind)
	}
	if strings.Contains(flags, model.AnomalySpecKindConflict) {
		t.Fatalf("合法的按条视频模型不应报冲突，实际 %q", flags)
	}

	// 依据 2：视频端点 + 只有 tier_price → 留冲突标记，但不改成不可用。
	imgOnly := model.SpecPrices{Kind: SpecKindPerImage, TierPrice: map[string]int64{"1K": 1000}}
	got2, flags2 := Effective(Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: imgOnly}, "video")
	if !strings.Contains(flags2, model.AnomalySpecKindConflict) {
		t.Fatalf("必须留 spec_kind_conflict，实际 %q", flags2)
	}
	if _, err := Charge(got2, Usage{ImageSpec: "1K", ImageN: 1}); err != nil {
		t.Fatalf("冲突时仍要保持可计费（不能把模型搞成不可用）：%v", err)
	}

	// 依据 3：形状与端点一致时不报冲突。
	vidOK := model.SpecPrices{Kind: SpecKindPerVideo, SecondPrice: map[string]int64{"720p": 1000}}
	if _, f := Effective(Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: vidOK}, "video"); strings.Contains(f, model.AnomalySpecKindConflict) {
		t.Fatalf("形状与端点一致时不应报冲突，实际 %q", f)
	}
}

// TestHasSpecPricingIgnoresZeroResidue 守「残留的空价目不能把模型纠偏成 spec」。
//
// 旧库迁移会给任意模态写 tier_price。若是全 0（且没有 kind），只看"map 非空"
// 会把它纠偏成 spec → per_image → 整单 0 元。0 只有在**显式声明 kind** 时才是
// 合法的免费配置。
func TestHasSpecPricingIgnoresZeroResidue(t *testing.T) {
	if HasSpecPricing(model.SpecPrices{TierPrice: map[string]int64{"1K": 0}}) {
		t.Fatal("无 kind 的全 0 残留不算配了价目（否则 chat 模型会变 0 元）")
	}
	if HasSpecPricing(model.SpecPrices{TierPrice: map[string]int64{"1K": 0}, Kind: SpecKindPerImage}) != true {
		t.Fatal("显式声明 kind 的全 0 免费模型必须算配了价目")
	}
	if !HasSpecPricing(model.SpecPrices{TierPrice: map[string]int64{"1K": 1}}) {
		t.Fatal("有正价但没写 kind 也应算配了价目")
	}
}

// TestPriceProblem 守「未配价」的唯一判据。
//
// 以前 stats 用 SpecPrices.IsZero()：kind=per_image 但价目全空时它不是零值，
// 于是看板报 0 个未配价，而用户一调就 400。
func TestPriceProblem(t *testing.T) {
	cases := []struct {
		name string
		mode string
		p    model.Prices
		sp   model.SpecPrices
		want bool // 是否有问题
	}{
		{"spec + 有价目", model.BillingModeSpec, model.Prices{}, model.SpecPrices{TierPrice: map[string]int64{"1K": 1}}, false},
		{"spec + 只有 kind 没价目", model.BillingModeSpec, model.Prices{}, model.SpecPrices{Kind: SpecKindPerImage}, true},
		{"tokens + 有价目（会纠偏）", model.BillingModeTokens, model.Prices{}, model.SpecPrices{TierPrice: map[string]int64{"1K": 1}}, false},
		{"tokens + 无价目无输入价", model.BillingModeTokens, model.Prices{}, model.SpecPrices{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PriceProblem(c.mode, c.p, c.sp) != ""
			if got != c.want {
				t.Fatalf("PriceProblem 判为有问题=%v，期望 %v（原因 %q）",
					got, c.want, PriceProblem(c.mode, c.p, c.sp))
			}
		})
	}
}

// TestImageCountHintSingleSource 守「这一次按几张计费」的规则只有一份。
//
// 同步网关与异步任务都调它：两边各写一遍的结果是「异步冻结 1 张、上游出 15 张」。
func TestImageCountHintSingleSource(t *testing.T) {
	cases := []struct {
		name string
		in   ImageCountInput
		want int64
	}{
		{"什么都没写 → 1 张", ImageCountInput{}, 1},
		{"显式 n=3", ImageCountInput{N: 3}, 3},
		{"n 超 max_n 夹到 max_n", ImageCountInput{N: 9, MaxN: 4}, 4},
		{"组图 auto 用 max_images", ImageCountInput{SeqAuto: true, MaxImages: 6, MaxN: 10}, 6},
		{"组图 auto 没给上限 → max_n", ImageCountInput{SeqAuto: true, MaxN: 4}, 4},
		{"图层拆分按 max_n", ImageCountInput{LayerDecomp: true, MaxN: 15}, 15},
		// 两个参数同时出现：图层是"一次调用的产物形态"，必须压过组图。
		// 以前是两个独立 if（SeqAuto 后写覆盖），这里返回 4，而注释与测试名都说 15。
		{"图层压过组图（显式 max_images）", ImageCountInput{LayerDecomp: true, SeqAuto: true, MaxImages: 4, MaxN: 15}, 15},
		// 参考图与生成数共享额度，但**只在组图**时成立：单张请求带参考图不扣额度。
		// 组图缺省按上限，参考图占位后要扣掉（否则冻结超过上游允许的张数）。
		{"组图缺省时参考图占额度", ImageCountInput{SeqAuto: true, RefImages: 2, MaxN: 6}, 4},
		{"单张请求带参考图不抵扣", ImageCountInput{N: 3, RefImages: 2, MaxN: 6}, 3},
		// max_n 未配置时上限按 1：按 128 冻一次会把用户余额直接打空（402）。
		{"max_n 未配置", ImageCountInput{MaxN: 0}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ImageCountHint(c.in); got != c.want {
				t.Fatalf("ImageCountHint=%d，期望 %d", got, c.want)
			}
		})
	}
}
