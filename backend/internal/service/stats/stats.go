// Package stats 是数据面（S11）：usage_daily 聚合、管理台看板与门户主页聚合、数据保留维护。
//
// **单位口径：本包对外（JSON）下发的一切金额都是「微」**，没有例外。充值也是：
// 自从改读 `transactions`（`amount` 列本身就是微，写入时取自 `orders.credited`），
// 本包不再做任何「分 → 微」的换算。订单侧的 `amount_cent` 是支付渠道的记账单位，
// 只出现在订单接口里，不进统计契约。
//
// 历史事故：这里曾经同时下发「消耗（微）」与「充值（分）」，前端为了画一张
// 同量纲的图，只能在页面里各自调一次换算函数。某个页面漏掉换算，
// 「今日毛利」就会显示成成本的一万分之一 —— 金额量纲一旦在契约层混用，
// 每个消费方都要自己记得换算，漏一个就是一个错账数字。
package stats

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/iconindex"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/pricing"
)

const (
	trendDays  = 7
	topLimit   = 8
	userLimit  = 10
	archiveLot = 1000
)

/* ------------------------------ usage_daily 聚合 ------------------------------ */

// RollupLookbackDays 是重算窗口（天）：默认 3 天。
//
// 原实现只重算「昨天 + 今天」，而 usages 行可能在结算后才被写入终态
// （异步任务结算跨天、回收任务 10 分钟后补账、归档前的手工修数），
// 只回看 1 天会让这些迟到的终态永远进不了聚合。3 天足够覆盖全部回写路径，
// 又不会让单轮扫描量失控（LD_USAGE_ROLLUP_DAYS 可调，0/负值回落默认）。
func RollupLookbackDays() int {
	if n := conf.EnvConfig().UsageRollupDays; n > 0 {
		return n
	}
	return 3
}

// RollupDaily 重新聚合最近 N 天的 usages → usage_daily（幂等 UPSERT）。
// 每次整窗重算而非增量累加，保证聚合结果与明细求和恒等（S11-R2 复算口径）。
//
// 新增列必须**三处同改**（INSERT 列 / SELECT 聚合 / ON CONFLICT SET）：
// 漏一处不会报错，只会被下一次 rollup 静默刷回 0 —— 这是聚合类改动最典型的坑。
func RollupDaily(ctx context.Context) (int64, error) {
	now := BizNow()
	// 起点取「N 天前的 0 点」而非「now - N×24h」：日桶以自然日为单位，
	// 用滚动 72 小时会在凌晨把前天的部分行算进来、却把前天的完整桶排除在外。
	start, _ := DayRange(now.AddDate(0, 0, -RollupLookbackDays()))
	_, end := DayRange(now)
	return RollupDailyWindow(ctx, start, end)
}

// RollupDailyWindow 重算 [start, end) 区间内的全部日桶。供 RollupDaily 与
// 「停机后补账」的宽窗调用共用 —— 扩窗逻辑只有调用方决定区间，聚合本身不变。
func RollupDailyWindow(ctx context.Context, start, end time.Time) (int64, error) {
	// date 桶必须按**业务时区**折算。改造前这里写的是 created_at::date，
	// 用的是数据库会话时区：DB 在 UTC、业务在 +08 时，每天 08:00 之前的调用
	// 会被塞进前一天的桶，而门户按业务时区读 —— 两边各算各的，永远差一格。
	//
	// 上界 created_at < end 不是冗余：时钟偏斜或补数据写入未来时间戳时，
	// 那些行会生成一个「未来日桶」，而未来那次 rollup 的窗口已经越过它，
	// 于是这个桶永远停在第一次写入的样子，再也不会被重算。
	res := db.DB.WithContext(ctx).Exec(`
INSERT INTO usage_daily (date, model, modality, user_id, requests, errors, prompt_tokens, completion_tokens, charge, cost)
SELECT (created_at AT TIME ZONE ?)::date, model, modality, user_id,
       COUNT(*),
       COUNT(*) FILTER (WHERE status IN ('released', 'rejected')),
       COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
       COALESCE(SUM(charge), 0), COALESCE(SUM(cost), 0)
  FROM usages
 WHERE created_at >= ? AND created_at < ?
 GROUP BY 1, 2, 3, 4
ON CONFLICT (date, model, modality, user_id) DO UPDATE
   SET requests = EXCLUDED.requests,
       errors = EXCLUDED.errors,
       prompt_tokens = EXCLUDED.prompt_tokens,
       completion_tokens = EXCLUDED.completion_tokens,
       charge = EXCLUDED.charge,
       cost = EXCLUDED.cost`, Location().String(), start, end)
	return res.RowsAffected, res.Error
}

