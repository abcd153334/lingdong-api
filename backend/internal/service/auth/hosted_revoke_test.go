package auth

// 改密/换绑对托管密钥的豁免语义：止血只禁自建 KEY，管理员强制重置仍全禁。

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

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/apikey"
)

const revokeSchema = "auth_revoke_test"

// withRevokeDB 在独立 schema 里准备 users/sessions/api_keys。
func withRevokeDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("LD_TEST_DSN"))
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + revokeSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+revokeSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.UserSession{}, &model.APIKey{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + revokeSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func testDSNOf(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(os.Getenv("LD_TEST_DSN"))
}

// seedUserWithKeys 造一个用户 + 一把自建 + 一把托管（均启用）。
func seedUserWithKeys(t *testing.T, gdb *gorm.DB) model.User {
	t.Helper()
	u := model.User{Username: "rev" + strings.TrimSpace(strconv.FormatInt(time.Now().UnixNano()%1_000_000_000_000, 36)), PasswordHash: "x", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	for _, k := range []model.APIKey{
		{UserID: u.ID, Kind: model.KeyKindUser, KeyCipher: "x", KeyHash: "u-" + u.Username, Name: "自建", Status: apikey.KeyStatusEnabled},
		{UserID: u.ID, AppID: 3, Kind: model.KeyKindHosted, KeyCipher: "x", KeyHash: "h-" + u.Username, Name: "托管", Status: apikey.KeyStatusEnabled},
	} {
		if err := gdb.Create(&k).Error; err != nil {
			t.Fatalf("建密钥失败: %v", err)
		}
	}
	return u
}

func keyStatus(t *testing.T, gdb *gorm.DB, hash string) int16 {
	t.Helper()
	var k model.APIKey
	if err := gdb.Where("key_hash = ?", hash).First(&k).Error; err != nil {
		t.Fatal(err)
	}
	return k.Status
}

// TestSetPasswordChange_KeepsHostedKeys：自助改密豁免托管密钥。
func TestSetPasswordChange_KeepsHostedKeys(t *testing.T) {
	gdb := withRevokeDB(t)
	u := seedUserWithKeys(t, gdb)
	s := New(gdb)

	if err := s.setPassword(context.Background(), u.ID, "Str0ng!NewPass", model.SessionRevokePasswordChanged, false); err != nil {
		t.Fatalf("改密失败: %v", err)
	}
	if got := keyStatus(t, gdb, "h-"+u.Username); got != apikey.KeyStatusEnabled {
		t.Fatalf("托管密钥必须保持启用，实际 status=%d", got)
	}
	if got := keyStatus(t, gdb, "u-"+u.Username); got != apikey.KeyStatusDisabled {
		t.Fatalf("自建密钥应被止血禁用，实际 status=%d", got)
	}
}

// TestAdminResetPassword_RevokesHosted：管理员强制重置 = 账号认定失陷，仍全禁。
func TestAdminResetPassword_RevokesHosted(t *testing.T) {
	gdb := withRevokeDB(t)
	u := seedUserWithKeys(t, gdb)
	s := New(gdb)

	if err := s.AdminResetPassword(context.Background(), u.ID, "Str0ng!NewPass"); err != nil {
		t.Fatalf("管理员重置失败: %v", err)
	}
	if got := keyStatus(t, gdb, "h-"+u.Username); got != apikey.KeyStatusDisabled {
		t.Fatalf("管理员重置必须连托管密钥一起禁用，实际 status=%d", got)
	}
	if got := keyStatus(t, gdb, "u-"+u.Username); got != apikey.KeyStatusDisabled {
		t.Fatalf("自建密钥应被禁用，实际 status=%d", got)
	}
	var uu model.User
	if err := gdb.First(&uu, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !uu.MustChangePassword {
		t.Fatal("管理员重置后必须带 must_change_password 标记")
	}
}
