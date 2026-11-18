package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestMetricsTokenAuth 口令正确才放行，缺/错一律 404（不是 403）。
func TestMetricsTokenAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/metrics", metricsTokenAuth("s3cret"), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	cases := []struct {
		name   string
		token  string
		status int
	}{
		{"无头 → 404", "", http.StatusNotFound},
		{"错口令 → 404", "wrong", http.StatusNotFound},
		{"正确口令 → 200", "s3cret", http.StatusOK},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/metrics", nil)
		if tc.token != "" {
			req.Header.Set("X-Metrics-Token", tc.token)
		}
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s：期望 %d，实际 %d", tc.name, tc.status, w.Code)
		}
	}
}

// TestMetricsTokenAuthConstantTimeLength 空口令也必须回 404（不因为 length 0 短路放行）。
func TestMetricsTokenAuthConstantTimeLength(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/metrics", metricsTokenAuth(""), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("口令为空时端点必须不存在，实际 %d", w.Code)
	}
}
