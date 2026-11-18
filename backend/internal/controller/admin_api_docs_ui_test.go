package controller

// 本文件守的是**Swagger UI 视图的三个安全边界**，不是"页面能不能开"。
//
// 三条锁各自对应一种真实后果，缺一条都能被利用：
//
//  1. 不读 query。Swagger UI 原生支持 `?url=` 让渲染器去拉任意 spec。
//     保留它 = 一个能让人在管理台域下把浏览器指向外部文档的入口
//     （自研页是无害的，但这一页本质是一个会执行远程内容的渲染器）。
//     断言方式：带上 `?url=https://evil/...` 请求，返回的 HTML 必须与不带时**逐字节相同**。
//
//  2. spec 地址是编译期常量且为相对路径。断言两件事：注入后出现
//     `url: "../openapi.json"`，且 HTML 里不存在任何 `http://` / `https://`
//     —— 出现即意味着某个字段能指向站外。
//
//  3. 静态资源只认 4 个名字。目录穿越必须 404 而不是 200，
//     否则 `go:embed` 里的 LICENSE、或者将来误提交的任意文件都会被读出去。
//     注意断言的是"不是 200"而不是"等于 404"：gin 的 cleanPath / 301
//     在不同路径形态下会先做一次规范化，只要**不吐出文件内容**就算守住。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/controller/swaggerui"
)

// serveUI 在一条独立的引擎上挂 catch-all 路由（与 router.go 的注册方式一致）。
//
// 为什么不像其他用例那样用 runHandler 直接绑死一条路径：
// catch-all 的匹配行为本身就是被测对象之一（`/ui` 的 301、`/ui/` 的兜底），
// 用真实的路由树才能测到。
func serveUI(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	eng := gin.New()
	eng.GET(APIDocsPathSwaggerAsset, AdminAPIDocsUIAsset)
	req := httptest.NewRequest(http.MethodGet, "http://example.com"+target, nil)
	rec := httptest.NewRecorder()
	eng.ServeHTTP(rec, req)
	return rec
}

// TestSwaggerUIIndexIsHTML 入口页是 HTML，且所有资源引用都是**相对**的。
func TestSwaggerUIIndexIsHTML(t *testing.T) {
	rec := serveUI(t, "/api/admin/api-docs/ui/")
	if rec.Code != http.StatusOK {
		t.Fatalf("入口页应 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type 应为 text/html，实际 %q", ct)
	}
	html := rec.Body.String()

	// spec 地址必须注入进去了（否则前端去拉字面量 %%SPEC_URL%%）。
	if strings.Contains(html, "%%SPEC_URL%%") {
		t.Error("spec 地址未注入，页面会去请求字面量占位符")
	}
	if !strings.Contains(html, `url: "../openapi.json"`) {
		t.Error(`spec 地址应为相对路径 "../openapi.json"`)
	}
	// 资源引用必须相对（`./x` 或同名），否则子路径部署下静态资源全 404。
	if !strings.Contains(html, `src="./swagger-ui-bundle.js"`) {
		t.Error("bundle 应以 ./ 相对引入")
	}
	// 任何绝对 URL 出现在 HTML 里都说明某个字段能指向站外。
	for _, bad := range []string{"http://", "https://"} {
		if strings.Contains(html, bad) {
			t.Errorf("入口页不得出现 %s（说明有字段能指向站外）", bad)
		}
	}
}

// TestSwaggerUIIgnoresQuery 带 `?url=` 的请求必须与不带时逐字节相同。
//
// 这一条比"HTML 里没有 ?url= 字样"更强：它验的是**行为**，
// 将来即使有人在 index.html 里加了一段读 query 的脚本，这里也会红。
func TestSwaggerUIIgnoresQuery(t *testing.T) {
	plain := serveUI(t, "/api/admin/api-docs/ui/")
	evil := serveUI(t, "/api/admin/api-docs/ui/?url=https://evil.example/spec.json")
	if plain.Code != http.StatusOK || evil.Code != http.StatusOK {
		t.Fatalf("两次都应 200，实际 %d / %d", plain.Code, evil.Code)
	}
	if plain.Body.String() != evil.Body.String() {
		t.Fatal("带 ?url= 的响应与不带时不同：说明页面接受了外部 spec 地址")
	}
	if strings.Contains(evil.Body.String(), "evil.example") {
		t.Fatal("外部地址被写进了响应体")
	}
}

