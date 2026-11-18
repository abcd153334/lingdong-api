package relay

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/netguard"
)

// 流式上游的两件套：**无整体超时的 client** + **空闲看门狗**。
//
// 为什么必须成对出现：SSE 的合法时长由内容决定（一个长回答可以跑 10 分钟），
// 任何整体超时都会切断合法请求 —— 所以 client 的 Timeout 必须是 0。
// 但 Timeout=0 之后，上游「接上就再也不说话」会让 goroutine 永久挂起，
// 必须由 IdleGuard 负责打断。去掉一个就是另一类事故，不允许拆开用。

// streamIdleDefault 是流式响应的**空闲**读超时（LD_STREAM_IDLE_TIMEOUT，默认 120s）。
const streamIdleDefault = 120 * time.Second

// UpstreamStreamClient 构造流式专用 client：**不设**整体超时，交给 IdleGuard。
//
// 每次调用新建 client 是有意的：netguard.Client 内部复用进程级共享 transport，
// 所以这里不产生新连接池，只是换一份 Client 参数。
func UpstreamStreamClient() *http.Client {
	c := netguard.Client(0, netguard.PolicyFromEnv())
	// 出站报文留痕必须包在 Client 层：transportFor 是进程级共享对象，
	// 支付、媒体代理与对象存储也在用，包在那里会把它们一起套上抓包层。
	c.Transport = netguard.WrapTap(c.Transport)
	return c
}

// UpstreamClient 构造带整体超时的 client（同步 JSON 调用用）。
// timeout <= 0 时回落 DefaultAdapterTimeout。
func UpstreamClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultAdapterTimeout
	}
	c := netguard.Client(timeout, netguard.PolicyFromEnv())
	c.Transport = netguard.WrapTap(c.Transport)
	return c
}

// StreamIdleTimeout 读取流式空闲超时（非法值回落默认；上限 30 分钟）。
func StreamIdleTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LD_STREAM_IDLE_TIMEOUT"))
	if raw == "" {
		return streamIdleDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		logger.L().Warn("LD_STREAM_IDLE_TIMEOUT 非法，使用默认值",
			zap.String("value", raw), zap.Duration("default", streamIdleDefault))
		return streamIdleDefault
	}
	if d > 30*time.Minute {
		return 30 * time.Minute
	}
	return d
}

// IdleGuard 是空闲看门狗：每收到一帧 Reset，超过 idle 未 Reset 就 cancel 派生 ctx。
//
// 语义刻意是「多久没有收到任何字节」，不是「整条流总共花了多久」——
// 后者会把任何超过 idle 的合法长回答中途掐断。
type IdleGuard struct {
	cancel context.CancelFunc
	timer  *time.Timer
	idle   time.Duration
	hit    atomic.Bool
}

// NewIdleGuard 派生一个带空闲超时的 ctx。调用方必须 defer guard.Stop()。
func NewIdleGuard(parent context.Context, idle time.Duration) (context.Context, *IdleGuard) {
	if idle <= 0 {
		idle = streamIdleDefault
	}
	ctx, cancel := context.WithCancel(parent)
	g := &IdleGuard{cancel: cancel, idle: idle}
	g.timer = time.AfterFunc(idle, func() {
		g.hit.Store(true)
		cancel()
	})
	return ctx, g
}

// Idle 返回这个 guard 用的空闲上限（只用于对外文案）。
func (g *IdleGuard) Idle() time.Duration {
	if g == nil || g.idle <= 0 {
		return streamIdleDefault
	}
	return g.idle
}

// Reset 收到数据后重置计时。AfterFunc 已触发后再 Reset 是安全的（Timer 语义）。
func (g *IdleGuard) Reset() {
	if g == nil || g.timer == nil {
		return
	}
	g.timer.Reset(g.idle)
}

// Hit 报告这次中断是否由**空闲超时**引起（决定对外文案与归因）。
func (g *IdleGuard) Hit() bool {
	return g != nil && g.hit.Load()
}

// Stop 停表并释放派生 ctx。必须 defer，否则 timer 与 ctx 泄漏。
func (g *IdleGuard) Stop() {
	if g == nil {
		return
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	g.cancel()
}
