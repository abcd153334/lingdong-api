package billing

import (
	"testing"

	"lingdong-api/internal/model"
)

// perSecondSnap 构造一份音频按秒计费的快照（¥0.001/秒，最低 ¥0.01）。
func perSecondSnap(perSecond, minPrice *int64, maxSeconds *int64) Snapshot {
	return Snapshot{
		ModelID:     "indextts",
		BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{
			Kind:           SpecKindPerSecond,
			PerSecondPrice: perSecond,
			MinPrice:       minPrice,
			MaxSeconds:     maxSeconds,
		},
	}
}

func micro(v int64) *int64 { return &v }

// TestPerSecondCharge 锁定音频按秒的三条规则：
//  1. 金额 = 产物秒数 × 每秒单价；
//  2. 不足最低消费时按最低消费收（上游的最低计费时长）；
//  3. max_seconds 饱和 + 引擎上限饱和，绝不按荒唐时长收。
func TestPerSecondCharge(t *testing.T) {
	cases := []struct {
		name string
		snap Snapshot
		secs int64
		want int64
	}{
		{"20 秒 × 1000 微 = 20000", perSecondSnap(micro(1000), micro(10000), nil), 20, 20000},
		{"3 秒不足最低消费 → 10000", perSecondSnap(micro(1000), micro(10000), nil), 3, 10000},
		{"刚好等于最低消费", perSecondSnap(micro(1000), micro(10000), nil), 10, 10000},
		{"无最低消费按实收", perSecondSnap(micro(1000), nil, nil), 3, 3000},
		{"max_seconds 饱和", perSecondSnap(micro(1000), nil, micro(5)), 100, 5000},
		{"引擎上限饱和（>3600 秒只按 3600 收）", perSecondSnap(micro(1000), nil, nil), 999999, 3600 * 1000},
	}
	for _, c := range cases {
		got, err := Charge(c.snap, Usage{AudioDurationSecs: c.secs})
		if err != nil {
			t.Fatalf("%s：不应报错: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s：金额应为 %d，实际 %d", c.name, c.want, got)
		}
	}
}

// TestPerSecondChargeMissingPrice 锁死 fail-closed：没配每秒单价不能变成免费。
func TestPerSecondChargeMissingPrice(t *testing.T) {
	_, err := Charge(perSecondSnap(nil, micro(10000), nil), Usage{AudioDurationSecs: 10})
	if err != ErrPriceMissing {
		t.Fatalf("未配置每秒单价应返回 ErrPriceMissing，实际 %v", err)
	}
}

// TestPerSecondEstimateFromChars 锁死预扣估算：结算前没有产物，
// 按文本长度估（中文约 4 字/秒，向上取整），并同样抬到最低消费。
//
// 没有这条估算，预扣额恒为最低消费 —— 10 分钟有声书会先冻 1 分钱，
// 结算时才发现余额不够，用户拿到音频而平台扣不到钱。
func TestPerSecondEstimateFromChars(t *testing.T) {
	snap := perSecondSnap(micro(1000), micro(10000), nil)
	// 400 字 → 100 秒 → 100000 微。
	got, err := Estimate(snap, 0, 0, Usage{AudioChars: 400})
	if err != nil {
		t.Fatalf("估算不应报错: %v", err)
	}
	if got != 100000 {
		t.Fatalf("400 字应估 100 秒 / 100000 微，实际 %d", got)
	}
	// 3 字 → 1 秒 → 1000 微，但最低消费 10000。
	got, err = Estimate(snap, 0, 0, Usage{AudioChars: 3})
	if err != nil {
		t.Fatalf("估算不应报错: %v", err)
	}
	if got != 10000 {
		t.Fatalf("1 秒应被最低消费抬到 10000，实际 %d", got)
	}
}