/* ------------------------------ 数据保留维护 ------------------------------ */

// archiveColumns 是 usages ↔ usages_archive 的同构列清单（"group" 为保留字须加引号）。
//
// 铁律：**model.Usage 新增任何列，这里必须同步加**，否则归档时该列的值会被静默丢弃
// （DELETE ... RETURNING 与 INSERT 用的是同一份清单，漏掉的列既不会报错也不会留痕）。
// stats/archive_columns_test.go 用反射逐列比对，漂移会让测试直接红。
const archiveColumns = `id, created_at, user_id, key_id, channel_id, channel_key_id, adapter_id, model, modality,
	http_status, kind, status, prompt_tokens, completion_tokens, cache_tokens,
	cache_write5m_tokens, cache_write1h_tokens, image_tokens, audio_input_tokens, audio_output_tokens,
	image_spec, image_ratio, image_spec_source, image_quality, image_n, upstream_spec, audio_duration_secs, audio_chars, rerank_docs,
	idem_key, idem_scope,
	cost, charge, pricing_snapshot, latency_ms, error_msg, error_code, request_digest,
	is_stream, "group", ip, request_id, upstream_request_id,
	usage_source, raw_usage, est_prompt_tokens, est_completion_tokens, anomaly,
	call_kind, usage_semantic, video_resolution, video_seconds, video_seconds_source,
	ref_images, ref_videos, ref_audios, ref_video_seconds,
	tier_price, tier_source, charge_before_cap, cap_reason, frozen,
	started_at, finished_at, upstream_ms, first_token_ms, attempts, task_id, currency,
	bill_prompt_tokens, bill_completion_tokens`

// ArchiveUsages 将超过 logs_retention_days 的明细按批迁入 usages_archive（冷热分离）。
func ArchiveUsages(ctx context.Context, batch int) (int64, error) {
	days := conf.System().Int("logs_retention_days", 180)
	if batch <= 0 {
		batch = archiveLot
	}
	// 截止点用业务日的 0 点：保留 N 天是运营口径（「保留到业务日的哪一天」），
	// 原来写 CURRENT_DATE 会让它在会话时区下浮动最多一天。
	cutoff, _ := DayRange(BizNow())
	cutoff = cutoff.AddDate(0, 0, -int(days))
	res := db.DB.WithContext(ctx).Exec(`
WITH batch AS (
    SELECT id FROM usages
     WHERE created_at < ?
       AND status <> 'pending'
     ORDER BY id LIMIT ?
), moved AS (
    DELETE FROM usages WHERE id IN (SELECT id FROM batch)
    RETURNING `+archiveColumns+`
)
INSERT INTO usages_archive (`+archiveColumns+`)
SELECT `+archiveColumns+` FROM moved
ON CONFLICT (id) DO NOTHING`, cutoff, batch)
	return res.RowsAffected, res.Error
}

// PurgeRetention 清理超期归档明细与审计记录（archive_retention_days / audit_retention_days）。
func PurgeRetention(ctx context.Context) (int64, error) {
	archDays := conf.System().Int("archive_retention_days", 365)
	auditDays := conf.System().Int("audit_retention_days", 180)
	dayStart, _ := DayRange(BizNow())
	a := db.DB.WithContext(ctx).Exec(`DELETE FROM usages_archive WHERE created_at < ?`, dayStart.AddDate(0, 0, -int(archDays)))
	b := db.DB.WithContext(ctx).Exec(`DELETE FROM admin_audit WHERE created_at < ?`, dayStart.AddDate(0, 0, -int(auditDays)))
	if a.Error != nil {
		return a.RowsAffected, a.Error
	}
	return a.RowsAffected + b.RowsAffected, b.Error
}

