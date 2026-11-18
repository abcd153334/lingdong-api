package router_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingdong-api/internal/conf"
)

// setAuthPolicy 直接改内存里的两个策略键（不落库）。
// 这两个键是 Required 的唯一输入：auth_captcha.scenes.login 与 auth_login.fail_threshold。
func setAuthPolicy(t *testing.T, login string, failThreshold int) {
	t.Helper()
	captchaRaw, err := json.Marshal(map[string]any{
		"scenes": map[string]any{"login": login},
	})
	if err != nil {
		t.Fatalf("构造 auth_captcha 失败: %v", err)
	}
	// delay_max_ms 压到 1：达阈值后服务端会真实 sleep，默认 800ms 会让用例变慢。
	loginRaw, err := json.Marshal(map[string]any{
		"fail_threshold": failThreshold, "delay_max_ms": 1,
		"ip_fail_limit": 200, "ip_fail_window_sec": 600,
		"backoff_seconds": []int{60, 300, 900},
	})
	if err != nil {
		t.Fatalf("构造 auth_login 失败: %v", err)
	}
	conf.System().Set("auth_captcha", captchaRaw)
	conf.System().Set("auth_login", loginRaw)
}

// 注册必须带图形验证码：不带 → 400 captcha_required；带 → 200。
func TestRegisterRequiresCaptcha(t *testing.T) {
	_, r := authTestEnv(t)
	setAuthPolicy(t, "after_fails", 3)

	body := map[string]any{
		"username": "nocap", "password": testPassword,
		"confirm_password": testPassword, "agreed": true,
	}
	rec := postJSON(t, r, "/api/portal/auth/register", body, nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺验证码应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "captcha_required") {
		t.Fatalf("应返回 captcha_required，实际 %s", rec.Body.String())
	}

	// 带上固定码即可通过。
	body["username"] = "withcap"
	body["captcha_id"] = "test-id"
	body["captcha_answer"] = testCaptchaCode
	rec = postJSON(t, r, "/api/portal/auth/register", body, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("带验证码应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 登录策略 off：失败再多次也只报"账号或密码错误"，绝不能出现 captcha_required。
// （历史缺陷：达阈值后无条件返回 captcha_required，而 Required 在 off 时恒 false，
// 前端永远不亮验证码 → 用户被永久要求"完成验证"却无从完成。）
func TestLoginOffNeverAsksCaptcha(t *testing.T) {
	_, r := authTestEnv(t)
	registerUser(t, r, "offuser")
	setAuthPolicy(t, "off", 3)

	for i := 0; i < 6; i++ {
		rec := postJSON(t, r, "/api/portal/auth/login", map[string]any{
			"account": "offuser", "password": "definitely-wrong",
		}, nil, "")
		body := rec.Body.String()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401，实际 %d %s", i+1, rec.Code, body)
		}
		if strings.Contains(body, "captcha_required") {
			t.Fatalf("off 策略下第 %d 次失败出现了 captcha_required（前端不会亮验证码，用户会被卡死）: %s",
				i+1, body)
		}
		if !strings.Contains(body, "invalid_credential") {
			t.Fatalf("第 %d 次失败应是 invalid_credential，实际 %s", i+1, body)
		}
	}
}

// 登录策略 after_fails：达阈值（= auth_login.fail_threshold）后要求验证码，
// 带上正确的答案后能登录（只多一步，不需要重试两遍）。
func TestLoginAfterFailsAsksCaptcha(t *testing.T) {
	_, r := authTestEnv(t)
	registerUser(t, r, "afuser")
	setAuthPolicy(t, "after_fails", 3)

	// 前 2 次：普通凭据错误。
	for i := 0; i < 2; i++ {
		rec := postJSON(t, r, "/api/portal/auth/login", map[string]any{
			"account": "afuser", "password": "wrong",
		}, nil, "")
		if strings.Contains(rec.Body.String(), "captcha_required") {
			t.Fatalf("阈值 3，第 %d 次不该要求验证码: %s", i+1, rec.Body.String())
		}
	}
	// 第 3 次：达阈值，要求验证码。
	rec := postJSON(t, r, "/api/portal/auth/login", map[string]any{
		"account": "afuser", "password": "wrong",
	}, nil, "")
	if !strings.Contains(rec.Body.String(), "captcha_required") {
		t.Fatalf("第 3 次失败应要求验证码，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 第 4 次：带验证码 + 正确密码 → 通过。
	body := map[string]any{
		"account": "afuser", "password": testPassword,
		"captcha_id": "test-id", "captcha_answer": testCaptchaCode,
	}
	rec = postJSON(t, r, "/api/portal/auth/login", body, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("带正确验证码应登录成功，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 验证码接口本身的契约：JSON {id, image(dataURL)}、no-store、非法 scene 400。
func TestCaptchaEndpointContract(t *testing.T) {
	_, r := authTestEnv(t)

	id, image := fetchCaptcha(t, r, "register")
	if id == "" || image == "" {
		t.Fatal("id/image 不能为空")
	}
	req := httptest.NewRequest(http.MethodGet,
		"http://example.com/api/portal/auth/captcha?scene=nope", nil)
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 scene 应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
}
