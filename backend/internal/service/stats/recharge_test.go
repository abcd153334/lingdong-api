package stats

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestRechargeIsNetAndIncludesBonus 锁定「充值」的唯一口径：transactions 净额（含赠送）。
//
// 三条性质各自对应一个真实错误：
//  1. 含赠送 —— 用 orders.amount_cent 会漏掉套餐赠送（实测 amount_cent=100 ↔ credited≈2 元）；
//  2. 含退款负数 —— 否则退款后「充值」还在，用户对不上余额变化；
//  3. 只认 recharge/refund/refund_revert —— adjust（人工调账）、redeem（兑换码）不是充值。
func TestRechargeIsNetAndIncludesBonus(t *testing.T) {
	gdb := withTestDB(t)

	loc := Location()
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, loc)
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = prevNow })

	u1 := &model.User{Username: "rc_a", PasswordHash: "x", Group: "default", Role: 1, Status: 1, AffCode: "rc-a"}
	u2 := &model.User{Username: "rc_b", PasswordHash: "x", Group: "default", Role: 1, Status: 1, AffCode: "rc-b"}
	for _, u := range []*model.User{u1, u2} {
		if err := gdb.Create(u).Error; err != nil {
			t.Fatalf("造用户失败: %v", err)
		}
	}

	at := func(d, h int) time.Time { return time.Date(2026, 3, d, h, 0, 0, 0, loc) }
	seed := func(userID uint64, typ string, amount int64, when time.Time) {
		t.Helper()
		if err := gdb.Create(&model.Transaction{
			UserID: userID, Type: typ, Amount: amount, CreatedAt: when,
		}).Error; err != nil {
			t.Fatalf("造流水失败: %v", err)
		}
	}

	// 1,999,994 微 = 实付 1 元 + 赠送约 1 元：若误用 amount_cent(=100 分) 这里会是 1_000_000
	seed(u1.ID, model.TxnRecharge, 1_999_994, at(10, 12))
	seed(u1.ID, model.TxnRefund, -1_000_000, at(12, 9))
	seed(u1.ID, model.TxnRefundRevert, 300_000, at(13, 9))
	// 以下三条不得计入：调账、兑换码、上月充值（2 月最后一天，本月区间外）
	seed(u1.ID, model.TxnAdjust, 5_000_000, at(11, 9))
	seed(u1.ID, model.TxnRedeem, 6_000_000, at(11, 10))
	seed(u1.ID, model.TxnRecharge, 2_000_000, time.Date(2026, 2, 28, 12, 0, 0, 0, loc))
	// 明日 00:00 整（右开区间外）与另一个用户
	seed(u1.ID, model.TxnRecharge, 2_000_000, time.Date(2026, 3, 16, 0, 0, 0, 0, loc))
	seed(u2.ID, model.TxnRecharge, 7_000_000, at(10, 12))

	monthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	_, dayEnd := DayRange(now)
	ctx := context.Background()

	const want = 1_999_994 - 1_000_000 + 300_000
	got, err := RechargeBetween(ctx, u1.ID, monthStart, dayEnd)
	if err != nil {
		t.Fatalf("聚合充值失败: %v", err)
	}
	if got != want {
		t.Fatalf("本月净充值=%d，期望 %d（含赠送、扣退款、排除调账/兑换码/跨月/明日 0 点）", got, want)
	}

	// userID=0 表示全平台（管理台看板用），不得漏人也不得重复
	all, err := RechargeBetween(ctx, 0, monthStart, dayEnd)
	if err != nil {
		t.Fatalf("全平台聚合失败: %v", err)
	}
	if all != want+7_000_000 {
		t.Fatalf("全平台净充值=%d，期望 %d", all, want+7_000_000)
	}

	// 逐日分桶必须按业务时区打标签，且与区间总和自洽
	daily, err := RechargeDaily(ctx, u1.ID, monthStart, dayEnd)
	if err != nil {
		t.Fatalf("分桶失败: %v", err)
	}
	if daily["2026-03-10"] != 1_999_994 {
		t.Fatalf("03-10 桶=%d，期望 1999994", daily["2026-03-10"])
	}
	if daily["2026-03-12"] != -1_000_000 {
		t.Fatalf("03-12 桶=%d，期望 -1000000（退款是负数）", daily["2026-03-12"])
	}
	if daily["2026-03-13"] != 300_000 {
		t.Fatalf("03-13 桶=%d，期望 300000", daily["2026-03-13"])
	}
	var sum int64
	for _, v := range daily {
		sum += v
	}
	if sum != want {
		t.Fatalf("逐日合计=%d 与区间总和=%d 不一致", sum, want)
	}
}
