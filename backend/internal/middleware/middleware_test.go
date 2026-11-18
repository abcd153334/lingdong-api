package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/session"
)

const testSchema = "ld_test_middleware"

// middlewareTestDB 在独立 schema 中准备 users/api_keys/admin_audit 表（需 LD_TEST_DSN；未设置则跳过）。
func middlewareTestDB(t *testing.T) *gorm.DB {
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
	if err := gdb.AutoMigrate(&model.AdminAudit{}, &model.User{}, &model.APIKey{}, &model.UserSession{}); err != nil {
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

// TestAuditMiddlewareDoesNotDuplicate 断言 Audit 中间件本身不落库 admin_audit：
// 请求内控制器显式调用 audit.Record 一次，最终 admin_audit 只能有 1 行（不重复记录）。
func TestAuditMiddlewareDoesNotDuplicate(t *testing.T) {
	gdb := middlewareTestDB(t)
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(Audit())
	r.POST("/api/admin/x", func(c *gin.Context) {
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionChannelDelete, "channel", "1", nil)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/x", nil)
	r.ServeHTTP(w, req)

	var n int64
	if err := gdb.Model(&model.AdminAudit{}).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("审计应仅落库 1 行（中间件不重复记录），实际 %d 行", n)
	}
}

// TestTokenAuthDisabledKey 断言已禁用 KEY 返回可区分的 key_disabled（而非 invalid_api_key）。
func TestTokenAuthDisabledKey(t *testing.T) {
	gdb := middlewareTestDB(t)
	gin.SetMode(gin.TestMode)

	u := model.User{Username: "u1", PasswordHash: "x", Group: "default", Role: 0, Status: 1}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	const plain = "ld-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	if err := gdb.Create(&model.APIKey{
		UserID: u.ID, KeyCipher: "x", KeyHash: apikey.Hash(plain), Status: 2,
	}).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}

	r := gin.New()
	r.Use(TokenAuth())
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })

	code := func(token string) string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("token=%q 期望 401，实际 %d", token, w.Code)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		return body.Error.Code
	}

	if got := code(plain); got != "key_disabled" {
		t.Fatalf("已禁用 KEY 应返回 key_disabled，实际 %q", got)
	}
	if got := code("ld-notexistAAAAAAAAAAAAAAAAAAAAAA"); got != "invalid_api_key" {
		t.Fatalf("不存在的 KEY 应返回 invalid_api_key，实际 %q", got)
	}
}

// newTestSession 建一条真实会话并返回票据明文（与登录成功后的凭证形态一致）。
//
// 改造前这里是「签一个 HS256 JWT」：JWT 无状态，测试里的角色快照与 DB 无关，
// 所以只能靠 token_version 间接表达「凭证还算不算数」。现在凭证是服务端行，
// 票据必须真的存在于 user_sessions 才能通过 middleware 校验。
func newTestSession(t *testing.T, gdb *gorm.DB, uid uint64) string {
	t.Helper()
	ticket, _, err := session.New(gdb).Create(context.Background(), uid, "127.0.0.1", "test-agent", 0)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	return ticket
}

// TestAdminAuthReadsLiveRole 断言管理面判权以 DB 实时角色为准（不信任会话快照）：
// 用户被降权（DB role=1）后，同一张仍然有效的会话票也必须 403。
func TestAdminAuthReadsLiveRole(t *testing.T) {
	gdb := middlewareTestDB(t)
	gin.SetMode(gin.TestMode)

	u := model.User{Username: "adm-live", PasswordHash: "x", Group: "default", Status: 1}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("造管理员失败: %v", err)
	}
	// role 带 default:1，Create 传 0 会被 DB 默认值覆盖 → 显式置 0（与登录种子同一手法）
	if err := gdb.Exec("UPDATE users SET role = 0 WHERE id = ?", u.ID).Error; err != nil {
		t.Fatalf("置管理员角色失败: %v", err)
	}
	ticket := newTestSession(t, gdb, u.ID)

	r := gin.New()
	r.Use(AdminAuth())
	r.GET("/api/admin/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	call := func() int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/x", nil)
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: ticket})
		r.ServeHTTP(w, req)
		return w.Code
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("在任管理员应放行 200，实际 %d", got)
	}
	// 旁路降权：只改 DB 角色、不动会话（验证判权读 DB 而非会话快照）
	if err := gdb.Exec("UPDATE users SET role = 1 WHERE id = ?", u.ID).Error; err != nil {
		t.Fatalf("降权失败: %v", err)
	}
	if got := call(); got != http.StatusForbidden {
		t.Fatalf("降权后有效会话必须 403（判权读 DB 实时角色），实际 %d", got)
	}
}

// TestUserAuthRejectsRevokedSession 断言「退出登录」真的能让票据失效：
// 这是 JWT 时代做不到的事（登出是空实现，令牌在有效期内始终可用）。
func TestUserAuthRejectsRevokedSession(t *testing.T) {
	gdb := middlewareTestDB(t)
	gin.SetMode(gin.TestMode)

	u := model.User{Username: "u-logout", PasswordHash: "x", Group: "default", Status: 1}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	ticket := newTestSession(t, gdb, u.ID)

	r := gin.New()
	r.Use(UserAuth())
	r.GET("/api/portal/me", func(c *gin.Context) { c.Status(http.StatusOK) })
	call := func() int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/portal/me", nil)
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: ticket})
		r.ServeHTTP(w, req)
		return w.Code
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("有效会话应放行 200，实际 %d", got)
	}
	if _, err := session.New(gdb).RevokeAll(context.Background(), u.ID, model.SessionRevokeLogout, 0); err != nil {
		t.Fatalf("撤销会话失败: %v", err)
	}
	if got := call(); got != http.StatusUnauthorized {
		t.Fatalf("撤销后票据必须 401，实际 %d", got)
	}
}
