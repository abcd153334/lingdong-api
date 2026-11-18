package router_test

// 会话验收用例（方案 §5「会话」条目 18/19/20/23/24/25）。
//
// 这一组守的不是「接口能返回 200」，而是几条**只有多会话并发才暴露**的语义：
//
//   - 撤销必须是「按 uid 收窄」的：只按自增 id 撤销 = 任何登录用户能踢掉别人；
//   - 改密/禁用必须**立刻**生效：票据是无状态字符串，唯一的真相源是库里的 revoked_at，
//     漏一次校验就是「改完密码旧设备还能用」；
//   - 登录时携带旧会话必须换票（防会话固定）：否则攻击者可以把自己已知的票据
//     「固定」在受害者登录后的会话上；
//   - 设备列表必须是非 nil 切片且不含票据：nil 序列化成 null，前端 `.length` 直接崩。
//
// 依赖 LD_TEST_DSN + 本地 Redis；任一不可用即 skip（不做假绿）。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/router"
	"lingdong-api/internal/service/session"
)

const (
	testPassword = "Passw0rd!"
	testNewPass  = "An0ther!pass"
)

// authTestEnv 起一套「隔离 schema + 测试 Redis + 固定验证码」的环境。
func authTestEnv(t *testing.T) (*gorm.DB, *gin.Engine) {
	t.Helper()
	gdb := authFlowDB(t)
	authFlowRedis(t)

	t.Setenv("LD_APP_ENV", "dev")
	// TOTP 密钥是加密落库的：没有这个密钥时 setup 会 500（AES-GCM 要求 16/24/32 字节）。
	// 用与 dev-env.ps1 相同的值，避免「本地能跑、CI 报加密失败」。
	t.Setenv("APP_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	setupTestCaptcha(t)
	gin.SetMode(gin.TestMode)
	return gdb, router.New()
}

// registerUser 走真实注册端点（不走 service 层），确保路由与校验都在链上。
func registerUser(t *testing.T, r *gin.Engine, username string) {
	t.Helper()
	rec := postJSON(t, r, "/api/portal/auth/register", map[string]any{
		"username":         username,
		"password":         testPassword,
		"confirm_password": testPassword,
		"agreed":           true,
		"captcha_id":       captchaBody()["captcha_id"],
		"captcha_answer":   captchaBody()["captcha_answer"],
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("注册 %s 应 200，实际 %d %s", username, rec.Code, rec.Body.String())
	}
}

// loginAs 登录并返回会话票 + CSRF 令牌。
//
// ua 用来区分「两台设备」：设备列表靠 UA/IP 辨认，同 UA 的两条记录在 UI 上无法区分。
// carry 非空时模拟「浏览器带着旧会话又登录一次」，用于验证防会话固定。
func loginAs(t *testing.T, r *gin.Engine, account, ua string, carry []*http.Cookie) (*http.Cookie, *http.Cookie, *httptest.ResponseRecorder) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"account": account, "password": testPassword})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/portal/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("User-Agent", ua)
	for _, c := range carry {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录 %s 应 200，实际 %d %s", account, rec.Code, rec.Body.String())
	}
	sess := cookieOf(rec, session.CookieName)
	csrf := cookieOf(rec, session.CSRFCookieName)
	if sess == nil || sess.Value == "" || csrf == nil || csrf.Value == "" {
		t.Fatal("登录必须同时种会话票与 CSRF 令牌")
	}
	return sess, csrf, rec
}

