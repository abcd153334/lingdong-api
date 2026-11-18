package billing

import (
	"testing"

	"lingdong-api/internal/model"
)

// TestDiscountMultiplierApplies 守「加价倍率必须真的加价」。
//
// 曾经 `applyDiscount` 写成 `d >= money.Scale` 一律不打折，于是所有 >1.0 的倍率
// 被静默丢弃：配置上界是 10 倍、管理台写着「1.2 = 加价 20%」、门户按 1.2 展示折后价，
// 实际却按 1.0 扣款。少收方向靠客诉发现不了，只能靠这条用例守住。
func TestDiscountMultiplierApplies(t *testing.T) {
	base := int64(10_000) // 0.01 元
	cases := []struct {
		name string
		d    int64
		want int64
	}{
		{"1.0 不打折", moneyScale, base},
		{"0.9 九折", 900_000, 9_000},
		{"1.2 加价 20%", 1_200_000, 12_000},
		{"1.5 加价 50%", 1_500_000, 15_000},
		{"10 倍是上界内", 10_000_000, base * 10},
		// 超过保存期上界（只可能来自手改库/迁移）：宁可按原价，也不能乘出荒谬金额。
		{"越界值忽略", 90_000_000, base},
		{"未配置", 0, base},
		{"负数忽略", -1, base},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := applyDiscount(c.d, base)
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if got != c.want {
				t.Fatalf("折扣 %d 应得 %d，实际 %d", c.d, c.want, got)
			}
		})
	}

	// 端到端：加价必须体现在 Charge 与 Estimate 两条路径上（冻结与结算同源）。
	snap := Snapshot{BillingMode: model.BillingModeSpec, Discount: 1_200_000,
		SpecPrice: model.SpecPrices{Kind: SpecKindPerImage,
			TierPrice: map[string]int64{"1K": 10_000}, MaxN: iptr(1)}}
	got, err := Charge(snap, Usage{ImageSpec: "1K", ImageN: 1})
	if err != nil || got != 12_000 {
		t.Fatalf("Charge 应按 1.2 倍得 12000，实际 %d/%v", got, err)
	}
	est, err := Estimate(snap, 0, 0, Usage{ImageSpec: "1K", ImageN: 1})
	if err != nil || est != 12_000 {
		t.Fatalf("Estimate 应与 Charge 同额 12000，实际 %d/%v", est, err)
	}
}

// moneyScale 是 money.Scale 的本地副本（同包内已有 import 但避免测试文件耦合）。
const moneyScale = 1_000_000

// TestRefVideoSecondsCumulative 守「参考素材累计秒不能被单条上限截断」。
//
// `RefVideoSeconds` 是**多段**素材的秒数总和。用单条视频的 3600 秒上限去截，
// 10 段 10 分钟（6000 秒）只收 3600 秒 —— 少收 40%，而且看不出任何异常。
// 上限必须跟着段数走，同时仍要挡住上游谎报（86400）。
func TestRefVideoSecondsCumulative(t *testing.T) {
	cases := []struct {
		name   string
		secs   int64
		videos int64
		want   int64
	}{
		{"单段短素材", 10, 1, 10},
		{"单段超上限被截", 86_400, 1, MaxVideoSeconds},
		{"未报段数按 1 段", 86_400, 0, MaxVideoSeconds},
		{"10 段累计不截断", 6_000, 10, 6_000},
		{"10 段但仍要防谎报", 999_999, 10, 10 * MaxVideoSeconds},
		{"负数归零", -5, 3, 0},
		{"段数异常大也不溢出", 999_999, 10_000, 64 * MaxVideoSeconds},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClampRefVideoSeconds(c.secs, c.videos); got != c.want {
				t.Fatalf("ClampRefVideoSeconds(%d,%d)=%d，期望 %d", c.secs, c.videos, got, c.want)
			}
		})
	}

	// 端到端：10 段 × 10 分钟的参考素材费用必须按 6000 秒计。
	s := videoSnapWith(`"ref_video_second_price":{"720p":12000}`)
	got, err := Charge(s, Usage{
		VideoResolution: "720p", VideoSeconds: 1,
		RefVideoSeconds: 6_000, RefVideos: 10,
	})
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if want := int64(12000 + 12000*6000); got != want {
		t.Fatalf("应按 6000 秒计素材费 %d，实际 %d", want, got)
	}
}
