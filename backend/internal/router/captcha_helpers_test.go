package router_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
)

// testCaptchaCode 是 dev/test 下的固定验证码（LD_CAPTCHA_TEST_CODE）。
//
// 为什么集成测试用它而不是"从 Redis 读图上的答案"：
//   - 这一层验的是**接线**（请求字段、策略、错误码），不是密码学；
//   - 「图上的答案与服务端存的答案是否一致」由 service/captcha 的真实用例守住，
//     那里不走任何后门（直接读 Redis 比对）。
//
// 生产不会生效：conf.ValidateSecrets 拒绝在非 dev/test 带这个变量启动。
const testCaptchaCode = "12345"

// setupTestCaptcha 声明处于 dev 并启用固定验证码。
//
// 必须在 `conf.Load()` **之前**设置环境变量 —— Load 只在调用时读 env。
func setupTestCaptcha(t *testing.T) {
	t.Helper()
	t.Setenv("LD_APP_ENV", "dev")
	t.Setenv("LD_CAPTCHA_TEST_CODE", testCaptchaCode)
	conf.Load()
}

// captchaBody 是提交时必带的两字段（id 只要非空即可，答案走测试码）。
func captchaBody() map[string]any {
	return map[string]any{
		"captcha_id":     "test-id",
		"captcha_answer": testCaptchaCode,
	}
}

// fetchCaptcha 取一张图，断言响应形状是 {id, image(dataURL)}。
// 用于验证 GET /api/portal/auth/captcha 的接口契约本身。
func fetchCaptcha(t *testing.T, r *gin.Engine, scene string) (id, image string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"http://example.com/api/portal/auth/captcha?scene="+scene, nil)
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("取验证码失败: HTTP %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			ID    string `json:"id"`
			Image string `json:"image"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("验证码响应不是 JSON: %v", err)
	}
	if len(body.Data.ID) != 32 {
		t.Fatalf("id 应是 32 位 hex，实际 %q", body.Data.ID)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(body.Data.Image, prefix) {
		t.Fatalf("image 应是 PNG dataURL，实际前 40 字节: %.40s", body.Data.Image)
	}
	return body.Data.ID, body.Data.Image
}
