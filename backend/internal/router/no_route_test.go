package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件锁死「入口链对每个协议面路径都一样」。
//
// 两条真实缺口：
//  1. 未注册的 /v1/xxx 走 NoRoute，**绕过鉴权与访问日志** —— 不用 KEY 就能免费探测
//     路径是否存在（404 与 401 的差异就是探针），出问题时连 request_id 都没有；
//  2. /dashboard/* 只挂了 TokenAuth —— 同一份 handler 在 /v1 上有 request_id、
//     401 会入调用记录，在这里什么都没有，错误体形状也与 /v1 分叉（D6）。

func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return New()
}

// TestUnmatchedProtocolPathIsAuthenticated 未注册的协议路径必须先鉴权。
func TestUnmatchedProtocolPathIsAuthenticated(t *testing.T) {
	r := newTestRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/definitely-not-exists", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未注册的 /v1 路径应要求鉴权（401），实际 %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("未注册的 /v1 路径也必须带 X-Request-Id：否则排障时连一次访问痕迹都没有")
	}
	body := w.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("错误体应为协议面形状，实际 %s", body)
	}
	if strings.Contains(body, `"error_code"`) {
		t.Fatalf("协议面不应出现控制面信封字段，实际 %s", body)
	}
}

// TestUnmatchedControlPathStays404 控制面与直链域的未知路径仍是 404，不能被协议化。
func TestUnmatchedControlPathStays404(t *testing.T) {
	r := newTestRouter()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("/nope 应为 404，实际 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"error_code"`) {
		t.Fatalf("控制面 404 应为平台信封，实际 %s", w.Body.String())
	}
}

// TestDashboardGroupHasRequestIDAndShape /dashboard 与 /v1 必须同一条链。
func TestDashboardGroupHasRequestIDAndShape(t *testing.T) {
	r := newTestRouter()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard/billing/subscription", nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无凭证应 401，实际 %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("/dashboard/* 也必须带 X-Request-Id（与 /v1 同一条链）")
	}
	body := w.Body.String()
	if strings.Contains(body, `"error_code"`) {
		t.Fatalf("/dashboard 的错误体不应是控制面信封（它与 /v1 是同一份协议面），实际 %s", body)
	}
	if !strings.Contains(body, `"message"`) {
		t.Fatalf("错误体应含 message，实际 %s", body)
	}
}
