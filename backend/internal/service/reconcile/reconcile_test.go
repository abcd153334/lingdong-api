package reconcile

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

const recTestSchema = "ld_test_reconcile"

func withReconcileDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + recTestSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+recTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	// Order 也要建：对账现在包含「订单 ↔ 到账流水」一致性检查。
	// AffiliateReward 同样要建：对账包含「返利欠款」汇总（checkAffiliate 直接查表）。
	if err := gdb.AutoMigrate(&model.User{}, &model.Usage{}, &model.Transaction{},
		&model.Task{}, &model.Order{}, &model.AffiliateReward{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + recTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// 干净数据：所有差异应为 0。
func TestRunCleanZeroDiff(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-clean", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if err := gdb.Create(&model.Transaction{UserID: u.ID, Type: "adjust", Amount: 1000,
		BalanceAfter: 1000, FrozenAfter: 0, RefType: "admin_adjust", RefID: "1"}).Error; err != nil {
		t.Fatalf("建流水失败: %v", err)
	}
	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Users.ChainMismatch != 0 || rep.Users.BalanceMismatch != 0 || rep.Users.FrozenMismatch != 0 {
		t.Fatalf("干净数据不应有用户差异: %+v", rep.Users)
	}
	if rep.Ledger.ChargeRows != 0 || rep.Ledger.ChargeDiff != 0 {
		t.Fatalf("干净数据不应有金额差异: %+v", rep.Ledger)
	}
	if rep.Usages.PendingTotal != 0 {
		t.Fatalf("不应有 pending: %+v", rep.Usages)
	}
}

// 人为改余额：必须被余额对账捕获并给出样本。
func TestRunDetectBalanceDrift(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-drift", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if err := gdb.Create(&model.Transaction{UserID: u.ID, Type: "adjust", Amount: 1000,
		BalanceAfter: 1000, FrozenAfter: 0, RefType: "admin_adjust", RefID: "1"}).Error; err != nil {
		t.Fatalf("建流水失败: %v", err)
	}
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).Update("balance", 1001).Error; err != nil {
		t.Fatalf("篡改余额失败: %v", err)
	}
	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Users.BalanceMismatch < 1 {
		t.Fatalf("应捕获余额差异: %+v", rep.Users)
	}
	if len(rep.Users.Samples) == 0 {
		t.Fatal("缺少差异样本")
	}
}

// 精确预扣（quota_type=1/2）的会话结算时 diff=0，`billing.Settle` 按设计**不写 settle 流水**，
// 于是末行流水永远停在 freeze 那一笔的 frozen_after 上，而用户 frozen 早已归零。
// 若用「末行 frozen_after」判冻结，这种完全正常的账户每天都会误报 frozen_mismatch
// （2026-10-24 线上实测踩到：用户 1 的 usage 27 就是这种形态，对账天天 error）。
func TestRunIgnoresExactFreezeWithoutSettleRow(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-exact-freeze", Balance: 9000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	// 冻结 1000：余额已被扣到 9000，frozen_after 记 1000。
	if err := gdb.Create(&model.Transaction{UserID: u.ID, Type: "freeze", Amount: -1000,
		BalanceAfter: 9000, FrozenAfter: 1000, RefType: "usage", RefID: "1"}).Error; err != nil {
		t.Fatalf("建预扣流水失败: %v", err)
	}
	// 会话已终态（settled），冻结额固化在 usages 上；没有任何 settle 流水。
	if err := gdb.Create(&model.Usage{UserID: u.ID, Status: "settled", Frozen: 1000}).Error; err != nil {
		t.Fatalf("建 usage 失败: %v", err)
	}

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Users.FrozenMismatch != 0 {
		t.Fatalf("已结算会话的冻结不该算差异，实际 frozen_mismatch=%d（样本 %+v）",
			rep.Users.FrozenMismatch, rep.Users.Samples)
	}
	if rep.Users.BalanceMismatch != 0 {
		t.Fatalf("余额不该有差异: %+v", rep.Users)
	}
}

