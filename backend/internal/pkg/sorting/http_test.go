package sorting_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/sorting"
)

// 这些用例走真实 HTTP 入口（sorting.Query），而不是只测 Parse。
//
// 两者的差别正是最容易写错的地方：Parse 返回的 error 里带"可选字段清单"，
// 但把它挂到响应上时，用 `response.FailDef(c, errs.InvalidParam)` 会丢掉清单，
// 接入方只能看到"参数错误"四个字，得回来翻文档才知道能传什么。

var testCols = map[string]string{
	"created_at": "created_at",
	"amount":     "amount_cent",
}

// runQuery 起一个只调 sorting.Query 的最小引擎，返回状态码与解析出的 order。
func runQuery(t *testing.T, raw string) (int, sorting.Order, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var (
		code  int
		order sorting.Order
	)
	r.GET("/x", func(c *gin.Context) {
		o, ok := sorting.Query(c, testCols)
		if !ok {
			code = c.Writer.Status()
			return
		}
		code = http.StatusOK
		order = o
	})
	target := "/x"
	if raw != "" {
		target += "?sort=" + raw
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

	body := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是合法 JSON：%v（原文 %s）", err, w.Body.String())
		}
	}
	return code, order, body
}

func TestQueryAcceptsKnownField(t *testing.T) {
	code, o, _ := runQuery(t, "-amount")
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200", code)
	}
	// 对外字段名 amount 必须映射到真实列 amount_cent，否则 ORDER BY 会报列不存在。
	if got, want := o.Clause(), "amount_cent DESC, id DESC"; got != want {
		t.Fatalf("Clause = %q，期望 %q", got, want)
	}
}

func TestQueryEmptyIsZero(t *testing.T) {
	code, o, _ := runQuery(t, "")
	if code != http.StatusOK {
		t.Fatalf("不传 sort 必须是 200，实际 %d", code)
	}
	if !o.IsZero() {
		t.Fatalf("不传 sort 必须是零值（由调用方回落各自默认顺序），实际 %+v", o)
	}
}

// TestQueryUnknownFieldTellsAllowed 是这套入口存在的理由：
// 400 的 message 里必须**同时**有用户传的值和可选清单。
func TestQueryUnknownFieldTellsAllowed(t *testing.T) {
	code, _, body := runQuery(t, "bogus")
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 %d，期望 400", code)
	}
	msg, _ := body["message"].(string)
	if msg == "" {
		t.Fatalf("400 响应没有 message：%+v", body)
	}
	for _, want := range []string{"bogus", "created_at", "amount"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message 里应包含 %q，实际 %q", want, msg)
		}
	}
	// 错误码必须还是既有的 invalid_param：新增一个码前端没登记，会显示成"未知错误"。
	if ec, _ := body["error_code"].(string); ec != "invalid_param" {
		t.Errorf("error_code = %q，期望 invalid_param", ec)
	}
}

func TestQueryMultipleFieldsRejected(t *testing.T) {
	code, _, body := runQuery(t, "created_at,amount")
	if code != http.StatusBadRequest {
		t.Fatalf("多字段排序必须 400，实际 %d", code)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "一个字段") {
		t.Errorf("message 应说明只支持一个字段，实际 %q", msg)
	}
}
