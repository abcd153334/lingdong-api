package router_test

// 本文件是**对外开放用户 API**（`/api/user/*`）的门禁。
//
// 它锁死的是四条边界，而不是"端点能返回 200"：
//  1. 无 Cookie 罐的客户端能完整走完 注册 → 登录 → 读自己 → 建 KEY；
//  2. token 形态登录**一个 Cookie 都不发**（发了就等于在无 CSRF 的组里造会话）；
//  3. 该组**不认 Cookie**（只带 ld_session、不带 Authorization 必须 401）；
//  4. API KEY 只能读，不能改密/管 KEY/看设备（403 session_required）。
//
// 第 3、4 条看起来"多余"，但它们正是两个最容易在后续重构里被顺手打开的口子：
// 把 authenticate() 复制过来就会认 Cookie；把 SessionTokenAuth 换成 UserTokenAuth
// 就会让一把泄露的 KEY 永久接管账号。两种改动都能通过"功能正常"的人工验收。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/router"
	"lingdong-api/internal/service/session"
)

// doAPI 发一个可带 Bearer 的 JSON 请求（body 为 nil 时发空体）。
func doAPI(t *testing.T, r *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "http://example.com"+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// dataOf 取出统一响应体里的 data 对象。
func dataOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	env := decodeEnv(t, rec)
	var out map[string]any
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatalf("data 不是对象: %s", string(env.Data))
	}
	return out
}

// expectNoAuthCookie 断言响应没有下发**会话类** Cookie。
//
// 这是开放 API 的硬约束：一旦某个端点开始发 ld_session/ld_csrf，浏览器就会在
// 这条没有 CSRF 校验的路由上获得一个会话，CSRF 防护直接失效。
//
// 允许 ld_cap（验证码 id）被清除：那不是凭证，而且验证码是一次性的，
// 注册/登录成功后清掉它才是正确行为（浏览器端同一段逻辑也这么做）。
func expectNoAuthCookie(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == session.CookieName || c.Name == session.CSRFCookieName {
			t.Fatalf("%s 不得下发会话 Cookie，实际下发了 %s", what, c.Name)
		}
	}
}

