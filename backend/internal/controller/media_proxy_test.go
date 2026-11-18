package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/mediaurl"
)

// mediaProxyEngine 装配一个只挂媒体代理路由的最小引擎。
//
// 刻意不走 router.New()：这里要验的是 handler 自己的边界行为，
// 带着鉴权/限流/采集中间件一起跑，失败时分不清是谁拦的。
func mediaProxyEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 与生产一致用 Any：只注册 GET 会让 POST 落到「无路由」，测不到方法分支。
	r.Any("/v1/media/:token", MediaProxy)
	return r
}

// upstreamCDN 起一个假的 CDN，按 Range 返回 206（模拟视频拖动/断点续传）。
func upstreamCDN(t *testing.T) *httptest.Server {
	t.Helper()
	payload := []byte("0123456789abcdef")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/expired.png":
			http.Error(w, "upstream secret detail", http.StatusForbidden)
			return
		case "/broken.png":
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case "/text.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<script>alert(1)</script>"))
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("ETag", `"v1"`)
		if rg := r.Header.Get("Range"); rg != "" {
			w.Header().Set("Content-Range", "bytes 4-7/16")
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[4:8])
			return
		}
		w.Header().Set("Content-Length", "16")
		_, _ = w.Write(payload)
	}))
}

// setupMedia 准备密钥与允许私网（httptest 跑在 127.0.0.1）。
func setupMedia(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	conf.Load()
}

func signedToken(t *testing.T, raw string, ttl time.Duration) string {
	t.Helper()
	tok, err := mediaurl.Sign(raw, ttl)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	return tok
}

// expiredToken 签一个已过期的 token（Sign 的 ttl<=0 表示「用默认值」，签不出过去的链接）。
func expiredToken(t *testing.T, raw string) string {
	t.Helper()
	tok, err := mediaurl.SignUntil(raw, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	return tok
}

func TestMediaProxyServesUpstreamBytes(t *testing.T) {
	setupMedia(t)
	srv := upstreamCDN(t)
	defer srv.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/media/"+signedToken(t, srv.URL+"/a.png", time.Hour), nil)
	mediaProxyEngine().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "0123456789abcdef" {
		t.Fatalf("响应体被改动: %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 应透传，实际 %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Fatalf("缺少缓存头: %q", cc)
	}
	if et := w.Header().Get("ETag"); et != `"v1"` {
		t.Fatalf("ETag 应透传，实际 %q", et)
	}
}

func TestMediaProxyPassesRange(t *testing.T) {
	setupMedia(t)
	srv := upstreamCDN(t)
	defer srv.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/media/"+signedToken(t, srv.URL+"/a.png", time.Hour), nil)
	req.Header.Set("Range", "bytes=4-7")
	mediaProxyEngine().ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("应 206，实际 %d", w.Code)
	}
	if w.Body.String() != "4567" {
		t.Fatalf("分段内容不对: %q", w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); cr != "bytes 4-7/16" {
		t.Fatalf("Content-Range 未透传: %q", cr)
	}
}

func TestMediaProxyHeadHasNoBody(t *testing.T) {
	setupMedia(t)
	srv := upstreamCDN(t)
	defer srv.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/v1/media/"+signedToken(t, srv.URL+"/a.png", time.Hour), nil)
	mediaProxyEngine().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD 不应有响应体: %q", w.Body.String())
	}
}

func TestMediaProxyRejectsBadTokens(t *testing.T) {
	setupMedia(t)
	srv := upstreamCDN(t)
	defer srv.Close()
	eng := mediaProxyEngine()

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"垃圾 token", "not-a-token", http.StatusNotFound},
		{"空 token", "%20", http.StatusNotFound},
		{"过期", expiredToken(t, srv.URL+"/a.png"), http.StatusGone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			eng.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/media/"+c.token, nil))
			if w.Code != c.want {
				t.Fatalf("应 %d，实际 %d", c.want, w.Code)
			}
		})
	}
}

func TestMediaProxyUpstreamFailureIsNeutral(t *testing.T) {
	setupMedia(t)
	srv := upstreamCDN(t)
	defer srv.Close()
	eng := mediaProxyEngine()

	cases := []struct {
		path string
		want int
	}{
		{"/expired.png", http.StatusNotFound},
		{"/broken.png", http.StatusBadGateway},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/media/"+signedToken(t, srv.URL+c.path, time.Hour), nil)
		eng.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Fatalf("%s 应 %d，实际 %d", c.path, c.want, w.Code)
		}
		body, _ := io.ReadAll(w.Body)
		if strings.Contains(string(body), "upstream") || strings.Contains(string(body), "secret") {
			t.Fatalf("错误响应泄漏上游信息: %q", body)
		}
	}
}

func TestMediaProxyNeverInlinesHTML(t *testing.T) {
	setupMedia(t)
	srv := upstreamCDN(t)
	defer srv.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/media/"+signedToken(t, srv.URL+"/text.html", time.Hour), nil)
	mediaProxyEngine().ServeHTTP(w, req)

	// text/html 必须降级成附件下载：能力 URL 的域名与门户同源，
	// 内联渲染一段 HTML 等于给上游一个「在我们域名下执行脚本」的机会。
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("HTML 必须按附件下载，实际 %q", cd)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("HTML 的 Content-Type 必须降级，实际 %q", ct)
	}
}

func TestMediaProxyRejectsOtherMethods(t *testing.T) {
	setupMedia(t)
	w := httptest.NewRecorder()
	mediaProxyEngine().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/media/x.y", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("应 405，实际 %d", w.Code)
	}
}
