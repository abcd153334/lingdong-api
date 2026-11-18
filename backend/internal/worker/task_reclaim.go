package worker

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/task"
)

// 回收参数（TTL 为常量，按裁定不做可配置）。
const (
	taskReclaimInterval = 10 * time.Minute
	taskReclaimBatch    = 200
)

// taskReclaimTTL 是「在途任务」被视为孤儿的阈值：超过 TaskTimeout 两倍仍停在 queued/running 且仍有冻结额，
// 说明上游提交从未真正接手（轮询 Worker 会跳过无上游任务 ID 的任务），其预扣将永久占用用户余额。
var taskReclaimTTL = 2 * task.TaskTimeout

// TaskReclaimJob 回收孤儿预扣：扫描超 TTL 的在途任务，回退资金并置 timeout。
type TaskReclaimJob struct{ mu sync.Mutex }

// NewTaskReclaimJob 构造孤儿预扣回收任务。
func NewTaskReclaimJob() *TaskReclaimJob { return &TaskReclaimJob{} }

// Name 返回任务标识。
func (*TaskReclaimJob) Name() string { return "task_reclaim" }

// Interval 返回节拍。
func (*TaskReclaimJob) Interval() time.Duration { return taskReclaimInterval }

// Run 执行一轮回收：命中任务走 service/task.FailTimeout（内部 CAS failed + Reopen/Release 全额回退）。
// 幂等性由 FailTimeout 的 CAS（queued/running → failed）保证：并发或重复执行只会退款一次。
func (j *TaskReclaimJob) Run(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	cutoff := time.Now().Add(-taskReclaimTTL)
	var rows []model.Task
	if err := db.DB.WithContext(ctx).
		Where("status IN ?", []string{task.StatusQueued, task.StatusRunning}).
		Where("frozen > 0").
		Where("created_at < ?", cutoff).
		Order("id ASC").Limit(taskReclaimBatch).Find(&rows).Error; err != nil {
		return err
	}
	for i := range rows {
		if ctx.Err() != nil {
			return nil
		}
		t := rows[i]
		if err := task.FailTimeout(ctx, &t, "任务长期未推进，回收孤儿预扣"); err != nil {
			logger.L().Warn("孤儿预扣回收失败", zap.Uint64("task_id", t.ID), zap.Error(err))
		}
	}
	return nil
}
