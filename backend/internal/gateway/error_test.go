package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/wireformat"
)

// TestFailDefMsgMarksInternalCode 锁死「覆盖文案时仍要登记内部码」。
//
// 必须挑一个会被投影的码（insufficient_balance → 对外 insufficient_user_quota），
// 否则内部码与对外码相同，测试看不出差别（这正是这个缺口长期没被发现的原因）。
//
// 影响：走 FailDefMsg 的失败（上游超时 / 上游报文异常 / 服务不可用）在访问日志与
// 预检记录里记的是对外投影码，与「日志侧按内部分类」的约定不符，排障时无法按根因
// 聚合，也无法与文档码表对上。
func TestFailDefMsgMarksInternalCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	SetFormat(c, wireformat.OpenAI)

	FailDefMsg(c, errs.InsufficientBalance, "余额不足，请先充值")

	body := w.Body.String()
	if !strings.Contains(body, "insufficient_user_quota") {
		t.Fatalf("对外仍必须是投影后的码，实际 %s", body)
	}
	if got := c.GetString("error_code"); got != errs.CodeInsufficientBalance {
		t.Fatalf("context 里应记内部码 %q，实际 %q（对外码 %q 会把根因抹掉）",
			errs.CodeInsufficientBalance, got, "insufficient_user_quota")
	}
}

// TestFailDefMarksInternalCode 是上一条的对照：FailDef 一直是对的，不能被改坏。
func TestFailDefMarksInternalCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	SetFormat(c, wireformat.OpenAI)

	FailDef(c, errs.InsufficientBalance)

	if got := c.GetString("error_code"); got != errs.CodeInsufficientBalance {
		t.Fatalf("context 里应记内部码 %q，实际 %q", errs.CodeInsufficientBalance, got)
	}
}