/* ------------------------------ 看板聚合 ------------------------------ */

// Board 是管理台看板视图（G17 / §4.9.4）。
type Board struct {
	// Currency / CurrencySymbol 是看板金额的展示口径（结算币种）。
	//
	// 顶层下发而不是让前端自己取：看板里所有金额都是微，前端要知道
	// 除以 1e6 之后该配哪个符号，否则人民币部署下会显示成「$12,345」。
	Currency       string               `json:"currency"`
	CurrencySymbol string               `json:"currency_symbol"`
	Today          TodayView            `json:"today"`
	Trend          []TrendPoint         `json:"trend"`
	FailTop        []RankView           `json:"fail_top"`
	ChannelTop     []RankView           `json:"channel_top"`
	Channels       []ChannelView        `json:"channels"`
	Boards         map[string]RangeView `json:"boards"`
}

// TodayView 是今日经营指标；**全部金额单位微**（与订单表的分不同，见包注释）。
type TodayView struct {
	Requests       int64   `json:"requests"`
	Recharge       int64   `json:"recharge"` // 今日已支付订单实付（微）
	Charge         int64   `json:"charge"`
	Cost           int64   `json:"cost"`
	CostCollected  bool    `json:"cost_collected"`
	SuccessRate    float64 `json:"success_rate"`
	ChannelsOnline int     `json:"channels_online"`
	ChannelsTotal  int     `json:"channels_total"`
	PriceMissing   int64   `json:"price_missing_count"`
}

// TrendPoint 是趋势单点（近 7 日；**全部金额单位微**，因此可与成本直接相减）。
type TrendPoint struct {
	Date     string `json:"date"`
	Recharge int64  `json:"recharge"`
	Charge   int64  `json:"charge"`
	Cost     int64  `json:"cost"`
}

// RankView 是横向排行项（label 展示名，value 指标值）。
type RankView struct {
	Label string `json:"label"`
	Value int64  `json:"value"`
	// IconURL 是实体图标的可渲染地址（模型榜/失败榜=模型图标，渠道榜=渠道图标）。
	//
	// omitempty：用户榜（UserTop）没有对应实体图标，带上空串只是给前端多一个
	// 必须判断的字段。模型/渠道榜一定有这个键（可能是 ""），前端据此决定画不画图。
	IconURL string `json:"icon_url,omitempty"`
}

// ChannelView 是渠道健康视图（含熔断态）。
type ChannelView struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	// IconURL 是渠道图标的可渲染地址（"" = 首字占位）：渠道没有回落语义，存的就是展示值。
	IconURL      string  `json:"icon_url"`
	Status       int     `json:"status"`
	AutoBan      bool    `json:"auto_ban"`
	FailRate     float64 `json:"fail_rate"`
	SuccessCount int     `json:"success_count"`
	FailCount    int     `json:"fail_count"`
}

// UserAggView 是用户聚合明细行（分组取用户当前计费分组）。
type UserAggView struct {
	UserID   uint64 `json:"user_id"`
	Username string `json:"username"`
	Group    string `json:"group"`
	Requests int64  `json:"requests"`
	Charge   int64  `json:"charge"`
}

// RangeView 是单档（d7 / d30）聚合结果，供看板切换零请求消费。
type RangeView struct {
	UserTop  []RankView    `json:"user_top"`
	ModelTop []RankView    `json:"model_top"`
	Users    []UserAggView `json:"users"`
}

