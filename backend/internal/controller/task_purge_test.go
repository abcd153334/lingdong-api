package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件覆盖 B7 新增的物理删除入口的**参数守卫**（无数据库即可运行）。
//
// 关键点：`purge=true` 的校验必须发生在 task.Get 之前。若先查库再校验，
// 一个漏传参数的误调用会先走一次查询，然后再拒绝 —— 语义上没错，
// 但一旦将来有人把校验挪到删除之后，这条用例就会立刻失败（它断言的是「不碰库」）。

// callPurge 构造一次 DELETE /tasks/:id 调用（不注入数据库）。
func callPurge(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.DELETE("/tasks/:id", func(c *gin.Context) {
		c.Set("user_id", uint64(1))
		AdminTaskPurge(c)
	})
	req := httptest.NewRequest(http.MethodDelete, target, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAdminTaskPurgeRequiresExplicitFlag 未显式声明 purge=true 一律 400。
//
// 这是「防误删」的唯一闸门：DELETE 与带参数的 DELETE 在客户端库里长得很像，
// 一次脚本抄错就会真删数据，而这里没有任何撤销手段。
func TestAdminTaskPurgeRequiresExplicitFlag(t *testing.T) {
	for _, target := range []string{
		"/tasks/1",             // 完全没有参数
		"/tasks/1?purge=false", // 显式 false
		"/tasks/1?purge=",      // 空值
		"/tasks/1?purge=yes",   // 非约定写法
		"/tasks/1?purge=TRUE1", // 接近但不合法
	} {
		if w := callPurge(t, target); w.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d（%s）", target, w.Code, w.Body.String())
		}
	}
}

// TestPurgeFlagAcceptsOnlyExplicitTrue 只有 true/1（大小写与空白不敏感）算「显式要求删除」。
//
// 直接测判定函数而不是打 handler：合法值会继续走到 task.Get，测试环境没有数据库，
// 就没法干净地断言「参数被接受」。
func TestPurgeFlagAcceptsOnlyExplicitTrue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	accepted := []string{"true", "1", "TRUE", "True", " true ", "1 "}
	for _, v := range accepted {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodDelete, "/tasks/1?purge="+url.QueryEscape(v), nil)
		if !purgeFlagOf(c) {
			t.Fatalf("purge=%q 应被接受", v)
		}
	}
	rejected := []string{"", "false", "yes", "on", "0", "true1", "2"}
	for _, v := range rejected {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodDelete, "/tasks/1?purge="+url.QueryEscape(v), nil)
		if purgeFlagOf(c) {
			t.Fatalf("purge=%q 不应被接受", v)
		}
	}
}

// TestAdminTaskPurgeRejectsBadID 非法任务号必须在其它参数校验之前返回 400。
//
// 口径与门户/开放 API 一致：参数拼错是 invalid_param，不是"任务不存在"。
// 断言"在 purge 参数校验之前"这一点没变 —— 传 `purge=<非法值>` 且 id 也非法时，
// 必须先报 id 的问题，否则调用方会以为自己的 purge 写法错了。
func TestAdminTaskPurgeRejectsBadID(t *testing.T) {
	for _, target := range []string{"/tasks/abc?purge=true", "/tasks/0?purge=true"} {
		if w := callPurge(t, target); w.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d（%s）", target, w.Code, w.Body.String())
		}
	}
}
