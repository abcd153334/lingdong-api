package router_test

// 应用级后台的路由形状。
//
// 这条用例的价值不在"路由存在"，而在**旧路由必须不存在**：
// 前端如果还在调 `/overview` 或 `/runtime`，Gin 会回 404，而前端的错误态
// 只是把弹窗/页签留白 —— 看起来像"还没加载完"，不会报错。

import (
	"net/http"
	"net/http/httptest"
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

// TestAdminAppOptionsRouting 静态段 `/apps/options` 与通配符 `/apps/:id/*` 必须同时可匹配。
//
// 为什么要**真的发请求**而不是只查路由表：gin 里"注册成功"与"能匹配到"是两件事。
// 静态子节点与通配符同级时，子节点顺序决定匹配结果（gin@v1.10.0/tree.go:85-92 的
// addChild 会把静态段插到通配符之前）；顺序一旦被写坏，`/apps/:id/*` 会**静默 404**，
// 而路由表里依然能看到这两条路径 —— 只查路由表的断言抓不到这种事故。
//
// 未带管理凭证时正确行为是 401（authenticate 在无 ticket 时直接返回，不碰数据库），
// 所以 404 = 路由坏了，401 = 路由正常。
func TestAdminAppOptionsRouting(t *testing.T) {
	r := router.New()
	for _, tc := range []struct {
		path string
		why  string
	}{
		{"/api/admin/apps/options", "新增的静态选项段"},
		{"/api/admin/apps/2/keys", "既有通配符段（不能被静态段挤掉）"},
		{"/api/admin/apps/2/stats", "既有通配符段"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code == http.StatusNotFound {
			t.Fatalf("GET %s 返回 404（%s）：路由未注册，或静态段破坏了通配符匹配", tc.path, tc.why)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s 未带凭证应为 401，实际 %d（%s）", tc.path, w.Code, tc.why)
		}
	}
}
