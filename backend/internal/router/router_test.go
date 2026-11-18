package router

import (
	"strings"
	"testing"
)

// TestPackageRoutesRegistered 守住套餐 5 条路由的注册结果。
//
// 价值不在于「路径拼写」本身，而在于**路由树能否构建**：Gin 在注册阶段就会对
// 冲突的通配符 panic（例如 `/packages/:id` 与 `/packages/:pid/status` 共存）。
// 这类问题只在启动瞬间暴露，单元测试里先撞一下比线上起不来便宜得多。
// 同时锁住「启停走独立 PATCH」这个口径：PUT 只负责编辑，不承担状态变更。
func TestPackageRoutesRegistered(t *testing.T) {
	r := New()
	got := map[string]bool{}
	for _, ri := range r.Routes() {
		got[ri.Method+" "+ri.Path] = true
	}
	want := []string{
		"GET /api/admin/packages",
		"POST /api/admin/packages",
		"PUT /api/admin/packages/:id",
		"PATCH /api/admin/packages/:id/status",
		"DELETE /api/admin/packages/:id",
		// 门户侧只读端点必须仍在（套餐改造不能把它弄丢）
		"GET /api/portal/packages",
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("路由缺失: %s\n实际含 packages 的路由: %v", w, filterPkg(got))
		}
	}
}

func filterPkg(got map[string]bool) []string {
	out := make([]string, 0, len(got))
	for k := range got {
		if strings.Contains(k, "packages") {
			out = append(out, k)
		}
	}
	return out
}
