package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/wireformat"
)

// 本文件锁死「panic 的错误体也必须遵守协议面形状」。
//
// 回归的真实影响：Recover 是全局中间件，原来无条件调 response.FailDef，
// 于是 /v1 上任何 panic 都返回控制面信封 {code,message,error_code,data}。
// Claude/Gemini SDK 按各自形状解析错误，拿到这个只会显示"未知错误"，
// 用户与客服都拿不到任何可定位信息 —— 而真相是服务端 500。

// TestRecoverUsesProtocolShape 中间件已写入线格式时，按该格式写错误体。
func TestRecoverUsesProtocolShape(t *testing.T) {
	cases := []struct {
		name   string
		format wireformat.Format
		want   []string // 必须出现的片段
		reject []string // 必须不出现的片段
	}{
		{
			name:   "claude",
			format: wireformat.Claude,
			want:   []string{`"type":"error"`, `"error"`},
			reject: []string{`"error_code"`, `"code":500`},
		},
		{
			name:   "gemini",
			format: wireformat.Gemini,
			want:   []string{`"error"`, `"code":500`},
			reject: []string{`"error_code"`},
		},
		{
			name:   "newapi",
			format: wireformat.NewAPI,
			want:   []string{`"code"`, `"message"`},
			reject: []string{`"error_code"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(Recover())
			r.GET("/v1/boom", func(ctx *gin.Context) {
				gateway.SetFormat(ctx, c.format)
				panic("boom")
			})

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/boom", nil))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("应为 500，实际 %d", w.Code)
			}
			body := w.Body.String()
			for _, s := range c.want {
				if !strings.Contains(body, s) {
					t.Fatalf("%s 形状缺少 %q：%s", c.name, s, body)
				}
			}
			for _, s := range c.reject {
				if strings.Contains(body, s) {
					t.Fatalf("%s 形状不应包含 %q（那是控制面信封）：%s", c.name, s, body)
				}
			}
		})
	}
}

// TestRecoverProtocolPathPanicBeforeFormat panic 发生在 IngressFormat 之前时，
// context 里还没有线格式，必须按路径判面 —— 否则 /v1/messages 的 panic 会退回
// OpenAI 形状，Claude SDK 一样读不懂。
func TestRecoverProtocolPathPanicBeforeFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Recover())
	r.POST("/v1/messages", func(*gin.Context) { panic("boom before format") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("应为 500，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("/v1/messages 的 panic 应为 Claude 形状，实际 %s", body)
	}
	if strings.Contains(body, `"error_code"`) {
		t.Fatalf("协议面不应出现控制面信封字段：%s", body)
	}
}

// TestRecoverKeepsControlEnvelope 控制面（/api）仍必须是控制面信封。
func TestRecoverKeepsControlEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Recover())
	r.GET("/api/boom", func(*gin.Context) { panic("boom") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/boom", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("应为 500，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"error_code"`) || !strings.Contains(body, `"data"`) {
		t.Fatalf("控制面应为 {code,message,error_code,data} 信封，实际 %s", body)
	}
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("控制面不应出现协议面形状：%s", body)
	}
}