// TestValidateSpecPricesAudio 锁定音频价目的保存期校验：
// kind 必须是 per_second、每秒单价必填，且不允许混入图片/视频字段。
func TestValidateSpecPricesAudio(t *testing.T) {
	// max_seconds 是必填：它是单次音频的最坏金额，也是结算上限（ChargeCeiling）。
	maxSec := int64(60)
	if _, err := ValidateSpecPrices("audio", model.SpecPrices{
		Kind: SpecKindPerSecond, PerSecondPrice: micro(1000), MinPrice: micro(10000), MaxSeconds: &maxSec,
	}); err != nil {
		t.Fatalf("合法音频价目应通过: %v", err)
	}
	// 缺 max_seconds 必须报错：没有它就只剩「零冻结兜底」，长音频会系统性少收。
	if _, err := ValidateSpecPrices("audio", model.SpecPrices{
		Kind: SpecKindPerSecond, PerSecondPrice: micro(1000),
	}); err == nil {
		t.Fatalf("缺 max_seconds 应被拒绝")
	}
	bad := []struct {
		name string
		sp   model.SpecPrices
	}{
		{"kind 用了按次", model.SpecPrices{Kind: SpecKindPerTask, PerSecondPrice: micro(1), MaxSeconds: &maxSec}},
		{"缺每秒单价", model.SpecPrices{Kind: SpecKindPerSecond, MaxSeconds: &maxSec}},
		{"负单价", model.SpecPrices{Kind: SpecKindPerSecond, PerSecondPrice: micro(-1), MaxSeconds: &maxSec}},
		{"混入图片档位", model.SpecPrices{Kind: SpecKindPerSecond, PerSecondPrice: micro(1), MaxSeconds: &maxSec,
			TierPrice: map[string]int64{"1K": 100}}},
		{"混入视频每秒档位", model.SpecPrices{Kind: SpecKindPerSecond, PerSecondPrice: micro(1), MaxSeconds: &maxSec,
			SecondPrice: map[string]int64{"720p": 100}}},
	}
	for _, c := range bad {
		if _, err := ValidateSpecPrices("audio", c.sp); err == nil {
			t.Fatalf("%s 应被拒绝", c.name)
		}
	}
}

// TestPerSecondSettlementEstimatesWhenDurationUnknown 锁死结算期的「时长不可得」口径。
//
// 场景：上游产物是 opus/aac 裸流（探测不出），或流式体没有 Content-Length。
// 旧口径是「按最低消费收」——没有最低消费时就是 0 元，等于把整段音频白送，
// 而且静默：一条渠道的按秒模型会长期按 0 收，月底对账才发现整体少收。
// 现在的口径：按文本估算秒数并打 audio_seconds_estimated，可解释、可审计。
func TestPerSecondSettlementEstimatesWhenDurationUnknown(t *testing.T) {
	snap := perSecondSnap(micro(1000), nil, nil)

	// 40 字 → ceil(40/4) = 10 秒 → 10000 微。
	r, err := ChargeDetailed(snap, Usage{AudioChars: 40})
	if err != nil {
		t.Fatalf("结算不应报错：%v", err)
	}
	if r.Amount != 10000 {
		t.Fatalf("应按估算收 10000 微，实际 %d", r.Amount)
	}
	if !hasFlag(r.Flags, model.AnomalyAudioSecondsEstimated) {
		t.Fatalf("必须打 audio_seconds_estimated 标记，实际 %v", r.Flags)
	}

	// 有真实时长时绝不能打估算标记：否则每一次正常 TTS 调用都被染成异常。
	r2, err := ChargeDetailed(snap, Usage{AudioDurationSecs: 3, AudioChars: 40})
	if err != nil {
		t.Fatalf("结算不应报错：%v", err)
	}
	if r2.Amount != 3000 {
		t.Fatalf("有真实时长应按 3 秒收 3000 微，实际 %d", r2.Amount)
	}
	if hasFlag(r2.Flags, model.AnomalyAudioSecondsEstimated) {
		t.Fatalf("有真实时长时不得打估算标记，实际 %v", r2.Flags)
	}

	// 预扣期按文本估是正常路径，同样不该打标记（否则异常标记失去意义）。
	est, err := Estimate(snap, 0, 0, Usage{AudioChars: 40})
	if err != nil {
		t.Fatalf("预扣不应报错：%v", err)
	}
	if est != 10000 {
		t.Fatalf("预扣应按估算冻结 10000 微，实际 %d", est)
	}
}
