package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRecoverKeepsStartedResponse 是 SSE 场景的回归锁。
//
// 流式响应一旦 flush 过帧，状态码与响应头就已经发给客户端了。此时若再写一段
// 统一错误 JSON，客户端解析器会在流尾读到非法数据，报"协议错误"——
// 于是排障方向被引到"上游返回了坏数据"，而真相是本地 panic。
//
// 正确处置：已经写过响应就只 Abort，不再追加任何字节。
func TestRecoverKeepsStartedResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Recover())
	r.GET("/stream", func(c *gin.Context) {
		c.Status(http.StatusOK)
		_, _ = c.Writer.WriteString("data: {\"delta\":\"hi\"}\n\n")
		c.Writer.Flush()
		panic("stream boom")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("已开始的响应不应改状态码，实际 %d", w.Code)
	}
	if body := w.Body.String(); body != "data: {\"delta\":\"hi\"}\n\n" {
		t.Fatalf("不应在流后追加错误体，实际 %q", body)
	}
}

// TestRecoverWritesEnvelopeWhenNotStarted：还没写过响应时必须给出统一错误信封
// （否则客户端拿到空 200，比 500 更难查）。
func TestRecoverWritesEnvelopeWhenNotStarted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Recover())
	r.GET("/boom", func(*gin.Context) { panic("boom") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("应为 500，实际 %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Fatal("未写响应时应回统一错误体")
	}
}
