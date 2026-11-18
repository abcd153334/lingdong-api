package middleware_test

// PublicNoStore 的单元用例。
//
// 它存在的唯一理由是「429 由限流中间件直接写出，handler 根本没运行」——
// 所以这里必须证明：**先置 no-store 的中间件**能覆盖到 handler 没参与的响应，
// 而成功响应又能被下游覆盖成 `public, max-age`。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/middleware"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
)

func TestPublicNoStoreCoversEarlyAbort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 模拟"限流中间件先拒"：它不看 handler，直接写响应。
	r.Use(middleware.PublicNoStore(), func(c *gin.Context) {
		response.FailDef(c, errs.RateLimited)
		c.Abort()
	})
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "never") })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("应 429，实际 %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("被中间件拒绝的响应必须 no-store（handler 没跑，只有这里能设），实际 %q", cc)
	}
}

func TestPublicNoStoreAllowsDownstreamOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.PublicNoStore())
	r.GET("/x", func(c *gin.Context) {
		// 成功路径由 writeETag 覆盖成可缓存。
		c.Header("Cache-Control", "public, max-age=60")
		c.String(http.StatusOK, "ok")
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Fatalf("成功响应应能覆盖成可缓存，实际 %q", cc)
	}
}