// BoardData 聚合看板全量（今日 + 近 7 日趋势 + d7/d30 双档）。
func BoardData(ctx context.Context, base string) (*Board, error) {
	b := &Board{Trend: []TrendPoint{}, ChannelTop: []RankView{}, Boards: map[string]RangeView{}}
	b.Currency, b.CurrencySymbol = conf.SiteCurrency(), conf.SiteCurrencySymbol()
	if err := b.fillToday(ctx); err != nil {
		return nil, err
	}
	if err := b.fillTrend(ctx); err != nil {
		return nil, err
	}
	if err := b.fillFailTop(ctx, base); err != nil {
		return nil, err
	}
	if err := b.fillChannelTop(ctx, base); err != nil {
		return nil, err
	}
	if err := b.fillChannels(ctx, base); err != nil {
		return nil, err
	}
	for _, r := range [2]struct {
		key  string
		days int
	}{{"d7", 7}, {"d30", 30}} {
		v, err := rangeBoard(ctx, r.days, base)
		if err != nil {
			return nil, err
		}
		b.Boards[r.key] = v
	}
	return b, nil
}

func (b *Board) fillToday(ctx context.Context) error {
	start, end := DayRange(BizNow())

	var row struct {
		Requests int64
		Charge   int64
		Cost     int64
		Settled  int64
	}
	// 成功判据用 status='settled' 而不是 http_status ∈ [200,300)：
	// 范围看板（rangeBoard）与 usage_daily.errors 都以 status 为唯一口径，
	// 两处各用一套的结果是「今日成功率」与「本月失败数」永远算不通。
	// pending 行既不是成功也不是失败，所以它只进分母 —— 这与「请求还没结束」的直觉一致。
	if err := db.DB.WithContext(ctx).Raw(`
SELECT COUNT(*) AS requests, COALESCE(SUM(charge), 0) AS charge,
       COALESCE(SUM(cost), 0) AS cost,
       COUNT(*) FILTER (WHERE status = 'settled') AS settled
  FROM usages WHERE created_at >= ? AND created_at < ?`, start, end).Scan(&row).Error; err != nil {
		return err
	}
	b.Today.Requests, b.Today.Charge, b.Today.Cost = row.Requests, row.Charge, row.Cost
	if row.Requests > 0 {
		b.Today.SuccessRate = float64(row.Settled) / float64(row.Requests)
	}

	// 本日充值：与门户走同一个出口（transactions 净额，含赠送、已扣退款）。
	recharge, err := RechargeBetween(ctx, 0, start, end)
	if err != nil {
		return err
	}
	b.Today.Recharge = recharge

	// 成本采集状态：近 7 日内是否存在上游自报成本
	var collected int64
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM usages WHERE created_at >= ? AND created_at < ? AND cost > 0`,
		start.AddDate(0, 0, -6), end,
	).Scan(&collected).Error; err != nil {
		return err
	}
	b.Today.CostCollected = collected > 0

	// 未配价模型计数（§4.5.5 防白嫖 / §4.9.6 看板计数）
	cat, err := pricing.Get()
	if err != nil {
		return err
	}
	b.Today.PriceMissing = countPriceMissing(cat)
	return nil
}

// countPriceMissing 统计启用但无法出价的模型数（口径同 billing.Estimate 的 ErrPriceMissing）。
//
// 判据必须与计费同源：按量看有没有任何 token 通道价，按次看 call 价，
// 按规格看 spec_prices 是否为空。各写一套的结果是「看板说 0 个未配价、
// 用户一调就 500」。
func countPriceMissing(cat *pricing.Snapshot) int64 {
	var n int64
	for i := range cat.Models {
		it := &cat.Models[i]
		if !it.Enabled {
			continue
		}
		// 单一判据：与启动体检、运行时容错共用 billing.PriceProblem。
		// 以前这里用 SpecPrices.IsZero()，而 kind=per_image 但价目全空时它不是零值 ——
		// 于是看板报 0 个未配价，用户一调就 400。
		if billing.PriceProblem(it.BillingMode, it.Prices, it.SpecPrices) != "" {
			n++
		}
	}
	return n
}

// fillTrend 组装近 7 日趋势（消耗/成本取 usage_daily，充值取 transactions）。
func (b *Board) fillTrend(ctx context.Context) error {
	now := BizNow()
	from, tomorrow := TrendWindow(now, trendDays)

	type dayRow struct {
		Date   time.Time
		Charge int64
		Cost   int64
	}
	var usage []dayRow
	if err := db.DB.WithContext(ctx).Raw(`
SELECT date, COALESCE(SUM(charge), 0) AS charge, COALESCE(SUM(cost), 0) AS cost
  FROM usage_daily WHERE date >= ?::date AND date <= ?::date GROUP BY date`,
		DayKey(from), DayKey(now)).Scan(&usage).Error; err != nil {
		return err
	}

	// 充值走唯一出口（transactions 净额）；与 fillToday 同一套区间语义 [from, tomorrow)
	recharge, err := RechargeDaily(ctx, 0, from, tomorrow)
	if err != nil {
		return err
	}

	charge := map[string]int64{}
	cost := map[string]int64{}
	for _, r := range usage {
		k := r.Date.Format("2006-01-02")
		charge[k], cost[k] = r.Charge, r.Cost
	}

	for i := 0; i < trendDays; i++ {
		k := DayKey(from.AddDate(0, 0, i))
		b.Trend = append(b.Trend, TrendPoint{
			Date: k, Recharge: recharge[k],
			Charge: charge[k], Cost: cost[k],
		})
	}
	return nil
}

// fillFailTop 取近 7 日失败 Top5（按模型；失败 = 4xx/5xx 或已释放）。
func (b *Board) fillFailTop(ctx context.Context, base string) error {
	b.FailTop = []RankView{}
	// 窗口与趋势图同一处（TrendWindow）：同屏两处「近 7 日」必须是同一个 7 日
	from, end := TrendWindow(BizNow(), trendDays)
	if err := db.DB.WithContext(ctx).Raw(`
SELECT model AS label, COUNT(*) AS value
  FROM usages
 WHERE created_at >= ? AND created_at < ? AND (http_status >= 400 OR status = 'released')
 GROUP BY model ORDER BY value DESC LIMIT 5`, from, end).Scan(&b.FailTop).Error; err != nil {
		return err
	}
	fillModelIcons(ctx, b.FailTop, base)
	return nil
}

// fillModelIcons 给排行榜补模型图标（label 就是 model id）。
//
// 这里是**唯一**的排行榜补图标点：失败榜与消耗榜都调它，
// 两处各写一遍的结果是有一个榜单忘了补，用户看到"一个图有一个没有"。
func fillModelIcons(ctx context.Context, rows []RankView, base string) {
	if len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].Label)
	}
	icons := iconindex.Resolve(ctx, ids, nil)
	for i := range rows {
		rows[i].IconURL = iconref.URL(base, icons.Model(rows[i].Label))
	}
}

// fillChannelTop 取近 7 日渠道消耗 Top8（按 channel_id 聚合 charge；无名渠道回退 #ID）。
func (b *Board) fillChannelTop(ctx context.Context, base string) error {
	b.ChannelTop = []RankView{}
	// icon 取 MAX()：它只在 GROUP BY 的渠道维度内取值，MAX 只是让它合法聚合。
	// 扫进临时行再转 RankView：SQL 里拿到的是 token，对外要的是地址，
	// 直接 Scan 到 RankView 就没有那个"转"的落点了。
	var rows []struct {
		Label     string
		Value     int64
		IconToken string
	}
	// 与 fillTrend / fillFailTop 同一窗口语义
	from, end := TrendWindow(BizNow(), trendDays)
	if err := db.DB.WithContext(ctx).Raw(`
SELECT COALESCE(channels.name, '#' || usages.channel_id::text) AS label,
       COALESCE(SUM(usages.charge), 0) AS value,
       COALESCE(MAX(channels.icon), '') AS icon_token
  FROM usages
  LEFT JOIN channels ON channels.id = usages.channel_id
 WHERE usages.created_at >= ? AND usages.created_at < ? AND usages.channel_id > 0
 GROUP BY usages.channel_id, channels.name ORDER BY value DESC LIMIT 8`, from, end).Scan(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		b.ChannelTop = append(b.ChannelTop, RankView{
			Label: r.Label, Value: r.Value, IconURL: iconref.URL(base, r.IconToken),
		})
	}
	return nil
}

// fillChannels 组装渠道健康与在线计数。
func (b *Board) fillChannels(ctx context.Context, base string) error {
	var rows []struct {
		ID      uint64
		Name    string
		Icon    string
		Status  int
		AutoBan bool
		Health  model.JSON
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, name, COALESCE(icon, '') AS icon, status, auto_ban, health
		   FROM channels WHERE deleted_at IS NULL ORDER BY priority, id`,
	).Scan(&rows).Error; err != nil {
		return err
	}
	b.Channels = make([]ChannelView, 0, len(rows))
	for _, r := range rows {
		v := ChannelView{ID: r.ID, Name: r.Name, IconURL: iconref.URL(base, r.Icon), Status: r.Status, AutoBan: r.AutoBan}
		var h struct {
			FailRate     float64 `json:"fail_rate"`
			SuccessCount int     `json:"success_count"`
			FailCount    int     `json:"fail_count"`
		}
		if len(r.Health) > 0 {
			_ = json.Unmarshal(r.Health, &h)
		}
		v.FailRate, v.SuccessCount, v.FailCount = h.FailRate, h.SuccessCount, h.FailCount
		b.Channels = append(b.Channels, v)
	}
	b.Today.ChannelsTotal = len(b.Channels)
	for _, c := range b.Channels {
		if c.Status == 1 {
			b.Today.ChannelsOnline++
		}
	}
	return nil
}

