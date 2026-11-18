package router_test

// 本文件守「门户端关闭（纯服务端模式）」的三条边界：
//   1. 门户会话面：非管理员既登录不了、也保不住已有会话；管理员一切照旧。
//   2. 门户注册入口：/api/portal/auth/register* 关闭。
//   3. API 面不受影响：/api/user/* 的注册与登录仍只由 auth_register.mode 决定。
//
// 第 3 条是本文件最重要的断言。controller.RegisterContactCode 被门户与 API 面
// **共用**，一旦有人把门户判断写进 handler，这里会立刻红（而只测门户时看不出来）。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/router"
	"lingdong-api/internal/service/session"
)

// portalClosed 关闭门户端；用例结束必须还原 —— conf.System() 是进程级单例，
// 残留 false 会让同包其它用例随机红（-shuffle 下更明显）。
func portalClosed(t *testing.T) {
	t.Helper()
	conf.System().Set("portal_enabled", json.RawMessage("false"))
	t.Cleanup(func() { conf.System().Set("portal_enabled", json.RawMessage("true")) })
}

const portalTestPassword = "Passw0rd!"

// seedUser 通过门户注册建号（注册入口关闭的用例里必须传 allowPortal=false，
// 改走 API 面注册 —— 这正是「两个入口独立」的第一手证据）。
func seedUser(t *testing.T, r *gin.Engine, name string, viaPortal bool) {
	t.Helper()
	body := map[string]any{
		"username":         name,
		"password":         portalTestPassword,
		"confirm_password": portalTestPassword,
		"agreed":           true,
		"captcha_id":       captchaBody()["captcha_id"],
		"captcha_answer":   captchaBody()["captcha_answer"],
	}
	path := "/api/user/register"
	if viaPortal {
		path = "/api/portal/auth/register"
	}
	if rec := postJSON(t, r, path, body, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("注册 %s 应 200，实际 %d %s", name, rec.Code, rec.Body.String())
	}
}

func promoteAdmin(t *testing.T, gdb *gorm.DB, name string) {
	t.Helper()
	if err := gdb.Model(&model.User{}).Where("username = ?", name).Update("role", 0).Error; err != nil {
		t.Fatalf("提权失败: %v", err)
	}
}

