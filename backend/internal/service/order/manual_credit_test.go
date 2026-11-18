package order

import (
	"context"
	"strconv"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestManualCreditExistingRechargeAdvances 是「人工补账入口恒失败」的回归锁。
//
// 现场：need_manual 订单的钱其实已经被并发回调入账了（同一订单号只能记一次），
// 旧实现无条件再调 CreditTx → 撞 (ref_type,ref_id) 唯一索引 → PG 把整个事务置为
// aborted → 随后的状态更新必然 25P02。于是这个「唯一的人工修复入口」永远报错，
// 订单永久卡在 need_manual。
//
// 断言：不报错、状态推进到 paid、余额只加一次、充值流水仍只有 1 条。
func TestManualCreditExistingRechargeAdvances(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano() % 1_000_000_000_000

	// 用户余额已经包含这笔到账（钱在、状态没推进）——正是 need_manual 的真实形态。
	u := &model.User{
		Username: "mc" + strconv.FormatInt(suffix, 10), PasswordHash: "x",
		Group: "default", Balance: 5000,
	}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	o := &model.Order{
		UserID: u.ID, OutTradeNo: "LDMC" + strconv.FormatInt(suffix, 10),
		AmountCent: 5000, Credited: 5000, Status: "need_manual",
		PayChannel: "虎皮椒-聚合支付", PayChannelID: "xunhupay", PayMethod: "聚合支付",
		Subject: "充值-体验",
	}
	if err := gdb.Create(o).Error; err != nil {
		t.Fatalf("造 need_manual 订单失败: %v", err)
	}
	txn := &model.Transaction{
		UserID: u.ID, Type: model.TxnRecharge, Amount: 5000, BalanceAfter: 5000,
		RefType: "order", RefID: strconv.FormatUint(o.ID, 10), Reason: "并发回调已入账",
	}
	if err := gdb.Create(txn).Error; err != nil {
		t.Fatalf("造已到账流水失败: %v", err)
	}

	if err := New(gdb).ManualCredit(ctx, o.ID, "人工核实补账"); err != nil {
		t.Fatalf("已有到账流水时补账必须幂等成功，实际 %v", err)
	}

	var got model.Order
	if err := gdb.First(&got, o.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "paid" || got.PaidAt == nil {
		t.Fatalf("订单应推进为 paid 且写 paid_at，实际 status=%s paid_at=%v", got.Status, got.PaidAt)
	}

	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Balance != 5000 {
		t.Fatalf("不得重复加钱：余额应为 5000，实际 %d", after.Balance)
	}

	var n int64
	if err := gdb.Model(&model.Transaction{}).
		Where("ref_type = 'order' AND ref_id = ? AND type = ?", strconv.FormatUint(o.ID, 10), model.TxnRecharge).
		Count(&n).Error; err != nil {
		t.Fatalf("统计充值流水失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("充值流水应仍为 1 条，实际 %d", n)
	}
}

// TestManualCreditRejectsNonNeedManual：状态不对时必须是业务错误，且不动账。
func TestManualCreditRejectsNonNeedManual(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano() % 1_000_000_000_000
	u := &model.User{Username: "mc2" + strconv.FormatInt(suffix, 10), PasswordHash: "x", Group: "default"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	o := &model.Order{
		UserID: u.ID, OutTradeNo: "LDMC2" + strconv.FormatInt(suffix, 10),
		AmountCent: 100, Credited: 100, Status: "paid",
		PayChannel: "虎皮椒-聚合支付", PayChannelID: "xunhupay", PayMethod: "聚合支付",
	}
	if err := gdb.Create(o).Error; err != nil {
		t.Fatalf("造订单失败: %v", err)
	}
	if err := New(gdb).ManualCredit(ctx, o.ID, "x"); err == nil {
		t.Fatal("已支付订单不应被人工补账")
	}
	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Balance != 0 {
		t.Fatalf("拒绝分支不得动账，余额 %d", after.Balance)
	}
}
