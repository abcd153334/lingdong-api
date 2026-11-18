package router

// 本文件守的是**接口文档的四个视图地址与真实路由的一致性**。
//
// 为什么必须单独守：
//
//	handler 里下发的 `links` 是四段字符串常量，router 里注册的是四次调用 ——
//	两处都是"手写"，编译器管不着。改了一处忘另一处，结果就是文档页上有一个
//	点了 404 的按钮，而这种错在本地开发时**永远发现不了**（开发时只会点自己
//	刚改的那个）。
//
// 做法：以 controller 导出的路径常量为准，逐个到 `engine.Routes()` 里查。
// 这样常量的改动会同时推动路由注册（否则红灯），而不是反过来。
//
// 请求级鉴权不在这里验（AdminAuth 需要 DB）：这里只回答"地址存在吗"，
// "没有会话能不能打开"由 e2e 覆盖。

import (
	"strings"
	"testing"

	"lingdong-api/internal/controller"
)

// TestAPIDocsLinksMatchRoutes 三个精确路径必须注册为 GET 路由。
func TestAPIDocsLinksMatchRoutes(t *testing.T) {
	got := map[string]bool{}
	for _, ri := range New().Routes() {
		got[ri.Method+" "+ri.Path] = true
	}
	want := []string{
		controller.APIDocsPathCatalog,
		controller.APIDocsPathOpenAPI,
		controller.APIDocsPathMarkdown,
		controller.APIDocsPathSwaggerAsset,
	}
	for _, p := range want {
		if !got["GET "+p] {
			t.Errorf("文档视图路由缺失: GET %s", p)
		}
	}
}

// TestAPIDocsSwaggerUIPathShape 入口页地址必须是 catch-all 的"前缀形态"。
//
// 前端拿 `links.swagger_ui` 直接渲染跳转，因此它必须**带尾斜杠**（否则
// gin 会 301，在新标签页里多一次往返），且必须是注册的 catch-all 路径的前缀
// （否则两者会各改各的）。这条把"两个常量之间"的关系也锁住。
func TestAPIDocsSwaggerUIPathShape(t *testing.T) {
	ui := controller.APIDocsPathSwaggerUI
	if !strings.HasSuffix(ui, "/") {
		t.Errorf("Swagger UI 地址必须带尾斜杠（否则 gin 301），实际 %q", ui)
	}
	if controller.APIDocsPathSwaggerAsset != ui+"*asset" {
		t.Errorf("catch-all 路由 %q 应等于入口地址 %q + %q",
			controller.APIDocsPathSwaggerAsset, ui, "*asset")
	}
	// 三个精确路径必须在同一个前缀下：接入方按前缀做代理放行。
	for _, p := range []string{controller.APIDocsPathOpenAPI, controller.APIDocsPathMarkdown} {
		if !strings.HasPrefix(p, controller.APIDocsPathCatalog+"/") {
			t.Errorf("%q 不在目录路径 %q 之下：代理按前缀放行时会漏掉它",
				p, controller.APIDocsPathCatalog)
		}
	}
}
