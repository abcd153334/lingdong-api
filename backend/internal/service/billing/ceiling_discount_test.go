package billing

import (
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

/*
本文件锁死「结算上限与实收同口径」。

回归的真实资损路径：`ChargeDetailed` = 取价 → 打折 → 抬到最低消费 → 截顶，
而 `ChargeCeiling` 只做了第一步（取价）。于是「上限」是**未打折的原价**：

  - 加价分组（1.2×）：实收 12000，上限 10000 → 实收被自己的上限削到 10000，
    `cap_reason` 记成 `configured_max`，看起来像「配置就这么贵」，实际是少收；
  - 最低消费（min_price）：实收 5000（地板），上限 1000 → 被削到 1000。

少收方向既不会告警也不会被对账发现（对账只比 charge 与流水，两者一致）。
*/

// TestChargeCeilingIncludesDiscount_MarkupGroup 加价分组下，上限必须跟着加价。
func TestChargeCeilingIncludesDiscount_MarkupGroup(t *testing.T) {
	sp := model.SpecPrices{
		Kind:      SpecKindPerImage,
		TierPrice: map[string]int64{"1K": 10_000},
		MaxN:      iptr(1),
	}
	snap := Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: sp, Discount: 1_200_000}

	charge, err := Charge(snap, Usage{ImageSpec: "1K", ImageN: 1})
	if err != nil {
		t.Fatalf("实收不应报错: %v", err)
	}
	if want := int64(12_000); charge != want {
		t.Fatalf("实收应为 %d（10000 × 1.2），实际 %d", want, charge)
	}

	ceiling, reason := ChargeCeiling(snap, Usage{ImageSpec: "1K", ImageN: 1})
	if want := int64(12_000); ceiling != want {
		t.Fatalf("上限应为 %d（与实收同口径加价），实际 %d（reason=%q）；"+
			"上限低于实收会让加价部分被静默截顶", want, ceiling, reason)
	}
}

// TestChargeCeilingIncludesMinPrice 最低消费必须也进上限，否则地板价被上限削平。
func TestChargeCeilingIncludesMinPrice(t *testing.T) {
	snap := perSecondSnap(micro(1_000), micro(5_000), micro(1))

	charge, err := Charge(snap, Usage{AudioDurationSecs: 1})
	if err != nil {
		t.Fatalf("实收不应报错: %v", err)
	}
	if want := int64(5_000); charge != want {
		t.Fatalf("实收应为最低消费 %d，实际 %d", want, charge)
	}

	ceiling, reason := ChargeCeiling(snap, Usage{AudioDurationSecs: 1})
	if want := int64(5_000); ceiling != want {
		t.Fatalf("上限应为 %d（含最低消费），实际 %d（reason=%q）", want, ceiling, reason)
	}
}

// TestChargeCeilingNotBelowChargeWithDiscount 把折扣作为新维度加进
// 「上限 ≥ 实收」这条不变量：任何折扣 × 任何张数都必须成立。
func TestChargeCeilingNotBelowChargeWithDiscount(t *testing.T) {
	sp := model.SpecPrices{
		Kind:      SpecKindPerImage,
		TierPrice: map[string]int64{"1K": 10_000, "4K": 40_000},
		MaxN:      iptr(128),
	}
	discounts := []int64{0, 500_000, money.Scale, 1_200_000, 10_000_000, 90_000_000}
	for _, d := range discounts {
		snap := Snapshot{BillingMode: model.BillingModeSpec, SpecPrice: sp, Discount: d}
		ceiling, _ := ChargeCeiling(snap, Usage{ImageSpec: "1K", ImageN: 1})
		if ceiling <= 0 {
			t.Fatalf("discount=%d：上限必须为正，实际 %d", d, ceiling)
		}
		for _, spec := range []string{"1K", "4K", ""} {
			for _, n := range []int64{1, 4, 128} {
				got, err := Charge(snap, Usage{ImageSpec: spec, ImageN: n})
				if err != nil {
					continue
				}
				if got > ceiling {
					t.Fatalf("discount=%d spec=%q n=%d：实收 %d 超过上限 %d（会被截顶少收）",
						d, spec, n, got, ceiling)
				}
			}
		}
	}
}