func TestPortalClosedAuthPlane(t *testing.T) {
	gdb := authFlowDB(t)
	authFlowRedis(t)
	setupTestCaptcha(t)
	gin.SetMode(gin.TestMode)
	r := router.New()

	// 先在「门户开放」时建号并拿一张有效会话票，后面用来验证存量会话被立即失效。
	seedUser(t, r, "member", true)
	seedUser(t, r, "root", true)
	promoteAdmin(t, gdb, "root")

	openLogin := postJSON(t, r, "/api/portal/auth/login", map[string]any{
		"account": "member", "password": portalTestPassword,
	}, nil, "")
	if openLogin.Code != http.StatusOK {
		t.Fatalf("门户开放时应能登录，实际 %d %s", openLogin.Code, openLogin.Body.String())
	}
	memberCookie := cookieOf(openLogin, session.CookieName)
	if memberCookie == nil {
		t.Fatal("门户开放时登录必须种会话 Cookie")
	}

	portalClosed(t)

	t.Run("非管理员登录被拒且不生会话", func(t *testing.T) {
		before := countActiveSessions(t, gdb)

		login := postJSON(t, r, "/api/portal/auth/login", map[string]any{
			"account": "member", "password": portalTestPassword,
		}, nil, "")
		if login.Code != http.StatusForbidden {
			t.Fatalf("门户关闭后非管理员登录应 403，实际 %d %s", login.Code, login.Body.String())
		}
		if code := decodeEnv(t, login).ErrorCode; code != "portal_disabled" {
			t.Fatalf("错误码应为 portal_disabled，实际 %q", code)
		}
		if c := cookieOf(login, session.CookieName); c != nil && c.Value != "" {
			t.Fatal("被拒的登录不得下发会话 Cookie")
		}
		// 登录流程内部已经建好会话：必须当场撤销，否则每次尝试都留一条
		// 客户端从未拿到的「幽灵设备」，还占 max_active 名额。
		if after := countActiveSessions(t, gdb); after != before {
			t.Fatalf("被拒登录不得留下活会话：拒前 %d，拒后 %d", before, after)
		}
	})

	t.Run("管理员登录与会话照旧", func(t *testing.T) {
		login := postJSON(t, r, "/api/portal/auth/login", map[string]any{
			"account": "root", "password": portalTestPassword,
		}, nil, "")
		if login.Code != http.StatusOK {
			t.Fatalf("门户关闭后管理员应能登录，实际 %d %s", login.Code, login.Body.String())
		}
		adminCookie := cookieOf(login, session.CookieName)
		if adminCookie == nil {
			t.Fatal("管理员登录必须种会话 Cookie")
		}
		// 管理台会话探测走的就是这条：它必须活，否则管理员进门即被弹回登录。
		if me := adminGET(t, r, "/api/portal/me", adminCookie); me.Code != http.StatusOK {
			t.Fatalf("管理员的 /api/portal/me 应 200，实际 %d %s", me.Code, me.Body.String())
		}
	})

	t.Run("存量非管理员会话立即失效", func(t *testing.T) {
		me := adminGET(t, r, "/api/portal/me", memberCookie)
		if me.Code != http.StatusForbidden {
			t.Fatalf("关闭后会员会话应 403，实际 %d %s", me.Code, me.Body.String())
		}
		if code := decodeEnv(t, me).ErrorCode; code != "portal_disabled" {
			t.Fatalf("错误码应为 portal_disabled，实际 %q", code)
		}
		// 不清 Cookie 的话，浏览器会长期带一张必然失败的票，而且
		// 连「退出登录」都做不到（登出同样过会话层）。
		c := cookieOf(me, session.CookieName)
		if c == nil || (c.Value != "" && c.MaxAge >= 0) {
			t.Fatalf("被拒的会话必须清除 Cookie，实际 %+v", c)
		}
	})

	t.Run("门户注册入口关闭", func(t *testing.T) {
		reg := postJSON(t, r, "/api/portal/auth/register", map[string]any{
			"username": "another", "password": portalTestPassword, "confirm_password": portalTestPassword,
			"agreed": true, "captcha_id": "test-id", "captcha_answer": testCaptchaCode,
		}, nil, "")
		if reg.Code != http.StatusForbidden || decodeEnv(t, reg).ErrorCode != "portal_disabled" {
			t.Fatalf("门户注册应 403 portal_disabled，实际 %d %s", reg.Code, reg.Body.String())
		}
		code := postJSON(t, r, "/api/portal/auth/register/contact-code", map[string]any{
			"channel": "email", "target": "a@b.c",
		}, nil, "")
		if code.Code != http.StatusForbidden || decodeEnv(t, code).ErrorCode != "portal_disabled" {
			t.Fatalf("门户注册发码应 403 portal_disabled，实际 %d %s", code.Code, code.Body.String())
		}
	})

	t.Run("API 面注册与登录不受影响", func(t *testing.T) {
		// 同一个 handler：这里必须仍按 auth_register.mode 工作（默认 open），
		// 而不是被门户开关连带打死。
		seedUser(t, r, "apiuser", false)

		apiCode := postJSON(t, r, "/api/user/register/contact-code", map[string]any{
			"channel": "email", "target": "api@example.com",
		}, nil, "")
		if code := decodeEnv(t, apiCode).ErrorCode; code == "portal_disabled" {
			t.Fatalf("API 面发码被门户开关误伤：%d %s", apiCode.Code, apiCode.Body.String())
		}

		apiLogin := postJSON(t, r, "/api/user/login", map[string]any{
			"account": "apiuser", "password": portalTestPassword,
		}, nil, "")

		if apiLogin.Code != http.StatusOK {
			t.Fatalf("API 面登录应 200（口径：只限门户），实际 %d %s", apiLogin.Code, apiLogin.Body.String())
		}
		var out struct {
			Data struct {
				Token string `json:"token"`
			} `json:"data"`
		}
		if err := json.Unmarshal(apiLogin.Body.Bytes(), &out); err != nil || out.Data.Token == "" {
			t.Fatalf("API 面登录必须回 Bearer 票，实际 %s", apiLogin.Body.String())
		}
	})
}

func TestPortalClosedKeepsPublicSurface(t *testing.T) {
	authFlowDB(t)
	authFlowRedis(t)
	setupTestCaptcha(t)
	gin.SetMode(gin.TestMode)
	r := router.New()

	portalClosed(t)

	cfg := adminGET(t, r, "/api/portal/config", nil)
	if cfg.Code != http.StatusOK {
		t.Fatalf("门户配置必须公开可读（登录页要靠它渲染），实际 %d", cfg.Code)
	}
	var body struct {
		Data struct {
			PortalEnabled *bool `json:"portal_enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cfg.Body.Bytes(), &body); err != nil {
		t.Fatalf("配置响应不是 JSON: %v", err)
	}
	if body.Data.PortalEnabled == nil || *body.Data.PortalEnabled {
		t.Fatalf("关闭后 portal_enabled 应为 false，实际 %s", cfg.Body.String())
	}

	// 公开只读面不得被门户闸拦下。断言只认「portal_disabled」这一个错误码：
	// 本用例的测试 schema 只迁移了认证相关表，/api/pricing 这类端点会因缺表 500，
	// 那是各自包的事，不是门户开关的事。
	for _, path := range []string{"/api/portal/vendors", "/api/pricing", "/api/docs/catalog"} {
		rec := adminGET(t, r, path, nil)
		if rec.Code == http.StatusForbidden && decodeEnv(t, rec).ErrorCode == "portal_disabled" {
			t.Fatalf("%s 被门户闸误伤：%s", path, rec.Body.String())
		}
	}
}

// countActiveSessions 数「未吊销」的门户会话。
func countActiveSessions(t *testing.T, gdb *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := gdb.Model(&model.UserSession{}).Where("revoked_at IS NULL").Count(&n).Error; err != nil {
		t.Fatalf("统计会话失败: %v", err)
	}
	return n
}
