package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/service/billing"
)

// TestGuardFreezeOnPanicRethrows 守的是「守卫不许吞掉业务 panic」。
//
// 释放动作本身可以失败（这里没有初始化 DB，Release 必然失败），但 panic 必须
// 原样抛回 middleware.Recover：吞掉就变成"静默 200 + 账没退"，
// 而客户端会以为请求成功。
func TestGuardFreezeOnPanicRethrows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	const want = "boom-from-handler"
	got := func() (caught any) {
		defer func() { caught = recover() }()
		func() {
			defer guardFreezeOnPanic(c, &billing.Session{UsageID: 1, UserID: 1})
			panic(want)
		}()
		return nil
	}()

	if got != want {
		t.Fatalf("守卫必须原样抛回 panic，实际收到 %v", got)
	}
}

// TestGuardFreezeOnPanicNoPanicIsNoop：正常返回时守卫必须什么都不做
// （包括不能写响应、不能改动 context）。
func TestGuardFreezeOnPanicNoPanicIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	func() {
		defer guardFreezeOnPanic(c, &billing.Session{UsageID: 1, UserID: 1})
	}()

	if w.Body.Len() != 0 {
		t.Fatalf("无 panic 时不得写响应体，实际 %q", w.Body.String())
	}
	if c.IsAborted() {
		t.Fatal("无 panic 时不得 Abort")
	}
}
