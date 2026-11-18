package worker

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/stats"
)

// 数据面维护节拍（S11）：聚合与归档均按小时跑，归档单轮批量上限固定。
const (
	usageRollupInterval  = time.Hour
	usageArchiveInterval = time.Hour
	usageArchiveBatch    = 1000
)

// UsageRollupJob 每小时把 usages 明细重算进 usage_daily（幂等 UPSERT，保证聚合与明细恒等）。
//
// 为什么需要 lastDay：RollupDaily 只回看 N 天（默认 3）。服务停 5 天再起时，
// 那 5 天永远进不了聚合表 —— 「本月费用」从此永久少一块，而且没有任何报错：
// 聚合表看起来有数据、查询也不报错，只是数字对不上。
//
// 所以每个**业务日**的首轮（进程启动、跨天、宕机数天后）走宽窗，其余轮次仍走窄窗。
// 宽窗不取「整个历史」的原因：那是全表重算，每次启动都会把大表扫一遍。
type UsageRollupJob struct {
	mu sync.Mutex
	// lastDay 是最近一次**成功**聚合的业务日（"2006-01-02"）。聚合失败时不更新，
	// 下一小时仍按宽窗重试 —— 否则一次瞬时故障就会把「补账」降级成「窄窗」。
	lastDay string
}

// NewUsageRollupJob 构造 usage_daily 聚合任务。
func NewUsageRollupJob() *UsageRollupJob { return &UsageRollupJob{} }

// Name 返回任务标识。
func (*UsageRollupJob) Name() string { return "usage_rollup" }

// Interval 返回聚合节拍。
func (*UsageRollupJob) Interval() time.Duration { return usageRollupInterval }

// Run 执行一轮聚合。
func (j *UsageRollupJob) Run(ctx context.Context) error {
	now := stats.BizNow()
	today := stats.DayKey(now)

	j.mu.Lock()
	wide := j.lastDay != today
	j.mu.Unlock()

	var (
		n   int64
		err error
	)
	if wide {
		// 起点取「月初」与「N 天前」更早的那个：
		//   - 月初 → 保证「本月费用」完整；
		//   - N 天前 → 保证跨月停机时，上月最后几天的桶也能补上。
		// 只取月初会让 10-30 停机到 11-04 的 10 月数据永久缺失（管理台查 10 月看板会少一截）。
		monthStart, end := stats.MonthRange(now)
		narrowStart, _ := stats.DayRange(now.AddDate(0, 0, -stats.RollupLookbackDays()))
		start := monthStart
		if narrowStart.Before(monthStart) {
			start = narrowStart
		}
		n, err = stats.RollupDailyWindow(ctx, start, end)
	} else {
		n, err = stats.RollupDaily(ctx)
	}
	if err != nil {
		logger.L().Warn("usage_daily 聚合失败", zap.Error(err))
		return nil
	}

	j.mu.Lock()
	j.lastDay = today
	j.mu.Unlock()

	if n > 0 {
		logger.L().Info("usage_daily 聚合完成", zap.Int64("rows", n), zap.Bool("wide", wide))
	}
	return nil
}

// UsageArchiveJob 每小时归档超期调用明细，并清理超期归档与审计（logs/archive/audit_retention_days 落地）。
type UsageArchiveJob struct{}

// NewUsageArchiveJob 构造数据保留维护任务。
func NewUsageArchiveJob() *UsageArchiveJob { return &UsageArchiveJob{} }

// Name 返回任务标识。
func (*UsageArchiveJob) Name() string { return "usage_archive" }

// Interval 返回维护节拍。
func (*UsageArchiveJob) Interval() time.Duration { return usageArchiveInterval }

// Run 执行一轮归档与清理。
func (*UsageArchiveJob) Run(ctx context.Context) error {
	archived, err := stats.ArchiveUsages(ctx, usageArchiveBatch)
	if err != nil {
		logger.L().Warn("调用日志归档失败", zap.Error(err))
		return nil
	}
	purged, err := stats.PurgeRetention(ctx)
	if err != nil {
		logger.L().Warn("数据保留清理失败", zap.Error(err))
		return nil
	}
	if archived > 0 || purged > 0 {
		logger.L().Info("数据保留维护完成", zap.Int64("archived", archived), zap.Int64("purged", purged))
	}
	return nil
}
