package app

// 应用的统计视图（概览图 + 在线状态）。
//
// 边界：这里只放"随时间和流量变化"的数字。四格汇总（累计消费/已付充值/密钥数/
// 用户数）不在这里 —— 它由列表接口随卡片一起下发，弹窗直接复用那一份。
// 两处各算一份的后果是"卡片 100、弹窗 98"，而运营只会问"到底哪个对"。

import (
	"context"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/stats"
)

// TrendPoint 是概览图的一个点（按业务时区分日，缺失日补 0）。
type TrendPoint struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
	Charge   int64  `json:"charge"`
}

// OnlineView 是「在线」页签的指标。
//
// 刻意**不含** active_key_count：它与列表接口的 agg.active_key_count 是同一个
// 事实，两处各查一次 api_keys 只会得到两个可能不一致的数。
type OnlineView struct {
	// Online：online（≤5 分钟）/ idle（≤60 分钟）/ offline。
	Online     string  `json:"online"`
	LastCallAt string  `json:"last_call_at"`
	Calls24h   int64   `json:"calls_24h"`
	Errors24h  int64   `json:"errors_24h"`
	ErrorRate  float64 `json:"error_rate"`
	Charge24h  int64   `json:"charge_24h"`
}

// StatsView 是 GET /admin/apps/{id}/stats 的返回体。
//
// 概览页签与在线页签读**同一份**：两个仪表盘各算一份的下场是
// "概览说今天 3,241 次、在线说 3,199 次"，而谁都不知道哪个对。
type StatsView struct {
	Trend  []TrendPoint `json:"trend"`
	Online OnlineView   `json:"online"`
}

// 在线判定的两个阈值。刻意写死而不是做成配置：
// 它们只影响"运营看到的灯是什么颜色"，不影响任何计费或准入，
// 做成配置只会多一个需要同步的键。
const (
	onlineWindow = 5 * time.Minute
	idleWindow   = 60 * time.Minute
)

// StatsOf 组装统计：3 条 SQL（最近调用时间 / 24h 聚合 / 7 日趋势）。
func StatsOf(ctx context.Context, gdb *gorm.DB, appID uint64, now time.Time) (StatsView, error) {
	trend, err := trendOf(ctx, gdb, appID, now)
	if err != nil {
		return StatsView{}, err
	}
	lastUsed, err := lastUsedOf(ctx, gdb, appID)
	if err != nil {
		return StatsView{}, err
	}
	ua, err := usage24hOf(ctx, gdb, appID, now)
	if err != nil {
		return StatsView{}, err
	}
	return StatsView{Trend: trend, Online: onlineFrom(lastUsed, ua, now)}, nil
}

// usage24h 是 24 小时聚合的中间结果。
type usage24h struct {
	Calls  int64
	Errors int64
	Charge int64
}

// trendOf 取最近 7 个自然日（业务时区）的调用数/错误数/消费。
//
// 为什么不用 usage_daily：它的主键是 (date, model, modality, user_id)，
// **没有 key/app 维度**，无法按应用聚合。这里走 usages JOIN api_keys。
// JOIN 不过滤 api_keys.deleted_at：密钥软删之后，历史用量仍属于该应用
// （账单不能因为"用户删了密钥"而少一段）。
func trendOf(ctx context.Context, gdb *gorm.DB, appID uint64, now time.Time) ([]TrendPoint, error) {
	loc := stats.Location()
	biz := now.In(loc)
	start := time.Date(biz.Year(), biz.Month(), biz.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -6)

	type row struct {
		Date     string
		Requests int64
		Errors   int64
		Charge   int64
	}
	var rows []row
	if err := gdb.WithContext(ctx).Raw(
		`SELECT to_char(date_trunc('day', u.created_at AT TIME ZONE ?), 'YYYY-MM-DD') AS date,
		        COUNT(*) AS requests,
		        COUNT(*) FILTER (WHERE u.http_status >= 400) AS errors,
		        COALESCE(SUM(u.charge), 0) AS charge
		 FROM usages u
		 JOIN api_keys k ON k.id = u.key_id
		 WHERE k.app_id = ? AND k.kind = ? AND u.created_at >= ?
		 GROUP BY 1 ORDER BY 1`,
		loc.String(), appID, model.KeyKindHosted, start).Scan(&rows).Error; err != nil {
		return nil, err
	}

	have := make(map[string]row, len(rows))
	for _, r := range rows {
		have[r.Date] = r
	}
	// 固定 7 个点并补 0：前端按点数画图，缺日会让"某天没调用"看起来像
	// "那天数据丢了"，也让两根柱子的间距不相等。
	out := make([]TrendPoint, 0, 7)
	for i := 0; i < 7; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		r := have[d]
		out = append(out, TrendPoint{Date: d, Requests: r.Requests, Errors: r.Errors, Charge: r.Charge})
	}
	return out, nil
}

