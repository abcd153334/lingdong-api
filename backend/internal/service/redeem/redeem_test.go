package redeem

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
)

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------

const testSchema = "ld_test_s9_redeem"

// withTestDB 在独立 schema 中准备兑换码相关表。
func withTestDB(t *testing.T) *gorm.DB {
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
	if err := gdb.AutoMigrate(&model.User{}, &model.RedeemCode{}, &model.Transaction{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func setCfg(key string, v any) {
	raw, _ := json.Marshal(v)
	conf.System().Set(key, raw)
}

func seedUser(t *testing.T, gdb *gorm.DB) *model.User {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	u := &model.User{Username: "rd" + suffix, PasswordHash: "x", Group: "default", AffCode: "aff" + suffix}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造测试用户失败: %v", err)
	}
	return u
}

func seedCode(t *testing.T, gdb *gorm.DB, code string, quota int64, expires *time.Time, usedBy *uint64) *model.RedeemCode {
	t.Helper()
	rc := &model.RedeemCode{Code: code, Amount: quota, Batch: "t", ExpiresAt: expires, UsedBy: usedBy}
	if usedBy != nil {
		now := time.Now()
		rc.UsedAt = &now
	}
	if err := gdb.Create(rc).Error; err != nil {
		t.Fatalf("造兑换码失败: %v", err)
	}
	return rc
}

func wantErr(t *testing.T, err error, want errs.Def) {
	t.Helper()
	if err != want {
		t.Fatalf("错误码不符: got %v, want %v", err, want)
	}
}

// TestRedeemBranches 覆盖：非法/不存在/已用/已过期 → 明确错误；成功入账 + 幂等；超上限拦截。
func TestRedeemBranches(t *testing.T) {
	gdb := withTestDB(t)
	setCfg("balance_limit", int64(500000000))
	svc := New(gdb)
	ctx := context.Background()
	u := seedUser(t, gdb)

	if _, err := svc.Redeem(ctx, u.ID, "   "); err != errs.RedeemInvalid {
		wantErr(t, err, errs.RedeemInvalid)
	}
	if _, err := svc.Redeem(ctx, u.ID, strings.Repeat("A", 33)); err != errs.RedeemInvalid {
		wantErr(t, err, errs.RedeemInvalid)
	}
	if _, err := svc.Redeem(ctx, u.ID, "NOPE-NOPE-NOPE"); err != errs.RedeemInvalid {
		wantErr(t, err, errs.RedeemInvalid)
	}

	used := u.ID
	seedCode(t, gdb, "USED-USED-USED", 1000, nil, &used)
	if _, err := svc.Redeem(ctx, u.ID, "used-used-used"); err != errs.RedeemUsed {
		wantErr(t, err, errs.RedeemUsed)
	}

	past := time.Now().Add(-time.Hour)
	seedCode(t, gdb, "EXPD-EXPD-EXPD", 1000, &past, nil)
	if _, err := svc.Redeem(ctx, u.ID, "expe-not-exist"); err != errs.RedeemInvalid {
		wantErr(t, err, errs.RedeemInvalid)
	}
	if _, err := svc.Redeem(ctx, u.ID, "EXPD-EXPD-EXPD"); err != errs.RedeemExpired {
		wantErr(t, err, errs.RedeemExpired)
	}

	// 成功：入账 + 写 redeem 流水 + 标记已用（同码再兑 → 已用）
	code := "OKOK-OKOK-OKOK"
	seedCode(t, gdb, code, 123456, nil, nil)
	got, err := svc.Redeem(ctx, u.ID, "okok-okok-okok")
	if err != nil {
		t.Fatalf("兑换应成功: %v", err)
	}
	if got != 123456 {
		t.Fatalf("入账额度不符: %d", got)
	}
	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if after.Balance != 123456 {
		t.Fatalf("余额不符: %d", after.Balance)
	}
	var tx model.Transaction
	if err := gdb.Where("user_id = ? AND type = ?", u.ID, "redeem").First(&tx).Error; err != nil {
		t.Fatalf("应写 redeem 流水: %v", err)
	}
	if tx.Amount != 123456 || tx.BalanceAfter != 123456 || tx.RefType != "redeem_code" {
		t.Fatalf("流水不符: %+v", tx)
	}
	if _, err := svc.Redeem(ctx, u.ID, code); err != errs.RedeemUsed {
		wantErr(t, err, errs.RedeemUsed)
	}

	// 超上限：不入账、不标记
	setCfg("balance_limit", int64(100000))
	t.Cleanup(func() { setCfg("balance_limit", int64(500000000)) })
	seedCode(t, gdb, "BIGX-BIGX-BIGX", 999999, nil, nil)
	if _, err := svc.Redeem(ctx, u.ID, "BIGX-BIGX-BIGX"); err != errs.BalanceLimitExceeded {
		wantErr(t, err, errs.BalanceLimitExceeded)
	}
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if after.Balance != 123456 {
		t.Fatalf("超限不应入账: %d", after.Balance)
	}
	var rc model.RedeemCode
	if err := gdb.Where("code = ?", "BIGX-BIGX-BIGX").First(&rc).Error; err != nil {
		t.Fatalf("查询兑换码失败: %v", err)
	}
	if rc.UsedBy != nil {
		t.Fatalf("超限不应标记已用")
	}
}
