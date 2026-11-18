package billing

import (
	"errors"
	"slices"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

/*
价格单位（写断言前必读，写错一位就是差 100 万倍的账）：
  model.Prices     = 微 / 百万 tokens  → $2.5/1M 写作 2_500_000
  model.SpecPrices = 微 / 张、微 / 秒、微 / 任务 → $0.001/秒 写作 1_000
  money.Scale      = 1_000_000（1 货币单位 = 100 万微），不可配
本文件的金额期望值全部是**微**。
*/

const (
	priceInput  = 2_500_000  // $2.5 / 1M tokens
	priceOutput = 10_000_000 // $10  / 1M tokens
)

func iptr(v int64) *int64 { return &v }

// baseSnap 是 token 型基准快照（输入 $2.5/1M、输出 $10/1M，无折扣）。
func baseSnap() Snapshot {
	return Snapshot{
		BillingMode: model.BillingModeTokens,
		Price:       model.Prices{Input: iptr(priceInput), Output: iptr(priceOutput)},
	}
}

func TestChargeTokenOnly(t *testing.T) {
	// 输入 1000 × $2.5/1M = 2500 微；输出 500 × $10/1M = 5000 微。
	got, err := Charge(baseSnap(), Usage{PromptTokens: 1000, CompletionTokens: 500})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 7500 {
		t.Fatalf("want 7500, got %d", got)
	}
}

// 分组折扣是**微比例**（1e6 = 不打折），不是倍率：7500 × 0.8 = 6000。
func TestChargeGroupDiscount(t *testing.T) {
	s := baseSnap()
	s.Discount = 800_000
	got, err := Charge(s, Usage{PromptTokens: 1000, CompletionTokens: 500})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 6000 {
		t.Fatalf("want 6000, got %d", got)
	}
}

// 折扣 >= 1e6 或 <= 0 一律视为不打折。
//
// 这条守的是旧数据的坑：库里若残留"倍率"语义的 0.9，会被当成 0.00009% 折扣 → 近乎免费。
func TestChargeDiscountOutOfRangeIgnored(t *testing.T) {
	for _, d := range []int64{0, money.Scale, 1, 900_000_000} {
		s := baseSnap()
		s.Discount = d
		got, err := Charge(s, Usage{PromptTokens: 1000, CompletionTokens: 500})
		if err != nil {
			t.Fatalf("discount=%d unexpected err: %v", d, err)
		}
		want := int64(7500)
		if d > 0 && d < money.Scale {
			want = 0 // 1 = 百万分之一，四舍五入后为 0
		}
		if got != want {
			t.Fatalf("discount=%d want %d, got %d", d, want, got)
		}
	}
}

// 未配置的通道单价回落 input 单价（同单位，能兜底且不漏收）。
func TestChargeNilLaneFallsBackToInput(t *testing.T) {
	s := Snapshot{BillingMode: model.BillingModeTokens, Price: model.Prices{Input: iptr(1_000_000)}}
	// 输入 100 + 输出 100（output 未配 → 用 input 价）= 200 微
	got, err := Charge(s, Usage{PromptTokens: 100, CompletionTokens: 100})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 200 {
		t.Fatalf("want 200, got %d", got)
	}
}

// 缓存四条通道各自取价；cache_create_1h 未配置时回落 cache_create（不再 ×1.6 派生）。
func TestChargeCacheChannels(t *testing.T) {
	s := baseSnap()
	s.Price.Cache = iptr(1_250_000)       // $1.25/1M
	s.Price.CacheCreate = iptr(3_125_000) // $3.125/1M
	u := Usage{PromptTokens: 1000, CacheTokens: 400, CacheWrite5mTokens: 100, CacheWrite1hTokens: 100}
	// base = 1000 − 400 − 100 − 100 = 400
	// 400×2.5 + 400×1.25 + 100×3.125 + 100×3.125
	// = 1000 + 500 + 312.5 + 312.5 = 2125 微（只在最后舍入一次）
	got, err := Charge(s, u)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 2125 {
		t.Fatalf("want 2125, got %d", got)
	}
	// 反向断言：1h 若按旧实现的 ×1.6 派生会得 312.5×1.6=500 → 总和 2312.5。
	if got == 2312 {
		t.Fatal("cache_create_1h 仍在用 ×1.6 派生，应回落 cache_create")
	}
}

// 媒体通道按各自单价计费，且 prompt=0 时不参与 base 扣减。
func TestChargeMediaChannels(t *testing.T) {
	s := baseSnap()
	s.Price.Image = iptr(25_000_000)       // 10× input
	s.Price.Audio = iptr(10_000_000)       // 4× input
	s.Price.AudioOutput = iptr(20_000_000) // 8× input
	// image 200×25 + audioIn 100×10 + audioOut 50×20 = 5000+1000+1000 = 7000
	got, err := Charge(s, Usage{ImageTokens: 200, AudioInputTokens: 100, AudioOutputTokens: 50})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 7000 {
		t.Fatalf("want 7000, got %d", got)
	}
}

// 缓存倒挂（cache > prompt）：base 钳 0 并打标记，缓存仍全额计费，不得算成负数。
func TestChargeCacheInvertedClamped(t *testing.T) {
	s := baseSnap()
	s.Price.Cache = iptr(250_000) // 0.1× input
	r, err := ChargeDetailed(s, Usage{PromptTokens: 100, CacheTokens: 200})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// base = max(100−200,0)=0；cache 200×0.25 = 50
	if r.Amount != 50 {
		t.Fatalf("want 50, got %d", r.Amount)
	}
	if !hasFlag(r.Flags, model.AnomalyCacheOverlapClamped) {
		t.Fatalf("缺少 cache_overlap_clamped 标记: %v", r.Flags)
	}
}

// Anthropic 语义：prompt 不含缓存，缓存需全额叠加。
func TestChargeAnthropicSemantic(t *testing.T) {
	s := baseSnap()
	s.Price.Cache = iptr(250_000)
	u := Usage{PromptTokens: 100, CacheTokens: 1000, Semantic: SemanticAnthropic}
	// base=100（Anthropic 语义不减 cache）→ 100×2.5 = 250；缓存 1000×0.25 = 250 → 500
	got, err := Charge(s, u)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 500 {
		t.Fatalf("want 500, got %d", got)
	}
}

// 负值用量归零并打标记，不得抵扣其它通道。
func TestChargeNegativeInputClamped(t *testing.T) {
	r, err := ChargeDetailed(baseSnap(), Usage{PromptTokens: -100, CompletionTokens: -50})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Amount != 0 {
		t.Fatalf("want 0, got %d", r.Amount)
	}
	if !hasFlag(r.Flags, model.AnomalyNegativeInput) {
		t.Fatalf("缺少 negative_input 标记: %v", r.Flags)
	}
}

// 结算金额截顶：单请求不得超过全局上限。
//
// 用按次价构造（而不是 token 用量）：token 型价目是"微/百万 token"，
// 要撞到 20,000 货币单位的上限需要 80 亿 token 量级，那种用例测的是输入合法性而不是截顶。
func TestChargeCapped(t *testing.T) {
	s := Snapshot{BillingMode: model.BillingModeCall, Price: model.Prices{Call: iptr(30_000 * money.Scale)}}
	r, err := ChargeDetailed(s, Usage{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Amount != maxCharge() {
		t.Fatalf("want cap %d, got %d", maxCharge(), r.Amount)
	}
	if r.Uncapped != 30_000*money.Scale {
		t.Fatalf("Uncapped 应保留截顶前金额，got %d", r.Uncapped)
	}
	if !hasFlag(r.Flags, model.AnomalyChargeCapped) {
		t.Fatalf("缺少 charge_capped 标记: %v", r.Flags)
	}
}

// 按次计费：固定价 × 折扣。
func TestChargePerCall(t *testing.T) {
	s := Snapshot{BillingMode: model.BillingModeCall, Price: model.Prices{Call: iptr(500_000)}, Discount: 800_000}
	got, err := Charge(s, Usage{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 400_000 {
		t.Fatalf("want 400000, got %d", got)
	}
}

// 计费方式未知 / 价目缺失一律 fail-closed，绝不当成免费放行。
func TestChargeUnknownModeFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		s    Snapshot
	}{
		{"空计费方式", Snapshot{}},
		{"未知计费方式", Snapshot{BillingMode: "per_token"}},
		{"token 无输入价", Snapshot{BillingMode: model.BillingModeTokens}},
		{"call 无单次价", Snapshot{BillingMode: model.BillingModeCall}},
		// 按规格但价目为空（kind 缺失）= 未配置价格，不是"不支持的 kind"。
		{"spec 空价目", Snapshot{BillingMode: model.BillingModeSpec}},
	}
	for _, c := range cases {
		if _, err := Charge(c.s, Usage{PromptTokens: 100}); !errors.Is(err, ErrPriceMissing) {
			t.Fatalf("%s: want ErrPriceMissing, got %v", c.name, err)
		}
	}
}

// imageSnap 构造按规格型图片模型快照（档位 1K/2K/4K + 默认档 + n 上限 4）。
//
// 默认档是**档位键**，不是金额：金额只在 TierPrice 里。以前这里传一个
// "兜底单价"，计费时反过来用"价格恰等于某档价"推默认档 —— 改一次档价就
// 悄悄换了默认档，是本次改造要消灭的那个坑。
func imageSnap(defTier string) Snapshot {
	return Snapshot{
		BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{
			Kind:            SpecKindPerImage,
			TierPrice:       map[string]int64{"1K": 14_000, "2K": 20_000, "4K": 28_000},
			ResolutionFixed: defTier,
			MaxN:            iptr(4),
		},
	}
}

func TestSpecChargeImage(t *testing.T) {
	// 命中档位：2K × 2 张 = 40000
	if got, err := Charge(imageSnap("4K"), Usage{ImageSpec: "2K", ImageN: 2}); err != nil || got != 40_000 {
		t.Fatalf("want 40000/nil, got %d/%v", got, err)
	}
	// 未命中档位 → 兜底 default_price × n
	if got, err := Charge(imageSnap("4K"), Usage{ImageSpec: "8K", ImageN: 1}); err != nil || got != 28_000 {
		t.Fatalf("want 28000/nil, got %d/%v", got, err)
	}
	// n 缺省 = 1
	if got, err := Charge(imageSnap("4K"), Usage{ImageSpec: "1K"}); err != nil || got != 14_000 {
		t.Fatalf("want 14000/nil, got %d/%v", got, err)
	}
	// 八折：1K(14000) × 3 张 × 0.8 = 33600
	s := imageSnap("4K")
	s.Discount = 800_000
	if got, err := Charge(s, Usage{ImageSpec: "1K", ImageN: 3}); err != nil || got != 33_600 {
		t.Fatalf("want 33600/nil, got %d/%v", got, err)
	}
}

func TestSpecChargeBoundaries(t *testing.T) {
	// 未命中档位 → 最高单价档（不再有"兜底价"这一层）。4K=28000。
	if got, err := Charge(imageSnap("1K"), Usage{ImageSpec: "8K"}); err != nil || got != 28_000 {
		t.Fatalf("未命中档应按最高档 28000，got %d/%v", got, err)
	}
	// n > MaxN(4)：预扣期也**夹紧**（按 4 张冻），不拒绝。
	// 拒绝等于"用户多要两张就让整单失败"；而上游若不接受会明确报错，
	// 平台不改写出站参数。真正该拒的只有引擎上限（见下一条）。
	est, err := Estimate(imageSnap("4K"), 0, 0, Usage{ImageSpec: "1K", ImageN: 5})
	if err != nil {
		t.Fatalf("max_n 超限不应拒绝预扣，got %v", err)
	}
	if est != 56_000 { // 14_000 × 4（夹到 max_n）
		t.Fatalf("预扣应按上限 4 张冻结 want 56000, got %d", est)
	}
	// 引擎上限（128）：预扣期拒绝。它不是模型能力，而是防 `n: 100000` 一次冻爆余额的护栏。
	if _, err := Estimate(imageSnap("4K"), 0, 0, Usage{ImageSpec: "1K", ImageN: MaxImageN + 1}); !errors.Is(err, ErrInvalidTier) {
		t.Fatalf("超引擎上限 want ErrInvalidTier, got %v", err)
	}
	// ……结算期饱和到 MaxN 并打标记，绝不报错：
	// 钱已经冻了、图已经生成了，报错只会让结算失败 → 任务停在 running →
	// deadline 全额退款 → 图白送。
	capSnap := imageSnap("4K")
	capSnap.SpecPrice.MaxN = iptr(4)
	got, err := ChargeDetailed(capSnap, Usage{ImageSpec: "1K", ImageN: 9})
	if err != nil {
		t.Fatalf("结算期不应报错，got %v", err)
	}
	if got.Amount != 56_000 { // 14_000 × 4
		t.Fatalf("结算期应按上限 4 张计费 want 56000, got %d", got.Amount)
	}
	if !slices.Contains(got.Flags, model.AnomalyImageNCapped) {
		t.Fatalf("饱和必须打 image_n_capped 标记，got %v", got.Flags)
	}
	// MaxImageN 引擎上限同理：结算期饱和而不是拒绝。
	noMax := imageSnap("4K")
	noMax.SpecPrice.MaxN = nil
	got, err = ChargeDetailed(noMax, Usage{ImageSpec: "1K", ImageN: MaxImageN + 10})
	if err != nil || got.Amount != int64(14_000)*MaxImageN {
		t.Fatalf("引擎上限饱和 want %d/nil, got %d/%v", 14_000*MaxImageN, got.Amount, err)
	}
	// 视频 per_video：未知分辨率档位 → 拒绝（per_video 无兜底默认价）
	if _, err := Charge(videoSnap(), Usage{VideoResolution: "4k", VideoSeconds: 5}); !errors.Is(err, ErrInvalidTier) {
		t.Fatalf("want ErrInvalidTier, got %v", err)
	}
	// 未知 kind → 不支持（用 per_minute 而不是 per_second：
	// per_second 是音频待支持的类型，断言它会随批次 B 翻转，不该在这里锁死）
	if _, err := Charge(Snapshot{BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{Kind: "per_minute"}}, Usage{}); !errors.Is(err, ErrSpecUnsupported) {
		t.Fatalf("want ErrSpecUnsupported, got %v", err)
	}
}

// videoSnap 构造按规格型视频模型快照（三档秒价 + 三类素材单价）。
func videoSnap() Snapshot {
	return Snapshot{
		BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{
			Kind:          SpecKindPerVideo,
			SecondPrice:   map[string]int64{"480p": 9_600, "720p": 12_000, "1080p": 18_000},
			RefImagePrice: iptr(15_000),
			RefVideoPrice: iptr(40_000),
			RefAudioPrice: iptr(25_000),
		},
	}
}

func TestSpecChargeVideo(t *testing.T) {
	// 720p × 5s = 60000
	if got, err := Charge(videoSnap(), Usage{VideoResolution: "720p", VideoSeconds: 5}); err != nil || got != 60_000 {
		t.Fatalf("want 60000/nil, got %d/%v", got, err)
	}
	// 素材累加：480p × 2s(19200) + 图×2(30000) + 视频×1(40000) = 89200
	if got, err := Charge(videoSnap(), Usage{VideoResolution: "480p", VideoSeconds: 2, RefImages: 2, RefVideos: 1}); err != nil || got != 89_200 {
		t.Fatalf("want 89200/nil, got %d/%v", got, err)
	}
	// 八折：720p × 3s = 36000 × 0.8 = 28800
	s := videoSnap()
	s.Discount = 800_000
	if got, err := Charge(s, Usage{VideoResolution: "720p", VideoSeconds: 3}); err != nil || got != 28_800 {
		t.Fatalf("want 28800/nil, got %d/%v", got, err)
	}
	// 秒数饱和上界：1080p × 3601s → 18000 × 3600 = 64800000
	if got, err := Charge(videoSnap(), Usage{VideoResolution: "1080p", VideoSeconds: 3601}); err != nil || got != 64_800_000 {
		t.Fatalf("want 64800000/nil, got %d/%v", got, err)
	}
	// 素材单价缺省 → 0（免费档）：仅按秒计费 720p × 2s = 24000
	noRef := Snapshot{
		BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{
			Kind:        SpecKindPerVideo,
			SecondPrice: map[string]int64{"720p": 12_000},
		},
	}
	if got, err := Charge(noRef, Usage{VideoResolution: "720p", VideoSeconds: 2, RefImages: 3}); err != nil || got != 24_000 {
		t.Fatalf("want 24000/nil, got %d/%v", got, err)
	}
}

func TestEstimateSpec(t *testing.T) {
	got, err := Estimate(imageSnap("4K"), 0, 0, Usage{ImageSpec: "4K", ImageN: 2})
	if err != nil || got != 56_000 {
		t.Fatalf("want 56000/nil, got %d/%v", got, err)
	}
}

func TestEstimateTokenCeil(t *testing.T) {
	// 预扣用"输入侧最高单价"加权：input 2500 + cache 0 + image 0 … 取 max = 2.5e6；
	// 输出侧取 output 与 audio_output 的最大值 = 1e7。
	// 1000×2.5 + 800×10 = 2500+8000 = 10500（整数，正好无余数）
	got, err := Estimate(baseSnap(), 1000, 800, Usage{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 10_500 {
		t.Fatalf("want 10500, got %d", got)
	}
	// 向上取整：1×2.5 + 1×10 = 12.5 → 13（多冻可退，少冻只能少收）
	if g, _ := Estimate(baseSnap(), 1, 1, Usage{}); g != 13 {
		t.Fatalf("want 13, got %d", g)
	}
}

func TestEstimateErrorsAndZero(t *testing.T) {
	if _, err := Estimate(Snapshot{BillingMode: model.BillingModeTokens}, 10, 10, Usage{}); !errors.Is(err, ErrPriceMissing) {
		t.Fatalf("want ErrPriceMissing, got %v", err)
	}
	if got, err := Estimate(baseSnap(), 0, 0, Usage{}); err != nil || got != 0 {
		t.Fatalf("want 0/nil, got %d/%v", got, err)
	}
}

// 预扣阶段超上限直接拒绝（不冻结不合理额度）。
func TestEstimateRejectsOverCap(t *testing.T) {
	// 1e12 tokens × $2.5/1M = 2.5e12 微 = 250 万货币单位 > 单次上限。
	_, err := Estimate(baseSnap(), 1_000_000_000_000, 0, Usage{})
	if !errors.Is(err, ErrChargeOverLimit) {
		t.Fatalf("want ErrChargeOverLimit, got %v", err)
	}
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// perTaskSnap 构造按任务计价的模型快照（音乐/歌词）。
func perTaskSnap(price int64) Snapshot {
	return Snapshot{
		BillingMode: model.BillingModeSpec,
		SpecPrice:   model.SpecPrices{Kind: SpecKindPerTask, PerTaskPrice: iptr(price)},
	}
}

// 按任务计费：一次任务一个固定价，与产物条数、时长、张数全部无关，末乘折扣。
func TestSpecChargePerTask(t *testing.T) {
	// 冻结（Estimate）与结算（Charge）必须同价：同一份价目两条路径都不能漂。
	frozen, err := Estimate(perTaskSnap(50_000), 0, 0, Usage{})
	if err != nil || frozen != 50_000 {
		t.Fatalf("Estimate want 50000/nil，got %d/%v", frozen, err)
	}
	got, err := Charge(perTaskSnap(50_000), Usage{})
	if err != nil || got != 50_000 {
		t.Fatalf("Charge want 50000/nil，got %d/%v", got, err)
	}
	// 上游一次出 2 首（ImageN=2）也不放大：per_task 不读任何用量字段。
	if got, err := Charge(perTaskSnap(50_000), Usage{ImageN: 2, VideoSeconds: 30}); err != nil || got != 50_000 {
		t.Fatalf("per_task 不应随用量放大，want 50000，got %d/%v", got, err)
	}
	// 八折末乘：50000 × 0.8 = 40000
	s := perTaskSnap(50_000)
	s.Discount = 800_000
	if got, err := Charge(s, Usage{}); err != nil || got != 40_000 {
		t.Fatalf("折扣 want 40000，got %d/%v", got, err)
	}
}

// 「显式 0 = 免费 / 缺失 = ErrPriceMissing」的边界。
func TestSpecChargePerTaskFailClosed(t *testing.T) {
	// 缺失 / 负价 → ErrPriceMissing（fail-closed，防白嫖）。
	for _, sp := range []model.SpecPrices{
		{Kind: SpecKindPerTask},
		{Kind: SpecKindPerTask, PerTaskPrice: iptr(-100)},
	} {
		s := Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: sp}
		if _, err := Estimate(s, 0, 0, Usage{}); !errors.Is(err, ErrPriceMissing) {
			t.Fatalf("Estimate 应报 ErrPriceMissing，实际 %v（%+v）", err, sp)
		}
		if _, err := Charge(s, Usage{}); !errors.Is(err, ErrPriceMissing) {
			t.Fatalf("Charge 应报 ErrPriceMissing，实际 %v（%+v）", err, sp)
		}
	}
	// 显式 0 → 免费（0, nil）。
	zero := Snapshot{BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{Kind: SpecKindPerTask, PerTaskPrice: iptr(0)}}
	if got, err := Estimate(zero, 0, 0, Usage{}); err != nil || got != 0 {
		t.Fatalf("Estimate 显式 0 应免费，want 0/nil，got %d/%v", got, err)
	}
	if got, err := Charge(zero, Usage{}); err != nil || got != 0 {
		t.Fatalf("Charge 显式 0 应免费，want 0/nil，got %d/%v", got, err)
	}
}

// 锁定「显式 0 = 免费 / nil = 未配置」这条核心不变式（0=免费一等公民）。
func TestChargeExplicitZeroIsFree(t *testing.T) {
	// 按量：Input = 0（显式）→ 免费；nil → ErrPriceMissing。
	zeroToken := Snapshot{BillingMode: model.BillingModeTokens, Price: model.Prices{Input: iptr(0)}}
	if got, err := Estimate(zeroToken, 100, 100, Usage{}); err != nil || got != 0 {
		t.Fatalf("tokens 显式 0 Estimate 应免费，want 0/nil，got %d/%v", got, err)
	}
	if got, err := Charge(zeroToken, Usage{PromptTokens: 100, CompletionTokens: 50}); err != nil || got != 0 {
		t.Fatalf("tokens 显式 0 Charge 应免费，want 0/nil，got %d/%v", got, err)
	}

	// 按次：Call = 0（显式）→ 免费；nil → ErrPriceMissing。
	zeroCall := Snapshot{BillingMode: model.BillingModeCall, Price: model.Prices{Call: iptr(0)}}
	if got, err := Estimate(zeroCall, 0, 0, Usage{}); err != nil || got != 0 {
		t.Fatalf("call 显式 0 Estimate 应免费，want 0/nil，got %d/%v", got, err)
	}
	if got, err := Charge(zeroCall, Usage{}); err != nil || got != 0 {
		t.Fatalf("call 显式 0 Charge 应免费，want 0/nil，got %d/%v", got, err)
	}

	// 按规格：档位价显式 0 → 免费（0 是合法配置，不是"未配置"）。
	zeroSpec := imageSnap("1K")
	zeroSpec.SpecPrice.TierPrice = map[string]int64{"1K": 0, "2K": 0, "4K": 0}
	if got, err := Estimate(zeroSpec, 0, 0, Usage{ImageSpec: "1K", ImageN: 2}); err != nil || got != 0 {
		t.Fatalf("spec 显式 0 Estimate 应免费，want 0/nil，got %d/%v", got, err)
	}
	if got, err := Charge(zeroSpec, Usage{ImageSpec: "1K", ImageN: 2}); err != nil || got != 0 {
		t.Fatalf("spec 显式 0 Charge 应免费，want 0/nil，got %d/%v", got, err)
	}
}

// ResolveImageCount 是同步与流式共用的**唯一**张数判定。这张表就是两份实现合并时
// 的等价性证据：每一行都对应改造前某一条分支的行为。
func TestResolveImageCount(t *testing.T) {
	cases := []struct {
		name                        string
		billed, delivered, required int64
		want                        int64
		partial                     bool
	}{
		{"上游报 10 张全交付", 10, 10, 1, 10, false},
		{"上游报 10 张只交付 7（按交付计）", 10, 7, 1, 7, true},
		{"上游报 10 张一张没交（不能免费）", 10, 0, 1, 1, true},
		{"上游没报但交付了 7", 0, 7, 1, 7, false},
		{"上游没报也没交付，回落请求 3", 0, 0, 3, 3, false},
		{"全零至少按 1 张", 0, 0, 0, 1, false},
		{"同步路径（产出=交付）", 4, 4, 1, 4, false},
		{"交付多于产出（不应发生，按产出）", 5, 8, 1, 5, false},
	}
	for _, c := range cases {
		n, partial := ResolveImageCount(c.billed, c.delivered, c.required)
		if n != c.want || partial != c.partial {
			t.Fatalf("%s：got (%d,%v)，want (%d,%v)", c.name, n, partial, c.want, c.partial)
		}
	}
}
