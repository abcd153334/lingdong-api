package relay

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// shortWriter 模拟「对端已关闭，只能写进去一部分」的连接。
//
// 这是反向代理读超时（504）、SDK timeout、用户关页面的共同表现：
// TCP 还能 write 一小段（进内核缓冲），然后 short write / EPIPE。
type shortWriter struct {
	gin.ResponseWriter
	remain int
}

func (w *shortWriter) Write(b []byte) (int, error) {
	if w.remain <= 0 {
		return 0, errors.New("write: connection reset by peer")
	}
	if len(b) > w.remain {
		n, _ := w.ResponseWriter.Write(b[:w.remain])
		w.remain = 0
		return n, nil // 短写：err 为 nil，只能靠 n < len 发现
	}
	w.remain -= len(b)
	return w.ResponseWriter.Write(b)
}

// TestWriteResponseDetectsShortWrite 守住「没送达」必须可识别。
//
// 判定依据只能是写入量：gin 的 c.Data / c.JSON 会把 Write 的错误吞掉，
// 调用方拿到的是「成功」，于是没送达的响应被记成 200 并正常扣费 ——
// 用户那边是 504，钱照扣，图没有。
func TestWriteResponseDetectsShortWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"created":1,"data":[{"url":"https://cdn.example.com/a.png"}]}`)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Writer = &shortWriter{ResponseWriter: c.Writer, remain: 10}

	n, err := writeResponse(c, &relayResult{status: 200, ctype: "application/json", body: body})
	if err != nil {
		t.Fatalf("短写场景 err 应为 nil（真实断连正是这样静默的），实际 %v", err)
	}
	if n >= int64(len(body)) {
		t.Fatalf("短写必须能被写入量发现：写进 %d / 总长 %d", n, len(body))
	}
}

// TestWriteResponseFullDelivery 正常链路必须报告「已全部送达」，否则每次调用都会被误判成断连。
func TestWriteResponseFullDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"created":1,"data":[{"url":"https://cdn.example.com/a.png"}]}`)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	n, err := writeResponse(c, &relayResult{status: 200, ctype: "application/json", body: body})
	if err != nil {
		t.Fatalf("正常写入不应报错：%v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("应写满 %d 字节，实际 %d", len(body), n)
	}
	if got := c.Writer.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type 必须按上游类型下发，实际 %q", got)
	}
}

// TestSyncAbandonedErrorKeepsTaskID 守住「客户端跑了也要留下上游任务号」。
//
// 上游任务在客户端走后仍可能成功；没有任务号，运维既补不了图也没法向渠道追偿，
// 调用记录里只剩一句「客户端断开」。
func TestSyncAbandonedErrorKeepsTaskID(t *testing.T) {
	err := &SyncAbandonedError{TaskID: "73ed1a98-9521", Err: context.Canceled}
	msg := err.Error()
	if !strings.Contains(msg, "73ed1a98-9521") {
		t.Fatalf("错误文案必须带上游任务号，实际 %q", msg)
	}
	if !strings.Contains(msg, "释放") {
		t.Fatalf("必须说明额度已释放，实际 %q", msg)
	}
	// dispatch 的 aborted 判定靠 ctx.Err()，Unwrap 断了就会变成 502 并记渠道健康度。
	if !errors.Is(err, context.Canceled) {
		t.Fatal("必须 Unwrap 回 context.Canceled，否则 dispatch 会当成上游故障")
	}
	// 不能误判成「等待超时」：那条路径会返回 504 且不换渠道，语义完全不同。
	var to *SyncWaitTimeoutError
	if errors.As(err, &to) {
		t.Fatal("断连错误不得被识别为 SyncWaitTimeoutError")
	}
}

// TestSyncAbandonedErrorWithoutTaskID 提交阶段就断开时没有任务号，文案不能出现空的「上游任务 」。
func TestSyncAbandonedErrorWithoutTaskID(t *testing.T) {
	msg := (&SyncAbandonedError{Err: context.Canceled}).Error()
	if strings.Contains(msg, "上游任务") {
		t.Fatalf("无任务号时不应出现该措辞，实际 %q", msg)
	}
	if !strings.Contains(msg, "客户端已断开") {
		t.Fatalf("文案应说明断开原因，实际 %q", msg)
	}
}
