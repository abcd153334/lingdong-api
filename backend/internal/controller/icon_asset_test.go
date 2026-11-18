package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/assets/brandicon"
)

// 本文件只覆盖**图片路由**（/api/icons/*）的 HTTP 契约。
//
// 不打库是对的：内置图标是编译进二进制的字节，上传图标的取流链路已在
// service/icon 的单测里覆盖（含 Active() 失败回落 Local）。这里要守的是
// "只有 HTTP 层才会错"的四件事：
//   1. Content-Type 必须是真图（写成 application/octet-stream，浏览器直接下载而不是显示）；
//   2. ETag + 304 必须对（错了就是每次刷新都重下 72 张图）；
//   3. 404 不能是 JSON（<img> 收到 JSON 只会显示坏图，且把错误结构暴露给了图片标签）；
//   4. HEAD 必须给 Content-Length 且无 body（前台预加载与 CDN 判活都靠它）。

func iconTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 与 router.go 的注册方式保持一致：GET 与 HEAD 都显式注册。
	r.GET("/api/icons/b/:key", BrandIconAsset)
	r.HEAD("/api/icons/b/:key", BrandIconAsset)
	r.GET("/api/icons/u/:id", UploadIconAsset)
	r.HEAD("/api/icons/u/:id", UploadIconAsset)
	return r
}

func firstBuiltinKey(t *testing.T) string {
	t.Helper()
	keys := brandicon.Keys()
	if len(keys) == 0 {
		t.Fatal("内置图标为空：go:embed 没把 icons/*.png 打进二进制")
	}
	return keys[0]
}

func TestBrandIconAssetServesPNGWithCacheHeaders(t *testing.T) {
	key := firstBuiltinKey(t)
	r := iconTestEngine()

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/icons/b/"+key, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q，期望 image/png", ct)
	}
	// 少了 nosniff，被塞进 HTML 的 polyglot 文件会按内容猜类型执行
	if v := w.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q，期望 nosniff", v)
	}
	if v := w.Header().Get("Cache-Control"); !strings.Contains(v, "max-age=") {
		t.Errorf("Cache-Control = %q，缺少 max-age", v)
	}
	if v := w.Header().Get("ETag"); v == "" {
		t.Error("缺少 ETag：没有它每次刷新都会重下整片图库")
	}
	body := w.Body.Bytes()
	if len(body) < 8 || !bytes.HasPrefix(body, []byte{0x89, 'P', 'N', 'G'}) {
		t.Errorf("响应体不是 PNG（前 8 字节 = %v）", body[:min(8, len(body))])
	}
}

func TestBrandIconAssetHonoursIfNoneMatch(t *testing.T) {
	key := firstBuiltinKey(t)
	r := iconTestEngine()

	first := httptest.NewRecorder()
	r.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/icons/b/"+key, nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("首次响应没有 ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/icons/b/"+key, nil)
	req.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotModified {
		t.Fatalf("状态码 = %d，期望 304", w.Code)
	}
	// 304 带 body 是非法响应：浏览器会丢弃它，但 CDN 可能把它当成新的实体体
	if w.Body.Len() != 0 {
		t.Errorf("304 响应不应有 body，实际 %d 字节", w.Body.Len())
	}
}

func TestBrandIconAssetHeadLengthWithoutBody(t *testing.T) {
	key := firstBuiltinKey(t)
	r := iconTestEngine()

	get := httptest.NewRecorder()
	r.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/icons/b/"+key, nil))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/api/icons/b/"+key, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD 状态码 = %d，期望 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 不应返回 body，实际 %d 字节", w.Body.Len())
	}
	// Content-Length 缺失时预加载/进度条无法判断体积
	if got, want := w.Header().Get("Content-Length"), itoa(len(get.Body.Bytes())); got != want {
		t.Errorf("HEAD Content-Length = %q，期望 %q（与 GET 一致）", got, want)
	}
}

func TestIconAssetNotFoundIsNotJSON(t *testing.T) {
	r := iconTestEngine()
	// 只放**形态非法**的 id：它们在 service 层就短路成 NotFound，不会查库，
	// 因此本文件不需要数据库。形态合法但库里没有的 id 同样 404（由
	// service/icon 的用例覆盖），这里重复一遍只会把测试绑到库上。
	cases := []struct{ name, path string }{
		{"不存在的内置 key", "/api/icons/b/no-such-brand-icon"},
		{"上传 id 含路径符号", "/api/icons/u/..%2f..%2fetc%2fpasswd"},
		{"上传 id 太短", "/api/icons/u/abc"},
		{"上传 id 含非法字符", "/api/icons/u/01jgz8z8z8z8z8z8z8z8z8z8zU"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d，期望 404", w.Code)
			}
			if body := w.Body.String(); strings.ContainsAny(body, "{<") {
				t.Errorf("404 响应带了结构化 body（%q）：<img> 只会显示坏图", body)
			}
		})
	}
}

// itoa 避免为一行转换引入 strconv（同包其他用例也这么写）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
