package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/callio"
)

// 调用报文保留清理（C1 · B2）。
//
// 与「任务保留」的 6 小时节拍一致，但**没有**先删对象再删行的顺序约束：
// 报文全部在 PG 里，一条 DELETE 即完成 —— 这正是方案把报文放进 PG 而不是
// 对象存储的核心收益（对象存储那条路必须"先删对象、删不掉就保留行"，否则留下孤儿）。
const (
	callIOPurgeInterval = 6 * time.Hour
	// callIOPurgeBatch 单轮删除上限：报文行比任务行重（可能各带 256KiB），
	// 批太大容易造成长事务与 WAL 峰值。
	callIOPurgeBatch = 2000
)

// CallIOPurgeJob 按 LD_CALL_IO_RETENTION_DAYS 清理过期报文。
//
// 默认 7 天；配成 0 时**只报告不删**（统计过期存量并打日志），
// 与 LD_TASK_RETENTION_DAYS 同一语义：删数据是不可逆动作，不能由升级替运营决定。
type CallIOPurgeJob struct{}

// NewCallIOPurgeJob 构造报文清理任务。
func NewCallIOPurgeJob() *CallIOPurgeJob { return &CallIOPurgeJob{} }

// Name 返回任务标识。
func (*CallIOPurgeJob) Name() string { return "call_io_purge" }

// Interval 返回清理节拍。
func (*CallIOPurgeJob) Interval() time.Duration { return callIOPurgeInterval }

// Run 执行一轮清理。
func (*CallIOPurgeJob) Run(ctx context.Context) error {
	n, err := callio.Purge(ctx, callIOPurgeBatch)
	if err != nil {
		// 不向上抛：Manager 会打 Error 但不重试，这里自己 Warn 更有信息量。
		logger.L().Warn("调用报文清理失败", zap.Error(err))
		return nil
	}
	if n > 0 {
		logger.L().Info("调用报文清理完成", zap.Int64("rows", n))
	}
	return nil
}