// setupUserAPI 准备一个可用的引擎与"一个无 Cookie 罐的注册+登录流程"。
func setupUserAPI(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	authFlowDB(t)
	authFlowRedis(t)
	// KEY 的明文要 AES-GCM 加密落库（key_cipher），少了这个密钥 CreateKey 直接 500。
	t.Setenv("APP_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	setupTestCaptcha(t) // 内含 LD_APP_ENV / LD_CAPTCHA_TEST_CODE + conf.Load()
	gin.SetMode(gin.TestMode)
	r := router.New()

	const password = "Passw0rd!"

	// ① 人机验证：先取图（验证接口形状），提交时用固定码。
	//    全程不带 Cookie —— 图形码的 id 在响应体里，不需要 Cookie 罐。
	if _, img := fetchCaptcha(t, r, "register"); img == "" {
		t.Fatal("验证码响应缺少 image")
	}

	// ② 注册：解走请求体，全程不带 Cookie。
	regRec := doAPI(t, r, http.MethodPost, "/api/user/register", "", map[string]any{
		"username":         "apiuser",
		"password":         password,
		"confirm_password": password,
		"agreed":           true,
		"captcha_id":       "test-id",
		"captcha_answer":   testCaptchaCode,
	})
	if regRec.Code != http.StatusOK {
		t.Fatalf("开放 API 注册应 200，实际 %d %s", regRec.Code, regRec.Body.String())
	}
	expectNoAuthCookie(t, regRec, "注册")

	// ③ 登录：只回 token。
	loginRec := doAPI(t, r, http.MethodPost, "/api/user/login", "", map[string]any{
		"account":  "apiuser",
		"password": password,
	})
	if loginRec.Code != http.StatusOK {
		t.Fatalf("开放 API 登录应 200，实际 %d %s", loginRec.Code, loginRec.Body.String())
	}
	expectNoAuthCookie(t, loginRec, "token 形态登录")
	data := dataOf(t, loginRec)
	token, _ := data["token"].(string)
	if token == "" {
		t.Fatalf("登录响应必须回 token: %s", loginRec.Body.String())
	}
	if data["token_type"] != "Bearer" {
		t.Fatalf("token_type 应为 Bearer，实际 %v", data["token_type"])
	}
	// token 必须是会话票形态（lds_）：写成 API KEY 会让客户端拿一张查不到会话的票调 /self。
	if !session.IsTicket(token) {
		t.Fatalf("登录下发的 token 必须是会话票（lds_），实际前缀不符: %.6s", token)
	}
	return r, token
}

// TestUserAPITokenFlow 覆盖 注册 → 登录 → self → 改密 → 旧票失效。
func TestUserAPITokenFlow(t *testing.T) {
	r, token := setupUserAPI(t)

	// ④ 读自己：会话票 Bearer。
	selfRec := doAPI(t, r, http.MethodGet, "/api/user/self", token, nil)
	if selfRec.Code != http.StatusOK {
		t.Fatalf("带会话票读 /self 应 200，实际 %d %s", selfRec.Code, selfRec.Body.String())
	}
	self := dataOf(t, selfRec)
	if self["username"] != "apiuser" {
		t.Fatalf("self.username 应为 apiuser，实际 %v", self["username"])
	}
	if self["auth_channel"] != "session-token" {
		t.Fatalf("auth_channel 应为 session-token，实际 %v", self["auth_channel"])
	}
	// 数组字段门禁：余额等数值字段必须在，key_count 必须是数字（nil 会变 null）。
	if _, ok := self["balance"].(float64); !ok {
		t.Fatalf("self 必须包含数值型 balance: %s", selfRec.Body.String())
	}

	// ⑤ 改密：成功后该用户**全部会话**失效，旧票立即不可用。
	pwRec := doAPI(t, r, http.MethodPost, "/api/user/password/change", token, map[string]any{
		"old_password": "Passw0rd!",
		"new_password": "NewPassw0rd!",
	})
	if pwRec.Code != http.StatusOK {
		t.Fatalf("改密应 200，实际 %d %s", pwRec.Code, pwRec.Body.String())
	}
	afterRec := doAPI(t, r, http.MethodGet, "/api/user/self", token, nil)
	if afterRec.Code != http.StatusUnauthorized {
		t.Fatalf("改密后旧票必须失效（401），实际 %d %s", afterRec.Code, afterRec.Body.String())
	}
}

// TestUserAPIIgnoresCookieSession 锁死「本组不认 Cookie」。
//
// 用一张**完全有效**的会话票塞进 ld_session Cookie：若某次重构把 authenticate()
// 复制进来，这条用例会变成 200 —— 而那个 200 意味着浏览器可以在无 CSRF 校验的
// 路由上使用登录态，是实打实的 CSRF 漏洞。
func TestUserAPIIgnoresCookieSession(t *testing.T) {
	r, token := setupUserAPI(t)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/user/self", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("只带 ld_session Cookie（无 Authorization）必须 401，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// TestUserAPIKeyTokenScope 锁死「API KEY 只能读」。
func TestUserAPIKeyTokenScope(t *testing.T) {
	r, token := setupUserAPI(t)

	// ① 用会话票建一把 KEY（这也是开放 API 的推荐姿势：
	//    会话票不落配置文件，长期运行的服务只持有 KEY）。
	keyRec := doAPI(t, r, http.MethodPost, "/api/user/keys", token, map[string]any{"name": "ci"})
	if keyRec.Code != http.StatusOK {
		t.Fatalf("带会话票建 KEY 应 200，实际 %d %s", keyRec.Code, keyRec.Body.String())
	}
	keyData := dataOf(t, keyRec)
	plain, _ := keyData["plaintext"].(string)
	if plain == "" {
		t.Fatalf("建 KEY 必须返回一次性明文: %s", keyRec.Body.String())
	}
	expectNoAuthCookie(t, keyRec, "建 KEY")

	// ② 读：KEY 可以。
	selfRec := doAPI(t, r, http.MethodGet, "/api/user/self", plain, nil)
	if selfRec.Code != http.StatusOK {
		t.Fatalf("带 API KEY 读 /self 应 200，实际 %d %s", selfRec.Code, selfRec.Body.String())
	}
	if got := dataOf(t, selfRec)["auth_channel"]; got != "api-key" {
		t.Fatalf("auth_channel 应为 api-key，实际 %v", got)
	}

	// ③ 写：KEY 一律 403 session_required（不是 401 —— 凭证有效，只是权限不够）。
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"改密", http.MethodPost, "/api/user/password/change", map[string]any{
			"old_password": "Passw0rd!", "new_password": "NewPassw0rd!",
		}},
		{"建设备列表", http.MethodGet, "/api/user/sessions", nil},
		{"再建 KEY", http.MethodPost, "/api/user/keys", map[string]any{"name": "evil"}},
		{"登出全部设备", http.MethodPost, "/api/user/sessions/revoke-all", nil},
	} {
		rec := doAPI(t, r, tc.method, tc.path, plain, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s：API KEY 必须被拒（403），实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if env := decodeEnv(t, rec); env.ErrorCode != "session_required" {
			t.Fatalf("%s：错误码应为 session_required，实际 %q", tc.name, env.ErrorCode)
		}
	}

	// ③-b 受限写面（AuthWriter）：用户自建 KEY 一律 403 scope_denied。
	//
	// 与上面的 session_required 分开断言，因为两者的"下一步该做什么"不同：
	// session_required = 请改用登录会话；scope_denied = 这把密钥没有这个权限，
	// 换会话或换托管密钥都可能解决。合成一个码会让接入方反复登出重登。
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"下单充值", http.MethodPost, "/api/user/orders", map[string]any{
			"amount": "10", "method": "wechat", "client_order_no": "ci-scope-1",
		}},
		{"取消任务", http.MethodPost, "/api/user/tasks/1/cancel", nil},
		{"删除文件", http.MethodDelete, "/api/user/files/ci-none", nil},
	} {
		rec := doAPI(t, r, tc.method, tc.path, plain, tc.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s：用户自建 KEY 必须被拒（403），实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if env := decodeEnv(t, rec); env.ErrorCode != "scope_denied" {
			t.Fatalf("%s：错误码应为 scope_denied，实际 %q", tc.name, env.ErrorCode)
		}
	}

	// ③-c 会话票能过写面守卫（不是 403）。
	//
	// 用一个不存在的任务 id：它一定会 404，而 404 证明请求已经穿过 ScopeGuard
	// 抵达 handler。这样既验证了"会话票在全权面上不被误拦"，又不碰资金链路。
	cancelRec := doAPI(t, r, http.MethodPost, "/api/user/tasks/999999/cancel", token, nil)
	if cancelRec.Code == http.StatusForbidden {
		t.Fatalf("会话票取消任务不应被守卫拒绝，实际 403 %s", cancelRec.Body.String())
	}

	// ④ 会话票仍然可以读设备列表，且当前这台被标为 current。
	sessRec := doAPI(t, r, http.MethodGet, "/api/user/sessions", token, nil)
	if sessRec.Code != http.StatusOK {
		t.Fatalf("带会话票读设备列表应 200，实际 %d %s", sessRec.Code, sessRec.Body.String())
	}
	sessions, _ := dataOf(t, sessRec)["sessions"].([]any)
	if len(sessions) == 0 {
		t.Fatalf("设备列表不该为空: %s", sessRec.Body.String())
	}
	first, _ := sessions[0].(map[string]any)
	if first["current"] != true {
		t.Fatalf("最新设备（就是本次登录）应标 current=true: %v", first)
	}

	// ⑤ 建 KEY 时关掉「允许查询账户额度」，该 KEY 就不能读 /self。
	//
	// 这个开关此前只在 /v1/account 生效（那边是省略 user 段），
	// 若开放 API 不认它，用户以为自己关掉了权限、实际上只是没在这个端点生效。
	noQuotaRec := doAPI(t, r, http.MethodPost, "/api/user/keys", token, map[string]any{
		"name": "no-quota", "allow_user_quota_query": false,
	})
	if noQuotaRec.Code != http.StatusOK {
		t.Fatalf("建 no-quota KEY 应 200，实际 %d %s", noQuotaRec.Code, noQuotaRec.Body.String())
	}
	noQuota, _ := dataOf(t, noQuotaRec)["plaintext"].(string)
	blockedRec := doAPI(t, r, http.MethodGet, "/api/user/self", noQuota, nil)
	if blockedRec.Code != http.StatusForbidden {
		t.Fatalf("关了额度查询的 KEY 读 /self 必须 403，实际 %d %s", blockedRec.Code, blockedRec.Body.String())
	}
}

