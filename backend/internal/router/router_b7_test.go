package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/metrics"
)

// TestB7RoutesRegistered 守住 B7 新增的两条路由（物理删除 + 容量体检）。
//
// 与 TestPackageRoutesRegistered 同理：价值主要在「路由树能否构建」——
// `/tasks/:id` 上同时挂 GET/DELETE，还要与 `/tasks/:id/contents/:index` 共存，
// 一旦 Gin 判成冲突就会 panic，这类问题只在启动瞬间暴露。
func TestB7RoutesRegistered(t *testing.T) {
	got := routeSet(New())
	for _, want := range []string{
		"DELETE /api/admin/tasks/:id",
		"GET /api/admin/data/capacity",
	} {
		if !got[want] {
			t.Fatalf("路由缺失: %s", want)
		}
	}
}

// TestMetricsRouteGatedByEnv /metrics 必须由 LD_METRICS_HTTP + LD_METRICS_TOKEN **同时**开启。
//
// 三个方向都要测：只测「配了能用」会漏掉「默认漏开」与「只开开关不配口令」这两种
// 最危险的回归 —— 后者等于把端点清单与渠道 id 匿名挂到公网。
func TestMetricsRouteGatedByEnv(t *testing.T) {
	t.Setenv("LD_METRICS_HTTP", "0")
	t.Setenv("LD_METRICS_TOKEN", "")
	conf.Load()
	if routeSet(New())["GET /metrics"] {
		t.Fatal("LD_METRICS_HTTP=0 时不得注册 /metrics")
	}

	// 只开开关、不配口令：端点必须不存在（否则就是匿名可读的内部拓扑）。
	t.Setenv("LD_METRICS_HTTP", "1")
	conf.Load()
	if routeSet(New())["GET /metrics"] {
		t.Fatal("未配置 LD_METRICS_TOKEN 时不得注册 /metrics")
	}

	t.Setenv("LD_METRICS_TOKEN", "test-metrics-token")
	conf.Load()
	eng := New()
	if !routeSet(eng)["GET /metrics"] {
		t.Fatal("LD_METRICS_HTTP=1 且配置口令时应注册 /metrics")
	}
	// 实际打一次：证明 Content-Type 正确、且没有数据库时采集器不会 panic（task_inflight 走查库）。
	scrape := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.Header.Set("X-Metrics-Token", "test-metrics-token")
		w := httptest.NewRecorder()
		eng.ServeHTTP(w, req)
		return w
	}
	// 口令错误必须 404（不是 403：403 等于确认端点存在）。
	badReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	badReq.Header.Set("X-Metrics-Token", "wrong")
	badW := httptest.NewRecorder()
	eng.ServeHTTP(badW, badReq)
	if badW.Code != http.StatusNotFound {
		t.Fatalf("口令错误时应 404，实际 %d", badW.Code)
	}
	first := scrape()
	if first.Code != http.StatusOK {
		t.Fatalf("/metrics 应 200，实际 %d（%s）", first.Code, first.Body.String())
	}
	if ct := first.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type 不符: %q", ct)
	}
	if !strings.Contains(first.Body.String(), "# TYPE "+metrics.NameTaskInflight) {
		t.Fatalf("指标输出缺少 task_inflight:\n%s", first.Body.String())
	}

	// 打一个**未注册**的 /v1 路径：验证「NoRoute 兜底的请求也计入网关指标」。
	// 这正是刻意把 GatewayMetrics 挂全局而不是挂 v1 组的原因，必须有用例守住。
	miss := httptest.NewRequest(http.MethodGet, "/v1/definitely-not-a-route", nil)
	eng.ServeHTTP(httptest.NewRecorder(), miss)

	second := scrape()
	body := second.Body.String()
	if !strings.Contains(body, "# TYPE "+metrics.NameGatewayRequests) {
		t.Fatalf("未注册端点未计入网关指标（应挂全局中间件）:\n%s", body)
	}
	if !strings.Contains(body, `endpoint="unmatched"`) {
		t.Fatalf("未命中路由的端点标签应为 unmatched（避免基数爆炸）:\n%s", body)
	}
}

// routeSet 把路由表摊平成 "METHOD PATH" 集合。
func routeSet(r *gin.Engine) map[string]bool {
	out := map[string]bool{}
	for _, ri := range r.Routes() {
		out[ri.Method+" "+ri.Path] = true
	}
	return out
}