// sessionState 读 /portal/me 的判定结果：返回 HTTP 状态码，供「会话是否还有效」断言。
func sessionState(t *testing.T, r *gin.Engine, sess *http.Cookie) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/portal/me", nil)
	req.Header.Set("Origin", "http://example.com")
	if sess != nil {
		req.AddCookie(sess)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

// listSessions 读设备列表，返回原始 data 与解析后的条目。
func listSessions(t *testing.T, r *gin.Engine, sess *http.Cookie) (json.RawMessage, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example.com/api/portal/auth/sessions", nil)
	req.Header.Set("Origin", "http://example.com")
	req.AddCookie(sess)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("设备列表应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	data := decodeEnv(t, rec).Data
	var body struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("解析设备列表失败: %v / %s", err, string(data))
	}
	return data, body.Sessions
}

func TestAuthSessionsListAndRevokeOwned(t *testing.T) {
	_, r := authTestEnv(t)
	registerUser(t, r, "dave")

	// 设备 A（Chrome）与设备 B（Safari）各登录一次。
	sessA, csrfA, _ := loginAs(t, r, "dave", "Mozilla/5.0 (Windows NT 10.0) Chrome/120.0 Safari/537.36", nil)
	sessB, _, _ := loginAs(t, r, "dave", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605.1 Version/17.0", nil)

	data, rows := listSessions(t, r, sessA)
	// 设备列表必须是真数组：nil 会变 `"sessions":null`，前端 `.length` 直接抛错整页崩。
	if string(data) == "null" || len(rows) != 2 {
		t.Fatalf("设备列表应为 2 条数组，实际 %s", string(data))
	}
	// 绝不回票据：设备列表的用途是「撤销」，不是「复制凭证」。
	for _, leak := range []string{sessA.Value, sessB.Value, "ticket", "hash"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Fatalf("设备列表不得包含 %q: %s", leak, string(data))
		}
	}
	var currentID, otherID any
	currentCount := 0
	for _, row := range rows {
		if row["current"] == true {
			currentCount++
			currentID = row["id"]
		} else {
			otherID = row["id"]
		}
	}
	if currentCount != 1 {
		t.Fatalf("必须恰好 1 条标记为当前设备，实际 %d（前端会同时给两台显示「本机」）", currentCount)
	}
	if currentID == nil || otherID == nil {
		t.Fatalf("条目缺少 id/current: %+v", rows)
	}

	// 撤销另一台设备（B）→ 只能由 A 发起，且必须带 CSRF 头。
	idNum := int64(otherID.(float64))
	revRec := postJSON(t, r, "/api/portal/auth/sessions/"+
		strconv.FormatInt(idNum, 10)+"/revoke", nil, []*http.Cookie{sessA, csrfA}, csrfA.Value)
	if revRec.Code != http.StatusOK {
		t.Fatalf("撤销他人设备应 200，实际 %d %s", revRec.Code, revRec.Body.String())
	}
	if got := sessionState(t, r, sessA); got != http.StatusOK {
		t.Fatalf("撤销 B 之后 A 必须仍有效，实际 HTTP %d", got)
	}
	if got := sessionState(t, r, sessB); got != http.StatusUnauthorized {
		t.Fatalf("被撤销的 B 必须立即 401，实际 HTTP %d", got)
	}

	// 撤销一个不属于自己的会话 id → 404，且**不得**产生副作用。
	// （用 999999：既不属于自己也不存在，两种情况的响应必须一样，否则可探测他人会话 id。）
	missRec := postJSON(t, r, "/api/portal/auth/sessions/999999/revoke", nil,
		[]*http.Cookie{sessA, csrfA}, csrfA.Value)
	if missRec.Code != http.StatusNotFound {
		t.Fatalf("撤销不存在/他人的会话应 404，实际 %d %s", missRec.Code, missRec.Body.String())
	}
	if got := sessionState(t, r, sessA); got != http.StatusOK {
		t.Fatalf("误撤销后自己必须仍有效，实际 HTTP %d", got)
	}

	// 缺 CSRF 头的写请求一律 403（组级 CSRF 生效，不是逐个端点补）。
	noCSRF := postJSON(t, r, "/api/portal/auth/sessions/"+strconv.FormatInt(idNum, 10)+"/revoke", nil,
		[]*http.Cookie{sessA, csrfA}, "")
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 头必须 403，实际 %d %s", noCSRF.Code, noCSRF.Body.String())
	}
}

