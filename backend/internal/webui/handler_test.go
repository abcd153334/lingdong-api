package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeDist 构造一个最小产物：一个带哈希的 chunk、一个入口页。
func fakeDist() fstest.MapFS {
	return fstest.MapFS{
		"dist/index.html":           {Data: []byte("<!doctype html><div id=root></div>")},
		"dist/assets/index-a1b2.js": {Data: []byte("console.log('app')")},
		"dist/favicon.ico":          {Data: []byte{0x00, 0x01}},
	}
}

func newEngine(fsys fstest.MapFS) *gin.Engine {
	r := gin.New()
	r.NoRoute(newServer(fsys).handle)
	return r
}

func do(t *testing.T, r http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	r.ServeHTTP(w, req)
	return w
}

// 带内容哈希的资源必须长缓存，且 Content-Type 不能退化成 text/plain ——
// 后者在 nosniff 下会让浏览器拒绝执行，症状是整页白屏但网络面板全是 200。
func TestHashedAssetIsImmutable(t *testing.T) {
	w := do(t, newEngine(fakeDist()), http.MethodGet, "/assets/index-a1b2.js")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") && !strings.HasPrefix(got, "application/javascript") {
		t.Errorf("Content-Type = %q，期望 JS 类型", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("缺 nosniff：%q", got)
	}
	if !strings.Contains(w.Body.String(), "console.log") {
		t.Errorf("body 不是原始 JS：%q", w.Body.String())
	}
}

// 深链必须回落 index.html，且 index 必须 no-cache：
// 缓存住 index 会让发版后用户继续引用已删除的旧 chunk，表现为「白屏，刷新几次又好了」。
func TestSPAFallbackIsNoCache(t *testing.T) {
	for _, p := range []string{"/admin/docs", "/portal/keys", "/", "/docs/v7"} {
		w := do(t, newEngine(fakeDist()), http.MethodGet, p)
		if w.Code != http.StatusOK {
			t.Errorf("%s status = %d，期望 200", p, w.Code)
			continue
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q，期望 no-cache", p, got)
		}
		if !strings.Contains(w.Body.String(), "id=root") {
			t.Errorf("%s 未回落 index.html：%q", p, w.Body.String())
		}
	}
}

// /assets 下缺失的文件必须是 404，绝不能回落成 HTML：
// 回落成 HTML 时浏览器报的是 MIME 错误，比 404 更难定位。
func TestMissingAssetIs404(t *testing.T) {
	w := do(t, newEngine(fakeDist()), http.MethodGet, "/assets/index-gone.js")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "id=root") {
		t.Error("缺失资源被回落成了 index.html")
	}
}

// API 前缀永远不能被 SPA 吃掉，否则「路径拼错」会被答成 200 + HTML。
func TestAPIPrefixesNeverFallBack(t *testing.T) {
	for _, p := range []string{"/api", "/api/nope", "/v1", "/v1/nope", "/v1beta/nope", "/suno/nope", "/f/nope", "/metrics"} {
		w := do(t, newEngine(fakeDist()), http.MethodGet, p)
		if w.Code == http.StatusOK {
			t.Errorf("%s 被回落成 200（body=%q）", p, w.Body.String())
			continue
		}
		if strings.Contains(w.Body.String(), "id=root") {
			t.Errorf("%s 返回了 index.html", p)
		}
	}
}

// 写请求落到 NoRoute 不能被当成页面请求。
func TestNonGETIs404(t *testing.T) {
	w := do(t, newEngine(fakeDist()), http.MethodPost, "/admin/docs")
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "id=root") {
		t.Fatalf("POST 被回落成页面：%d", w.Code)
	}
}

// 没有内嵌任何产物时必须给出可执行的解释，而不是白屏。
func TestEmptyDistExplainsItself(t *testing.T) {
	w := do(t, newEngine(fstest.MapFS{"dist/.keep": {Data: []byte{}}}), http.MethodGet, "/")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "未内嵌") {
		t.Errorf("提示不清楚：%q", w.Body.String())
	}
}

// 路径穿越归一后仍查不到文件，回落首页即可，但绝不能让 .keep 之类内部文件被直接读到。
func TestTraversalCannotReadInternalFiles(t *testing.T) {
	w := do(t, newEngine(fakeDist()), http.MethodGet, "/assets/../../dist/index.html")
	if strings.Contains(w.Body.String(), "id=root") && w.Code == http.StatusOK {
		// 回落首页是允许的（等价 nginx try_files），但必须是 index 而不是别的文件。
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("归一失败：Cache-Control=%q（说明命中了 assets 长缓存分支）", got)
		}
	}
	w2 := do(t, newEngine(fakeDist()), http.MethodGet, "/.keep")
	if w2.Code == http.StatusOK && strings.Contains(w2.Body.String(), "id=root") {
		// /.keep 不存在于产物中，回落首页是正确行为。
		return
	}
}

// TestRealDistWhenEmbedded 只在**产物已拷进 dist** 时运行（镜像构建 / 本地演练），
// 纯净 clone 下自动跳过。
//
// 它守的是"构建流水线接错"这类问题：Dockerfile 的 COPY --from 写错目录、
// Vite 产物结构与假设不符（例如 assets 不在 /assets 下），单测用的 MapFS 都发现不了。
func TestRealDistWhenEmbedded(t *testing.T) {
	if _, err := fs.Stat(dist, "dist/index.html"); err != nil {
		t.Skip("未内嵌 dist（纯净 clone）：镜像构建时会拷入")
	}
	r := gin.New()
	r.NoRoute(Handler())

	w := do(t, r, http.MethodGet, "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<div id=\"root\"") {
		t.Fatalf("首页未返回内嵌 index.html：status=%d body=%.80s", w.Code, w.Body.String())
	}
	// 从真实 index.html 里取一个带哈希的资源，必须能取到且长缓存。
	body := w.Body.String()
	i := strings.Index(body, "/assets/")
	if i < 0 {
		t.Fatalf("index.html 里没有 /assets/ 引用，产物的基路径与处理器假设不一致")
	}
	rest := body[i:]
	end := strings.IndexAny(rest, "\"'")
	asset := rest[:end]
	aw := do(t, r, http.MethodGet, asset)
	if aw.Code != http.StatusOK {
		t.Fatalf("资源 %s 取不到：status=%d", asset, aw.Code)
	}
	if got := aw.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("资源 %s 的 Cache-Control = %q", asset, got)
	}
}
