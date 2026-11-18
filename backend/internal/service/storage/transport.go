package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

// 三个时间上限（沿用 s3.go 原有的口径，现在由所有驱动共用）。
//
// 改造前是一个 `Timeout: 60s` 的总超时，它同时被用在语义完全相反的场景上：
// 小对象（几 KB 的 JSON/图片）60s 绰绰有余；大对象（产物上限 512MB，慢速跨境链路）
// 60s 必然超时，而超时是**整体取消** —— 已收下的字节全部作废、连接被丢，
// 重试仍会在同一处失败，表现为「大视频永远转存失败」。
//
// 现在的口径是「连接快、单次读慢、总量封顶」：
//   - 建连/TLS 10s：由 netguard.Transport 固定，握手就该是毫秒级；
//   - 响应头 60s：由 metaOpTimeout / dataOpTimeout 的 context 覆盖前段；
//   - 单次读 60s：由 idleTimeoutBody 实现（每次 Read 后重置）；
//   - 整体 30min：数据面操作（512MB @ 300KB/s ≈ 29min，更长的链路本就该判失败）。
const (
	dataOpTimeout   = 30 * time.Minute
	metaOpTimeout   = 60 * time.Second
	readIdleTimeout = 60 * time.Second
)

// guardedClient 返回所有对象存储驱动共用的 HTTP 客户端。
//
// 为什么必须过 netguard：endpoint / region 是**运营可配置的半可信输入**。
// 不注入受管 Transport 就等于「运营填个内网地址，后端替调用方访问内网，
// 还会把签名头一起发过去」。自建 MinIO 落在私有网段时用
// LD_UPSTREAM_ALLOW_PRIVATE=1 显式放行。
//
// 刻意不设 http.Client.Timeout：那是整体超时，会截断 30 分钟级的大文件传输。
// 整体上限由每次操作的 context 承担（dataContext / metaContext），
// 单次读空闲由 newIdleTimeoutBody 承担。
func guardedClient() *http.Client {
	return &http.Client{
		Transport:     netguard.Transport(netguard.PolicyFromEnv()),
		CheckRedirect: netguard.DenyRedirect,
	}
}

// dataContext 给数据面操作（Put / Open）套整体超时。
func dataContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, dataOpTimeout)
}

// metaContext 给元数据面操作（Delete / 探针）套整体超时。
func metaContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, metaOpTimeout)
}

// cancelingBody 把 context 的 cancel 与响应体生命周期绑定。
//
// 数据面操作的 context 有 30 分钟上限；不在这里 cancel 的话，
// 每次读完都要等计时器到期才释放（长连接场景下会堆积 timer）。
type cancelingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

// Close 先关底层流再取消 context：顺序反过来会让 Read 提前拿到
// "context canceled" 而不是正常的 EOF。
func (b *cancelingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

// newIdleTimeoutBody 给响应体加**单次读**超时：每次 Read 返回后重置计时器，
// 计时器触发时关闭底层连接，让阻塞中的 Read 立即返回错误。
//
// 为什么不用 http.Client.Timeout 做这件事：那是整体超时，会误杀「慢但在持续传输」的大文件；
// 也不能用 SetReadDeadline —— http.Response.Body 不暴露该方法（Go 的 transport 内部持有连接）。
func newIdleTimeoutBody(rc io.ReadCloser, d time.Duration) io.ReadCloser {
	if d <= 0 {
		return rc
	}
	b := &idleTimeoutBody{rc: rc, timeout: d}
	b.timer = time.AfterFunc(d, func() {
		b.mu.Lock()
		b.fired = true
		b.mu.Unlock()
		// 关闭底层连接（不是 b.Close()，避免和调用方并发 Stop 打架）。
		_ = rc.Close()
	})
	return b
}

type idleTimeoutBody struct {
	rc      io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	mu      sync.Mutex
	fired   bool
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err == nil {
		// 有进展就重置：判定的是「单次读」而不是「总耗时」。
		b.timer.Reset(b.timeout)
		return n, nil
	}
	b.mu.Lock()
	fired := b.fired
	b.mu.Unlock()
	if fired && !errors.Is(err, io.EOF) {
		return n, ErrObjectReadTimeout
	}
	return n, err
}

// Close 停掉计时器再关流：不停的话每个响应都会留下一个 60s 后才触发的 timer，
// 长连接场景下会堆积。
func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}

// readBody 把一次 HTTP 响应体包装成「单次读超时 + 随 Close 取消 context」的读流。
func readBody(body io.ReadCloser, cancel context.CancelFunc) io.ReadCloser {
	return &cancelingBody{ReadCloser: newIdleTimeoutBody(body, readIdleTimeout), cancel: cancel}
}
