package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newGzipEngine 构造只挂压缩中间件的测试引擎。
func newGzipEngine(mw gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mw)
	return r
}

// doGzipReq 发起一次带 Accept-Encoding 的请求。
func doGzipReq(t *testing.T, r *gin.Engine, method, path, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestGzipSmartDecisions 是 B1-1 的决策矩阵：只有「文本类 + 够长 + 未压过 + 未 opt-out」才压缩。
func TestGzipSmartDecisions(t *testing.T) {
	const big = 8192

	r := newGzipEngine(GzipSmart())
	r.GET("/json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"payload": strings.Repeat("x", big)})
	})
	r.GET("/text", func(c *gin.Context) {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		_, _ = c.Writer.Write([]byte(strings.Repeat("a", big)))
	})
	r.GET("/svg", func(c *gin.Context) {
		c.Header("Content-Type", "image/svg+xml")
		_, _ = c.Writer.Write([]byte("<svg>" + strings.Repeat("<rect/>", big) + "</svg>"))
	})
	r.GET("/small", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.GET("/sse", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.Write([]byte("data: hello\n\n"))
		c.Writer.Flush()
	})
	r.GET("/octet", func(c *gin.Context) {
		c.Header("Content-Type", "application/octet-stream")
		c.DataFromReader(http.StatusOK, int64(big), "application/octet-stream",
			bytes.NewReader(bytes.Repeat([]byte{0x00, 0xff}, big/2)), nil)
	})
	r.GET("/mp4", func(c *gin.Context) {
		c.DataFromReader(http.StatusOK, int64(big), "video/mp4",
			bytes.NewReader(bytes.Repeat([]byte{0x01}, big)), nil)
	})
	r.GET("/png", func(c *gin.Context) {
		c.DataFromReader(http.StatusOK, int64(big), "image/png",
			bytes.NewReader(bytes.Repeat([]byte{0x02}, big)), nil)
	})
	r.GET("/already", func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		c.Header("Content-Encoding", "gzip")
		_, _ = c.Writer.Write(bytes.Repeat([]byte("z"), big))
	})
	r.GET("/optout", func(c *gin.Context) {
		c.Header("X-No-Compress", "1")
		c.Header("Content-Type", "application/json")
		_, _ = c.Writer.Write(bytes.Repeat([]byte("o"), big))
	})
	r.GET("/nocontent", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	cases := []struct {
		name      string
		path      string
		wantGzip  bool
		wantBodyN int // >0 时断言响应体长度（透传场景）
	}{
		{name: "json 大响应压缩", path: "/json", wantGzip: true},
		{name: "text/plain 压缩", path: "/text", wantGzip: true},
		{name: "image/svg+xml 压缩", path: "/svg", wantGzip: true},
		{name: "小 json 不压缩", path: "/small", wantGzip: false},
		{name: "SSE 不压缩", path: "/sse", wantGzip: false},
		{name: "application/octet-stream 不压缩", path: "/octet", wantGzip: false, wantBodyN: big},
		{name: "video/mp4 不压缩", path: "/mp4", wantGzip: false, wantBodyN: big},
		{name: "image/png 不压缩", path: "/png", wantGzip: false, wantBodyN: big},
		{name: "上游已压缩不二次压缩", path: "/already", wantGzip: true},
		{name: "opt-out 不压缩", path: "/optout", wantGzip: false},
		{name: "204 不压缩", path: "/nocontent", wantGzip: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doGzipReq(t, r, http.MethodGet, c.path, "gzip, deflate")
			gotGzip := w.Header().Get("Content-Encoding") == "gzip"
			if gotGzip != c.wantGzip {
				t.Fatalf("Content-Encoding=%q，期望 gzip=%v", w.Header().Get("Content-Encoding"), c.wantGzip)
			}
			if c.wantBodyN > 0 && w.Body.Len() != c.wantBodyN {
				t.Fatalf("透传响应体长度 %d，期望 %d", w.Body.Len(), c.wantBodyN)
			}
			// 压缩分支必须删掉 Content-Length：原始长度与压缩体长度不等，
			// 留着就是 P0-06 的根因（客户端按旧长度读取 → 截断）。
			if c.wantGzip && c.path != "/already" {
				if cl := w.Header().Get("Content-Length"); cl != "" {
					t.Fatalf("压缩响应不得带 Content-Length，实际 %q", cl)
				}
			}
			// 透传分支必须保留原始 Content-Length（DataFromReader 场景）。
			if !c.wantGzip && c.wantBodyN > 0 {
				if cl := w.Header().Get("Content-Length"); cl == "" {
					t.Fatal("透传响应应保留 Content-Length")
				}
			}
			// opt-out 的内部约定不得泄漏给客户端。
			if v := w.Header().Get("X-No-Compress"); v != "" {
				t.Fatalf("X-No-Compress 不应出现在响应头，实际 %q", v)
			}
		})
	}
}

