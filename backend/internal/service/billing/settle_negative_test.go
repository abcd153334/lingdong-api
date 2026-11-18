package billing

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

/*
金额单位：本文件所有额度都是**微**（money.Scale = 1e6）。
环境变量按「货币单位」写（"0.001" = 1000 微），非法值会 panic 而不是静默回落。
*/

const testSchema = "ld_test_billing"

func withTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		// LD_REQUIRE_TEST_DSN=1 时不许跳过：门禁「全绿」必须等于「真跑过」。
		// 计费是这套系统里最不能靠人眼保证的部分，跳过它等于没跑门禁。
		if os.Getenv("LD_REQUIRE_TEST_DSN") == "1" {
			t.Fatal("LD_REQUIRE_TEST_DSN=1 但未设置 LD_TEST_DSN：集成用例不允许被静默跳过")
		}
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}, &model.Usage{}, &model.Transaction{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// callSnap 构造按次计费快照（每次 500 微）。
func callSnap() Snapshot {
	return Snapshot{ModelID: "vid", BillingMode: model.BillingModeCall, Price: model.Prices{Call: iptr(500)}}
}

// TestSettleNegativeBlockedByDefault 断言默认**不允许**结算把余额击穿到负。
//
// 改造前「允许无限透支」——一次异常用量可以把用户余额扣成负数，之后该用户
// 所有请求都被 402 挡住，而平台账上多了一笔收不回来的欠款。现在默认不透支：
// 能扣多少扣多少（余额落到 0），差额写 negative_balance_blocked 并留 anomaly。
func TestSettleNegativeBlockedByDefault(t *testing.T) {
	t.Setenv("LD_NEGATIVE_LIMIT", "") // 默认 0 = 不透支
	ResetLimitsForTest()              // 进程级缓存需重置，否则读到上一次用例的值
	gdb := withTestDB(t)
	ctx := context.Background()

	var warned int
	var warnedBalance int64
	prevWarn := quotaNegativeWarn
	quotaNegativeWarn = func(userID, keyID, usageID uint64, settled, frozen, balanceAfter int64) {
		warned++
		warnedBalance = balanceAfter
	}
	defer func() { quotaNegativeWarn = prevWarn }()

	u := &model.User{Username: "neg-blocked", PasswordHash: "x", Balance: 1000, Group: "default", AffCode: "ab1"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := &model.APIKey{UserID: u.ID, KeyCipher: "x", KeyHash: "hb1", Name: "k", Status: 1, Group: "default"}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}

	svc := New(gdb)
	sess, err := svc.Freeze(ctx, FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 200,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}
	// 模拟「冻结之后、结算之前」余额被其它并发请求扣到只剩 50。
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).Update("balance", 50).Error; err != nil {
		t.Fatalf("调整余额失败: %v", err)
	}

	if _, err := svc.Settle(ctx, sess, SettleInput{StatusCode: 200, ChannelID: 1, AdapterID: "openai"}); err != nil {
		t.Fatalf("结算不应被拒绝（只是少扣）: %v", err)
	}

	var got model.User
	if err := gdb.First(&got, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Balance != 0 {
		t.Fatalf("余额应停在 0（不透支），实际 %d", got.Balance)
	}
	if got.Frozen != 0 {
		t.Fatalf("冻结额应清零，实际 %d", got.Frozen)
	}
	// charge = 冻结 200 + 可扣 50 = 250。
	// 注意：这里**不应**触发 quotaNegativeWarn —— 该告警专指「余额真的变负」，
	// 本次被下限挡住后余额停在 0，负额告警反而不该响（否则告警语义被稀释）。
	if warned != 0 {
		t.Fatalf("余额未转负时不应触发负额告警，实际 warned=%d balance=%d", warned, warnedBalance)
	}

	var usage model.Usage
	if err := gdb.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatal(err)
	}
	if usage.Status != "settled" {
		t.Fatalf("状态应为 settled，实际 %s", usage.Status)
	}
	if usage.Charge != 250 {
		t.Fatalf("实际扣费应为 250（200 冻结 + 50 可扣），实际 %d", usage.Charge)
	}
	if !strings.Contains(usage.Anomaly, model.AnomalyNegativeBalanceBlocked) {
		t.Fatalf("anomaly 应标记 negative_balance_blocked，实际 %q", usage.Anomaly)
	}
	var txn model.Transaction
	if err := gdb.Where("ref_type = ? AND ref_id = ? AND type = ?", "usage",
		strconv.FormatUint(sess.UsageID, 10), "settle").First(&txn).Error; err != nil {
		t.Fatalf("未写 settle 流水: %v", err)
	}
	if txn.BalanceAfter != 0 {
		t.Fatalf("settle 流水 balance_after 应为 0，实际 %d", txn.BalanceAfter)
	}
}

