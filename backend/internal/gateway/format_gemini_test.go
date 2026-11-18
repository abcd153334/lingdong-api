package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/wireformat"
)

// geminiErrorOf 用给定格式与错误码写一次错误响应，返回解析后的 error 对象。
func geminiErrorOf(t *testing.T, def errs.Def) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent", nil)
	SetFormat(c, wireformat.Gemini)
	FailDef(c, def)

	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %s", w.Body.String())
	}
	return w.Code, body.Error
}

// TestGeminiErrorBody 守住 Gemini 错误体的两个硬差别：
// code 是**整数 HTTP 状态**（不是业务码字符串），status 是 Google 的枚举。
//
// 把业务码塞进 code 会让 google-genai 的 APIError.code 变成
// "insufficient_user_quota"，SDK 里所有 `if code == 429` 的判断全部失效。
func TestGeminiErrorBody(t *testing.T) {
	cases := []struct {
		def    errs.Def
		status int
		want   string
	}{
		{errs.InvalidParam, http.StatusBadRequest, "INVALID_ARGUMENT"},
		{errs.Unauthorized, http.StatusUnauthorized, "UNAUTHENTICATED"},
		{errs.NotFound, http.StatusNotFound, "NOT_FOUND"},
		{errs.ModelNotAllowed, http.StatusForbidden, "PERMISSION_DENIED"},
		{errs.RateLimited, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED"},
		{errs.Internal, http.StatusInternalServerError, "INTERNAL"},
	}
	for _, c := range cases {
		code, e := geminiErrorOf(t, c.def)
		if code != c.status {
			t.Errorf("%s: HTTP %d，期望 %d", c.def.Code, code, c.status)
		}
		if e["status"] != c.want {
			t.Errorf("%s: status=%v，期望 %s", c.def.Code, e["status"], c.want)
		}
		if got, ok := e["code"].(float64); !ok || int(got) != code {
			t.Errorf("%s: code 必须是整数 HTTP 状态，实际 %v", c.def.Code, e["code"])
		}
		if msg, _ := e["message"].(string); msg == "" {
			t.Errorf("%s: message 不能为空", c.def.Code)
		}
	}
}

// TestGeminiQuotaIsResourceExhausted 守住「先按业务码、再按状态码」的顺序。
//
// 额度不足在协议面是 403；若先按状态码判就会落成 PERMISSION_DENIED，
// Gemini SDK 会去查 IAM 权限而不是提示充值 —— 与 Claude 面把额度不足
// 归 api_error 的教训同源。
func TestGeminiQuotaIsResourceExhausted(t *testing.T) {
	code, e := geminiErrorOf(t, errs.InsufficientBalance)
	if code != http.StatusForbidden {
		t.Fatalf("额度不足的对外状态应为 403，实际 %d", code)
	}
	if e["status"] != "RESOURCE_EXHAUSTED" {
		t.Fatalf("额度不足必须是 RESOURCE_EXHAUSTED，实际 %v", e["status"])
	}
}
