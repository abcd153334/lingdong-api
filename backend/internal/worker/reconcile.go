package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/reconcile"
)

// 对账节拍：每日一次（只读，成本低；与分钟级业务 Job 解耦）。
const reconcileInterval = 24 * time.Hour

// ReconcileJob 每日执行一次计费对账。
//
// 差异**分两档**，不是一个总数：
//
//	资金档（Error + billing_reconcile_diff）—— 账算错了、钱对不上、该收没收。
//	观察档（Warn  + billing_reconcile_watch）—— 只说明「有东西停在中间态」。
//
// 为什么要分开：pending_old（跑得久的请求）与 affiliate.outstanding（退款时扣不回来
// 的返利）天然长期非零。把它们混进资金档，等于让告警**每天必响**；
// 响到最后没人再看，真正的「余额链断了」就淹没在里面。这是告警系统最常见的死法。
type ReconcileJob struct{}

// NewReconcileJob 构造对账任务。
func NewReconcileJob() *ReconcileJob { return &ReconcileJob{} }

// Name 返回任务标识。
func (*ReconcileJob) Name() string { return "billing_reconcile" }

// Interval 返回节拍。
func (*ReconcileJob) Interval() time.Duration { return reconcileInterval }

// Run 执行一轮对账（只读）。
//
// 执行失败从 Warn 升级为 Error：对账是资金问题的**唯一**自动发现通道，
// 它没跑成等于今天没有任何人看账。降级成 Warn 时，日志里只多一行谁也不会看的
// 记录，而告警规则抓的是 Error。
func (*ReconcileJob) Run(ctx context.Context) error {
	rep, err := reconcile.Run(ctx)
	if err != nil {
		logger.L().Error("计费对账执行失败（今日无对账结论）",
			zap.Bool("billing_reconcile_failed", true), zap.Error(err))
		return nil
	}

	// 资金档：任何一项非零都意味着「账面与事实不符」。
	fundDiff := fundDiffOf(*rep)

	// 观察档：中间态堆积与「口径占比」，需要人看但不等于账错了。
	watchDiff := rep.Users.FrozenMismatch +
		rep.Tasks.PendingAfterTerminal + rep.Tasks.FrozenMismatch +
		rep.Usages.PendingOld + rep.Usages.PendingOrphan +
		// 三个「口径发生占比」的计数器：估算入账、回收入账、零用量成功。
		// 它们不是"账错了"，但每一个都代表平台在替用户垫成本 —— 之前算了却
		// 没有任何消费者，等于少收观测整条断掉。
		rep.Usages.Estimated24h + rep.Usages.Reclaimed24h + rep.Usages.ZeroUsage24h +
		rep.Usages.CalcFailed24h +
		// 有段没跑完本身就是「今天这份结论不完整」，必须落在观察档里可见；
		// 它的计数此刻全是 0，看起来与「查过、无差异」完全一样。
		int64(len(rep.Errors))
	// 冷却中的返利**不进任何一档**：默认 48 小时冷却，正常经营下它每天都非零。
	// 把它当「需要关注」，结果就是每天一条 WARN —— 真正的差异反而没人看。
	// 它是纯观察值，只出现在下面的日志字段里。

	fields := []zap.Field{
		// 资金档
		zap.Int64("chain_mismatch", rep.Users.ChainMismatch),
		zap.Int64("balance_mismatch", rep.Users.BalanceMismatch),
		zap.Int64("negative_frozen", rep.Users.NegativeFrozen),
		zap.Int64("charge_rows", rep.Ledger.ChargeRows),
		zap.Int64("settled_mismatch", rep.Tasks.SettledMismatch),
		zap.Int64("settled_shortfall", rep.Tasks.SettledShortfall),
		zap.Int64("charged_but_failed", rep.Tasks.ChargedButFailed),
		zap.Int64("orders_missing_ledger", rep.Orders.MissingLedger),
		zap.Int64("orders_duplicate_ledger", rep.Orders.DuplicateLedger),
		zap.Int64("orders_pending_but_credited", rep.Orders.PendingButCredited),
		zap.Int64("orders_amount_mismatch", rep.Orders.AmountMismatch),
		zap.Int64("orders_need_manual_uncredited", rep.Orders.NeedManualUncredited),
		zap.Int64("refund_missing_ledger", rep.Refunds.MissingRefundLedger),
		zap.Int64("refund_upstream_only", rep.Refunds.UpstreamOnly),
		zap.Int64("refund_stuck", rep.Refunds.StuckRefunding),
		zap.Strings("indexes_missing", rep.Indexes.Missing),
		// 观察档
		zap.Int64("frozen_mismatch", rep.Users.FrozenMismatch),
		zap.Int64("task_frozen_mismatch", rep.Tasks.FrozenMismatch),
		zap.Int64("pending_after_terminal", rep.Tasks.PendingAfterTerminal),
		zap.Int64("pending_total", rep.Usages.PendingTotal),
		zap.Int64("pending_old", rep.Usages.PendingOld),
		zap.Int64("pending_orphan", rep.Usages.PendingOrphan),
		zap.Int64("estimated_24h", rep.Usages.Estimated24h),
		zap.Int64("reclaimed_24h", rep.Usages.Reclaimed24h),
		zap.Int64("zero_usage_24h", rep.Usages.ZeroUsage24h),
		zap.Int64("calc_failed_24h", rep.Usages.CalcFailed24h),
		zap.Int64("affiliate_leak_rows", rep.Affiliate.LeakRows),
		zap.Int64("affiliate_leak_total", rep.Affiliate.LeakTotal),
		zap.Int64("affiliate_missing_rows", rep.Affiliate.MissingRows),
		zap.Int64("affiliate_transferred_sum", rep.Affiliate.TransferredSum),
		zap.Int64("affiliate_ledger_diff", rep.Affiliate.LedgerDiff),
		zap.Int64("affiliate_cooling_rows", rep.Affiliate.CoolingRows),
		zap.Int64("affiliate_cooling_total", rep.Affiliate.CoolingTotal),
		// 分段执行状态
		zap.Strings("segments_incomplete", rep.Errors),
	}

	if fundDiff > 0 {
		logger.L().Error("计费对账发现资金差异", append([]zap.Field{
			zap.Bool("billing_reconcile_diff", true),
			zap.Int64("fund_diff", fundDiff),
			zap.Int64("watch_diff", watchDiff),
		}, fields...)...)
		return nil
	}
	if watchDiff > 0 {
		logger.L().Warn("计费对账：无资金差异，但有中间态需要关注", append([]zap.Field{
			zap.Bool("billing_reconcile_watch", true),
			zap.Int64("watch_diff", watchDiff),
		}, fields...)...)
		return nil
	}
	logger.L().Info("计费对账通过",
		zap.Int64("users", rep.Users.Checked),
		zap.Int64("transactions", rep.Ledger.TotalRows),
		zap.Int64("estimated_24h", rep.Usages.Estimated24h))
	return nil
}

