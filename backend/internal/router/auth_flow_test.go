package router_test

// 本文件是**登录态全链路**的门禁：注册 → 登录（HttpOnly Cookie）→ /me → 登出 → /me 401。
//
// 为什么必须有它：这条链路的每一环单独看都对，但接起来的失败模式是
// 「登录返回 200，之后每个接口都 401」——Cookie 属性漏一个（Secure/Path/SameSite）、
// 中间件读错 Cookie 名、CSRF 头没对上，表现完全一样，而且只有在浏览器里点才看得到。
//
// 依赖：LD_TEST_DSN（Postgres）+ 本地 Redis。任一不可用则 skip —— 不做假绿。

import (
	"bytes"
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
	"lingdong-api/internal/router"
	"lingdong-api/internal/service/session"
)

const authFlowSchema = "ld_test_router_auth"

func authFlowDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + authFlowSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+authFlowSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	// 两张 MFA 表必须一起迁移：登录第一步会读 user_mfa（判断要不要挑战），
	// 缺表时的表现是**登录 500**，而不是「当作没开 2FA」。
	if err := gdb.AutoMigrate(
		&model.User{}, &model.APIKey{}, &model.UserSession{},
		&model.AuthEvent{}, &model.AdminAudit{},
		&model.UserMFA{}, &model.UserMFABackupCode{},
		// 应用表：托管密钥签发用例要插一行 app（apps 没有 DeletedAt，迁移很轻）。
		&model.App{},
		// 公告/版本表：公开面（/api/apps/{slug}/notices|release）要真查这两张表。
		// 不迁的话缺表 → 500，用例会把它读成"接口坏了"，而实际只是脚手架没建表。
		&model.AppNotice{}, &model.AppRelease{},
		// 订单表：托管密钥的**作用域隔离**要靠订单的 app_id 才能验证 ——
		// 不迁它，那条用例只能靠读代码"相信"过滤生效。
		&model.Order{},
	); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + authFlowSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// authFlowRedis 初始化测试用 Redis（DB 15）；不可用则跳过用例。
