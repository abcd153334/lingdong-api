package router_test

// 应用级后台的路由形状。
//
// 这条用例的价值不在"路由存在"，而在**旧路由必须不存在**：
// 前端如果还在调 `/overview` 或 `/runtime`，Gin 会回 404，而前端的错误态
// 只是把弹窗/页签留白 —— 看起来像"还没加载完"，不会报错。

import (
	"testing"

	"lingdong-api/internal/router"
)

// TestAdminAppRoutesSplit 应用级后台的路由形状。
//
// 不建库（router.New）：这条断言必须在**任何**环境都执行。挂在 setupUserAPI
// 上时，缺 LD_TEST_DSN 的 CI 会静默 skip —— 而它守的正是"前端还在调已删路由"
// 这类只能靠路由表发现的事故。
func TestAdminAppRoutesSplit(t *testing.T) {
	r := router.New()

	live := map[string]bool{}
	for _, rt := range r.Routes() {
		live[rt.Method+" "+rt.Path] = true
	}

	for _, want := range []string{
		"GET /api/admin/apps/:id/brand",
		"PUT /api/admin/apps/:id/brand",
		"GET /api/admin/apps/:id/stats",
		"PATCH /api/admin/apps/:id/maintenance",
		// 公告与版本此前完全没有路由断言：漏一条的后果是前端 404 后留白。
		"GET /api/admin/apps/:id/notices",
		"POST /api/admin/apps/:id/notices",
		"PUT /api/admin/apps/:id/notices/:nid",
		"PATCH /api/admin/apps/:id/notices/:nid/status",
		"DELETE /api/admin/apps/:id/notices/:nid",
		"GET /api/admin/apps/:id/releases",
		"POST /api/admin/apps/:id/releases",
		"PUT /api/admin/apps/:id/releases/:rid",
		"PATCH /api/admin/apps/:id/releases/:rid/status",
		"DELETE /api/admin/apps/:id/releases/:rid",
	} {
		if !live[want] {
			t.Fatalf("缺少管理端路由 %s", want)
		}
	}

	// 品牌与统计必须分开：合成一条会让"改 9 个字符串"先等一次 7 日聚合算完。
	if live["GET /api/admin/apps/:id/overview"] {
		t.Fatal("旧的 /overview 必须删除：它把品牌回填与统计绑在一起，前端漏改会静默 404")
	}
}