// TestSwaggerUIAssetsServed 4 个合法资源都能取到，且缓存策略区分入口页与资源。
func TestSwaggerUIAssetsServed(t *testing.T) {
	cases := []struct{ path, mime string }{
		{"/api/admin/api-docs/ui/swagger-ui.css", "text/css"},
		{"/api/admin/api-docs/ui/swagger-ui-bundle.js", "application/javascript"},
		{"/api/admin/api-docs/ui/LICENSE", "application/octet-stream"},
	}
	for _, tc := range cases {
		rec := serveUI(t, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 应 200，实际 %d", tc.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.mime) {
			t.Errorf("%s 的 Content-Type 应为 %s*，实际 %q", tc.path, tc.mime, ct)
		}
		if len(rec.Body.Bytes()) == 0 {
			t.Errorf("%s 返回了空内容", tc.path)
		}
		// 资源名不带内容哈希，二进制更新后 URL 不变 —— 长缓存是有意的取舍。
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=86400") {
			t.Errorf("%s 应长缓存，实际 %q", tc.path, cc)
		}
	}
}

// TestSwaggerUIIndexNotCached 入口页必须不缓存（它内嵌 spec 地址）。
func TestSwaggerUIIndexNotCached(t *testing.T) {
	rec := serveUI(t, "/api/admin/api-docs/ui/")
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "max-age=0") {
		t.Fatalf("入口页必须 max-age=0（改 spec 地址后不能被旧缓存挡住），实际 %q", cc)
	}
	if !strings.Contains(cc, "private") {
		t.Fatalf("管理台资源必须是 private 缓存，实际 %q", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("入口页应带 ETag：重复打开走 304，省掉 3KB 传输")
	}
}

// TestSwaggerUITraversalRejected 目录穿越与未知文件名一律不出内容。
func TestSwaggerUITraversalRejected(t *testing.T) {
	paths := []string{
		"/api/admin/api-docs/ui/../embed.go",
		"/api/admin/api-docs/ui/../../admin_api_docs.go",
		"/api/admin/api-docs/ui/../../../../../../etc/passwd",
		"/api/admin/api-docs/ui/%2e%2e%2fembed.go",
		"/api/admin/api-docs/ui/..%2Fembed.go",
		"/api/admin/api-docs/ui/assets/index.html",
		"/api/admin/api-docs/ui/nope.js",
		"/api/admin/api-docs/ui/",
	}
	for _, p := range paths {
		rec := serveUI(t, p)
		// 唯一例外：/ui/ 自己落到 catch-all，给的是入口页（这是设计，不是泄漏）。
		if p == "/api/admin/api-docs/ui/" {
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "swagger-ui") {
				t.Errorf("%s 应返回入口页，实际 %d", p, rec.Code)
			}
			continue
		}
		if rec.Code == http.StatusOK {
			t.Errorf("%s 不应 200（实际吐出了 %d 字节）", p, rec.Body.Len())
		}
		body := rec.Body.String()
		if strings.Contains(body, "package ") || strings.Contains(body, "root:") {
			t.Errorf("%s 泄漏了文件内容", p)
		}
	}
}

// TestSpecRelPathIsValid spec 地址是编译期常量，且必须是**相对**路径。
//
// 绝对路径 `/api/admin/api-docs/openapi.json` 在 nginx 子路径部署
// （`/lingdong/api/...`）下会丢掉前缀直接 404；而它是页面唯一的数据源，
// 错了整页就是空白。这条用例把"改常量时别改成绝对路径"变成红灯。
func TestSpecRelPathIsValid(t *testing.T) {
	if !strings.HasPrefix(specRelPath, "../") {
		t.Fatalf("specRelPath 必须是 ../ 开头的相对路径（当前 %q），绝对路径在子路径部署下会 404", specRelPath)
	}
	if !swaggerui.ValidSpecPath(specRelPath) {
		t.Fatalf("specRelPath %q 过不了白名单：入口页会直接 500", specRelPath)
	}
}
