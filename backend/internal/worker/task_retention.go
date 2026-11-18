package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/task"
)

// 任务保留策略（B7 §2.10）。
//
// 节拍取 6 小时而不是每天：单轮有批上限（200），超期任务积压时一天只跑一轮
// 会永远清不完；而 6 小时一轮在幂等的前提下也不会造成额外压力。
const (
	taskRetentionInterval = 6 * time.Hour
	// taskRetentionBatch 是单轮删除上限：与产物清理同量级，避免长事务把
	// 轮询任务的 UPDATE 全堵在后面。
	taskRetentionBatch = 200
)

// TaskRetentionJob 按 `LD_TASK_RETENTION_DAYS` 清理终态任务与产物。
//
// 默认（0）只报告：统计终态任务存量与最老任务年龄，不做任何删除。
type TaskRetentionJob struct{}

// NewTaskRetentionJob 构造任务保留清理任务。
func NewTaskRetentionJob() *TaskRetentionJob { return &TaskRetentionJob{} }

// Name 返回任务标识。
func (*TaskRetentionJob) Name() string { return "task_retention" }

// Interval 返回清理节拍。
func (*TaskRetentionJob) Interval() time.Duration { return taskRetentionInterval }

// Run 执行一轮清理（只报告模式下等价于一次体检）。
func (*TaskRetentionJob) Run(ctx context.Context) error {
	days := conf.EnvConfig().TaskRetentionDays
	rep, err := task.PurgeTerminalTasks(ctx, days, taskRetentionBatch, time.Now())
	if err != nil {
		// 不向外抛错：Manager 会把它打成 Error 但不会重试，这里自己 Warn 更有信息量。
		logger.L().Warn("任务保留清理失败", zap.Int("days", days), zap.Error(err))
		return nil
	}
	if !rep.Enabled {
		// 只报告模式的摘要日志由 service/task 打印（那里能拿到最老任务年龄）。
		return nil
	}
	if rep.Tasks > 0 || rep.Skipped > 0 {
		logger.L().Info("任务保留清理完成",
			zap.Int("days", days), zap.Int64("candidates", rep.Candidates),
			zap.Int("tasks", rep.Tasks), zap.Int("files", rep.Files),
			zap.Int64("bytes", rep.Bytes), zap.Int("skipped", rep.Skipped))
	}
	return nil
}
