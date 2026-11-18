package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
)

// agentDryRunCleanupInterval 是探测凭据清理的节拍（六小时一次）。
//
// 不需要更快：探测凭据的有效期是 24h，且**读取时判定**（闸门只认
// expires_at > now()），清理只回收磁盘，晚几小时不会让任何人多探一次。
const agentDryRunCleanupInterval = 6 * time.Hour

// agentDryRunRetentionDays 是"过期之后还留几天"。
//
// 7 天：过期行是"上周这条为什么没通过探测"的唯一证据（状态码、上游原话），
// 而运营问这个问题通常在一周之内。留太久则表会一直涨（每轮探测至少一行）。
const agentDryRunRetentionDays = 7

// AgentDryRunCleanupJob 清理过期的连通性探测凭据。
type AgentDryRunCleanupJob struct{}

// NewAgentDryRunCleanupJob 构造清理任务。
func NewAgentDryRunCleanupJob() *AgentDryRunCleanupJob { return &AgentDryRunCleanupJob{} }

// Name 返回任务标识。
func (*AgentDryRunCleanupJob) Name() string { return "agent_dryrun_cleanup" }

// Interval 返回清理节拍。
func (*AgentDryRunCleanupJob) Interval() time.Duration { return agentDryRunCleanupInterval }

// Run 执行一轮清理。
func (*AgentDryRunCleanupJob) Run(ctx context.Context) error {
	gdb := db.DB
	if gdb == nil {
		return nil
	}
	// 按 expires_at 删（不是 created_at）：TTL 是这一列的唯一含义，
	// 用创建时间会跟"有效期"这份定义分家，改 TTL 时清理口径不会跟着变。
	if err := gdb.WithContext(ctx).Exec(
		`DELETE FROM agent_dry_runs WHERE expires_at < now() - make_interval(days => ?)`,
		agentDryRunRetentionDays).Error; err != nil {
		logger.L().Warn("清理过期探测凭据失败", zap.Error(err))
	}
	return nil
}
