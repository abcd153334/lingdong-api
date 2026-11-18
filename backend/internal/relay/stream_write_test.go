package relay

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖二进制端点的**流式回写**（P1-30）。
// 关注点不是「能不能写出去」，而是「写出去的东西和上游一致、头正确、超限能中断且归因正确」。

func newTestGinCtx() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	return c, w
}

func TestWriteResponseStreamsBody(t *testing.T) {
	audio := bytes.Repeat([]byte("x"), 100*1024) // 超过单次 Write 的常规缓冲，确保走多轮 Copy
	c, w := newTestGinCtx()
	res := &relayResult{status: http.StatusOK, ctype: "audio/mpeg", stream: io.NopCloser(bytes.NewReader(audio))}

	n, err := writeResponse(c, res)
	if err != nil {
		t.Fatalf("流式写出失败：%v", err)
	}
	// 字节数必须等于实际写出量：TTS 的 call_io 用它记"音频多大"，
	// 记 0 会让详情出现"mime=audio/mpeg · 0 B"这种自相矛盾的行。
	if n != int64(len(audio)) {
		t.Fatalf("写出字节数应为 %d，实际 %d", len(audio), n)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type 必须回传上游声明，实际 %q", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), audio) {
		t.Fatalf("响应体与上游不一致：%d != %d 字节", w.Body.Len(), len(audio))
	}
}

func TestWriteResponseStreamOverLimit(t *testing.T) {
	// 上游给出超过上限的音频体：必须中断且错误能被网关用 errors.Is 判成上游违约。
	c, _ := newTestGinCtx()
	src := io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("y"), 64)))
	res := &relayResult{status: http.StatusOK, ctype: "audio/mpeg", stream: &cappedForTest{rc: src, limit: 16}}

	_, err := writeResponse(c, res)
	if !errors.Is(err, ErrUpstreamBodyTooLarge) {
		t.Fatalf("超限必须返回 ErrUpstreamBodyTooLarge，实际 %v", err)
	}
}

func TestWriteResponseBufferedBody(t *testing.T) {
	c, w := newTestGinCtx()
	res := &relayResult{status: http.StatusOK, ctype: "application/json", body: []byte(`{"ok":true}`)}
	if _, err := writeResponse(c, res); err != nil {
		t.Fatalf("写出失败：%v", err)
	}
	if w.Body.String() != `{"ok":true}` {
		t.Fatalf("响应体不一致：%q", w.Body.String())
	}
}

// cappedForTest 是 relay 包内的最小超限读取器（生产实现在 adapters 包，此处只验证写路径的归因）。
type cappedForTest struct {
	rc    io.ReadCloser
	limit int
	read  int
}

func (c *cappedForTest) Read(p []byte) (int, error) {
	if c.read >= c.limit {
		return 0, ErrUpstreamBodyTooLarge
	}
	if remain := c.limit - c.read; len(p) > remain {
		p = p[:remain]
	}
	n, err := c.rc.Read(p)
	c.read += n
	return n, err
}

func (c *cappedForTest) Close() error { return c.rc.Close() }