func TestAuthSessionsRevokeAllIncludesCurrent(t *testing.T) {
	_, r := authTestEnv(t)
	registerUser(t, r, "erin")

	sessA, csrfA, _ := loginAs(t, r, "erin", "UA/A", nil)
	sessB, _, _ := loginAs(t, r, "erin", "UA/B", nil)

	rec := postJSON(t, r, "/api/portal/auth/sessions/revoke-all", nil,
		[]*http.Cookie{sessA, csrfA}, csrfA.Value)
	if rec.Code != http.StatusOK {
		t.Fatalf("退出全部设备应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 「退出全部」的语义包含当前设备：前端在这里跳登录页，后端必须把两个都废掉。
	for name, sess := range map[string]*http.Cookie{"A": sessA, "B": sessB} {
		if got := sessionState(t, r, sess); got != http.StatusUnauthorized {
			t.Fatalf("退出全部后 %s 必须 401，实际 HTTP %d", name, got)
		}
	}
	// 撤销原因要能与「单台撤销」区分开（对账与审计按 reason 统计）。
	var n int64
	if err := db.DB.Model(&model.UserSession{}).
		Where("revoked_at IS NOT NULL AND revoke_reason = ?", model.SessionRevokeUserRevoked).
		Count(&n).Error; err != nil {
		t.Fatalf("统计会话失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("两条会话都应以 user_revoked 落库，实际 %d", n)
	}
}

func TestAuthLoginWithExistingSessionSupersedes(t *testing.T) {
	gdb, r := authTestEnv(t)
	registerUser(t, r, "frank")

	old, _, _ := loginAs(t, r, "frank", "UA/old", nil)

	// 浏览器带着旧票据再登录一次：旧会话必须被标记 superseded —— 否则攻击者
	// 预先塞给受害者的票据会在受害者登录后继续有效（会话固定）。
	fresh, _, _ := loginAs(t, r, "frank", "UA/new", []*http.Cookie{old})

	if got := sessionState(t, r, old); got != http.StatusUnauthorized {
		t.Fatalf("携带旧会话登录后旧票必须 401，实际 HTTP %d", got)
	}
	if got := sessionState(t, r, fresh); got != http.StatusOK {
		t.Fatalf("新票必须有效，实际 HTTP %d", got)
	}

	var superseded int64
	if err := gdb.Model(&model.UserSession{}).
		Where("revoked_at IS NOT NULL AND revoke_reason = ?", model.SessionRevokeSuperseded).
		Count(&superseded).Error; err != nil {
		t.Fatalf("统计会话失败: %v", err)
	}
	if superseded != 1 {
		t.Fatalf("旧会话应以 superseded 落库 1 条，实际 %d", superseded)
	}
}

func TestAuthPasswordChangeRevokesAllSessions(t *testing.T) {
	gdb, r := authTestEnv(t)
	registerUser(t, r, "grace")

	sessA, csrfA, _ := loginAs(t, r, "grace", "UA/A", nil)
	sessB, _, _ := loginAs(t, r, "grace", "UA/B", nil)

	rec := postJSON(t, r, "/api/portal/auth/password/change", map[string]any{
		"old_password": testPassword,
		"new_password": testNewPass,
	}, []*http.Cookie{sessA, csrfA}, csrfA.Value)
	if rec.Code != http.StatusOK {
		t.Fatalf("改密应 200，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 改密的动机通常是「怀疑凭证泄露」，留任何一个旧会话等于没改。
	for name, sess := range map[string]*http.Cookie{"A(当前)": sessA, "B": sessB} {
		if got := sessionState(t, r, sess); got != http.StatusUnauthorized {
			t.Fatalf("改密后 %s 必须 401，实际 HTTP %d", name, got)
		}
	}

	// 新密码可登录、旧密码被拒。
	raw, _ := json.Marshal(map[string]any{"account": "grace", "password": testNewPass})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/portal/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("新密码应能登录，实际 %d %s", rec2.Code, rec2.Body.String())
	}

	oldRaw, _ := json.Marshal(map[string]any{"account": "grace", "password": testPassword})
	req3 := httptest.NewRequest(http.MethodPost, "http://example.com/api/portal/auth/login", bytes.NewReader(oldRaw))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Origin", "http://example.com")
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)
	if rec3.Code == http.StatusOK {
		t.Fatal("旧密码不得再能登录")
	}

	var revoked int64
	if err := gdb.Model(&model.UserSession{}).
		Where("revoked_at IS NOT NULL AND revoke_reason = ?", model.SessionRevokePasswordChanged).
		Count(&revoked).Error; err != nil {
		t.Fatalf("统计会话失败: %v", err)
	}
	if revoked != 2 {
		t.Fatalf("两条会话都应以 password_changed 落库，实际 %d", revoked)
	}
}

func TestAuthDisabledUserSessionRejected(t *testing.T) {
	gdb, r := authTestEnv(t)
	registerUser(t, r, "heidi")
	sess, _, _ := loginAs(t, r, "heidi", "UA/A", nil)

	if got := sessionState(t, r, sess); got != http.StatusOK {
		t.Fatalf("前置条件：登录后应 200，实际 HTTP %d", got)
	}

	// 管理台封禁（status=2）必须立刻生效：会话行还在，但每次请求都重查用户状态。
	if err := gdb.Model(&model.User{}).Where("username = ?", "heidi").
		Update("status", 2).Error; err != nil {
		t.Fatalf("更新用户状态失败: %v", err)
	}
	if got := sessionState(t, r, sess); got != http.StatusForbidden {
		t.Fatalf("封禁后该用户全部会话应 403，实际 HTTP %d", got)
	}
}
