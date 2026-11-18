package router

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/middleware"
)

// consoleOnlyPrefixes 是「纯 API 节点明确不提供」的路由前缀。
//
// 这些路由在 full 模式下要注册（门户/管理台/文档站在用），api 模式下由
// middleware.APISurfaceOnly 拦成 404。把它们显式列出来，是为了让覆盖测试
// 能区分「有意不暴露」与「新增端点忘了决策」。
var consoleOnlyPrefixes = []string{
	"/api/portal/",
	"/api/admin/",
	"/api/docs/",
	"/api/auth/",
}

func isConsoleOnly(path string) bool {
	if path == "/api/pricing" {
		return true
	}
	for _, prefix := range consoleOnlyPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// TestAPISurfaceCoversAllRoutes 是节点模式的防漂移门禁。
//
// 背景：允许表是 fail-closed 的 —— 新增端点默认在 API 节点不可见。这是安全的默认值，
// 但会让"新加的 API 忘记录入允许表"表现为**线上静默 404**，而测试环境（full 模式）
// 一切正常。这条用例把"必须显式决策"变成编译后立刻能发现的红灯：
// 每条路由要么在允许表里，要么在门户清单里，二者都不在就失败。
func TestAPISurfaceCoversAllRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := New()

	for _, rt := range r.Routes() {
		if middleware.APISurfaceAllows(rt.Path) {
			continue
		}
		if isConsoleOnly(rt.Path) {
			continue
		}
		t.Errorf("路由 %s %s 未决策：既不在 API 允许表（middleware.APISurfaceAllows），也不在 consoleOnlyPrefixes。新增端点时必须显式选择其中一边", rt.Method, rt.Path)
	}
}
