package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// mustURL 解析测试用 URL（解析失败直接失败测试）。
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析 URL %q 失败: %v", raw, err)
	}
	return u
}

// TestAccessLogRequestID 验证 request_id 的注入、回写与透传（B1-3）。
//
// 「回写请求头」这一点尤其关键：relay/task 全链路都用 c.GetHeader("X-Request-Id")
// 取它落库，只在响应头里给客户端设值是不够的。
func TestAccessLogRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("客户端传合法 id 时沿用", func(t *testing.T) {
		r := gin.New()
		r.Use(AccessLog())
		var seen string
		r.GET("/v1/x", func(c *gin.Context) {
			seen = c.GetHeader("X-Request-Id")
			c.Status(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
		req.Header.Set("X-Request-Id", "client-abc-123")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if seen != "client-abc-123" {
			t.Fatalf("上下文内 request_id = %q，期望沿用客户端值", seen)
		}
		if got := w.Header().Get("X-Request-Id"); got != "client-abc-123" {
			t.Fatalf("响应头 request_id = %q", got)
		}
	})

	t.Run("客户端未传时生成 32 位 hex", func(t *testing.T) {
		r := gin.New()
		r.Use(AccessLog())
		var seen string
		r.GET("/v1/x", func(c *gin.Context) {
			seen = c.GetHeader("X-Request-Id")
			c.Status(http.StatusOK)
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/x", nil))

		if len(seen) != 32 {
			t.Fatalf("生成的 request_id 长度 = %d，期望 32（%q）", len(seen), seen)
		}
		if got := w.Header().Get("X-Request-Id"); got != seen {
			t.Fatalf("响应头与请求头不一致：%q vs %q", got, seen)
		}
	})

	t.Run("非法 id 被替换（防日志注入）", func(t *testing.T) {
		r := gin.New()
		r.Use(AccessLog())
		var seen string
		r.GET("/v1/x", func(c *gin.Context) {
			seen = c.GetHeader("X-Request-Id")
			c.Status(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
		// 含换行：直接进日志会把一行拆成两行，伪造出额外的日志条目。
		req.Header.Set("X-Request-Id", "abc\ndef")
		r.ServeHTTP(httptest.NewRecorder(), req)

		if strings.ContainsAny(seen, "\r\n") || seen == "" {
			t.Fatalf("非法 request_id 未被替换：%q", seen)
		}
	})
}

// TestScrubPath 验证访问日志不记录可能携带密钥的 query。
func TestScrubPath(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"/v1/chat/completions", "/v1/chat/completions"},
		// 打码而不是删键：保留「这里曾有参数」，排障时能与「客户端没传」区分开。
		{"/v1/models?key=sk-secret", "/v1/models?key=REDACTED"},
		{"/v1/models?key=sk-secret&model=gpt-4o", "/v1/models?key=REDACTED&model=gpt-4o"},
		{"/v1/models?api_key=x&token=y&page=2", "/v1/models?api_key=REDACTED&page=2&token=REDACTED"},
		{"/v1/models?page=2", "/v1/models?page=2"},
		// 大写写法能过鉴权（取钥用 EqualFold），脱敏若区分大小写就会明文落日志。
		{"/v1/models?Key=sk-secret", "/v1/models?Key=REDACTED"},
	}
	for _, c := range cases {
		got := scrubPath(mustURL(t, c.raw))
		if got != c.want {
			t.Fatalf("scrubPath(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

// TestValidRequestID 校验外部传入 id 的形态约束。
func TestValidRequestID(t *testing.T) {
	cases := map[string]bool{
		"":                      false,
		"abc":                   true,
		"abc-123_XYZ":           true,
		"abc def":               false, // 空格
		"abc\ndef":              false, // 换行
		strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false,
	}
	for in, want := range cases {
		if got := validRequestID(in); got != want {
			t.Fatalf("validRequestID(%q) = %v，期望 %v", in, got, want)
		}
	}
}