func authFlowRedis(t *testing.T) {
	t.Helper()
	addr := os.Getenv("LD_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	prev := db.RDB
	db.InitRedis(addr, "", 15)
	if !db.RedisReady() {
		if prev != nil {
			db.RDB = prev
		}
		t.Skip("Redis 不可用，跳过认证链路集成用例")
	}
	t.Cleanup(func() {
		if db.RDB != nil {
			_ = db.RDB.FlushDB(context.Background()).Err()
			_ = db.RDB.Close()
		}
		db.RDB = prev
	})
}

type envelopeBody struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"error_code"`
	Data      json.RawMessage `json:"data"`
}

func decodeEnv(t *testing.T, rec *httptest.ResponseRecorder) envelopeBody {
	t.Helper()
	var out envelopeBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是统一错误体（HTTP %d）: %s", rec.Code, rec.Body.String())
	}
	return out
}

func cookieOf(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func postJSON(t *testing.T, r *gin.Engine, path string, body any, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://example.com"+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	// 同源来源：CSRFPublic/CSRF 都要求来源校验，浏览器一定会带 Origin。
	req.Header.Set("Origin", "http://example.com")
	if csrf != "" {
		req.Header.Set(session.CSRFHeaderName, csrf)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestAuthFlowCookieSession 覆盖「注册 → 登录 → 取用户 → 登出 → 凭证失效」。
func TestAuthFlowCookieSession(t *testing.T) {
	gdb := authFlowDB(t)
	authFlowRedis(t)

	setupTestCaptcha(t)
	gin.SetMode(gin.TestMode)

	r := router.New()

	// ② 注册
	const password = "Passw0rd!"
	regRec := postJSON(t, r, "/api/portal/auth/register", map[string]any{
		"username":         "alice",
		"password":         password,
		"confirm_password": password,
		"agreed":           true,
		"captcha_id":       captchaBody()["captcha_id"],
		"captcha_answer":   captchaBody()["captcha_answer"],
	}, nil, "")
	if regRec.Code != http.StatusOK {
		t.Fatalf("注册应 200，实际 %d %s", regRec.Code, regRec.Body.String())
	}

	// ③ 登录
	loginRec := postJSON(t, r, "/api/portal/auth/login", map[string]any{
		"account":  "alice",
		"password": password,
	}, nil, "")
	if loginRec.Code != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d %s", loginRec.Code, loginRec.Body.String())
	}
	sessCookie := cookieOf(loginRec, session.CookieName)
	csrfCookie := cookieOf(loginRec, session.CSRFCookieName)
	if sessCookie == nil || sessCookie.Value == "" {
		t.Fatalf("登录必须种 %s Cookie", session.CookieName)
	}
	// 会话票必须是 HttpOnly + SameSite=Lax：少了 HttpOnly 等于 XSS 可直接偷走凭证。
	if !sessCookie.HttpOnly {
		t.Fatal("会话 Cookie 必须 HttpOnly")
	}
	if sessCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("会话 Cookie 的 SameSite 应为 Lax，实际 %v", sessCookie.SameSite)
	}
	if csrfCookie == nil || csrfCookie.Value == "" {
		t.Fatalf("登录必须种 %s Cookie（双提交令牌）", session.CSRFCookieName)
	}
	if csrfCookie.HttpOnly {
		t.Fatal("CSRF Cookie 必须可被 JS 读取（双提交的前提）")
	}
	// 响应体不得含票据（票据只走 Cookie，进了 body 就等于进了日志/前端内存）
	if strings.Contains(loginRec.Body.String(), sessCookie.Value) {
		t.Fatal("响应体不得包含会话票据")
	}

	// ④ 带 Cookie 取当前用户
	meReq := httptest.NewRequest(http.MethodGet, "http://example.com/api/portal/me", nil)
	meReq.Header.Set("Origin", "http://example.com")
	meReq.AddCookie(sessCookie)
	meRec := httptest.NewRecorder()
	r.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("/portal/me 应 200，实际 %d %s", meRec.Code, meRec.Body.String())
	}
	var me struct {
		ID       uint64 `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(decodeEnv(t, meRec).Data, &me); err != nil {
		t.Fatalf("解析 me 失败: %v", err)
	}
	if me.Username != "alice" || me.ID == 0 {
		t.Fatalf("me 返回的用户不对: %+v", me)
	}

	// ⑤ 门户写端点缺 CSRF 头 → 403（组级 CSRF 生效，而不是逐个端点补）
	keysRec := postJSON(t, r, "/api/portal/keys", map[string]any{"name": "k1"}, []*http.Cookie{sessCookie}, "")
	if keysRec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 头的写请求必须 403，实际 %d %s", keysRec.Code, keysRec.Body.String())
	}

	// ⑥ 登出（带 CSRF）→ 撤销当前会话
	// 双提交：Cookie 里的值与请求头必须一致。浏览器两者都会自动带上，
	// 这里手工模拟，所以两个 Cookie 都得给。
	outRec := postJSON(t, r, "/api/portal/auth/logout", nil,
		[]*http.Cookie{sessCookie, csrfCookie}, csrfCookie.Value)
	if outRec.Code != http.StatusOK {
		t.Fatalf("登出应 200，实际 %d %s", outRec.Code, outRec.Body.String())
	}

	// ⑦ 旧票据立即失效（JWT 时代做不到的事：登出曾是一个空实现）
	meRec2 := httptest.NewRecorder()
	meReq2 := httptest.NewRequest(http.MethodGet, "http://example.com/api/portal/me", nil)
	meReq2.AddCookie(sessCookie)
	r.ServeHTTP(meRec2, meReq2)
	if meRec2.Code != http.StatusUnauthorized {
		t.Fatalf("登出后旧票据必须 401，实际 %d %s", meRec2.Code, meRec2.Body.String())
	}

	// ⑧ 会话行应带 logout 撤销原因（审计与对账都按它统计）
	var revoked int64
	if err := gdb.Model(&model.UserSession{}).
		Where("uid = ? AND revoked_at IS NOT NULL AND revoke_reason = ?", me.ID, model.SessionRevokeLogout).
		Count(&revoked).Error; err != nil {
		t.Fatalf("统计会话失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("应恰有 1 条 logout 撤销记录，实际 %d", revoked)
	}
}