// rangeBoard 按档位聚合用户/模型消耗 Top 与用户聚合明细。
func rangeBoard(ctx context.Context, days int, base string) (RangeView, error) {
	out := RangeView{UserTop: []RankView{}, ModelTop: []RankView{}, Users: []UserAggView{}}

	// 窗口按业务日算好再传（date 是业务日键）：写成 `date >= CURRENT_DATE - n` 会让
	// d7/d30 的边界跟着 DB 会话时区漂移，与同屏的趋势图差一格。
	now := BizNow()
	from, _ := TrendWindow(now, days)
	fromKey, toKey := DayKey(from), DayKey(now)

	var aggs []struct {
		UserID   uint64
		Requests int64
		Charge   int64
	}
	if err := db.DB.WithContext(ctx).Raw(`
SELECT user_id, COALESCE(SUM(requests), 0) AS requests, COALESCE(SUM(charge), 0) AS charge
  FROM usage_daily WHERE date >= ?::date AND date <= ?::date
 GROUP BY user_id ORDER BY charge DESC LIMIT ?`, fromKey, toKey, userLimit).Scan(&aggs).Error; err != nil {
		return out, err
	}
	ids := make([]uint64, 0, len(aggs))
	for _, a := range aggs {
		ids = append(ids, a.UserID)
	}
	names := map[uint64]model.User{}
	if len(ids) > 0 {
		var us []model.User
		if err := db.DB.WithContext(ctx).Select("id", "username", "group").Where("id IN ?", ids).Find(&us).Error; err != nil {
			return out, err
		}
		for _, u := range us {
			names[u.ID] = u
		}
	}
	for _, a := range aggs {
		u := names[a.UserID]
		label := u.Username
		if label == "" {
			label = "#" + strconv.FormatUint(a.UserID, 10)
		}
		out.UserTop = append(out.UserTop, RankView{Label: label, Value: a.Charge})
		out.Users = append(out.Users, UserAggView{
			UserID: a.UserID, Username: label, Group: u.Group, Requests: a.Requests, Charge: a.Charge,
		})
	}

	if err := db.DB.WithContext(ctx).Raw(`
SELECT model AS label, COALESCE(SUM(charge), 0) AS value
  FROM usage_daily WHERE date >= ?::date AND date <= ?::date
 GROUP BY model ORDER BY value DESC LIMIT ?`, fromKey, toKey, topLimit).Scan(&out.ModelTop).Error; err != nil {
		return out, err
	}
	fillModelIcons(ctx, out.ModelTop, base)
	return out, nil
}

