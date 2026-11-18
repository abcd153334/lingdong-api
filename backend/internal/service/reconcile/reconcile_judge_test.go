package reconcile

// W2 的两类新判据与「分段隔离」行为。
//
// 这三件事的共同点是：**只看计数永远发现不了**。
//   - 交付了没收钱：钱压根没动，余额链、流水、charge 全部自洽；
//   - 收了钱但任务失败：Settle 写下的每一个数都「对」，错的是它与任务状态的关系；
//   - 某一段查询失败：失败段的计数全是 0，与「查过、没差异」在页面上完全同形。
// 所以它们必须由独立的判据与独立的字段兜住，而不是靠人盯总数。

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// seedTaskPair 造一条 usage + 一条 task（本文件只关心两者状态组合）。
func seedTaskPair(t *testing.T, gdb *gorm.DB, userID uint64,
	usageStatus string, charge int64, taskStatus string) {
	t.Helper()
	u := model.Usage{UserID: userID, Status: usageStatus, Charge: charge, Kind: "relay"}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建 usage 失败: %v", err)
	}
	tk := model.Task{UserID: userID, UsageID: u.ID, Status: taskStatus, Kind: "video"}
	if err := gdb.Create(&tk).Error; err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
}

// TestRunDetectSettledShortfall 校验「任务成功但账已释放」被抓到。
//
// 这是迟到成功补收失败之外的另一种形态：usage 已归档/用户已删除时，
// SucceedLate 只交付不记账，于是 tasks.succeeded + usages.released。
// 平台的真实上游成本已经花了，账面却一分未收，且没有任何既有判据能看见。
func TestRunDetectSettledShortfall(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-shortfall", Balance: 0}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	// 命中：交付了但钱退回去了
	seedTaskPair(t, gdb, u.ID, model.UsageStatusReleased, 0, "succeeded")
	// 不命中：正常成功（账已结算）
	seedTaskPair(t, gdb, u.ID, model.UsageStatusSettled, 800, "succeeded")

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Tasks.SettledShortfall != 1 {
		t.Fatalf("应捕获 1 条「成功但已释放」，实际 %d", rep.Tasks.SettledShortfall)
	}
	found := false
	for _, s := range rep.Tasks.Samples {
		if s.Issue == "settled_shortfall" {
			found = true
		}
	}
	if !found {
		t.Fatalf("样本里必须有 settled_shortfall 归类，实际 %+v", rep.Tasks.Samples)
	}
}

// TestRunDetectChargedButFailed 校验「收了钱但任务失败」被抓到。
//
// 场景是 Settle 成功 → CAS succeeded 失败这半成功：钱扣了、状态没落，
// 随后超时任务被判 expired。用户付了钱拿到的是失败结果，必须人工处理。
//
// 同时校验 charge=0 的免费调用**不**命中：免费模型上这条判据若长期非零，
// 就会被当成噪声忽略，等到真出事时没人再看它。
func TestRunDetectChargedButFailed(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-charged-failed", Balance: 0}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	seedTaskPair(t, gdb, u.ID, model.UsageStatusSettled, 1200, "failed")
	seedTaskPair(t, gdb, u.ID, model.UsageStatusSettled, 0, "expired") // 免费：不应命中

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Tasks.ChargedButFailed != 1 {
		t.Fatalf("应捕获 1 条「收了钱但失败」，实际 %d", rep.Tasks.ChargedButFailed)
	}
	found := false
	for _, s := range rep.Tasks.Samples {
		if s.Issue == "charged_but_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("样本里必须有 charged_but_failed 归类，实际 %+v", rep.Tasks.Samples)
	}
}

// TestRunErrorsNeverNil 校验 Report.Errors 始终是数组而不是 null。
//
// 前端按 `Report.Errors.length` 判断要不要显示「本次未完成」提示条，
// 拿到 null 会直接抛错白屏 —— 而这一页是运营排查资金问题的入口。
func TestRunErrorsNeverNil(t *testing.T) {
	withReconcileDB(t)
	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Errors == nil {
		t.Fatal("Errors 必须是空数组而不是 nil（前端 .length 会抛错）")
	}
}

// TestRunPartialFailureKeepsOtherSegments 校验「一段炸了不牵连其余段」。
//
// 手法：把 orders 表改名，让订单段必然查失败，其余段照常。
// 期望：Run 返回报告（不是 error）、Errors 里恰好有 orders、
// 而用户段仍然给出了正确的计数 —— 这正是分段执行的全部意义。
func TestRunPartialFailureKeepsOtherSegments(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-partial", Balance: 0}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	seedTaskPair(t, gdb, u.ID, model.UsageStatusReleased, 0, "succeeded")

	if err := gdb.Exec(`ALTER TABLE orders RENAME TO orders_broken`).Error; err != nil {
		t.Fatalf("改名 orders 失败: %v", err)
	}

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("只有一段失败时不应整体报错（否则其余段的结论全丢）: %v", err)
	}
	if len(rep.Errors) == 0 {
		t.Fatal("失败段必须出现在 Errors 里，否则页面会显示成「无差异」")
	}
	var hasOrders bool
	for _, e := range rep.Errors {
		if e == "orders" {
			hasOrders = true
		}
	}
	if !hasOrders {
		t.Fatalf("Errors 应含 orders，实际 %v", rep.Errors)
	}
	if rep.Users.Checked != 1 {
		t.Fatalf("用户段应照常完成（checked=1），实际 %d", rep.Users.Checked)
	}
	if rep.Tasks.SettledShortfall != 1 {
		t.Fatalf("任务段应照常完成（1 条少收），实际 %d", rep.Tasks.SettledShortfall)
	}
}