// TestSettleNegativeAllowedWhenConfigured 断言显式配置透支额度后，允许负到该额度。
//
// 需要透支的业务（后付费大客户、内部账号）用 LD_NEGATIVE_LIMIT 显式开启，
// 而不是像改造前那样对所有人默认开放。
func TestSettleNegativeAllowedWhenConfigured(t *testing.T) {
	t.Setenv("LD_NEGATIVE_LIMIT", "0.001") // 1000 微
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	prevWarn := quotaNegativeWarn
	var warned int
	quotaNegativeWarn = func(userID, keyID, usageID uint64, settled, frozen, balanceAfter int64) { warned++ }
	defer func() { quotaNegativeWarn = prevWarn }()

	u := &model.User{Username: "neg-allowed", PasswordHash: "x", Balance: 1000, Group: "default", AffCode: "aa1"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := &model.APIKey{UserID: u.ID, KeyCipher: "x", KeyHash: "ha1", Name: "k", Status: 1, Group: "default"}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}

	svc := New(gdb)
	sess, err := svc.Freeze(ctx, FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 200,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).Update("balance", 50).Error; err != nil {
		t.Fatalf("调整余额失败: %v", err)
	}
	if _, err := svc.Settle(ctx, sess, SettleInput{StatusCode: 200, ChannelID: 1, AdapterID: "openai"}); err != nil {
		t.Fatalf("结算不应被拒绝: %v", err)
	}

	var got model.User
	if err := gdb.First(&got, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	// charge 500，余额 50 → 结算后 -250（在 1000 微的透支额度内）
	if got.Balance != -250 {
		t.Fatalf("余额应为 -250（透支额度内允许），实际 %d", got.Balance)
	}
	var usage model.Usage
	if err := gdb.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatal(err)
	}
	if usage.Charge != 500 {
		t.Fatalf("实际扣费应为 500，实际 %d", usage.Charge)
	}
	if strings.Contains(usage.Anomaly, model.AnomalyNegativeBalanceBlocked) {
		t.Fatalf("配置了透支额度就不应写 negative_balance_blocked，实际 %q", usage.Anomaly)
	}
	// 余额真的变负 → 必须留结构化告警（允许透支不等于不告警）。
	if warned != 1 {
		t.Fatalf("余额转负应触发一次告警，实际 %d", warned)
	}
}

