package router_test

// 二次验证（TOTP）验收用例（方案 §5「2FA」条目 36–42）。
//
// 这一组守的是「多因素到底有没有真的多出来」：
//   - 第一步（密码）绝不能发凭证 —— 发了就等于只有一因素；
//   - 动态码必须防重放（同一步只能一次），否则嗅探一次就能在 30s 内复用；
//   - 挑战的试错次数必须在 Redis 里原子累加，且绑出口 IP；
//   - 备份码一次性；
//   - 强制管理员绑定时，白名单只放「查状态 / 生成 / 确认」三条，
//     否则被拦的管理员连绑定页都进不去。
//
// 时间技巧：TOTP 每 30 秒换一步，而「绑定」「登录」「关闭」会连着用到同一个窗口。
// 这里给后续步骤统一用 **+30s** 的码（校验允许 ±1 步），既避开防重放，
// 又不依赖 sleep 30 秒。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/totp"
	"lingdong-api/internal/service/mfa"
	"lingdong-api/internal/service/session"
)

// backupCodeShape 是备份码的对外形态：5 位 + 短横线 + 5 位（字母表去掉了 0/O/1/I）。
var backupCodeShape = regexp.MustCompile(`^[2-9A-HJ-NP-Z]{5}-[2-9A-HJ-NP-Z]{5}$`)

// seedAuthConfig 把认证相关配置写进内存镜像（authcfg 的唯一读取点）。
// 不走库：这些用例验的是**网关行为**，配置来源只需可读；
// 走库还要处理 LoadSystemConfig 的时序，收益为零。
func seedAuthConfig(t *testing.T, policy string) {
	t.Helper()
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	values := map[string]json.RawMessage{
		"auth_captcha": raw(map[string]any{
			"scenes": map[string]any{"login": "after_fails"},
		}),
		"auth_login": raw(map[string]any{
			"fail_threshold": 5, "ip_fail_limit": 50, "ip_fail_window_sec": 600, "delay_max_ms": 1,
		}),
		"auth_session": raw(map[string]any{
			"absolute_ttl_sec": 604800, "idle_ttl_sec": 43200, "max_active": 10, "touch_interval_sec": 0,
		}),
		"auth_register":             raw(map[string]any{"mode": "open", "contact_verify": "off"}),
		"auth_mfa":                  raw(map[string]any{"policy": policy}),
		"auth_event_retention_days": raw(90),
	}
	conf.System().Reload(values)
	t.Cleanup(func() { seedAuthConfigDefaults() })
}

// seedAuthConfigDefaults 复位成出厂策略（optional），避免污染同包其它用例。
func seedAuthConfigDefaults() {
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	conf.System().Reload(map[string]json.RawMessage{
		"auth_captcha": raw(map[string]any{
			"scenes": map[string]any{"login": "after_fails"},
		}),
		"auth_login":                raw(map[string]any{"fail_threshold": 5, "ip_fail_limit": 50, "ip_fail_window_sec": 600, "delay_max_ms": 1}),
		"auth_session":              raw(map[string]any{"absolute_ttl_sec": 604800, "idle_ttl_sec": 43200, "max_active": 10, "touch_interval_sec": 0}),
		"auth_register":             raw(map[string]any{"mode": "open", "contact_verify": "off"}),
		"auth_mfa":                  raw(map[string]any{"policy": "optional"}),
		"auth_event_retention_days": raw(90),
	})
}

