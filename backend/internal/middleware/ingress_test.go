package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/wireformat"
)

// TestIngressFormatByRoute 验证「按已匹配路由判定线格式」。
//
// 这是错误体形状的唯一开关：判错的表现是 Claude 客户端显示"未知错误"，
// 而我们在日志里看到的是一次正常的 400。
func TestIngressFormatByRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var got wireformat.Format
	g := r.Group("/v1", IngressFormat())
	handler := func(c *gin.Context) {
		got = gateway.FormatOf(c)
		c.Status(http.StatusOK)
	}
	g.POST("/messages", handler)
	g.POST("/chat/completions", handler)
	g.POST("/messages/count_tokens", handler)

	cases := map[string]wireformat.Format{
		"/v1/messages":              wireformat.Claude,
		"/v1/messages/count_tokens": wireformat.Claude,
		"/v1/chat/completions":      wireformat.OpenAI,
	}
	for path, want := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		r.ServeHTTP(w, req)
		if got != want {
			t.Fatalf("%s 的格式=%q，期望 %q", path, got, want)
		}
	}
}

// TestIngressFormatDefaultsToOpenAI 未登记路由不写格式（保持默认）。
//
// NoRoute 的 404/501 也应当是默认形状：那时我们并不知道调用方想要什么协议，
// 猜错比不猜更容易误导。
func TestIngressFormatDefaultsToOpenAI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(IngressFormat())
	var got wireformat.Format
	r.POST("/v1/unknown", func(c *gin.Context) {
		got = gateway.FormatOf(c)
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/unknown", nil))
	if got != wireformat.OpenAI {
		t.Fatalf("未登记路由应回落 openai，实际 %q", got)
	}
}
