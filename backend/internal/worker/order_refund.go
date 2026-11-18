package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/order"
)

// 退款结果巡检参数。
//
// 为什么节拍比查单慢（60s vs 60s，但只取退款中订单）：退款是低频人工动作，
// 且虎皮椒没有退款查询接口，这里只是「回调丢了」的兜底，不需要秒级。
const (
	refundQueryInterval = 60 * time.Second
	refundQueryBatch    = 20
	refundQueryWorkers  = 2
	// refundQueryCooldown 是同一订单两次退款查单的最小间隔（上游有风控）。
	refundQueryCooldown = 60 * time.Second
	// refundQueryHorizon 是巡检窗口：超过 24 小时仍未终结的退款由人工确认。
	// 不做无限巡检 —— 无限重试只会把「需要人核对」的问题永久掩盖在日志里。
	refundQueryHorizon = 24 * time.Hour
)

// OrderRefundQueryJob 对「退款中」订单定期向上游确认结果。
//
// 三条硬约束：
//  1. 只查不重发：绝不自动重试退款请求（重复退款会造成真实资金损失）；
//  2. 查不到不动状态：上游说 OD 只代表「还没退完」，不代表失败；
//  3. 超过窗口不再查：转人工确认，避免无限轮询持续消耗上游配额。
type OrderRefundQueryJob struct{}

// NewOrderRefundQueryJob 构造退款巡检任务。
func NewOrderRefundQueryJob() *OrderRefundQueryJob { return &OrderRefundQueryJob{} }

// Name 返回任务标识。
func (*OrderRefundQueryJob) Name() string { return "order_refund_query" }

// Interval 返回巡检节拍。
func (*OrderRefundQueryJob) Interval() time.Duration { return refundQueryInterval }

// Run 执行一轮退款结果巡检。
func (*OrderRefundQueryJob) Run(ctx context.Context) error {
	// 窗口外的悬挂单**不会**被巡检（第 3 条硬约束），但它们仍然欠着用户钱。
	// 过去这种单子完全静默：既不查、也不报，只有人工去翻订单列表才会发现。
	// 这里每轮统计一次并 Warn —— 只报不改（改状态要人工确认，见 refund.go 的注释）。
	reportRefundHorizonOverdue(ctx)

	ids, err := claimRefundTargets(ctx)
	if err != nil {
		logger.L().Warn("领取退款巡检订单失败", zap.Error(err))
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	queryRefundBatch(ctx, ids)
	return nil
}

// countRefundHorizonOverdue 统计「已超出巡检窗口、仍在 refunding」的订单数。
//
// 只统计 gateway 模式：balance 模式没有出网动作，停在 refunding 说明进程崩在
// 事务与落库之间，属于另一类问题（对账会报），不在这里混算。
func countRefundHorizonOverdue(ctx context.Context) (int64, error) {
	var n int64
	err := db.DB.WithContext(ctx).Model(&model.Order{}).
		Where(fmt.Sprintf("status = ? AND refund_mode = ? AND refund_at < now() - interval '%d hours'",
			int(refundQueryHorizon.Hours())),
			model.OrderRefunding, "gateway").
		Count(&n).Error
	return n, err
}

func reportRefundHorizonOverdue(ctx context.Context) {
	n, err := countRefundHorizonOverdue(ctx)
	if err != nil {
		logger.L().Warn("统计超窗退款单失败", zap.Error(err))
		return
	}
	if n > 0 {
		logger.L().Warn("存在超出巡检窗口仍未确认的退款单，需人工核实上游",
			zap.Int64("count", n), zap.String("action", "核对支付渠道后台后调用 /api/admin/order/refund/confirm"))
	}
}

// claimRefundTargets 领取 + 打标（与查单同一套 SKIP LOCKED 写法，避免多副本重复请求）。
func claimRefundTargets(ctx context.Context) ([]uint64, error) {
	var rows []struct{ ID uint64 }
	sql := fmt.Sprintf(`
		WITH picked AS (
		  SELECT id FROM orders
		  WHERE status = 'refunding' AND deleted_at IS NULL AND refund_mode = 'gateway'
		    AND refund_at > now() - interval '%d hours'
		    AND (last_refund_query_at IS NULL OR last_refund_query_at < now() - interval '%d seconds')
		  ORDER BY id ASC
		  LIMIT ?
		  FOR UPDATE SKIP LOCKED
		)
		UPDATE orders o SET last_refund_query_at = now()
		FROM picked p WHERE o.id = p.id
		RETURNING o.id`,
		int(refundQueryHorizon.Hours()), int(refundQueryCooldown.Seconds()))

	if err := db.DB.WithContext(ctx).Raw(sql, refundQueryBatch).Scan(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// queryRefundBatch 并发巡检；单笔失败只记日志。
func queryRefundBatch(ctx context.Context, ids []uint64) {
	svc := order.New(db.DB)
	sem := make(chan struct{}, refundQueryWorkers)
	var wg sync.WaitGroup
	var failed int64
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

			if err := svc.RefundQueryOnce(ctx, orderID); err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				logger.L().Warn("退款结果巡检失败（不改状态，下轮重试）",
					zap.Uint64("order_id", orderID), zap.Error(err))
			}
		}(id)
	}
	wg.Wait()

	if failed > 0 {
		logger.L().Info("退款结果巡检完成", zap.Int("picked", len(ids)), zap.Int64("failed", failed))
	}
}

// 编译期断言：OrderRefundQueryJob 实现 Job 接口。
var _ Job = (*OrderRefundQueryJob)(nil)