// absInt64 把差额转成「非零即异常」的计数。
//
// 返利转出与 invite 流水之差有正负两个方向（标记了没入账 / 入账了没标记），
// 但两者都是错的，求和时必须取绝对值，否则同日一正一负会互相抵消，
// 账面看起来完全正常。
func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// fundDiffOf 汇总「资金档」差异：任何一项非零都意味着账面与事实不符。
//
// 独立成函数是为了能被用例直接断言 —— 这一类判据最容易出的问题不是算法，
// 而是「采集了却忘了加进来」（negative_frozen 就这样躺了几个月）。
func fundDiffOf(rep reconcile.Report) int64 {
	d := rep.Users.ChainMismatch + rep.Users.BalanceMismatch +
		// 负冻结 = 凭空多冻了用户的钱（用户可用余额没少、但被记成欠着）。
		// 这条判据早就采了，却一直没进任何一档 —— 采了没人消费，等于没查。
		rep.Users.NegativeFrozen +
		rep.Ledger.ChargeRows +
		rep.Tasks.SettledMismatch + rep.Tasks.SettledShortfall + rep.Tasks.ChargedButFailed +
		rep.Orders.MissingLedger + rep.Orders.DuplicateLedger + rep.Orders.PendingButCredited +
		rep.Orders.AmountMismatch + rep.Orders.NeedManualUncredited +
		rep.Refunds.MissingRefundLedger + rep.Refunds.UpstreamOnly +
		rep.Refunds.StuckRefunding + int64(len(rep.Indexes.Missing)) +
		// 返利应作废未作废：钱已经退给用户、返利却还能被转出 = 实打实的资损。
		rep.Affiliate.LeakRows +
		// 已入账却没发返利：用户少拿了钱，是平台欠用户，同样是资金差异。
		rep.Affiliate.MissingRows +
		// 转出行与 invite 流水不平：正=标记了没入账，负=入账了没标记。
		absInt64(rep.Affiliate.LedgerDiff)
	return d
}
