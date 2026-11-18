package db

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

const ledgerAlignSchema = "ld_test_ledger_align"

// withLedgerDB 在独立 schema 里建 users / transactions / usages 三张表。
//
// 为什么要独立 schema：这些用例会**改写资金流水**（哪怕只是快照列），
// 落在公共 schema 上等于拿正在跑的账本做实验。
func withLedgerDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + ledgerAlignSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+ledgerAlignSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Transaction{}, &model.Usage{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + ledgerAlignSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// seedChainUser 造一个用户与一串流水，末行余额与 users.balance 一致。
func seedChainUser(t *testing.T, gdb *gorm.DB, id uint64, balance int64, rows []model.Transaction) {
	t.Helper()
	if err := gdb.Create(&model.User{ID: id, Username: "u" + itoa(id), Balance: balance}).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	for i := range rows {
		rows[i].UserID = id
		if rows[i].Type == "" {
			rows[i].Type = model.TxnFreeze
		}
		if rows[i].RefType == "" {
			rows[i].RefType = model.RefTypeUsage
		}
		if err := gdb.Create(&rows[i]).Error; err != nil {
			t.Fatalf("建流水失败: %v", err)
		}
	}
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// TestAlignLedgerChainFixesRoundingDrift 是本文件的核心用例：
// 一条流水的快照差 1 微（模拟 M30 逐列取整），校正后链必须自洽，
// 且**余额与金额一分不动**。
func TestAlignLedgerChainFixesRoundingDrift(t *testing.T) {
	gdb := withLedgerDB(t)

	// 反向推导的期望值：末行 = 999999974；
	// 中间行 = 999999974 - (-10) = 999999984（这里故意写成 999999985）；
	// 首行   = 999999984 - (-15) = 999999999（写对了）。
	seedChainUser(t, gdb, 1, 999999974, []model.Transaction{
		{Amount: 1000000, BalanceAfter: 999999999},
		{Amount: -15, BalanceAfter: 999999985},
		{Amount: -10, BalanceAfter: 999999974},
	})

	rep, err := AlignLedgerChain(gdb, true)
	if err != nil {
		t.Fatalf("预演失败: %v", err)
	}
	if rep.BadRows != 1 || rep.FixedRows != 0 {
		t.Fatalf("预演应只报 1 行差异且不写库，实际 bad=%d fixed=%d", rep.BadRows, rep.FixedRows)
	}
	if rep.MaxDrift != 1 {
		t.Fatalf("偏移应为 1 微，实际 %d", rep.MaxDrift)
	}

	rep, err = AlignLedgerChain(gdb, false)
	if err != nil {
		t.Fatalf("校正失败: %v", err)
	}
	if rep.FixedRows != 1 {
		t.Fatalf("应改写 1 行，实际 %d", rep.FixedRows)
	}

	var mid model.Transaction
	if err := gdb.Where("user_id = ? AND amount = ?", 1, -15).First(&mid).Error; err != nil {
		t.Fatalf("读回流水失败: %v", err)
	}
	if mid.BalanceAfter != 999999984 {
		t.Fatalf("快照应校正为 999999984，实际 %d", mid.BalanceAfter)
	}
	if mid.Amount != -15 {
		t.Fatalf("金额绝不能被改：%d", mid.Amount)
	}
	var u model.User
	if err := gdb.First(&u, 1).Error; err != nil {
		t.Fatalf("读回用户失败: %v", err)
	}
	if u.Balance != 999999974 {
		t.Fatalf("余额绝不能被改：%d", u.Balance)
	}

	// 幂等：再跑一次应无差异。
	again, err := AlignLedgerChain(gdb, false)
	if err != nil {
		t.Fatalf("二次校正失败: %v", err)
	}
	if again.BadRows != 0 {
		t.Fatalf("校正后应无差异，实际 %d", again.BadRows)
	}
}

// TestAlignLedgerChainSkipsRealDiscrepancy 守「不许把真账务差异对齐掉」。
//
// 用户余额 100 万，但流水链推出的末行是 500 万 —— 这不是舍入，是丢账/重复入账。
// 校正必须整体跳过该用户并给出原因，否则一次「修复」就把真实缺口抹平了。
func TestAlignLedgerChainSkipsRealDiscrepancy(t *testing.T) {
	gdb := withLedgerDB(t)
	seedChainUser(t, gdb, 2, 1000000, []model.Transaction{
		{Amount: 5000000, BalanceAfter: 5000000},
	})

	rep, err := AlignLedgerChain(gdb, false)
	if err != nil {
		t.Fatalf("校正失败: %v", err)
	}
	if rep.FixedRows != 0 {
		t.Fatalf("超容差的用户不许被改写，实际改了 %d 行", rep.FixedRows)
	}
	if len(rep.Skipped) == 0 {
		t.Fatal("必须报告跳过原因")
	}

	var tx model.Transaction
	if err := gdb.Where("user_id = ?", 2).First(&tx).Error; err != nil {
		t.Fatalf("读回流水失败: %v", err)
	}
	if tx.BalanceAfter != 5000000 {
		t.Fatalf("被跳过的用户流水不应被改写，实际 %d", tx.BalanceAfter)
	}
}

// TestAlignUsageChargeOnlyTinyDrift 断言只有「与流水差 ≤ 容差」的用量会被对齐，
// 且**没有任何流水的用量一律不动**（那是「算出应收但一分没扣到」，必须继续报警）。
func TestAlignUsageChargeOnlyTinyDrift(t *testing.T) {
	gdb := withLedgerDB(t)
	if err := gdb.Create(&model.User{ID: 3, Username: "u3", Balance: 0}).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	// 三条用量：① 差 1 微（舍入）；② 差 500 微（真差异）；③ 完全没有流水。
	usages := []model.Usage{
		{UserID: 3, Status: model.UsageStatusSettled, Charge: 130},
		{UserID: 3, Status: model.UsageStatusSettled, Charge: 600},
		{UserID: 3, Status: model.UsageStatusSettled, Charge: 5},
	}
	for i := range usages {
		if err := gdb.Create(&usages[i]).Error; err != nil {
			t.Fatalf("建用量失败: %v", err)
		}
	}
	// ① 的流水合计 -129（与 130 差 1）；② 的流水合计 -100（与 600 差 500）。
	mk := func(usageID uint64, amount int64) {
		if err := gdb.Create(&model.Transaction{
			UserID: 3, Type: model.TxnSettle, Amount: amount, BalanceAfter: 0,
			RefType: model.RefTypeUsage, RefID: itoa(usageID),
		}).Error; err != nil {
			t.Fatalf("建流水失败: %v", err)
		}
	}
	mk(usages[0].ID, -129)
	mk(usages[1].ID, -100)

	// 预演必须先报对「原值」：报告里的金额列如果扫错了列，运维看到的是
	// 「charge 0 → 129」这种看不懂的样本，反而会怀疑工具本身。
	dry, err := AlignUsageCharge(gdb, true, 2)
	if err != nil {
		t.Fatalf("预演失败: %v", err)
	}
	if len(dry.Samples) != 1 {
		t.Fatalf("预演应只报 1 条，实际 %d", len(dry.Samples))
	}
	if dry.Samples[0].Stored != 130 || dry.Samples[0].Ledger != 129 {
		t.Fatalf("预演样本金额错: stored=%d ledger=%d（应为 130 / 129）",
			dry.Samples[0].Stored, dry.Samples[0].Ledger)
	}

	rep, err := AlignUsageCharge(gdb, false, 2)
	if err != nil {
		t.Fatalf("校正失败: %v", err)
	}
	if rep.BadRows != 1 || rep.FixedRows != 1 {
		t.Fatalf("应只处理「差 1 微」的那一条，实际 bad=%d fixed=%d", rep.BadRows, rep.FixedRows)
	}

	var got []model.Usage
	if err := gdb.Where("user_id = ?", 3).Order("id").Find(&got).Error; err != nil {
		t.Fatalf("读回用量失败: %v", err)
	}
	if got[0].Charge != 129 {
		t.Fatalf("① 应对齐到流水净额 129，实际 %d", got[0].Charge)
	}
	if got[1].Charge != 600 {
		t.Fatalf("② 差 500 微属真实差异，不许改，实际 %d", got[1].Charge)
	}
	if got[2].Charge != 5 {
		t.Fatalf("③ 无流水的用量不许动，实际 %d", got[2].Charge)
	}
}