// pending 会话的冻结额必须能被对上；对不上才是真漂移（例如解冻了但会话仍 pending）。
func TestRunDetectPendingFreezeDrift(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-freeze-drift", Balance: 9000, Frozen: 0}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if err := gdb.Create(&model.Transaction{UserID: u.ID, Type: "adjust", Amount: 9000,
		BalanceAfter: 9000, FrozenAfter: 0, RefType: "admin_adjust", RefID: "1"}).Error; err != nil {
		t.Fatalf("建流水失败: %v", err)
	}
	// 会话还在跑、账面也冻结了 500，但用户侧冻结是 0 → 真漂移。
	if err := gdb.Create(&model.Usage{UserID: u.ID, Status: "pending", Frozen: 500}).Error; err != nil {
		t.Fatalf("建 pending usage 失败: %v", err)
	}

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Users.FrozenMismatch < 1 {
		t.Fatalf("pending 冻结未落到用户侧应被捕获: %+v", rep.Users)
	}
}

// 卡死 pending（无 task）应计入 pending_old / pending_orphan。
func TestRunDetectPendingOrphan(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-pending", Balance: 0}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	old := model.Usage{UserID: u.ID, Status: "pending"}
	if err := gdb.Create(&old).Error; err != nil {
		t.Fatalf("建 usage 失败: %v", err)
	}
	if err := gdb.Model(&model.Usage{}).Where("id = ?", old.ID).
		Update("created_at", gorm.Expr("now() - interval '1 hour'")).Error; err != nil {
		t.Fatalf("调整时间失败: %v", err)
	}
	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Usages.PendingTotal != 1 || rep.Usages.PendingOld != 1 || rep.Usages.PendingOrphan != 1 {
		t.Fatalf("pending 统计不符: %+v", rep.Usages)
	}
}