// TestGzipSmartBodyRoundTrip 断言压缩后可正确解回原文（防止「头对了体坏了」）。
func TestGzipSmartBodyRoundTrip(t *testing.T) {
	const payload = "多米API 适配器 gzip round trip"
	r := newGzipEngine(GzipSmart())
	r.GET("/json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"payload": strings.Repeat(payload, 200)})
	})
	w := doGzipReq(t, r, http.MethodGet, "/json", "gzip")
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("应压缩，实际 %q", w.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("响应不是合法 gzip：%v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	if !strings.Contains(string(raw), payload) {
		t.Fatalf("解压内容不含原始文本，实际长度 %d", len(raw))
	}
}

// TestGzipSmartAcceptEncoding 断言 q=0 与不含 gzip 的客户端一律不压缩。
func TestGzipSmartAcceptEncoding(t *testing.T) {
	r := newGzipEngine(GzipSmart())
	r.GET("/json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"payload": strings.Repeat("q", 4096)})
	})
	cases := []struct {
		ae   string
		want bool
	}{
		{"gzip", true},
		{"GZIP", true},
		{"deflate, gzip;q=0.5", true},
		{"*", true},
		{"gzip;q=0", false},
		{"gzip; q=0.0", false},
		{"deflate", false},
		{"identity", false},
		{"", false},
	}
	for _, c := range cases {
		w := doGzipReq(t, r, http.MethodGet, "/json", c.ae)
		got := w.Header().Get("Content-Encoding") == "gzip"
		if got != c.want {
			t.Fatalf("Accept-Encoding=%q 期望压缩=%v，实际=%v", c.ae, c.want, got)
		}
	}
}

// TestGzipSmartVary 断言压缩响应带 Vary: Accept-Encoding（缓存正确性），不重复追加。
func TestGzipSmartVary(t *testing.T) {
	r := newGzipEngine(GzipSmart())
	r.GET("/json", func(c *gin.Context) {
		c.Header("Vary", "Accept-Encoding")
		c.JSON(http.StatusOK, gin.H{"payload": strings.Repeat("v", 4096)})
	})
	w := doGzipReq(t, r, http.MethodGet, "/json", "gzip")
	if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary 应去重为 Accept-Encoding，实际 %q", got)
	}
}

// TestGzipLegacyStillWorks 断言 LD_GZIP_SMART=0 的灰度回退路径仍能编译运行
// （行为是已知有缺陷的旧实现，此处只锁「能跑」）。
func TestGzipLegacyStillWorks(t *testing.T) {
	t.Setenv("LD_GZIP_SMART", "0")
	r := newGzipEngine(Gzip())
	r.GET("/json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := doGzipReq(t, r, http.MethodGet, "/json", "gzip")
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("旧实现对所有响应都压缩，应带 Content-Encoding")
	}
}

// TestGzipSmartMinBytesEnv 断言 LD_GZIP_MIN_BYTES 可调，且 0 表示不限长度。
func TestGzipSmartMinBytesEnv(t *testing.T) {
	t.Setenv("LD_GZIP_MIN_BYTES", "0")
	r := newGzipEngine(GzipSmart())
	r.GET("/small", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := doGzipReq(t, r, http.MethodGet, "/small", "gzip")
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("阈值为 0 时小响应也应压缩")
	}
}