// enableMFA 走完整绑定流程（setup → enable），返回密钥与备份码。
func enableMFA(t *testing.T, r *gin.Engine, sess, csrf *http.Cookie) (secret string, backupCodes []string) {
	t.Helper()
	setupRec := postJSON(t, r, "/api/portal/auth/mfa/setup", map[string]any{"password": testPassword},
		[]*http.Cookie{sess, csrf}, csrf.Value)
	if setupRec.Code != http.StatusOK {
		t.Fatalf("mfa/setup 应 200，实际 %d %s", setupRec.Code, setupRec.Body.String())
	}
	var setup struct {
		Secret string `json:"secret"`
		URI    string `json:"otpauth_uri"`
	}
	if err := json.Unmarshal(decodeEnv(t, setupRec).Data, &setup); err != nil {
		t.Fatalf("解析 setup 响应失败: %v", err)
	}
	if setup.Secret == "" || setup.URI == "" {
		t.Fatalf("setup 必须返回 secret 与 otpauth_uri: %+v", setup)
	}
	code, err := totp.Code(setup.Secret, time.Now())
	if err != nil {
		t.Fatalf("算码失败: %v", err)
	}
	enRec := postJSON(t, r, "/api/portal/auth/mfa/enable", map[string]any{"code": code, "password": testPassword},
		[]*http.Cookie{sess, csrf}, csrf.Value)
	if enRec.Code != http.StatusOK {
		t.Fatalf("mfa/enable 应 200，实际 %d %s", enRec.Code, enRec.Body.String())
	}
	var enabled struct {
		BackupCodes []string `json:"backup_codes"`
	}
	if err := json.Unmarshal(decodeEnv(t, enRec).Data, &enabled); err != nil {
		t.Fatalf("解析 enable 响应失败: %v", err)
	}
	if len(enabled.BackupCodes) != 10 {
		t.Fatalf("enable 应一次性返回 10 个备份码，实际 %d", len(enabled.BackupCodes))
	}
	// 长度是安全参数（32^10），且分组格式要与输入归一化对得上：
	// 多一个字符少一个字符都会让「抄在纸上的码」在登录时被判成无效。
	for _, c := range enabled.BackupCodes {
		if !backupCodeShape.MatchString(c) {
			t.Fatalf("备份码形态应为 5+5 位大写（如 ABCDE-FGHJK），实际 %q", c)
		}
	}
	return setup.Secret, enabled.BackupCodes
}

// loginStage1 只做密码阶段，返回响应体（data 解析后的 map）。
func loginStage1(t *testing.T, r *gin.Engine, account string) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"account": account, "password": testPassword})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/portal/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录第一步应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &body); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	// 要求二次验证时**绝不能**发会话票：发了就等于密码通过即完成认证。
	// （没开 2FA 的账号走的就是这里的一阶段直通，此时票据必须存在。）
	if body["mfa_required"] == true {
		if c := cookieOf(rec, session.CookieName); c != nil {
			t.Fatalf("mfa_required 阶段不得下发 %s", session.CookieName)
		}
	} else if c := cookieOf(rec, session.CookieName); c == nil {
		t.Fatalf("未开二次验证的账号，第一步应直接下发 %s", session.CookieName)
	}
	return body
}

