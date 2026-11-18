package stats

import (
	"context"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// rechargeTypes 是「充值」统计包含的资金流水类型。
//
// 用白名单而不是黑名单：transactions 里还有 freeze/release/settle/redeem/adjust/
// invite/refund_revert 等类型，漏进来任何一个都会把「充值」变成「全部资金变动」。
// 新增类型时必须在这里显式决定是否计入 —— 不写即不计入，比默认计入安全。
var rechargeTypes = []string{
	model.TxnRecharge,     // 支付到账（金额写的是 orders.credited，含套餐赠送）
	model.TxnRefund,       // 退款扣回（负数）
	model.TxnRefundRevert, // 退款撤销（正数）
}

// RechargeBetween 返回 [from, to) 内的**净充值**（微）。全系统唯一的充值聚合出口：
// 门户主页、门户趋势图、管理台看板、管理台趋势全部走这里，禁止任何地方直接 SUM orders。
//
// userID == 0 表示全平台（管理台看板用）；> 0 按用户过滤。
//
// 为什么是 transactions 而不是 orders.amount_cent：
//
//  1. **赠送**。orders.amount_cent 是实付（分），而用户看到余额涨的是 credited
//     （= 实付 + 套餐赠送，见 service/order/order.go 的 credit = base + Bonus）。
//     实测存在 amount_cent=100（1 元）对应 credited≈2 元的订单：用 amount_cent
//     会让卡片写「本月充值 ¥1」而余额涨了 ¥2，「充值 − 消耗 ≈ 余额变化」永远不成立。
//
//  2. **退款**。退款会把 orders.status 从 paid 改成 refunding/refunded，而统计条件
//     恒为 status='paid' —— 于是一笔退款会让**已经过去的上个月**数字变小，
//     用户翻历史账单会发现同一段时间的合计变了。流水里 refund 是一条负数，
//     天然得到净额，且历史区间不会被后来发生的退款改写。
//
//  3. **同源**。门户「资金流水」页签读的就是这张表，同一个数字只有一个来源。
func RechargeBetween(ctx context.Context, userID uint64, from, to time.Time) (int64, error) {
	sql := `SELECT COALESCE(SUM(amount), 0) FROM transactions
	         WHERE type IN ? AND created_at >= ? AND created_at < ?`
	args := []any{rechargeTypes, from, to}
	if userID > 0 {
		sql = `SELECT COALESCE(SUM(amount), 0) FROM transactions
		        WHERE user_id = ? AND type IN ? AND created_at >= ? AND created_at < ?`
		args = []any{userID, rechargeTypes, from, to}
	}
	var sum int64
	if err := db.DB.WithContext(ctx).Raw(sql, args...).Scan(&sum).Error; err != nil {
		return 0, err
	}
	return sum, nil
}

// RechargeDaily 返回 [from, to) 内按**业务日**分桶的净充值（微），key 为 "2006-01-02"。
//
// 分桶必须用 `AT TIME ZONE 业务时区`：与 RechargeBetween 用同一套 created_at 区间，
// 但 GROUP BY 需要一个日期标签。若这里改用 DB 会话时区，就会出现
// 「合计对得上、每天的点错一格」——趋势图看起来正常，逐日核对全错位。
func RechargeDaily(ctx context.Context, userID uint64, from, to time.Time) (map[string]int64, error) {
	var rows []struct {
		Date   time.Time
		Amount int64
	}
	sql := `SELECT (created_at AT TIME ZONE ?)::date AS date, COALESCE(SUM(amount), 0) AS amount
	          FROM transactions
	         WHERE type IN ? AND created_at >= ? AND created_at < ?
	         GROUP BY 1`
	args := []any{Location().String(), rechargeTypes, from, to}
	if userID > 0 {
		sql = `SELECT (created_at AT TIME ZONE ?)::date AS date, COALESCE(SUM(amount), 0) AS amount
		         FROM transactions
		        WHERE user_id = ? AND type IN ? AND created_at >= ? AND created_at < ?
		        GROUP BY 1`
		args = []any{Location().String(), userID, rechargeTypes, from, to}
	}
	if err := db.DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Date.Format("2006-01-02")] = r.Amount
	}
	return out, nil
}