/* --------------------------- 门户主页聚合（S15） --------------------------- */

// UserOverviewView 是门户主页（Bento 工作台）的聚合视图。
//
// 口径（v2）：
//   - **Today 实时直查 `usages` 明细**。与管理台看板的 `fillToday` 同源（那里一直是实时）。
//     改造前这里读 `usage_daily`，而它每小时才 rollup 一次 —— 刚调完接口卡片最长 1 小时
//     不动，且同一时刻门户与管理台两个「今日」数字不同，等于同一指标两个真相源。
//   - **Month = `usage_daily`（月初~昨日）+ Today**。单用户满速一个月可达 259 万行明细，
//     月区间绝不能直查 `usages`。
//   - **Trend 消耗取 `usage_daily`，但末点强制等于 Today**（否则卡片与图上最后一根打架）。
//   - **充值统一走 `RechargeBetween`/`RechargeDaily`（transactions 净额）**，不再读 orders。
//
// 余额与冻结**不在此视图内**：由既有 `/api/portal/me` 独占提供（users 列仅由 service/billing 写入），
// 避免同一数据出现两个出口（单一真相源）。
type UserOverviewView struct {
	Today         UserUsageStat    `json:"today"`
	Month         UserUsageStat    `json:"month"`
	MonthRecharge int64            `json:"month_recharge"` // 本月净充值（微，含赠送、已扣退款）
	Trend         []UserTrendPoint `json:"trend"`
	ActiveTasks   int64            `json:"active_tasks"`
	// GeneratedAt 是**取数时刻**（业务时区 RFC3339），前端据此显示「截至 14:32」。
	// 它不会自己走动：页面不轮询，这个时间就是数字的真实年龄。
	GeneratedAt string `json:"generated_at"`
}