// 返利「应作废未作废」必须被对账看见。
//
// 为什么单独测：返利额度是派生量，一行没作废就等于那笔钱还能被转出。
// 判据只看「未作废 + 未转出」—— 与是否到期无关（钱在不在用户手里由
// transferred_at 决定）。同时必须覆盖 `upstream_only`：那条路径曾经只把订单
// 打上标记就转人工，完全没调作废，是最直接的资损面。
func TestRunDetectAffiliateLeak(t *testing.T) {
	gdb := withReconcileDB(t)
	now := time.Now()
	done := now
	mk := func(orderID uint64, refund string, row model.AffiliateReward) {
		t.Helper()
		if err := gdb.Create(&model.Order{
			ID: orderID, UserID: 11, OutTradeNo: "T" + strconv.FormatUint(orderID, 10),
			AmountCent: 100_00, Credited: 100_000_000, Currency: "CNY",
			Status: model.OrderRefunded, RefundStatus: refund, RefundDoneAt: &done,
		}).Error; err != nil {
			t.Fatalf("建订单 %d 失败: %v", orderID, err)
		}
		row.OrderID = orderID
		row.InviteeID = 11
		row.BeneficiaryID = 12
		row.BaseAmount = 100_000_000
		row.CreatedAt = now
		if err := gdb.Create(&row).Error; err != nil {
			t.Fatalf("建返利行 %d 失败: %v", orderID, err)
		}
	}
	// 1) 原路退款成功、返利未作废未转出 → 泄露
	mk(7001, model.RefundStateSuccess, model.AffiliateReward{
		Role: model.AffRoleInviter, Amount: 5_000_000, Rate: 100_000, AvailableAt: now.Add(time.Hour),
	})
	// 2) 上游单独退款（upstream_only）、返利仍有效 → 同样是泄露
	mk(7002, model.RefundStateUpstreamOnly, model.AffiliateReward{
		Role: model.AffRoleInvitee, Amount: 3_000_000, AvailableAt: now.Add(time.Hour),
	})
	// 3) 已作废 → 不算差异
	mk(7003, model.RefundStateSuccess, model.AffiliateReward{
		Role: model.AffRoleInvitee, Amount: 2_000_000, Voided: true, VoidedAt: &done,
		AvailableAt: now.Add(time.Hour),
	})
	// 4) 已转出（钱已到手，按设计退款不追回）→ 不算差异
	mk(7004, model.RefundStateSuccess, model.AffiliateReward{
		Role: model.AffRoleInvitee, Amount: 4_000_000, TransferredAt: &done,
		AvailableAt: now.Add(-time.Hour),
	})

	// 5) 已入账却没有返利行 → 补发缺口（不是 leak，走 missing）
	if err := gdb.Create(&model.Order{
		ID: 7005, UserID: 11, OutTradeNo: "T7005", AmountCent: 100_00,
		Credited: 100_000_000, Currency: "CNY", Status: model.OrderPaid,
	}).Error; err != nil {
		t.Fatalf("建订单 7005 失败: %v", err)
	}
	if err := gdb.Create(&model.Transaction{
		UserID: 11, Type: model.TxnRecharge, Amount: 100_000_000,
		RefType: "order", RefID: "7005",
	}).Error; err != nil {
		t.Fatalf("建充值流水失败: %v", err)
	}

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Affiliate.LeakRows != 2 || rep.Affiliate.LeakTotal != 8_000_000 {
		t.Fatalf("泄露汇总不符（应含 upstream_only）: %+v", rep.Affiliate)
	}
	if rep.Affiliate.MissingRows != 1 {
		t.Fatalf("补发缺口应为 1: %+v", rep.Affiliate)
	}
	// 冷却观察值：7001/7002 两行仍在冷却且未转出
	if rep.Affiliate.CoolingRows != 2 || rep.Affiliate.CoolingTotal != 8_000_000 {
		t.Fatalf("冷却观察值不符: %+v", rep.Affiliate)
	}
	// 样本：2 条泄露 + 1 条缺发
	if len(rep.Affiliate.Samples) != 3 {
		t.Fatalf("样本数不符: %+v", rep.Affiliate.Samples)
	}
	var sawLeak, sawMissing bool
	for _, s := range rep.Affiliate.Samples {
		switch s.Issue {
		case "refund_leak":
			sawLeak = true
		case "missing_reward":
			sawMissing = true
		}
	}
	if !sawLeak || !sawMissing {
		t.Fatalf("两类成因都应出现在样本里: %+v", rep.Affiliate.Samples)
	}
}

// 无欠款时 samples 必须是空数组而不是 null：前端对 null 调 .length 会整页崩。
func TestRunAffiliateSamplesNeverNil(t *testing.T) {
	_ = withReconcileDB(t)
	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Affiliate.Samples == nil {
		t.Fatal("affiliate.samples 不得为 nil（JSON 会序列化成 null）")
	}
	if rep.Orders.Samples == nil || rep.Users.Samples == nil || rep.Ledger.Samples == nil {
		t.Fatal("对账各段 samples 都不得为 nil")
	}
	if rep.Tasks.Samples == nil {
		t.Fatal("tasks.samples 不得为 nil（前端 .length 会抛错）")
	}
}