// TestSettleChargeCappedByFrozen 断言结算金额被冻结额截顶（冻结 1 却要扣 500 → 只扣 3）。
//
// 冻结额是平台对「这次请求最多花多少」的承诺。上游返回天文数字用量（或适配器解析错），
// 不截顶就会把用户余额一次性清空。3 倍是宽容上限：正常业务不会超过预扣的 3 倍。
func TestSettleChargeCappedByFrozen(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()

	prevWarn := quotaNegativeWarn
	quotaNegativeWarn = func(userID, keyID, usageID uint64, settled, frozen, balanceAfter int64) {}
	defer func() { quotaNegativeWarn = prevWarn }()

	u := &model.User{Username: "cap1", PasswordHash: "x", Balance: 100000, Group: "default", AffCode: "ac1"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := &model.APIKey{UserID: u.ID, KeyCipher: "x", KeyHash: "hc1", Name: "k", Status: 1, Group: "default"}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}

	svc := New(gdb)
	sess, err := svc.Freeze(ctx, FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Group: "default",
		Snapshot: callSnap(), Amount: 1,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}
	if _, err := svc.Settle(ctx, sess, SettleInput{StatusCode: 200, ChannelID: 1, AdapterID: "openai"}); err != nil {
		t.Fatalf("结算不应被拒绝: %v", err)
	}

	var usage model.Usage
	if err := gdb.First(&usage, sess.UsageID).Error; err != nil {
		t.Fatal(err)
	}
	if usage.Charge != 3 {
		t.Fatalf("冻结 1 时最多扣 3，实际 %d", usage.Charge)
	}
	if !strings.Contains(usage.Anomaly, model.AnomalyChargeCapped) {
		t.Fatalf("anomaly 应标记 charge_capped，实际 %q", usage.Anomaly)
	}
	var got model.User
	if err := gdb.First(&got, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Balance != 100000-3 {
		t.Fatalf("余额应只扣 3，实际 %d", got.Balance)
	}
}

// TestChargeCapFor 锁定截顶上限的三档来源：冻结额 3 倍 / 零冻结兜底 / 全局上限。
func TestChargeCapFor(t *testing.T) {
	// 冻结额 > 0：min(冻结额×3, 全局)。冻结 1000 → 3000 < 全局。
	if cap, reason, overflow := chargeCapFor(1000, 0); cap != 3000 || reason != "frozen3x" || overflow {
		t.Fatalf("冻结 1000 应 cap=3000/frozen3x，got %d/%s/%v", cap, reason, overflow)
	}
	// 冻结额 0：零冻结兜底，reason=zero_frozen。
	if cap, reason, _ := chargeCapFor(0, 0); cap != zeroFrozenCap() || reason != "zero_frozen" {
		t.Fatalf("零冻结应 cap=%d/zero_frozen，got %d/%s", zeroFrozenCap(), cap, reason)
	}
	// 冻结额极大：3 倍超全局 → 全局上限 + global。
	if cap, reason, _ := chargeCapFor(maxCharge(), 0); cap != maxCharge() || reason != "global" {
		t.Fatalf("冻结超限应 cap=global/global，got %d/%s", cap, reason)
	}
	// 冻结额×3 溢出 int64 → 退回全局上限，且必须标记 overflow（不能静默）。
	big := int64(1<<62) / 2
	if cap, _, overflow := chargeCapFor(big, 0); cap != maxCharge() || !overflow {
		t.Fatalf("溢出应 cap=%d 且 overflow=true，got %d/%v", maxCharge(), cap, overflow)
	}
	// 配置最坏情形可以**抬高**上限：冻结 0（ASR）时不再被 2 元零冻结兜底削掉。
	if cap, reason, _ := chargeCapFor(0, 50_000_000); cap != 50_000_000 || reason != "configured_max" {
		t.Fatalf("ceiling 应抬高上限到 50 元/configured_max，got %d/%s", cap, reason)
	}
	// ceiling 不能突破全局上限。
	if cap, reason, _ := chargeCapFor(0, maxCharge()+1); cap != maxCharge() || reason != "global" {
		t.Fatalf("ceiling 超全局应回落 global，got %d/%s", cap, reason)
	}
}

// TestZeroFrozenCap 锁定零冻结兜底的默认值与环境变量覆盖。
func TestZeroFrozenCap(t *testing.T) {
	t.Cleanup(func() { ResetLimitsForTest() })

	// 默认：2 货币单位（与 qpc 无关 —— 刻度概念已删除）。
	t.Setenv("LD_ZERO_FROZEN_CAP", "")
	ResetLimitsForTest()
	if got := zeroFrozenCap(); got != DefaultZeroFrozenCap {
		t.Fatalf("默认零冻结兜底应 %d，got %d", DefaultZeroFrozenCap, got)
	}
	// 显式覆盖：环境变量按「货币单位」写，"0.5" = 500000 微。
	t.Setenv("LD_ZERO_FROZEN_CAP", "0.5")
	ResetLimitsForTest()
	if got := zeroFrozenCap(); got != 500_000 {
		t.Fatalf("显式覆盖应 500000，got %d", got)
	}
}

// TestZeroFrozenCapPanicsOnInvalidEnv 锁定「非法金额必须启动即崩」。
//
// 上一版是"解析失败退回默认值"，症状是金额上限悄悄变回旧值且线上无从发现；
// 现在宁可 panic —— 这类配置每次结算都要读，静默回落的代价是多收/少收。
func TestZeroFrozenCapPanicsOnInvalidEnv(t *testing.T) {
	t.Setenv("LD_ZERO_FROZEN_CAP", "abc")
	ResetLimitsForTest()
	t.Cleanup(func() {
		t.Setenv("LD_ZERO_FROZEN_CAP", "")
		ResetLimitsForTest()
	})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("非法金额必须 panic，不能静默回落默认值")
		}
	}()
	_ = zeroFrozenCap()
}
