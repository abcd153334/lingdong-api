package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
)

// 本文件守住「长等待探针」的两条安全边界：
//   1. 没配口令时端点**必须不存在**（404），否则给公网留了一个可以任意睡的端点；
//   2. 口令错误时也不能泄露端点存在（同样 404，不用 403）。
//
// 探针本身是排查「同步图像为什么总在 60 秒被切断」的唯一手段：
// 它保持连接、中途不写任何字节，和同步图像等待上游时的行为完全一致，
// 所以任何一层代理的读超时都会在这一层复现，且不消耗上游额度。

func newHoldProbeCtx(t *testing.T, token, query string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/diag/hold"+query, nil)
	if token != "" {
		c.Request.Header.Set("X-Diag-Token", token)
	}
	return c, w
}

// TestHoldProbeDisabledByDefault 确认未配置口令时端点表现为「不存在」。
func TestHoldProbeDisabledByDefault(t *testing.T) {
	t.Setenv("LD_DIAG_HOLD_TOKEN", "")
	conf.Load()

	c, w := newHoldProbeCtx(t, "anything", "?seconds=1")
	HoldProbe(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未配置口令时应返回 404，实际 %d", w.Code)
	}
}

// TestHoldProbeRejectsWrongToken 确认口令错误与「未启用」不可区分。
func TestHoldProbeRejectsWrongToken(t *testing.T) {
	t.Setenv("LD_DIAG_HOLD_TOKEN", "the-right-one")
	conf.Load()

	c, w := newHoldProbeCtx(t, "wrong", "?seconds=1")
	HoldProbe(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("口令错误应返回 404（不暴露端点存在），实际 %d", w.Code)
	}
}

// TestHoldProbeHoldsAndReturns 确认口令正确时真的挂满指定时长再回包。
//
// 只验 1 秒：这条用例要保证的是「确实在等、且等完才写响应」，
// 时长本身由 nginx/代理去量，测试里等 60 秒毫无意义。
func TestHoldProbeHoldsAndReturns(t *testing.T) {
	t.Setenv("LD_DIAG_HOLD_TOKEN", "tok")
	conf.Load()

	start := time.Now()
	c, w := newHoldProbeCtx(t, "tok", "?seconds=1")
	HoldProbe(c)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("口令正确时应返回 200，实际 %d", w.Code)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("应在挂满 1 秒后才回包，实际耗时 %v", elapsed)
	}
}

// TestHoldProbeRejectsBadSeconds 确认非法参数是 400 而不是被静默当成默认值。
func TestHoldProbeRejectsBadSeconds(t *testing.T) {
	t.Setenv("LD_DIAG_HOLD_TOKEN", "tok")
	conf.Load()

	c, w := newHoldProbeCtx(t, "tok", "?seconds=abc")
	HoldProbe(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 seconds 应返回 400，实际 %d", w.Code)
	}
}
