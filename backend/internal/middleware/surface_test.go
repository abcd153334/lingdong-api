package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestAPISurfaceAllowList 锁住「纯 API 节点到底保留哪些路径」。
//
// 这张表是安全边界：多一项就多一个对外面，少一项就会把正常调用打成 404。
// 任何增删都必须同时改这里与 surface.go，不允许只改一处。
func TestAPISurfaceAllowList(t *testing.T) {
	allowed := []string{
		"/api/health",
		"/metrics",
		"/v1/models",
		"/v1/chat/completions",
		"/v1/media/abc",
		"/dashboard/billing/subscription",
		"/api/user/me",
		"/api/user/keys",
		"/api/pay/notify",
		"/api/pay/epay/notify",
		"/api/icons/b/openai",
		"/api/icons/u/42",
		// 应用公开面三条：终端在登录前就要读，纯 API 节点必须提供。
		"/api/apps/default/profile",
		"/api/apps/default/notices",
		"/api/apps/default/release",
		"/api/apps/default/p/ticket/ticket", // 插件数据对外只读面（默认关闭，路径本身要放行）
		"/f/01jabc/file.png",
		"/api/diag/hold",
	}
	for _, p := range allowed {
		if !APISurfaceAllows(p) {
			t.Errorf("应放行但被拒绝: %s", p)
		}
	}

	denied := []string{
		"/",
		"/index.html",
		"/assets/index-abc.js",
		"/api/portal/config",
		"/api/portal/auth/login",
		"/api/auth/capabilities",
		"/api/admin/system/health",
		"/api/docs/catalog",
		"/api/pricing",
		"/api/health/", // 精确匹配：带尾斜杠不放行（不当成"前缀白名单"的近似写法）
		"/api/healthz", // 防止前缀逃逸
		"/v1",          // 无尾斜杠：今天也没有对应路由，保持一致
		"/f",
		"",
	}
	for _, p := range denied {
		if APISurfaceAllows(p) {
			t.Errorf("应拒绝但被放行: %s", p)
		}
	}
}

// TestAPISurfaceOnlyReturns404 验证守卫本身：被拒路径回 404，放行路径能进 handler。
func TestAPISurfaceOnlyReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(APISurfaceOnly())
	r.GET("/api/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/api/portal/config", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.Any("/v1/*p", func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		path string
		want int
	}{
		{"/api/health", http.StatusOK},
		{"/v1/models", http.StatusOK},
		{"/api/portal/config", http.StatusNotFound},
		{"/", http.StatusNotFound},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s: got %d want %d", tc.path, w.Code, tc.want)
		}
	}
}
