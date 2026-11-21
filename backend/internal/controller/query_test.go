package controller

// query.go 里两个"可选 uint64"解析器的差异用例。
//
// 这两个函数的坑长得很像但结论相反，必须由用例把差别钉死：
//   - optionalUintQuery："" 与 "0" 都算"没传"（用于 admin_id/uid 这类"0 不是合法值"的筛选）；
//   - appScopeQuery：app_id=0 有业务含义（平台直连），必须能与"没传"区分开。

import (
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
)

func appScopeCtx(t *testing.T, raw string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	// 必须转义：raw 里有 " 12 " 这种带空格的用例，直接拼进 URL 会让
	// httptest.NewRequest panic（解析失败）。QueryEscape 之后 c.Query 会解回来。
	target := "/api/admin/orders"
	if raw != "" {
		target += "?app_id=" + url.QueryEscape(raw)
	}
	c.Request = httptest.NewRequest("GET", target, nil)
	return c
}

func TestAppScopeQuery(t *testing.T) {
	cases := []struct {
		raw    string
		wantID uint64
		scoped bool
		ok     bool
	}{
		{"", 0, false, true},     // 未传 ⇒ 不过滤
		{"0", 0, true, true},     // 平台直连 ⇒ 必须过滤（这正是 optionalUintQuery 做不到的）
		{"12", 12, true, true},   // 某应用
		{" 12 ", 12, true, true}, // 前后空白按未填处理
		{"abc", 0, false, false},
		{"-1", 0, false, false},
		{"1.5", 0, false, false},
	}
	for _, tc := range cases {
		id, scoped, ok := appScopeQuery(appScopeCtx(t, tc.raw))
		if id != tc.wantID || scoped != tc.scoped || ok != tc.ok {
			t.Fatalf("app_id=%q ⇒ (%d,%v,%v)，期望 (%d,%v,%v)",
				tc.raw, id, scoped, ok, tc.wantID, tc.scoped, tc.ok)
		}
	}
}

// TestOptionalUintQuerySwallowsZero 记录既有解析器的**故意**行为，
// 防止有人为了"统一"把 app_id 也交给它（那会让「筛平台直连」变成「筛全量」）。
func TestOptionalUintQuerySwallowsZero(t *testing.T) {
	for _, raw := range []string{"", "0"} {
		v, ok := optionalUintQuery(appScopeCtx(t, raw), "app_id")
		if v != 0 || !ok {
			t.Fatalf("app_id=%q ⇒ (%d,%v)，期望 (0,true)", raw, v, ok)
		}
	}
}
