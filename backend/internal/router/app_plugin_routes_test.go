package router_test

// 应用插件的路由形状与**旧命名空间守卫**。
//
// 守卫的理由不是洁癖：2026-10-25 下线的那套插件系统用 /api/admin/plugins，
// 它是"执行第三方代码"。新方案的路径一旦漂回那个前缀，读代码的人会以为
// 执行型插件复活了，而两者的安全边界完全不同。

import (
	"strings"
	"testing"

	"lingdong-api/internal/router"
)

func TestAppPluginRoutesRegistered(t *testing.T) {
	r := router.New()

	live := map[string]bool{}
	for _, rt := range r.Routes() {
		live[rt.Method+" "+rt.Path] = true
		if strings.HasPrefix(rt.Path, "/api/admin/plugins") {
			t.Fatalf("旧插件系统的命名空间不得复活：%s %s", rt.Method, rt.Path)
		}
	}

	for _, want := range []string{
		"GET /api/admin/app-plugins",
		"GET /api/admin/app-plugins/sample",
		"GET /api/admin/app-plugins/:key",
		"PUT /api/admin/app-plugins/:key",
		"PATCH /api/admin/app-plugins/:key/status",
		"DELETE /api/admin/app-plugins/:key",

		"GET /api/admin/apps/:id/plugins",
		"POST /api/admin/apps/:id/plugins",
		"GET /api/admin/apps/:id/plugins/:key",
		"PATCH /api/admin/apps/:id/plugins/:key",
		"DELETE /api/admin/apps/:id/plugins/:key",
		"DELETE /api/admin/apps/:id/plugins/:key/data",

		"GET /api/admin/apps/:id/plugins/:key/records",
		"POST /api/admin/apps/:id/plugins/:key/records",
		// 导出必须排在 :rk 之前，否则 /export 会被当成 record_key。
		"GET /api/admin/apps/:id/plugins/:key/records/export",
		"PUT /api/admin/apps/:id/plugins/:key/records/:rk",
		"DELETE /api/admin/apps/:id/plugins/:key/records/:rk",
	} {
		if !live[want] {
			t.Fatalf("缺少管理端路由 %s", want)
		}
	}
}
