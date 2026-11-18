package relay

import (
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// TestBuildBillingUsageKeepsSpecProvenance 守「尺寸的来源与显式性必须落库」。
//
// 这两列不是装饰：`image_spec_explicit=true` 才允许做 tier_mismatch 比对
// （auto / 没传 size 的请求由上游选档，报不一致全是噪声）；`image_spec_source`
// 决定运营能不能回答"这笔钱是按 size、image_size 还是 resolution 收的"。
//
// 历史缺陷：buildBillingUsage 只搬 ImageSpec/Quality/N，另三列恒为零值 ——
// 于是 tier_mismatch 条件恒假（永远不会告警），来源列恒空。
func TestBuildBillingUsageKeepsSpecProvenance(t *testing.T) {
	body := []byte(`{"data":[{"url":"https://cdn.example.com/a.png"},{"url":"https://cdn.example.com/b.png"}]}`)
	spec := billing.Usage{
		ImageSpec:         "2K",
		ImageQuality:      "high",
		ImageRatio:        "1:1",
		ImageSpecExplicit: true,
		ImageSpecSource:   billing.ImageSizeSourceTier,
		ImageN:            2,
	}
	u, _, _ := buildBillingUsage(kindImagesGenerations, UsageInfo{}, promptCompletionEst{}, model.BillingModeSpec,
		spec, body, 0, false, false, 2)

	if u.ImageSpec != "2K" || u.ImageQuality != "high" || u.ImageRatio != "1:1" {
		t.Fatalf("规格字段丢失：%+v", u)
	}
	if !u.ImageSpecExplicit {
		t.Fatal("ImageSpecExplicit 必须落库（否则 tier_mismatch 永远不会触发）")
	}
	if u.ImageSpecSource != billing.ImageSizeSourceTier {
		t.Fatalf("ImageSpecSource 应为 tier，实际 %q", u.ImageSpecSource)
	}
	// 张数按实际交付：上游给了 2 张、用户也下单 2 张。
	if u.ImageN != 2 {
		t.Fatalf("ImageN 应为 2，实际 %d", u.ImageN)
	}
}

// TestImageMaxNSeparatesCapFromDefault 守「上限」与「默认冻结张数」不能混成一个数。
//
// 未配置 max_n 时：
//   - 上限按引擎上限（否则用户显式要的 3 张会被削成 1 张 = 少交付）；
//   - 默认冻结张数仍是 1（否则一次普通请求冻 128 张 → 余额打空 402）。
func TestImageMaxNSeparatesCapFromDefault(t *testing.T) {
	sp := model.SpecPrices{Kind: billing.SpecKindPerImage, TierPrice: map[string]int64{"1K": 1000}}
	if got := imageMaxN(sp); got != billing.MaxImageN {
		t.Fatalf("未配置 max_n 时上限应为引擎上限 %d，实际 %d", billing.MaxImageN, got)
	}
	if got := imageCountHint(relayHead{}, imageMaxN(sp), imageCaps{Known: true, Group: true, Layer: true}); got != 1 {
		t.Fatalf("普通请求默认冻结 1 张，实际 %d", got)
	}
	if got := imageCountHint(relayHead{N: 3}, imageMaxN(sp), imageCaps{Known: true, Group: true, Layer: true}); got != 3 {
		t.Fatalf("用户显式要 3 张必须冻 3 张，实际 %d", got)
	}

	// 配了 max_n 就按它夹紧。
	limit := int64(4)
	sp.MaxN = &limit
	if got := imageCountHint(relayHead{N: 9}, imageMaxN(sp), imageCaps{Known: true, Group: true, Layer: true}); got != 4 {
		t.Fatalf("应夹到 max_n=4，实际 %d", got)
	}
}

// TestDeclaredImageCountRespectsMissingMaxN 守「结算上限不被漏配的 max_n 压死」。
//
// 未配 max_n 时组图/图层无法在提交期定价上限，此时只认用户显式写的 n：
// 拿兜底的 1 张去收敛会把上游真实交付的 15 张压成 1 张（系统性少收）。
func TestDeclaredImageCountRespectsMissingMaxN(t *testing.T) {
	auto := relayHead{SeqImageGen: "auto"}
	if got := declaredImageCount(auto, billing.MaxImageN, false); got != 0 {
		t.Fatalf("未配 max_n 且未写 n 时对账上限应为 0（不收敛），实际 %d", got)
	}
	if got := declaredImageCount(relayHead{N: 2, SeqImageGen: "auto"}, billing.MaxImageN, false); got != 2 {
		t.Fatalf("未配 max_n 时只认显式 n=2，实际 %d", got)
	}
	// 配了 max_n 就按组图规则给出上限。
	if got := declaredImageCount(auto, 15, true); got != 15 {
		t.Fatalf("配了 max_n 时组图按 max_n=15，实际 %d", got)
	}
}
