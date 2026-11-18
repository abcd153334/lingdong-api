package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/session"
)

// 本文件覆盖凭证失效契约：管理端改角色/停用必须**当场撤销该用户全部会话**，
// 使其旧票立即失效；同值提交不撤销（不做无意义登出）。

const adminUserTestSchema = "ld_test_r2_adminuser"

// withAdminUserDB 在独立 schema 中准备 users/admin_audit 表并指向 db.DB（需 LD_TEST_DSN；未设置则跳过）。
func withAdminUserDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + adminUserTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+adminUserTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.AdminAudit{}, &model.UserSession{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + adminUserTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// activeSessions 返回该用户当前仍有效的会话数（撤销语义的唯一判据是 revoked_at）。
func activeSessions(t *testing.T, gdb *gorm.DB, id uint64) int64 {
	t.Helper()
	var n int64
	if err := gdb.Model(&model.UserSession{}).
		Where("uid = ? AND revoked_at IS NULL", id).
		Count(&n).Error; err != nil {
		t.Fatalf("统计会话失败: %v", err)
	}
	return n
}

// grantSession 给用户发一张真实会话票（模拟其已登录的一台设备）。
func grantSession(t *testing.T, gdb *gorm.DB, id uint64) {
	t.Helper()
	if _, _, err := session.New(gdb).Create(context.Background(), id, "127.0.0.1", "test", 0); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
}

// TestAdminUpdateUserRevokesSession 断言：角色/状态实际变更 → 200 且该用户会话全部撤销；同值提交 → 不撤销。
func TestAdminUpdateUserRevokesSession(t *testing.T) {
	gdb := withAdminUserDB(t)
	gin.SetMode(gin.TestMode)

	u := model.User{Username: "victim", PasswordHash: "x", Group: "default", Status: 1}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	// role 带 default:1，Create 传 0 会被 DB 默认值覆盖 → 显式置 0，使「同值提交 role=0」成为无变更
	if err := gdb.Exec("UPDATE users SET role = 0 WHERE id = ?", u.ID).Error; err != nil {
		t.Fatalf("置普通用户角色失败: %v", err)
	}
	// 另一个在任管理员：否则本次降权会撞上「最后管理员保护」直接 400/403，
	// 测的就不是会话撤销了（保护逻辑由 admin_last_admin_test 覆盖）。
	peer := model.User{Username: "peer-admin", PasswordHash: "x", Group: "default", Status: 1}
	if err := gdb.Create(&peer).Error; err != nil {
		t.Fatalf("造备用管理员失败: %v", err)
	}
	if err := gdb.Exec("UPDATE users SET role = 0 WHERE id = ?", peer.ID).Error; err != nil {
		t.Fatalf("置备用管理员角色失败: %v", err)
	}
	grantSession(t, gdb, u.ID)
	grantSession(t, gdb, u.ID)
	if n := activeSessions(t, gdb, u.ID); n != 2 {
		t.Fatalf("夹具应有两张有效会话，实际 %d", n)
	}

	r := gin.New()
	r.PATCH("/api/admin/users/:id", func(c *gin.Context) {
		c.Set("user_id", uint64(999)) // 模拟已鉴权的操作者
		AdminUpdateUser(c)
	})
	patch := func(body string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+strconv.FormatUint(u.ID, 10), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w.Code
	}

	// 同值提交（role 未变）→ 放行但**不撤销**（避免无意义登出）
	if got := patch(`{"role":0}`); got != http.StatusOK {
		t.Fatalf("同值提交应放行 200，实际 %d", got)
	}
	if n := activeSessions(t, gdb, u.ID); n != 2 {
		t.Fatalf("同值提交不应撤销会话，期望 2 实际 %d", n)
	}

	// 角色实际变更（0→1 降权）→ 200 且全部会话撤销
	if got := patch(`{"role":1}`); got != http.StatusOK {
		t.Fatalf("改角色应 200，实际 %d", got)
	}
	if n := activeSessions(t, gdb, u.ID); n != 0 {
		t.Fatalf("角色变更必须撤销全部会话，剩余 %d", n)
	}
	var reason string
	if err := gdb.Model(&model.UserSession{}).Where("uid = ?", u.ID).
		Order("id DESC").Limit(1).Pluck("revoke_reason", &reason).Error; err != nil {
		t.Fatalf("读取撤销原因失败: %v", err)
	}
	if reason != model.SessionRevokeAdminRevoked {
		t.Fatalf("撤销原因应为 %q，实际 %q", model.SessionRevokeAdminRevoked, reason)
	}

	// 状态实际变更（1→2 停用）→ 200 且新一轮会话同样被撤销
	grantSession(t, gdb, u.ID)
	if got := patch(`{"status":2}`); got != http.StatusOK {
		t.Fatalf("停用应 200，实际 %d", got)
	}
	if n := activeSessions(t, gdb, u.ID); n != 0 {
		t.Fatalf("状态变更必须撤销全部会话，剩余 %d", n)
	}

	// 仅改分组：非权限敏感操作，不撤销
	grantSession(t, gdb, u.ID)
	if got := patch(`{"group":"vip"}`); got != http.StatusOK {
		t.Fatalf("改分组应 200，实际 %d", got)
	}
	if n := activeSessions(t, gdb, u.ID); n != 1 {
		t.Fatalf("仅改分组不应撤销会话，期望 1 实际 %d", n)
	}
}