// TestUserAPICredentialErrors 覆盖凭证形态的各种错误输入。
func TestUserAPICredentialErrors(t *testing.T) {
	r, _ := setupUserAPI(t)

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"没带凭证", ""},
		{"形态未知", "not-a-token"},
		{"伪造的会话票", "lds_forgedcredentialvalue"},
		{"伪造的 API KEY", "ld-abcdefghijklmnopqrstuvwxyz012345"},
	} {
		rec := doAPI(t, r, http.MethodGet, "/api/user/self", tc.token, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 应 401，实际 %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// TestUserAPILogoutRevokesCurrentTicket 覆盖 token 登出：撤销当前票且不发 Cookie。
func TestUserAPILogoutRevokesCurrentTicket(t *testing.T) {
	r, token := setupUserAPI(t)

	outRec := doAPI(t, r, http.MethodPost, "/api/user/logout", token, nil)
	if outRec.Code != http.StatusOK {
		t.Fatalf("登出应 200，实际 %d %s", outRec.Code, outRec.Body.String())
	}
	expectNoAuthCookie(t, outRec, "token 形态登出")

	if rec := doAPI(t, r, http.MethodGet, "/api/user/self", token, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("登出后该票必须失效（401），实际 %d %s", rec.Code, rec.Body.String())
	}
}
