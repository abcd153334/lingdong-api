package task

import (
	"context"

	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/service/callio"
)

// submitBodyTap 收集异步提交阶段**真正出站**的请求体。
//
// 为什么异步链路要单独一个采集器：它不走 relay 的 ioRecorder（那条路径是同步的），
// 而适配器的 client 只认 ctx 上的 tap。没有它，异步任务的调用记录里永远只有
// 「客户端入站参数」，看不到平台重建后的字段（2026-11-04 排障就卡在这）。
type submitBodyTap struct {
	raw       []byte
	url       string
	status    int
	attempts  int
	truncated bool
}

func (t *submitBodyTap) NoteUpstreamRequest(_ context.Context, _ string, url, _ string, body []byte, truncated bool) {
	t.attempts++
	t.url = url
	// 每次尝试重置状态：status 属于「这一次」，上一条的 200 不能留给失败的那次。
	t.status = 0
	t.raw = body
	t.truncated = truncated
}

func (t *submitBodyTap) NoteUpstreamResponse(_ context.Context, status int) { t.status = status }

var _ netguard.BodyTap = (*submitBodyTap)(nil)

// persistUpstreamIO 把出站报文补写进调用记录。
//
// Patch 语义：只更新「本次有值」的列，不会把提交前写好的入站参数清掉。
// 失败只 Warn（callio.Store 内部吞掉），绝不影响任务创建。
func persistUpstreamIO(ctx context.Context, usageID, userID uint64, t *submitBodyTap) {
	if t == nil || usageID == 0 || t.attempts == 0 {
		return
	}
	callio.Store(context.WithoutCancel(ctx), callio.Rec{
		UsageID:           usageID,
		UserID:            userID,
		Patch:             true,
		UpstreamRaw:       t.raw,
		UpstreamURL:       t.url,
		UpstreamStatus:    t.status,
		UpstreamAttempts:  t.attempts,
		UpstreamTruncated: t.truncated,
	})
}
