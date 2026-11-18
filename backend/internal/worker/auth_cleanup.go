package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/authcfg"
)

// authCleanupInterval 是认证数据清理的节拍（一小时一次）。
//
// 不需要更快：会话过期与吊销都是**读取时判定**的（Authenticate 会检查
// expires_at / revoked_at），清理只是回收磁盘，晚一小时不会让任何人多登录一秒。
const authCleanupInterval = time.Hour

// AuthCleanupJob 清理过期的会话、过期认证事件与被用过的备份码。
type AuthCleanupJob struct{}

// NewAuthCleanupJob 构造清理任务。
func NewAuthCleanupJob() *AuthCleanupJob { return &AuthCleanupJob{} }

// Name 返回任务标识。
func (*AuthCleanupJob) Name() string { return "auth_cleanup" }

// Interval 返回清理节拍。
func (*AuthCleanupJob) Interval() time.Duration { return authCleanupInterval }

// Run 执行一轮清理。
func (*AuthCleanupJob) Run(ctx context.Context) error {
	gdb := db.DB
	if gdb == nil {
		return nil
	}

	// 1) 已过期的会话：保留 7 天再删。
	//
	// 为什么不是立即删：设备列表与审计要能回答「三天前那台设备为什么掉线」，
	// 而过期行正是那个答案。7 天足够覆盖一次客诉的取证周期。
	if err := gdb.WithContext(ctx).Exec(
		`DELETE FROM user_sessions WHERE expires_at < now() - interval '7 days'`).Error; err != nil {
		logger.L().Warn("清理过期会话失败", zap.Error(err))
		return nil
	}
	// 2) 已吊销且超过 7 天的会话。
	if err := gdb.WithContext(ctx).Exec(
		`DELETE FROM user_sessions WHERE revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days'`).Error; err != nil {
		logger.L().Warn("清理已吊销会话失败", zap.Error(err))
		return nil
	}
	// 3) 认证事件按保留期清理（默认 90 天，配置键 auth_event_retention_days）。
	days := authcfg.EventRetentionDays()
	if err := gdb.WithContext(ctx).Exec(
		`DELETE FROM auth_events WHERE created_at < now() - make_interval(days => ?)`, days).Error; err != nil {
		logger.L().Warn("清理认证事件失败", zap.Error(err))
		return nil
	}
	// 4) 已被使用的备份码：30 天后删（用户可能还需要回头核对"哪些码用过了"）。
	if err := gdb.WithContext(ctx).Exec(
		`DELETE FROM user_mfa_backup_codes WHERE used_at IS NOT NULL AND used_at < now() - interval '30 days'`).Error; err != nil {
		logger.L().Warn("清理备份码失败", zap.Error(err))
	}
	return nil
}
