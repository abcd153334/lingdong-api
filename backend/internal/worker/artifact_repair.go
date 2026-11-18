package worker

// 产物补转存 Job（见 task.RepairArtifacts 里的事故记录）。
//
// 为什么必须是独立 Job 而不是塞进任务轮询：
//   - 轮询只取 `status IN (queued, running)`，终态任务永远进不去；
//   - 补转存要按 `updated_at` 排序（另一条判据），混进轮询会改掉那条
//     `ORDER BY last_polled_at` 的索引依赖 —— 这正是 expire/orphan 被拆出去的原因。
//
// 节拍 2 分钟：一次失败的转存大概率是瞬时网络问题，2 分钟足够跨过一次抖动；
// 而它本身只扫「最近 6 小时内、artifact_count 小于清单长度」的少量行，代价可控。

import (
	"context"
	"sync/atomic"
	"time"

	"lingdong-api/internal/service/task"
)

const (
	artifactRepairInterval = 2 * time.Minute
	artifactRepairBatch    = 20
)

// ArtifactRepairJob 周期性补转存「已成功但产物缺失」的任务。
type ArtifactRepairJob struct{ last atomic.Int64 }

// NewArtifactRepairJob 构造补转存任务。
func NewArtifactRepairJob() *ArtifactRepairJob { return &ArtifactRepairJob{} }

// Name 返回任务标识。
func (*ArtifactRepairJob) Name() string { return "artifact_repair" }

// Interval 返回节拍。
func (*ArtifactRepairJob) Interval() time.Duration { return artifactRepairInterval }

// Run 执行一轮补转存。
//
// 单条任务的失败不影响其余任务（转存失败是常态：链接过期、存储抖动）。
// 只有「查不出候选」这种整体性错误才向上返回，让看板看到 Job 真的坏了。
func (j *ArtifactRepairJob) Run(ctx context.Context) error {
	since := time.Now().Add(-task.ArtifactRepairWindow)
	rows, err := task.MissingArtifactTasks(ctx, since, artifactRepairBatch)
	if err != nil {
		return err
	}
	fixed := 0
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		n, rerr := task.RepairArtifacts(ctx, &rows[i])
		if rerr != nil {
			// 逐条记由 task 包里打点；这里只累计，不中断整轮。
			continue
		}
		fixed += n
	}
	j.last.Store(int64(fixed))
	return nil
}

// LastCount 返回上一轮补成功的产物条数（worker.Counted 接口）。
func (j *ArtifactRepairJob) LastCount() int { return int(j.last.Load()) }
