package worker

import (
	"context"
	"sync/atomic"
	"time"

	"lingdong-api/internal/service/task"
	"lingdong-api/internal/service/upload"
)

// 清理参数：uploads 每 5 分钟清理到期件；files 任务产物每小时清理。
const (
	filePurgeInterval     = 5 * time.Minute
	filePurgeBatch        = 500
	artifactPurgeInterval = time.Hour
	artifactPurgeBatch    = 200
)

// FilePurgeJob 清理到期上传件（对象 + 记录）。
//
// 逐行的结果统计与聚合日志在 storage.PurgeExpired 里（三种清理共用一份），
// 这里只保留「上一轮删掉多少条」供健康页展示。
type FilePurgeJob struct{ last atomic.Int64 }

// NewFilePurgeJob 构造上传件清理任务。
func NewFilePurgeJob() *FilePurgeJob { return &FilePurgeJob{} }

// Name 返回任务标识。
func (*FilePurgeJob) Name() string { return "file_purge" }

// Interval 返回清理节拍。
func (*FilePurgeJob) Interval() time.Duration { return filePurgeInterval }

// Run 执行一轮清理。
//
// 查询级错误向上返回：Manager 会记一条「任务执行失败」，这是「Job 在跑但一直失败」
// 与「Job 悄悄停了」唯一可区分的地方（旧实现把所有错误吞成 nil）。
func (j *FilePurgeJob) Run(ctx context.Context) error {
	st, err := upload.PurgeExpired(ctx, filePurgeBatch)
	j.last.Store(int64(st.Deleted))
	return err
}

// LastCount 返回上一轮实际删除的记录数（worker.Counted 接口）。
func (j *FilePurgeJob) LastCount() int { return int(j.last.Load()) }

// ArtifactPurgeJob 清理到期任务产物（files 表，保留天数由 file_retention_days 配置）。
type ArtifactPurgeJob struct{ last atomic.Int64 }

// NewArtifactPurgeJob 构造产物清理任务。
func NewArtifactPurgeJob() *ArtifactPurgeJob { return &ArtifactPurgeJob{} }

// Name 返回任务标识。
func (*ArtifactPurgeJob) Name() string { return "artifact_purge" }

// Interval 返回清理节拍。
func (*ArtifactPurgeJob) Interval() time.Duration { return artifactPurgeInterval }

// Run 执行一轮清理。
func (j *ArtifactPurgeJob) Run(ctx context.Context) error {
	st, err := task.PurgeArtifacts(ctx, artifactPurgeBatch)
	j.last.Store(int64(st.Deleted))
	return err
}

// LastCount 返回上一轮实际删除的记录数。
func (j *ArtifactPurgeJob) LastCount() int { return int(j.last.Load()) }