// 返利转出行与 invite 流水必须一一对应（差应为 0）。
//
// 两个方向都是错的：标记了没入账 = 用户的钱丢了；入账了没标记 = 同一笔能再转一次。
func TestRunDetectAffiliateLedgerDiff(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-aff", Balance: 0}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	done := time.Now()
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 7101, InviteeID: u.ID, BeneficiaryID: u.ID, Role: model.AffRoleInvitee,
		Amount: 5_000_000, BaseAmount: 100_000_000, AvailableAt: done.Add(-time.Hour),
		TransferredAt: &done, CreatedAt: done,
	}).Error; err != nil {
		t.Fatalf("建已转出行失败: %v", err)
	}
	// 只入账 4 元：少入 1 元，差额必须是 +1e6
	if err := gdb.Create(&model.Transaction{
		UserID: u.ID, Type: model.TxnInvite, Amount: 4_000_000,
		RefType: model.RefTypeAffiliate, RefID: strconv.FormatUint(u.ID, 10),
	}).Error; err != nil {
		t.Fatalf("建邀请流水失败: %v", err)
	}

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Affiliate.TransferredSum != 5_000_000 {
		t.Fatalf("已转出总额不符: %+v", rep.Affiliate)
	}
	if rep.Affiliate.LedgerDiff != 1_000_000 {
		t.Fatalf("转出与流水之差应为 +1e6，实际 %d", rep.Affiliate.LedgerDiff)
	}
	if rep.Affiliate.LeakRows != 0 || rep.Affiliate.MissingRows != 0 {
		t.Fatalf("这条用例不该产生 leak/missing: %+v", rep.Affiliate)
	}
}

// 任务与账务的口径差异必须被对账看见（P0-8）。
//
// 这段是「结算公式写错」唯一的自动发现通道：公式错时账面依然平（错的公式是一致的错），
// 余额链、流水、pending 规模全都正常，只有把 tasks 与 usages 摆在一起才看得出来。
func TestRunDetectTaskMismatch(t *testing.T) {
	gdb := withReconcileDB(t)
	u := model.User{Username: "rec-task", Balance: 10000, Frozen: 0}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	mk := func(charge, frozen int64, taskStatus, usageStatus string, settled, taskFrozen int64) (uint64, uint64) {
		t.Helper()
		usage := model.Usage{UserID: u.ID, Status: usageStatus, Charge: charge, Frozen: frozen, Kind: "relay"}
		if err := gdb.Create(&usage).Error; err != nil {
			t.Fatalf("建 usage 失败: %v", err)
		}
		tk := model.Task{UserID: u.ID, UsageID: usage.ID, Status: taskStatus,
			Settled: settled, Frozen: taskFrozen, Kind: "video"}
		if err := gdb.Create(&tk).Error; err != nil {
			t.Fatalf("建 task 失败: %v", err)
		}
		return tk.ID, usage.ID
	}
	// ① 结算成功但 settled≠charge（少记 1 分钱）
	mk(1000, 0, "succeeded", "settled", 0, 0)
	// ② 任务已终结、账务还 pending（那笔钱永远处于冻结中）
	mk(0, 500, "failed", "pending", 0, 500)
	// ③ 在途任务，但两边冻结额不一致（任务侧偏大 700 微）
	//    刻意用 running：它不该被算进「终态仍冻结」，否则这项计数会把在途任务也卷进来。
	mk(0, 0, "running", "pending", 0, 700)

	rep, err := Run(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Tasks.SettledMismatch != 1 {
		t.Fatalf("应捕获 1 条 settled≠charge，实际 %d", rep.Tasks.SettledMismatch)
	}
	if rep.Tasks.PendingAfterTerminal != 1 {
		t.Fatalf("应只捕获 1 条「终态仍 pending」（在途任务不算），实际 %d", rep.Tasks.PendingAfterTerminal)
	}
	if rep.Tasks.FrozenMismatch != 1 {
		t.Fatalf("应捕获 1 条冻结不符，实际 %d", rep.Tasks.FrozenMismatch)
	}
	if rep.Tasks.FrozenDiff != 700 {
		t.Fatalf("冻结差额应为 700（task.frozen − usage.frozen），实际 %d", rep.Tasks.FrozenDiff)
	}
	if len(rep.Tasks.Samples) == 0 {
		t.Fatal("缺少任务口径差异样本")
	}
	if rep.Tasks.Samples[0].Issue == "" {
		t.Fatal("样本必须带 issue 归类，否则运维不知道是哪一类")
	}
}
