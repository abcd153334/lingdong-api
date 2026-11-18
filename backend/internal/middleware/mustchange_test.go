package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/model"
)

// 本文件守住 C1：临时口令（must_change_password）闸必须下沉到**所有**鉴权出口。
//
// 改造前只有门户 UserAuth 与 AdminAuth 过这道闸，而豁免是
// `HasPrefix(path, "/api/portal/auth/")` 整段放行 —— 于是：
//  1. 拿临时口令 `POST /api/user/login` 换票，再调 `/api/user/*` 或 `/v1` 就绕开了改密；
//  2. 换绑手机/邮箱、重绑 2FA（真·账号接管路径）在临时口令下全部放行。
//
// 这两个都是「功能看起来都正常」的缺陷：没有任何报错、没有任何日志差异。

// TestPasswordSetupExemptIsExactPath 守住白名单是**精确匹配**。
func TestPasswordSetupExemptIsExactPath(t *testing.T) {
	allow := []string{
		"/api/portal/me",
		"/api/portal/auth/password/change",
		"/api/portal/auth/logout",
		"/api/portal/auth/sessions/revoke-all",
		"/api/user/self",
		"/api/user/password/change",
		"/api/user/logout",
		"/api/user/sessions/revoke-all",
	}
	for _, p := range allow {
		if !passwordSetupExemptPath(p) {
			t.Fatalf("%s 必须豁免（否则临时口令账号无法自救）", p)
		}
	}

	// 这几条是**账号接管路径**：临时口令必须先失效才允许触碰。
	deny := []string{
		"/api/portal/auth/contact/code",
		"/api/portal/auth/contact/bind",
		"/api/portal/auth/mfa/setup",
		"/api/portal/auth/mfa/enable",
		"/api/portal/auth/sessions",
		"/api/portal/auth/sessions/123/revoke",
		"/api/user/keys",
		"/api/user/mfa/setup",
		"/api/user/contact/bind",
		"/api/user/orders",
		"/api/portal/usage",
		"/api/portal/me/avatar",
		"/v1/chat/completions",
	}
	for _, p := range deny {
		if passwordSetupExemptPath(p) {
			t.Fatalf("%s 不能豁免：临时口令下允许它会绕过改密（或直接接管账号）", p)
		}
	}

	// 前缀放行的回归断言：任何以 /api/portal/auth/ 开头的**未列出**路径都必须被拦。
	if passwordSetupExemptPath("/api/portal/auth/anything-new") {
		t.Fatal("白名单必须精确匹配：新增的 /api/portal/auth/* 端点默认应被拦")
	}
}

// TestMustChangeBlocked 守住判据本体：只有「待改密 + 非豁免路径」才拦。
func TestMustChangeBlocked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	blocked := func(path string, u *model.User) bool {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		return mustChangeBlocked(c, u)
	}

	pending := &model.User{MustChangePassword: true}
	normal := &model.User{MustChangePassword: false}

	if !blocked("/v1/chat/completions", pending) {
		t.Fatal("临时口令 + /v1 必须被拦")
	}
	if !blocked("/api/user/keys", pending) {
		t.Fatal("临时口令 + 账号面建 KEY 必须被拦（否则改密后 KEY 仍可用）")
	}
	if blocked("/api/user/password/change", pending) {
		t.Fatal("改密本身必须放行")
	}
	if blocked("/v1/chat/completions", normal) {
		t.Fatal("正常账号不得被拦")
	}
	if mustChangeBlocked(nil, nil) {
		t.Fatal("nil 用户不得被拦（防御性：调用方已鉴权失败）")
	}
}