// lastUsedOf 取该应用最近一次调用时间（来自 api_keys.last_used_at，5 分钟节流写入）。
//
// 单独一条查询而不是混进 24h 聚合：前者是"最后一次"，后者是"24 小时内多少次"，
// 语义不同、表也不同（api_keys vs usages），合成一条只会多一个易错的 join。
func lastUsedOf(ctx context.Context, gdb *gorm.DB, appID uint64) (*time.Time, error) {
	var row struct {
		LastCall *time.Time
	}
	if err := gdb.WithContext(ctx).Raw(
		`SELECT MAX(last_used_at) AS last_call
		 FROM api_keys WHERE app_id = ? AND kind = ? AND deleted_at IS NULL`,
		appID, model.KeyKindHosted).Scan(&row).Error; err != nil {
		return nil, err
	}
	return row.LastCall, nil
}

// usage24hOf 取近 24 小时的调用数/错误数/消费。
func usage24hOf(ctx context.Context, gdb *gorm.DB, appID uint64, now time.Time) (usage24h, error) {
	var out usage24h
	since := now.Add(-24 * time.Hour)
	if err := gdb.WithContext(ctx).Raw(
		`SELECT COUNT(*) AS calls,
		        COUNT(*) FILTER (WHERE u.http_status >= 400) AS errors,
		        COALESCE(SUM(u.charge), 0) AS charge
		 FROM usages u JOIN api_keys k ON k.id = u.key_id
		 WHERE k.app_id = ? AND k.kind = ? AND u.created_at >= ?`,
		appID, model.KeyKindHosted, since).Scan(&out).Error; err != nil {
		return usage24h{}, err
	}
	return out, nil
}

// onlineFrom 把"最近调用时间 + 24h 聚合"折算成对外指标（纯函数，便于单测）。
func onlineFrom(lastUsed *time.Time, ua usage24h, now time.Time) OnlineView {
	out := OnlineView{
		Online:    "offline",
		Calls24h:  ua.Calls,
		Errors24h: ua.Errors,
		Charge24h: ua.Charge,
	}
	if ua.Calls > 0 {
		// 直接给比值（不做四舍五入）：前端按百分比展示，0.4% 这种值在折算成
		// 整数百分比时会变成 0%；把精度留给展示层，判断与展示才用同一份数。
		out.ErrorRate = float64(ua.Errors) / float64(ua.Calls)
	}
	if lastUsed != nil {
		// 截到分钟：秒级时间戳会让"最近调用"在两次刷新之间跳动，
		// 看起来像有流量但其实只是同一个请求的两次读取。
		out.LastCallAt = lastUsed.In(stats.Location()).Format("2006-01-02 15:04")
		switch age := now.Sub(*lastUsed); {
		case age <= onlineWindow:
			out.Online = "online"
		case age <= idleWindow:
			out.Online = "idle"
		}
	}
	return out
}
