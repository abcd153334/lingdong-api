package db

import (
	"strings"
	"testing"
)

// TestPlanPackagesPrice 锁死「售价回填」的三条规则。
//
// 这段逻辑决定用户能不能买到套餐：`price_cent = 0` 会被 `order.Create` 判
// 400 `invalid_param` —— 表现是"所有套餐都买不了"，而错误码指向参数非法，
// 从日志里完全看不出是数据没回填。
func TestPlanPackagesPrice(t *testing.T) {
	t.Run("旧列有值就搬，单位不换算", func(t *testing.T) {
		upd, warn := planPackagesPrice([]legacyPackageRow{
			{ID: 1, Name: "体验包", PriceCent: 0, PriceCnyCent: ptr(int64(500))},
		})
		if len(upd) != 1 || upd[0].NewCent != 500 {
			t.Fatalf("应把 500 分原样搬到 price_cent（不许 ×100），实际 %+v", upd)
		}
		if len(warn) != 0 {
			t.Fatalf("不应有告警: %v", warn)
		}
	})

	t.Run("新列已有值绝不覆盖", func(t *testing.T) {
		upd, _ := planPackagesPrice([]legacyPackageRow{
			{ID: 2, Name: "已定价", PriceCent: 1200, PriceCnyCent: ptr(int64(500))},
		})
		if len(upd) != 0 {
			t.Fatalf("运营手工定过的价不能被旧列覆盖，实际 %+v", upd)
		}
	})

	t.Run("两列都空必须告警而不是静默跳过", func(t *testing.T) {
		upd, warn := planPackagesPrice([]legacyPackageRow{
			{ID: 3, Name: "从没定价", PriceCent: 0, PriceCnyCent: nil},
		})
		if len(upd) != 0 {
			t.Fatalf("没有来源价就不该写库，实际 %+v", upd)
		}
		if len(warn) != 1 || !strings.Contains(warn[0], "需要人工定价") {
			t.Fatalf("必须提醒人工定价，实际 %v", warn)
		}
	})

	t.Run("旧赠送额度不自动折算", func(t *testing.T) {
		// 1 额度 ≈ 1/500000 美元，与「微」不同量纲：折算出来的数字没人看得懂，
		// 而且会悄悄改变「充 100 送多少」—— 只能让人确认。
		_, warn := planPackagesPrice([]legacyPackageRow{
			{ID: 4, Name: "旗舰包", PriceCent: 10000, PriceCnyCent: ptr(int64(10000)), BonusQuota: ptr(int64(2))},
		})
		if len(warn) != 1 || !strings.Contains(warn[0], "旧赠送额度") {
			t.Fatalf("必须提醒旧赠送额度需人工确认，实际 %v", warn)
		}
	})

	t.Run("赠送已有值就不再提醒", func(t *testing.T) {
		_, warn := planPackagesPrice([]legacyPackageRow{
			{ID: 5, Name: "已配赠送", PriceCent: 10000, Bonus: 1000000, BonusQuota: ptr(int64(2))},
		})
		if len(warn) != 0 {
			t.Fatalf("新列已有赠送金额时不该再提醒，实际 %v", warn)
		}
	})
}
