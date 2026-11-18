package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/order"
	"lingdong-api/internal/service/payment"
)

// 主动查单参数。
const (
	// orderQueryInterval 是查单节拍：60s 一轮。
	orderQueryInterval = 60 * time.Second
	// orderQueryBatch 是单轮最多领取的订单数。
	orderQueryBatch = 50
	// orderQueryWorkers 是单轮并发度。上限不能太大：虎皮椒有风控与限频，
	// 多副本各开 4 个并发已经是 4N 次/轮。
	orderQueryWorkers = 4
	// orderQueryCooldown 是同一订单两次查单的最小间隔。
	orderQueryCooldown = 30 * time.Second
	// orderQueryHorizon 是「过期后继续补查」的窗口，与 order.querySettleWindow 保持一致。
	// 二维码 5 分钟有效，但用户可能在最后一秒付款、回调几分钟后才到，过期即刻放弃会真丢单。
	orderQueryHorizon = 30 * time.Minute
	// orphanPurgeBatch 是单轮清理孤儿回调记录的行数上限（避免长事务）。
	orphanPurgeBatch = 200
)

// OrderQueryJob 对虎皮椒 pending 订单主动查单，兜住「回调丢了」的场景。
//
// 为什么必须有它：网关重试 6 次后放弃、或回调到达时服务正在重启，都会导致
// 「钱收了、额度不到账」。没有主动查单，这种情况只能等用户投诉才发现。
type OrderQueryJob struct{}

// NewOrderQueryJob 构造查单任务。
func NewOrderQueryJob() *OrderQueryJob { return &OrderQueryJob{} }

// Name 返回任务标识。
func (*OrderQueryJob) Name() string { return "order_query" }

// Interval 返回查单节拍。
func (*OrderQueryJob) Interval() time.Duration { return orderQueryInterval }

// Run 执行一轮查单。
func (*OrderQueryJob) Run(ctx context.Context) error {
	ids, err := claimQueryTargets(ctx)
	if err != nil {
		logger.L().Warn("领取待查单订单失败", zap.Error(err))
		return nil
	}
	if len(ids) > 0 {
		queryBatch(ctx, ids)
	}
	purgeOrphanNotifies(ctx)
	return nil
}

// claimQueryTargets 用**单条 SQL** 完成「领取 + 打标」。
//
// 为什么不是「先 SELECT 一批、再 UPDATE last_query_at」：两步之间别的实例可以领到同一批，
// N 个副本就会对同一订单发起 N 次上游请求（虎皮椒有风控，会被限频）。
// 这里用 `FOR UPDATE SKIP LOCKED` + `UPDATE ... RETURNING`，锁只持有毫秒级。
func claimQueryTargets(ctx context.Context) ([]uint64, error) {
	queried := payment.QueryProtocols()
	if len(queried) == 0 {
		// `IN ()` 是 SQL 语法错误：没有查单协议时本轮直接空转，而不是每轮刷一条 WARN。
		return nil, nil
	}
	ph := make([]string, len(queried))
	args := make([]any, 0, len(queried)+1)
	for i, p := range queried {
		ph[i] = "?"
		args = append(args, string(p))
	}
	args = append(args, orderQueryBatch)

	var rows []struct{ ID uint64 }
	sql := fmt.Sprintf(`
		WITH picked AS (
		  SELECT id FROM orders
		  WHERE status = 'pending' AND deleted_at IS NULL AND pay_protocol IN (%s)
		    AND (last_query_at IS NULL OR last_query_at < now() - interval '%d seconds')
		    AND (
		          (expire_at IS NOT NULL AND expire_at > now() - interval '%d minutes')
		       OR (expire_at IS NULL AND created_at > now() - interval '%d minutes')
		        )
		  ORDER BY (expire_at < now()) ASC, id ASC
		  LIMIT ?
		  FOR UPDATE SKIP LOCKED
		)
		UPDATE orders o SET last_query_at = now()
		FROM picked p WHERE o.id = p.id
		RETURNING o.id`,
		strings.Join(ph, ","),
		int(orderQueryCooldown.Seconds()), int(orderQueryHorizon.Minutes()), int(orderQueryHorizon.Minutes())+5)

	if err := db.DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// queryBatch 并发查单；单笔失败只记日志，不影响同批其他订单。
func queryBatch(ctx context.Context, ids []uint64) {
	svc := order.New(db.DB)
	sem := make(chan struct{}, orderQueryWorkers)
	var wg sync.WaitGroup

	var hit, failed int64
	var mu sync.Mutex

	for _, id := range ids {
		wg.Add(1)
		go func(orderID uint64) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			paid, err := svc.QueryOnce(ctx, orderID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				logger.L().Warn("主动查单失败（不关单，下轮重试）",
					zap.Uint64("order_id", orderID), zap.Error(err))
				return
			}
			if paid {
				hit++
			}
		}(id)
	}
	wg.Wait()

	if hit > 0 || failed > 0 {
		logger.L().Info("主动查单完成",
			zap.Int("picked", len(ids)), zap.Int64("credited", hit), zap.Int64("failed", failed))
	}
}

// purgeOrphanNotifies 清理过期的孤儿回调记录（复用日志保留天数）。
//
// 这张表会被扫描场景写入，没有清理策略就是无限增长。
func purgeOrphanNotifies(ctx context.Context) {
	days := orphanRetentionDays()
	res := db.DB.WithContext(ctx).Exec(fmt.Sprintf(`
		DELETE FROM pay_orphan_notify WHERE id IN (
		  SELECT id FROM pay_orphan_notify
		  WHERE created_at < now() - interval '%d days'
		  LIMIT %d)`, days, orphanPurgeBatch))
	if res.Error != nil {
		logger.L().Warn("清理孤儿回调记录失败", zap.Error(res.Error))
		return
	}
	if res.RowsAffected > 0 {
		logger.L().Info("已清理孤儿回调记录", zap.Int64("rows", res.RowsAffected))
	}
}

// orphanRetentionDays 读取日志保留天数（默认 180 天）。
func orphanRetentionDays() int {
	const def = 180
	raw, ok := conf.System().Get("logs_retention_days")
	if !ok {
		return def
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil || v <= 0 {
		return def
	}
	return v
}

// 编译期断言：OrderQueryJob 实现 Job 接口。
var _ Job = (*OrderQueryJob)(nil)
