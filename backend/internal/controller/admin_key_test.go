package controller

// 管理台 KEY 兜底启停端点：状态落库、解禁刷新闲置基准、审计留痕、非法入参拒绝。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/audit"
)

const adminKeyTestSchema = "ld_test_hk_adminkey"

func withAdminKeyDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + adminKeyTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+adminKeyTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}, &model.AdminAudit{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + adminKeyTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// patchKeyStatus 模拟管理台调用，返回 HTTP 状态码。
func patchKeyStatus(t *testing.T, id uint64, body string, adminID uint64) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: itoaU(id)}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/keys/"+itoaU(id)+"/status", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", adminID)
	AdminKeyStatus(c)
	return w.Code
}

func itoaU(v uint64) string { return strconv.FormatUint(v, 10) }

// TestAdminKeyStatus_DisableThenEnable 断言两次启停的状态落库与审计留痕，
// 且解禁会刷新 last_used_at（否则禁用超过闲置期的密钥解禁后仍然不可用）。
func TestAdminKeyStatus_DisableThenEnable(t *testing.T) {
	gdb := withAdminKeyDB(t)
	u := model.User{Username: "admkey", PasswordHash: "x", Balance: 1000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -300)
	k := model.APIKey{
		UserID: u.ID, AppID: 1, Kind: model.KeyKindHosted, KeyCipher: "x",
		KeyHash: "adminkey-h", Name: "h", Status: 1, LastUsedAt: &old,
	}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatal(err)
	}

	if code := patchKeyStatus(t, k.ID, `{"status":2}`, 99); code != http.StatusOK {
		t.Fatalf("禁用应 200，实际 %d", code)
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != 2 {
		t.Fatalf("禁用后 status 应为 2，实际 %d", got.Status)
	}
	// 管理台禁用必须是 admin_block：否则一次 rotate 就能绕过管理处置。
	if got.DisabledReason != model.KeyDisabledAdminBlock {
		t.Fatalf("管理台禁用应写入 admin_block，实际 %q", got.DisabledReason)
	}
	if got.LastUsedAt == nil || got.LastUsedAt.Sub(old).Abs() > time.Second {
		t.Fatalf("禁用不得改 last_used_at，实际 %v", got.LastUsedAt)
	}

	if code := patchKeyStatus(t, k.ID, `{"status":1}`, 99); code != http.StatusOK {
		t.Fatalf("启用应 200，实际 %d", code)
	}
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != 1 {
		t.Fatalf("启用后 status 应为 1，实际 %d", got.Status)
	}
	// 解禁即恢复信任：不清掉标记，下一次轮换会把它当成不可自愈。
	if got.DisabledReason != "" {
		t.Fatalf("解禁应清空 disabled_reason，实际 %q", got.DisabledReason)
	}
	if got.LastUsedAt == nil || time.Since(*got.LastUsedAt) > time.Minute {
		t.Fatalf("解禁必须刷新 last_used_at（否则仍被判闲置过期），实际 %v", got.LastUsedAt)
	}

	var n int64
	if err := gdb.Model(&model.AdminAudit{}).
		Where("action = ? AND target_id = ?", audit.ActionKeyAdminStatus, itoaU(k.ID)).
		Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("两次启停应各留一条审计，实际 %d", n)
	}
}

// TestAdminKeyStatus_RejectsInvalidInput 断言非法状态码被拒且不动库。
func TestAdminKeyStatus_RejectsInvalidInput(t *testing.T) {
	gdb := withAdminKeyDB(t)
	k := model.APIKey{UserID: 1, Kind: model.KeyKindUser, KeyCipher: "x", KeyHash: "adminkey-u", Name: "u", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{`{"status":3}`, `{}`, `not-json`} {
		if code := patchKeyStatus(t, k.ID, body, 99); code != http.StatusBadRequest {
			t.Fatalf("入参 %s 应 400，实际 %d", body, code)
		}
	}
	var got model.APIKey
	if err := gdb.First(&got, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != 1 {
		t.Fatalf("非法入参不得改状态，实际 %d", got.Status)
	}

	if code := patchKeyStatus(t, 9_999_999, `{"status":2}`, 99); code != http.StatusNotFound {
		t.Fatalf("不存在的 KEY 应 404，实际 %d", code)
	}
}