// loginWithTOTP 是「已开 2FA 的账号」完整登录（密码 + 动态码）。
//
// offset 用来错开已被消费的时间步：绑定/关闭都会占用当前步，
// 防重放会让同一步的第二次使用失败。
func loginWithTOTP(t *testing.T, r *gin.Engine, account, secret string, offset time.Duration) (*http.Cookie, *http.Cookie) {
	t.Helper()
	body := loginStage1(t, r, account)
	challenge, _ := body["challenge"].(string)
	if challenge == "" {
		t.Fatalf("%s 已开 2FA，第一步必须返回挑战", account)
	}
	code, err := totp.Code(secret, time.Now().Add(offset))
	if err != nil {
		t.Fatalf("算码失败: %v", err)
	}
	rec := postJSON(t, r, "/api/portal/auth/login/mfa", map[string]any{
		"challenge": challenge, "code": code,
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("第二步应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	sess := cookieOf(rec, session.CookieName)
	csrf := cookieOf(rec, session.CSRFCookieName)
	if sess == nil || csrf == nil {
		t.Fatal("第二步成功后必须同时下发会话票与 CSRF 令牌")
	}
	return sess, csrf
}

// TestMFABindRequiresPassword 断言「绑定 / 开启」必须重认证当前密码。
//
// 没有这一道，拿到一个已登录的浏览器就能给自己的验证器绑上去 ——
// 一次临时的会话劫持直接变成永久接管（连改密都还要过他手上的码）。
func TestMFABindRequiresPassword(t *testing.T) {
	gdb, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	registerUser(t, r, "nina")
	sess, csrf, _ := loginAs(t, r, "nina", "UA/A", nil)
	ck := []*http.Cookie{sess, csrf}

	// 1) 不带密码：400（body 里没有 password，provider 视为空串）
	if rec := postJSON(t, r, "/api/portal/auth/mfa/setup", map[string]any{}, ck, csrf.Value); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少密码应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 2) 密码错：401 invalid_credential，且不得生成密钥
	rec := postJSON(t, r, "/api/portal/auth/mfa/setup", map[string]any{"password": "wrong-password"}, ck, csrf.Value)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("密码错误应 401，实际 %d %s", rec.Code, rec.Body.String())
	}
	if n := countRows(t, gdb, "user_mfa"); n != 0 {
		t.Fatalf("重认证失败不得落库，实际 %d 条", n)
	}
	// 3) 密码对：放行
	if rec := postJSON(t, r, "/api/portal/auth/mfa/setup", map[string]any{"password": testPassword}, ck, csrf.Value); rec.Code != http.StatusOK {
		t.Fatalf("密码正确应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
}

func TestMFAEnableRejectsWrongCode(t *testing.T) {
	gdb, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	registerUser(t, r, "ivan")
	sess, csrf, _ := loginAs(t, r, "ivan", "UA/A", nil)

	setupRec := postJSON(t, r, "/api/portal/auth/mfa/setup", map[string]any{"password": testPassword},
		[]*http.Cookie{sess, csrf}, csrf.Value)
	if setupRec.Code != http.StatusOK {
		t.Fatalf("setup 应 200，实际 %d", setupRec.Code)
	}
	// 错码不得把 enabled 置真：否则「绑定」这一步就成了走过场。
	enRec := postJSON(t, r, "/api/portal/auth/mfa/enable", map[string]any{"code": "000000", "password": testPassword},
		[]*http.Cookie{sess, csrf}, csrf.Value)
	if enRec.Code != http.StatusBadRequest {
		t.Fatalf("错误动态码应 400，实际 %d %s", enRec.Code, enRec.Body.String())
	}
	var row model.UserMFA
	if err := gdb.Where("uid = (SELECT id FROM users WHERE username = ?)", "ivan").First(&row).Error; err != nil {
		t.Fatalf("应存在 user_mfa 行: %v", err)
	}
	if row.Enabled {
		t.Fatal("错误码不得把 enabled 置为 true")
	}
	// 未启用时不应有任何备份码：绕过 enable 直接拿到 10 个可用码＝凭空多出凭证。
	if n := countRows(t, gdb, "user_mfa_backup_codes"); n != 0 {
		t.Fatalf("未启用就不该生成备份码，实际 %d 条", n)
	}
}

func TestMFALoginTwoStageAndBackupCodeSingleUse(t *testing.T) {
	_, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	registerUser(t, r, "judy")
	sess, csrf, _ := loginAs(t, r, "judy", "UA/A", nil)
	secret, backupCodes := enableMFA(t, r, sess, csrf)

	// ① 密码阶段：只回挑战，不发 Cookie。
	body := loginStage1(t, r, "judy")
	if body["mfa_required"] != true {
		t.Fatalf("已开 2FA 的账号登录第一步必须要求二次验证，实际 %v", body)
	}
	challenge, _ := body["challenge"].(string)
	if challenge == "" {
		t.Fatal("第一步必须返回挑战令牌")
	}

	// ② 第二步：用一个备份码登录（同时也验证了备份码可用）。
	rec := postJSON(t, r, "/api/portal/auth/login/mfa", map[string]any{
		"challenge": challenge, "code": backupCodes[0],
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("第二步应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	sess2 := cookieOf(rec, session.CookieName)
	if sess2 == nil {
		t.Fatal("第二步成功后必须下发会话票")
	}
	if got := sessionState(t, r, sess2); got != http.StatusOK {
		t.Fatalf("第二步的会话应有效，实际 HTTP %d", got)
	}

	// ③ 同一备份码再用一次 → 拒绝（一次性）。
	body2 := loginStage1(t, r, "judy")
	challenge2, _ := body2["challenge"].(string)
	rec2 := postJSON(t, r, "/api/portal/auth/login/mfa", map[string]any{
		"challenge": challenge2, "code": backupCodes[0],
	}, nil, "")
	if rec2.Code == http.StatusOK {
		t.Fatal("备份码必须是**一次性**的，第二次使用应失败")
	}

	// ④ 动态码通道仍然可用（用 +30s 的码避开与未用完的窗口冲突）。
	code, err := totp.Code(secret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("算码失败: %v", err)
	}
	rec3 := postJSON(t, r, "/api/portal/auth/login/mfa", map[string]any{
		"challenge": challenge2, "code": code,
	}, nil, "")
	if rec3.Code != http.StatusOK {
		t.Fatalf("同一挑战用正确动态码应 200，实际 %d %s", rec3.Code, rec3.Body.String())
	}
}

func TestMFAChallengeTriesLimit(t *testing.T) {
	_, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	registerUser(t, r, "karl")
	sess, csrf, _ := loginAs(t, r, "karl", "UA/A", nil)
	secret, _ := enableMFA(t, r, sess, csrf)

	body := loginStage1(t, r, "karl")
	challenge, _ := body["challenge"].(string)

	// 前 5 次错误 → 每次都是「码不正确」（挑战还有效）。
	for i := 0; i < 5; i++ {
		rec := postJSON(t, r, "/api/portal/auth/login/mfa", map[string]any{
			"challenge": challenge, "code": "000000",
		}, nil, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次错码应 400，实际 %d %s", i+1, rec.Code, rec.Body.String())
		}
		if code := decodeEnv(t, rec).ErrorCode; code != "mfa_code_invalid" {
			t.Fatalf("第 %d 次错码应是 mfa_code_invalid，实际 %s", i+1, code)
		}
	}
	// 第 6 次即使码正确也必须失败：挑战已作废（6 位码只有 5 次机会）。
	code, _ := totp.Code(secret, time.Now())
	rec := postJSON(t, r, "/api/portal/auth/login/mfa", map[string]any{
		"challenge": challenge, "code": code,
	}, nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超过试错上限后即便码正确也应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if c := decodeEnv(t, rec).ErrorCode; c != "mfa_challenge_expired" {
		t.Fatalf("超限后应报 mfa_challenge_expired（不泄露「挑战是否存在」），实际 %s", c)
	}
}

func TestMFADisableRevokesAllSessions(t *testing.T) {
	_, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	registerUser(t, r, "laura")
	first, csrfFirst, _ := loginAs(t, r, "laura", "UA/A", nil)
	secret, backupCodes := enableMFA(t, r, first, csrfFirst)
	// 开 2FA 之后登录必须走两阶段；用 +30s 的码避开绑定已消费的那一步。
	second, _ := loginWithTOTP(t, r, "laura", secret, 30*time.Second)

	// 关闭 2FA 需要第二因素；用备份码（动态码窗口已被绑定/登录占用）。
	rec := postJSON(t, r, "/api/portal/auth/mfa/disable", map[string]any{"code": backupCodes[0]},
		[]*http.Cookie{first, csrfFirst}, csrfFirst.Value)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭 2FA 应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	// 关闭是降低安全等级：全部会话（含当前）必须失效。
	for name, c := range map[string]*http.Cookie{"A": first, "B": second} {
		if got := sessionState(t, r, c); got != http.StatusUnauthorized {
			t.Fatalf("关闭 2FA 后会话 %s 应 401，实际 HTTP %d", name, got)
		}
	}
	// 关掉之后登录回到一阶段（不再要码）。
	body := loginStage1(t, r, "laura")
	if body["mfa_required"] == true {
		t.Fatal("关闭 2FA 后登录不应再要求二次验证")
	}
}

func TestMFARequiredForAdminGate(t *testing.T) {
	gdb, r := authTestEnv(t)
	seedAuthConfig(t, "required_for_admin")
	registerUser(t, r, "mike")
	// 提为管理员（管理台判权读 DB 实时角色）。
	if err := gdb.Model(&model.User{}).Where("username = ?", "mike").Update("role", 0).Error; err != nil {
		t.Fatalf("提权失败: %v", err)
	}
	sess, csrf, _ := loginAs(t, r, "mike", "UA/A", nil)

	// 未绑定 → /admin/** 403 mfa_setup_required。
	adminReq := httptest.NewRequest(http.MethodGet, "http://example.com/api/admin/users", nil)
	adminReq.Header.Set("Origin", "http://example.com")
	adminReq.AddCookie(sess)
	adminRec := httptest.NewRecorder()
	r.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusForbidden {
		t.Fatalf("未绑定 2FA 的管理员访问管理台应 403，实际 %d %s", adminRec.Code, adminRec.Body.String())
	}
	if c := decodeEnv(t, adminRec).ErrorCode; c != "mfa_setup_required" {
		t.Fatalf("应是 mfa_setup_required（前端据此跳绑定页），实际 %s", c)
	}

	// 白名单三条必须放行，否则被拦的管理员无从绑定 —— 自己把自己锁死。
	statusReq := httptest.NewRequest(http.MethodGet, "http://example.com/api/portal/auth/mfa/status", nil)
	statusReq.Header.Set("Origin", "http://example.com")
	statusReq.AddCookie(sess)
	statusRec := httptest.NewRecorder()
	r.ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("mfa/status 在强制策略下必须放行，实际 %d", statusRec.Code)
	}

	// 绑定后 /admin/** 恢复正常。
	enableMFA(t, r, sess, csrf)
	adminRec2 := httptest.NewRecorder()
	adminReq2 := httptest.NewRequest(http.MethodGet, "http://example.com/api/admin/users", nil)
	adminReq2.Header.Set("Origin", "http://example.com")
	adminReq2.AddCookie(sess)
	r.ServeHTTP(adminRec2, adminReq2)
	if adminRec2.Code != http.StatusOK {
		t.Fatalf("绑定 2FA 后应放行管理台，实际 %d %s", adminRec2.Code, adminRec2.Body.String())
	}
}

func TestMFAStatusReportsPolicyAndBackupLeft(t *testing.T) {
	_, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	registerUser(t, r, "nora")
	sess, csrf, _ := loginAs(t, r, "nora", "UA/A", nil)

	readStatus := func() mfa.Status {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/api/portal/auth/mfa/status", nil)
		req.Header.Set("Origin", "http://example.com")
		req.AddCookie(sess)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status 应 200，实际 %d %s", rec.Code, rec.Body.String())
		}
		var st mfa.Status
		if err := json.Unmarshal(decodeEnv(t, rec).Data, &st); err != nil {
			t.Fatalf("解析 status 失败: %v", err)
		}
		return st
	}

	if st := readStatus(); st.Enabled || st.BackupCodesLeft != 0 || st.Policy != "optional" {
		t.Fatalf("未绑定时应为 {enabled:false, left:0, policy:optional}，实际 %+v", st)
	}
	_, codes := enableMFA(t, r, sess, csrf)
	if st := readStatus(); !st.Enabled || st.BackupCodesLeft != 10 {
		t.Fatalf("绑定后应有 10 个备份码，实际 %+v（codes=%d）", st, len(codes))
	}
	// 用掉一个备份码后剩余数应减 1（前端据此提示「请重新生成」）。
	rec := postJSON(t, r, "/api/portal/auth/mfa/backup-codes/regenerate",
		map[string]any{"code": codes[0]}, []*http.Cookie{sess, csrf}, csrf.Value)
	if rec.Code != http.StatusOK {
		t.Fatalf("重生成备份码应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if st := readStatus(); st.BackupCodesLeft != 10 {
		t.Fatalf("重生成后应重新有 10 个可用码，实际 %+v", st)
	}
}

// TestMFASupersedeOnlyAfterSecondFactor 守「会话固定防护」在 2FA 下的两条边界。
//
// 这两条曾经都被写错过，而且**错的方向正好相反**：
//   - 早期版本第二步拿不到第一步的票据（挑战里只存 uid/ip），旧会话永远不撤，
//     攻击者预种的票据在受害者完成 2FA 后依然有效；
//   - 修的时候又矫枉过正，在第一步（密码刚通过）就撤——于是「密码对、动态码
//     输错」的用户被顺手登出，多设备场景下表现为「另一台设备突然掉线」。
//
// 正确时机只有一个：**第二因素通过之后**。
func TestMFASupersedeOnlyAfterSecondFactor(t *testing.T) {
	_, r := authTestEnv(t)
	seedAuthConfig(t, "optional")
	registerUser(t, r, "judy")

	sess1, csrf1, _ := loginAs(t, r, "judy", "device-old", nil)
	secret, _ := enableMFA(t, r, sess1, csrf1)

	// 带着旧会话再登录一次（同浏览器重登，或攻击者预种的票据）
	raw, _ := json.Marshal(map[string]any{"account": "judy", "password": testPassword})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/portal/auth/login", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	req.AddCookie(sess1)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("第一步应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if c := cookieOf(rec, session.CookieName); c != nil {
		t.Fatal("mfa_required 阶段不得下发新会话票")
	}
	var body map[string]any
	if err := json.Unmarshal(decodeEnv(t, rec).Data, &body); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	challenge, _ := body["challenge"].(string)
	if challenge == "" {
		t.Fatal("已开 2FA 的账号第一步必须返回挑战")
	}

	// ① 密码通过、第二因素未过：旧会话必须仍然有效
	if code := sessionState(t, r, sess1); code != http.StatusOK {
		t.Fatalf("第二因素未完成时旧会话必须有效，实际 %d", code)
	}

	// ② 动态码错误：同样不能因为一次失败就踢掉旧会话
	wrong := postJSON(t, r, "/api/portal/auth/login/mfa",
		map[string]any{"challenge": challenge, "code": "000000"}, nil, "")
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("错误动态码应 400，实际 %d %s", wrong.Code, wrong.Body.String())
	}
	if code := sessionState(t, r, sess1); code != http.StatusOK {
		t.Fatalf("动态码错误时旧会话必须有效，实际 %d", code)
	}

	// ③ 动态码正确：旧会话立刻失效，新会话可用。
	//    用 +30s 的码避开防重放（绑定已经消费了当前时间步）。
	code, err := totp.Code(secret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("算码失败: %v", err)
	}
	ok := postJSON(t, r, "/api/portal/auth/login/mfa",
		map[string]any{"challenge": challenge, "code": code}, nil, "")
	if ok.Code != http.StatusOK {
		t.Fatalf("第二步应 200，实际 %d %s", ok.Code, ok.Body.String())
	}
	sess2 := cookieOf(ok, session.CookieName)
	if sess2 == nil {
		t.Fatal("第二步成功必须下发新会话票")
	}
	if c := sessionState(t, r, sess1); c != http.StatusUnauthorized {
		t.Fatalf("登录成功后旧会话必须失效（会话固定防护），实际 %d", c)
	}
	if c := sessionState(t, r, sess2); c != http.StatusOK {
		t.Fatalf("新会话应有效，实际 %d", c)
	}
}

// countRows 是「这张表有几行」的测试工具（不依赖具体模型的字段名）。
func countRows(t *testing.T, gdb *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := gdb.Raw("SELECT count(*) FROM " + table).Scan(&n).Error; err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}
