package worker

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// 参数：5min 一次；孤儿阈值 10min（必须大于单轮 240s 硬上限，否则会把正在跑的轮次误杀）。
const (
	agentRunReclaimInterval = 5 * time.Minute
	agentRunReclaimTTL      = 10 * time.Minute
	agentRunReclaimBatch    = 200
)

// AgentRunReclaimJob 回收"进程重启后卡在 running"的编排轮次。
//
// 为什么必须有这个 Job：`agent_runs.status='running'` 是进程内的状态，
// 进程被 kill（发布、OOM、容器重启）不会执行任何清理，行会永远停在 running。
// 后果有三个，而且都不报错：
//  1. 成本统计把"其实早就死了"的轮次算进在跑，预算闸永远偏紧；
//  2. 管理台的"今日花费"包含这些幽灵行；
//  3. 运营看到会话"一直在处理中"，重发又会因为会话锁（TTL 300s）已过期而成功 ——
//     于是同一条需求被跑了两遍，但没人知道第一遍是怎么结束的。
type AgentRunReclaimJob struct{ mu sync.Mutex }

// NewAgentRunReclaimJob 构造编排轮次回收任务。
func NewAgentRunReclaimJob() *AgentRunReclaimJob { return &AgentRunReclaimJob{} }

// Name 返回任务标识。
func (*AgentRunReclaimJob) Name() string { return "agent_run_reclaim" }

// Interval 返回节拍。
func (*AgentRunReclaimJob) Interval() time.Duration { return agentRunReclaimInterval }

// Run 把超时仍停在 running 的轮次标成 failed。
//
// 幂等：条件里带 status='running' 且限定 created_at 更早的行，
// 重复执行只会得到 RowsAffected=0。
func (j *AgentRunReclaimJob) Run(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	cutoff := time.Now().Add(-agentRunReclaimTTL)
	res := db.DB.WithContext(ctx).Model(&model.AgentRun{}).
		Where("status = ? AND created_at < ?", "running", cutoff).
		Limit(agentRunReclaimBatch).
		Updates(map[string]any{"status": "failed", "error_code": "reclaimed"})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		logger.L().Warn("回收卡住的 AI 编排轮次",
			zap.Int64("rows", res.RowsAffected),
			zap.Duration("older_than", agentRunReclaimTTL))
	}
	return nil
}