// UserUsageStat 是用户区间用量汇总（请求数 / 消耗（微）/ 双向 token）。
//
// Pending 只在 Today 有意义（见 UserOverview 里对 Month.Pending 的说明）。
type UserUsageStat struct {
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
	Pending          int64 `json:"pending"`
	Charge           int64 `json:"charge"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// UserTrendPoint 是用户趋势单点。**两个金额都是微**，可直接相减/同图绘制。
//
// 历史教训：这里曾下发 `recharge_cent`（分）与 `charge`（微）两个量纲，
// 前端每画一次图就要各自换算一次；漏掉一次就是「充值那条线贴着 0」。
type UserTrendPoint struct {
	Date     string `json:"date"`
	Charge   int64  `json:"charge"`
	Recharge int64  `json:"recharge"`
}

// UserOverview 聚合门户主页数据：今日/本月用量 + 本月充值 + 近 days 日趋势 + 在途任务数。
// days 仅接受 7/30，其它值（含 0/负数/非法）回落 7，与看板 d7/d30 档位语义对齐。
func UserOverview(ctx context.Context, userID uint64, days int) (*UserOverviewView, error) {
	if days != 7 && days != 30 {
		days = 7
	}
	now := BizNow()
	dayStart, dayEnd := DayRange(now)
	monthStart, _ := MonthRange(now)

	out := &UserOverviewView{Trend: []UserTrendPoint{}, GeneratedAt: now.Format(time.RFC3339)}
	gdb := db.DB.WithContext(ctx)

	// ① 今日：实时直查明细（走 idx_usages_user_created）。
	//    半开区间 [今0点, 明0点)：闭区间会把次日零点整那条同时算进两天。
	if err := gdb.Raw(`
SELECT COUNT(*) AS requests,
       COUNT(*) FILTER (WHERE status IN ('released', 'rejected')) AS errors,
       COUNT(*) FILTER (WHERE status = 'pending') AS pending,
       COALESCE(SUM(charge), 0) AS charge,
       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
       COALESCE(SUM(completion_tokens), 0) AS completion_tokens
  FROM usages WHERE user_id = ? AND created_at >= ? AND created_at < ?`,
		userID, dayStart, dayEnd).Scan(&out.Today).Error; err != nil {
		return nil, err
	}

	// ② 月初~昨日：取聚合表。
	//    **上界是开区间 date < 今日**：写成 <= 今日会让今天的用量被算两次
	//    （一次来自聚合表、一次来自①），而且两次用的还是不同口径（聚合滞后 vs 实时）。
	//    月初当天 monthStart == dayStart，区间为空，直接跳过这次查询。
	if monthStart.Before(dayStart) {
		var past UserUsageStat
		if err := gdb.Raw(`
SELECT COALESCE(SUM(requests), 0) AS requests, COALESCE(SUM(errors), 0) AS errors,
       COALESCE(SUM(charge), 0) AS charge,
       COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
       COALESCE(SUM(completion_tokens), 0) AS completion_tokens
  FROM usage_daily WHERE user_id = ? AND date >= ?::date AND date < ?::date`,
			userID, DayKey(monthStart), DayKey(dayStart)).Scan(&past).Error; err != nil {
			return nil, err
		}
		out.Month.Requests = past.Requests
		out.Month.Errors = past.Errors
		out.Month.Charge = past.Charge
		out.Month.PromptTokens = past.PromptTokens
		out.Month.CompletionTokens = past.CompletionTokens
	}

	// ③ 本月 = ② + ①。逐字段相加之所以成立，是因为两侧口径一致：
	//    usage_daily.requests 就是 COUNT(*)（含 pending/rejected，见 model.UsageDaily 注释），
	//    usage_daily.errors 就是 released+rejected 的条数。
	out.Month.Requests += out.Today.Requests
	out.Month.Errors += out.Today.Errors
	out.Month.Charge += out.Today.Charge
	out.Month.PromptTokens += out.Today.PromptTokens
	out.Month.CompletionTokens += out.Today.CompletionTokens
	// usage_daily **没有 pending 列**（进行中不是终态，不该被固化进聚合表），
	// 所以本月的「进行中」只能等于今天的 —— 历史 pending 行早已转成终态。
	out.Month.Pending = out.Today.Pending

	// ④ 本月充值（同一刻度：月初0点 ~ 明日0点）
	monthRecharge, err := RechargeBetween(ctx, userID, monthStart, dayEnd)
	if err != nil {
		return nil, err
	}
	out.MonthRecharge = monthRecharge

	// ⑤ 趋势：消耗取 usage_daily，充值取 transactions（两者都按业务日分桶）
	trendFrom := dayStart.AddDate(0, 0, -(days - 1))
	var usageRows []struct {
		Date   time.Time
		Charge int64
	}
	if err := gdb.Raw(`
SELECT date, COALESCE(SUM(charge), 0) AS charge
  FROM usage_daily WHERE user_id = ? AND date >= ?::date AND date <= ?::date
 GROUP BY date`, userID, DayKey(trendFrom), DayKey(now)).Scan(&usageRows).Error; err != nil {
		return nil, err
	}
	recharge, err := RechargeDaily(ctx, userID, trendFrom, dayEnd)
	if err != nil {
		return nil, err
	}
	charge := make(map[string]int64, len(usageRows))
	for _, r := range usageRows {
		charge[r.Date.Format("2006-01-02")] = r.Charge
	}
	// 不变式：趋势末点恒等于「今日费用」卡。今日桶在聚合表里最多滞后 1 小时，
	// 不覆盖就会出现「卡片 ¥1.20、图上最后一根 ¥0.80」——同一张卡片两个数字。
	charge[DayKey(now)] = out.Today.Charge
	for i := 0; i < days; i++ {
		k := DayKey(trendFrom.AddDate(0, 0, i))
		out.Trend = append(out.Trend, UserTrendPoint{
			Date: k, Charge: charge[k], Recharge: recharge[k],
		})
	}

	// ⑥ 在途任务数（与 Frozen 自洽：queued/running 的预扣尚未结算）
	if err := gdb.Raw(
		`SELECT COUNT(*) FROM tasks WHERE user_id = ? AND deleted_at IS NULL AND status IN ('queued', 'running')`,
		userID,
	).Scan(&out.ActiveTasks).Error; err != nil {
		return nil, err
	}

	// 自观测：单日请求超过 10 万时实时直查会开始变慢（见方案 §性能边界），
	// 提前告警而不是等首屏超过 500ms 才发现。
	if out.Today.Requests >= 100_000 {
		logger.L().Warn("门户今日用量超过实时查询阈值，建议改为分钟级微聚合",
			zap.Uint64("user_id", userID), zap.Int64("requests", out.Today.Requests))
	}
	return out, nil
}
