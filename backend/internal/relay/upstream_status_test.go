package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestFailUpstreamCallerMapping 锁住「上游 4xx 不再一律变 503」这一契约。
//
// 背景：非 429 的 4xx 曾统一回 503，而主流 SDK 把 503 当服务故障**立即重试** ——
// 调用方参数写错时，一次错误请求被放大成 N 次真实出站，每次都付上游成本。
func TestFailUpstreamCallerMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		upstream   int
		wantHandle bool
		wantStatus int
		wantCode   string
	}{
		{http.StatusBadRequest, true, 400, "invalid_param"},
		{http.StatusNotFound, true, 404, "not_found"},
		{http.StatusUnprocessableEntity, true, 400, "invalid_param"},
		// 413 映射到已登记的 invalid_param，但状态码降为 400、文案里保留真实上游码：
		// errs 词表没有 413 的定义，新建裸码会绕开码表（文档与预检都挂登记的码）。
		{http.StatusRequestEntityTooLarge, true, 400, "invalid_param"},
		// 以下不属于调用方问题，必须交回原有 503 分支。
		{http.StatusUnauthorized, false, 0, ""},
		{http.StatusForbidden, false, 0, ""},
		{http.StatusInternalServerError, false, 0, ""},
		{http.StatusBadGateway, false, 0, ""},
		// 429 由 dispatch 里的独立分支处理（要带 Retry-After），这里不接管。
		{http.StatusTooManyRequests, false, 0, ""},
	}

	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

		handled := failUpstreamCaller(c, tc.upstream)
		if handled != tc.wantHandle {
			t.Fatalf("upstream=%d handled=%v，期望 %v", tc.upstream, handled, tc.wantHandle)
		}
		if !handled {
			if w.Body.Len() != 0 {
				t.Fatalf("upstream=%d 不应写响应，实际: %s", tc.upstream, w.Body.String())
			}
			continue
		}
		if w.Code != tc.wantStatus {
			t.Fatalf("upstream=%d 对外状态=%d，期望 %d", tc.upstream, w.Code, tc.wantStatus)
		}
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("upstream=%d 响应不是 JSON: %v（%s）", tc.upstream, err, w.Body.String())
		}
		if body.Error.Code != tc.wantCode {
			t.Fatalf("upstream=%d code=%q，期望 %q", tc.upstream, body.Error.Code, tc.wantCode)
		}
		// 文案必须带上真实上游码：413 被折成 400 后，这是调用方唯一的定位线索。
		if !strings.Contains(body.Error.Message, "HTTP ") {
			t.Fatalf("upstream=%d 文案未带上游码: %q", tc.upstream, body.Error.Message)
		}
		if tc.upstream == http.StatusRequestEntityTooLarge &&
			!strings.Contains(body.Error.Message, "413") {
			t.Fatalf("413 的文案必须保留真实上游码: %q", body.Error.Message)
		}
	}
}

// TestFailUpstreamCallerNoUpstreamText 确认不透传上游原文。
//
// 上游报文里有上游模型名与主机名（见 dispatch 里 setErr 的注释），
// 原样回给调用方等于把供应链交出去；完整原因走「调用记录」按 request_id 查。
func TestFailUpstreamCallerNoUpstreamText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	if !failUpstreamCaller(c, http.StatusBadRequest) {
		t.Fatal("400 应被接管")
	}
	if strings.Contains(w.Body.String(), "doubao") || strings.Contains(w.Body.String(), "internal-proxy") {
		t.Fatalf("响应里出现了上游标识: %s", w.Body.String())
	}
}
