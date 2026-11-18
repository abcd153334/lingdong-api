// Package logs 是管理台调用日志查询面（S11）：明细/归档查询、CSV 导出、类型分布与错误排行。
// 事件类型由 status + http_status 在查询层派生（§4.9.9），不落冗余列。
package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/service/iconindex"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/taskview"
)

// 明细/归档范围。
const (
	ScopeHot     = "hot"
	ScopeArchive = "archive"
)

// Filter 是日志查询条件（明细与归档同构）。
type Filter struct {
	Scope     string
	UserID    uint64
	Model     string
	ChannelID uint64
	// ChannelKeyID 精确筛「渠道里的哪把 key」（管理台排障用；门户不暴露该筛选）。
	ChannelKeyID string
	Modality     string
	Kind         string // 派生事件类型（consume/client_abort/error/upstream_error/pending/rejected:*）
	Status       string // 状态分组（ok/429/err/5xx）
	From         time.Time
	To           time.Time
	// 三个精确检索项（C1）：排障时最先拿到的是 request_id，其次是上游单号，
	// 最后才是「用户报的那条记录 id」。没有它们只能按时间和模型翻页找。
	RequestID         string
	UpstreamRequestID string
	UsageID           uint64
	// CallKind 过滤记录来源（sync/async/plugin/precheck）。
	CallKind string
}

// Item 是明细行视图（含解析出的用户/KEY/渠道展示名与派生事件类型）。
type Item struct {
	ID        uint64    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UserID    uint64    `json:"user_id"`
	Username  string    `json:"username"`
	KeyID     uint64    `json:"key_id"`
	KeyName   string    `json:"key_name"`
	Model     string    `json:"model"`
	// ModelIconURL 是模型**生效**图标的可渲染地址（自有 → 供应商回落，见 iconindex）。
	// 历史行必须有图标：改完名字/图标后回看三天前的调用，认模型靠的就是这张图。
	ModelIconURL string `json:"model_icon_url"`
	ChannelID    uint64 `json:"channel_id"`
	// ChannelKeyID 是实际出站的上游渠道密钥标识（HMAC 指纹，非明文）。
	// **只在管理台出现**：门户投影必须清空 —— 它回答"走了哪把上游 key"，
	// 属于供应链信息，与 ChannelName 同一级别（portal_leak_test 守着）。
	ChannelKeyID string `json:"channel_key_id"`
	// ChannelName 只下发给管理台；门户详情必须清空（见 Detail 的门户裁剪）。
	ChannelName string `json:"channel_name"`
	// ChannelIconURL 同理只在管理台出现：门户投影一旦带出渠道信息，
	// 就等于把"这张请求走了哪个上游"泄露给终端用户（portal_leak_test 守着这条）。
	ChannelIconURL string `json:"channel_icon_url"`
	// AdapterID 用于区分 plugin:<id> 与内置适配器（插件排障第一步就是确认"这条是不是插件的"）
	AdapterID         string `json:"adapter_id"`
	Modality          string `json:"modality"`
	Kind              string `json:"kind"`
	Status            string `json:"status"`
	HTTPStatus        int    `json:"http_status"`
	PromptTokens      int64  `json:"prompt_tokens"`
	CompletionTokens  int64  `json:"completion_tokens"`
	TotalTokens       int64  `json:"total_tokens"`
	CacheTokens       int64  `json:"cache_tokens"`
	ImageTokens       int64  `json:"image_tokens"`
	AudioInputTokens  int64  `json:"audio_input_tokens"`
	AudioOutputTokens int64  `json:"audio_output_tokens"`
	// Charge / Cost 单位是**微**（1 货币单位 = 1e6 微）。
	Charge int64 `json:"charge"`
	Cost   int64 `json:"cost"`
	// LatencyMs 是**总耗时**（finished_at − started_at，含重试与排队）。
	// ElapsedMs 是"进行中"的实时值（finished_at 为空时按 now() 算）；终态两者相等。
	LatencyMs int   `json:"latency_ms"`
	ElapsedMs int64 `json:"elapsed_ms"`
	// UpstreamMs 是最后一次上游调用的耗时：与总耗时一起看才能判断慢在网关还是上游。
	UpstreamMs   int `json:"upstream_ms"`
	FirstTokenMs int `json:"first_token_ms"`
	Attempts     int `json:"attempts"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	ErrorMsg  string `json:"error_msg"`
	IsStream  bool   `json:"is_stream"`
	IP        string `json:"ip"`
	RequestID string `json:"request_id"`
	// UpstreamRequestID 是上游开单号：与上游对账时唯一的共同标识。
	UpstreamRequestID string `json:"upstream_request_id"`
	// 计费口径（S8 整改）：来源 upstream|mixed|estimated|none|unknown；异常标记逗号分隔。
	UsageSource string `json:"usage_source,omitempty"`
	Anomaly     string `json:"anomaly,omitempty"`
	// CallKind 区分 sync/async/plugin/precheck；TaskID 指回异步任务（0 = 同步）。
	CallKind string `json:"call_kind"`
	// TaskID 是**内部自增号**，只做外键/排障用，`json:"-"` 保证它永远不会被
	// 当成任务号下发。对外任务号只有 TaskPublicID 一个来源。
	//
	// 为什么要显式写 json:"-"：结构体上原本是 `json:"task_id"`，一旦有人把这个
	// 内部结构直接序列化（debug 口、日志回放、导出），客户端拿到的就是
	// `task_id: 1024` —— 填回 /v1/tasks/1024 必然 400，而错误信息只说"任务号无效"。
	TaskID uint64 `json:"-"`
	// TaskPublicID 是任务号（`task_` 前缀），**只给控制面投影用**，不进 JSON：
	// 这个结构同时被管理台序列化，多一个键就是多一个没人读的字段。
	TaskPublicID string `json:"-"`
	Group        string `json:"group"`
	// Currency 是行内快照的结算币种：金额列必须用**当时的**币种展示，
	// 用当前值会让历史记录在改配置后自行"变价"，对账永远对不上。
	Currency string `json:"currency,omitempty"`
}

// KindStat 是事件类型分布项。
type KindStat struct {
	Label string `json:"label"`
	Value int64  `json:"value"`
}

// ErrorStat 是错误排行项（label + 次数 + 占比）。
type ErrorStat struct {
	Label string  `json:"label"`
	Value int64   `json:"value"`
	Share float64 `json:"share"`
}

// Analytics 是日志分析视图（类型分布 + 错误排行）。
type Analytics struct {
	Kinds  []KindStat  `json:"kinds"`
	Errors []ErrorStat `json:"errors"`
}

// elapsedExpr 返回"已进行多久"的毫秒表达式（数据库时钟）。
//
// 用 DB 时钟而不是应用时钟：多副本部署时各机器时钟可能差几秒，
// 前端再拿自己的时钟去补，三者叠加后"进行中 3s"这种短记录会跳来跳去。
// started_at 为 NULL 的历史行回落 created_at（Freeze 就是建行时刻，语义等价）。
const elapsedExpr = `(EXTRACT(EPOCH FROM (COALESCE(finished_at, now()) - COALESCE(started_at, created_at))) * 1000)::bigint`

const rawColumns = `id, created_at, user_id, key_id, channel_id, channel_key_id, adapter_id, model, modality, kind, status,
	http_status, prompt_tokens, completion_tokens, cache_tokens, image_tokens,
	audio_input_tokens, audio_output_tokens, charge, cost, latency_ms,
	error_msg, error_code, is_stream, ip, request_id, upstream_request_id,
	usage_source, anomaly, call_kind, task_id, "group",
	started_at, finished_at, upstream_ms, first_token_ms, attempts, currency,
	` + elapsedExpr + ` AS elapsed_ms`

/* ------------------------------ 条件构造 ------------------------------ */

// table 返回查询表名（明细 / 归档）。
func table(scope string) string {
	if scope == ScopeArchive {
		return "usages_archive"
	}
	return "usages"
}

// where 构造 WHERE 子句（返回 " AND ..." 形式，空条件返回空串）。
func where(f Filter) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, 8)
	add := func(cond string, vs ...any) {
		sb.WriteString(" AND " + cond)
		args = append(args, vs...)
	}
	if f.UserID > 0 {
		add("user_id = ?", f.UserID)
	}
	if f.UsageID > 0 {
		add("id = ?", f.UsageID)
	}
	if f.Model != "" {
		add("model = ?", f.Model)
	}
	if f.ChannelID > 0 {
		add("channel_id = ?", f.ChannelID)
	}
	if f.ChannelKeyID != "" {
		add("channel_key_id = ?", f.ChannelKeyID)
	}
	if f.Modality != "" {
		add("modality = ?", f.Modality)
	}
	if f.CallKind != "" {
		add("call_kind = ?", f.CallKind)
	}
	if f.RequestID != "" {
		add("request_id = ?", f.RequestID)
	}
	if f.UpstreamRequestID != "" {
		add("upstream_request_id = ?", f.UpstreamRequestID)
	}
	if !f.From.IsZero() {
		add("created_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		add("created_at < ?", f.To)
	}
	if c, ok := kindCond(f.Kind); ok {
		add(c)
	}
	if c, ok := statusCond(f.Status); ok {
		add(c)
	}
	return sb.String(), args
}

// kindCond 将派生事件类型映射为状态条件。
func kindCond(kind string) (string, bool) {
	switch kind {
	case "consume":
		return "status = 'settled'", true
	case "pending":
		return "status = 'pending'", true
	case "client_abort":
		return "status = 'released' AND http_status = 499", true
	case "upstream_error":
		return "status = 'released' AND (http_status >= 500 OR http_status = 0)", true
	case "error":
		return "status = 'released' AND http_status >= 400 AND http_status < 499", true
	// 预检失败（C1）：一次都没打到上游就被拒，与 released 语义完全不同
	// （released 是"打出去了但失败"，rejected 是"根本没发"）。
	case "rejected":
		return "status = 'rejected'", true
	// 业务码优先、状态码回落：历史行没有 error_code（空串），
	// 它们的语义就是旧的「402 = 余额不足」，按状态码判仍然正确。
	// 判据集中成三个片段，SQL 与 Go 两处派生共用同一份措辞。
	case "rate_limited":
		return "status = 'rejected' AND http_status = 429", true
	case "insufficient_balance":
		return "status = 'rejected' AND " + insufficientCond, true
	case "auth_error":
		return "status = 'rejected' AND http_status IN (401, 403) AND NOT " + insufficientCond, true
	case "invalid_request":
		return "status = 'rejected' AND http_status NOT IN (401, 402, 403, 429) AND NOT " + insufficientCond, true
	}
	return "", false
}

// statusCond 将状态分组映射为 HTTP 状态条件。
func statusCond(status string) (string, bool) {
	switch status {
	case "ok":
		return "http_status >= 200 AND http_status < 300", true
	case "429":
		return "http_status = 429", true
	case "err":
		return "http_status >= 400 AND http_status < 500 AND http_status <> 429", true
	case "5xx":
		return "http_status >= 500 OR http_status = 0", true
	}
	return "", false
}

// insufficientCond 是「额度不足」的判据片段（SQL 与 kindOf 共用一份语义）。
//
// 为什么不能只看 http_status：402 是内部码，协议面按兼容要求已改成 403，
// 于是 403 同时承载「余额不足」与「权限不足」。业务码落列之后按码判定，
// 历史行（error_code 为空）才退回 402。
const insufficientCond = `(COALESCE(error_code, '') IN ('insufficient_balance', 'request_quota_exceeded')` +
	` OR (COALESCE(error_code, '') = '' AND http_status = 402))`

// kindOf 派生单行事件类型（与 kindCond 口径一致）。
func kindOf(status string, httpStatus int, errorCode string) string {
	switch status {
	case "settled":
		return "consume"
	case "pending":
		return "pending"
	case "rejected":
		switch {
		case httpStatus == 429:
			return "rate_limited"
		case isInsufficient(errorCode, httpStatus):
			return "insufficient_balance"
		case httpStatus == 401 || httpStatus == 403:
			return "auth_error"
		default:
			return "invalid_request"
		}
	case "released":
		switch {
		case httpStatus == 499:
			return "client_abort"
		case httpStatus >= 500 || httpStatus == 0:
			return "upstream_error"
		default:
			return "error"
		}
	}
	return status
}

// isInsufficient 是 insufficientCond 的 Go 版本（同一份语义，两种载体）。
func isInsufficient(errorCode string, httpStatus int) bool {
	switch errorCode {
	case "insufficient_balance", "request_quota_exceeded":
		return true
	}
	return errorCode == "" && httpStatus == 402
}

/* ------------------------------ 查询 ------------------------------ */

// RawRow 是明细行的数据库形态（列清单见 rawColumns）。
//
// 必须是**导出**类型：详情查询用 `struct { RawRow; ... }` 内嵌它，
// 而 GORM 的 Scan 按字段的可导出性决定是否解析 —— 内嵌未导出类型会被整体跳过，
// 结果是「SQL 成功、结构体全零」，详情页一片空白却没有任何报错。
// 这条踩过：detail_test.go 的列对齐用例就是为了不再踩第二次。
type RawRow struct {
	ID                uint64
	CreatedAt         time.Time
	UserID            uint64
	KeyID             uint64
	ChannelID         uint64
	ChannelKeyID      string
	AdapterID         string
	Model             string
	Modality          string
	Kind              string
	Status            string
	HTTPStatus        int
	PromptTokens      int64
	CompletionTokens  int64
	CacheTokens       int64
	ImageTokens       int64
	AudioInputTokens  int64
	AudioOutputTokens int64
	Charge            int64
	Cost              int64
	LatencyMs         int
	ErrorMsg          string
	ErrorCode         string
	IsStream          bool
	IP                string
	RequestID         string
	UpstreamRequestID string
	UsageSource       string
	Anomaly           string
	CallKind          string
	TaskID            uint64
	Group             string
	StartedAt         *time.Time
	FinishedAt        *time.Time
	UpstreamMs        int
	FirstTokenMs      int
	Attempts          int
	ElapsedMs         int64
	// Currency 是本次调用的结算币种（空串 = 老行，按当前结算币种展示）。
	Currency string
}

// toItem 把数据库行转成对外视图（展示名由调用方补齐）。
func (r RawRow) toItem() Item {
	return Item{
		ID: r.ID, CreatedAt: r.CreatedAt,
		UserID: r.UserID, KeyID: r.KeyID,
		Model: r.Model, ChannelID: r.ChannelID, ChannelKeyID: r.ChannelKeyID, AdapterID: r.AdapterID,
		Modality: r.Modality,
		Kind:     kindOf(r.Status, r.HTTPStatus, r.ErrorCode), Status: r.Status, HTTPStatus: r.HTTPStatus,
		PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
		TotalTokens:       r.PromptTokens + r.CompletionTokens,
		CacheTokens:       r.CacheTokens,
		ImageTokens:       r.ImageTokens,
		AudioInputTokens:  r.AudioInputTokens,
		AudioOutputTokens: r.AudioOutputTokens,
		Charge:            r.Charge, Cost: r.Cost,
		LatencyMs: r.LatencyMs, ElapsedMs: r.ElapsedMs,
		UpstreamMs: r.UpstreamMs, FirstTokenMs: r.FirstTokenMs, Attempts: r.Attempts,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		ErrorMsg: r.ErrorMsg, IsStream: r.IsStream, IP: r.IP,
		RequestID: r.RequestID, UpstreamRequestID: r.UpstreamRequestID,
		UsageSource: r.UsageSource, Anomaly: r.Anomaly,
		CallKind: r.CallKind, TaskID: r.TaskID, Group: r.Group,
		Currency: currencyOr(r.Currency),
	}
}

// List 分页查询明细（scope=hot/archive），并解析用户/KEY/渠道展示名。
//
// base 是站点对外基址，只用于把图标 token 投影成可渲染地址。
func List(ctx context.Context, f Filter, page, size int, base string) ([]Item, int64, error) {
	tbl := table(f.Scope)
	cond, args := where(f)

	var total int64
	if err := db.DB.WithContext(ctx).
		Raw("SELECT COUNT(*) FROM "+tbl+" WHERE 1=1"+cond, args...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []Item{}, 0, nil
	}

	var raw []RawRow
	q := "SELECT " + rawColumns + " FROM " + tbl + " WHERE 1=1" + cond + " ORDER BY id DESC LIMIT ? OFFSET ?"
	// 用 int64 算再夹紧：page 已在入口被 MaxPage 拦住，这里是第二道防线 ——
	// 负数 OFFSET 会让 PG 报 2201X，最终折成 500。
	offset := (int64(page) - 1) * int64(size)
	if offset < 0 {
		offset = 0
	}
	args = append(args, size, offset)
	if err := db.DB.WithContext(ctx).Raw(q, args...).Scan(&raw).Error; err != nil {
		return nil, 0, err
	}

	items := make([]Item, 0, len(raw))
	userIDs := make([]uint64, 0, len(raw))
	keyIDs := make([]uint64, 0, len(raw))
	chIDs := make([]uint64, 0, len(raw))
	modelIDs := make([]string, 0, len(raw))
	for _, r := range raw {
		userIDs = append(userIDs, r.UserID)
		keyIDs = append(keyIDs, r.KeyID)
		chIDs = append(chIDs, r.ChannelID)
		modelIDs = append(modelIDs, r.Model)
	}
	users := names(ctx, "users", "username", userIDs)
	keys := KeyNames(ctx, keyIDs)
	channels := names(ctx, "channels", "name", chIDs)
	// 图标和上面三张名字表在同一个位置补（见 iconindex 的包注释）：
	// 回落规则只在那里一份 —— 在 SQL 里再 JOIN 一次就是第二个派生点。
	icons := iconindex.Resolve(ctx, modelIDs, chIDs)
	// 对外任务号同批补：控制面只认 `task_` 串，下发内部自增 id 等于给一个
	// 「填进去必然 400」的值。放这里而不是 SQL 子查询里：调用记录页不依赖
	// tasks 表存在（归档/排障库、历史 schema 都可能有 usages 而没有 tasks）。
	for _, r := range raw {
		it := r.toItem()
		it.Username = users[r.UserID]
		it.KeyName = keys[r.KeyID]
		it.ChannelName = channels[r.ChannelID]
		it.ModelIconURL = iconref.URL(base, icons.Model(r.Model))
		it.ChannelIconURL = iconref.URL(base, icons.Channel(r.ChannelID))
		items = append(items, it)
	}
	FillTaskPublicIDs(ctx, items)
	return items, total, nil
}

// FillTaskPublicIDs 批量补对外任务号（`task_` 串），原地修改。
//
// 这里曾经是**唯一**的补号点，但只服务 `List` —— 用户端调用记录
// （`/api/portal/usage`、`/v1/usage`）走 relay.usagePage 自己扫 usage 表，
// 于是那两处的 `task_id` 恒为空（omitempty 还会把键整个吞掉），
// 而同一个用户点进详情又能看到任务号：列表与详情不一致，且没有任何报错。
//
// 现在这个函数是两个列表的共同补号点：谁再写一条列表链路，调它一次即可；
// 不调的时候会在契约测试里以"列表缺 task_id"当场失败。
func FillTaskPublicIDs(ctx context.Context, items []Item) {
	ids := make([]uint64, 0, len(items))
	for i := range items {
		if items[i].TaskID != 0 {
			ids = append(ids, items[i].TaskID)
		}
	}
	if len(ids) == 0 {
		return
	}
	m := taskPublicIDs(ctx, ids)
	for i := range items {
		if items[i].TaskID == 0 {
			continue
		}
		items[i].TaskPublicID = m[items[i].TaskID]
	}
}

// Export 返回当前筛选下的导出行（上限 limit，按时间倒序）。
func Export(ctx context.Context, f Filter, limit int, base string) ([]Item, error) {
	items, _, err := List(ctx, f, 1, limit, base)
	return items, err
}

// AnalyticsData 返回类型分布与错误排行（与筛选条件联动，不受分页影响）。
func AnalyticsData(ctx context.Context, f Filter) (*Analytics, error) {
	out := &Analytics{Kinds: []KindStat{}, Errors: []ErrorStat{}}

	// 类型分布：SQL 内派生事件类型（口径与 kindOf 一致）。
	// 分支顺序不能调整：rejected/429 必须先于通用的 released 判断，
	// 否则预检失败会被算成"客户端断开"（http_status 0 落进 upstream_error）。
	base, args := where(Filter{
		UserID: f.UserID, Model: f.Model, ChannelID: f.ChannelID, Modality: f.Modality,
		Status: f.Status, From: f.From, To: f.To, CallKind: f.CallKind,
	})
	// 分支顺序与 kindOf 必须逐条一致（顺序即语义：429 要先于通用 rejected，
	// 额度不足要先于 401/403 的鉴权桶）。两处同源靠 insufficientCond 共用措辞，
	// 剩余顺序由 logs_test 的对拍用例守住。
	kindSQL := `SELECT CASE
		  WHEN status = 'settled' THEN 'consume'
		  WHEN status = 'pending' THEN 'pending'
		  WHEN status = 'rejected' AND http_status = 429 THEN 'rate_limited'
		  WHEN status = 'rejected' AND ` + insufficientCond + ` THEN 'insufficient_balance'
		  WHEN status = 'rejected' AND http_status IN (401, 403) THEN 'auth_error'
		  WHEN status = 'rejected' THEN 'invalid_request'
		  WHEN http_status = 499 THEN 'client_abort'
		  WHEN http_status >= 500 OR http_status = 0 THEN 'upstream_error'
		  ELSE 'error' END AS label, COUNT(*) AS value
	  FROM ` + table(f.Scope) + ` WHERE 1=1` + base + ` GROUP BY 1 ORDER BY value DESC`
	if err := db.DB.WithContext(ctx).Raw(kindSQL, args...).Scan(&out.Kinds).Error; err != nil {
		return nil, err
	}

	// 错误排行：仅失败行（4xx/5xx），label = 错误摘要
	errSQL := `SELECT COALESCE(NULLIF(LEFT(error_msg, 80), ''), 'HTTP ' || http_status::text) AS label,
	       COUNT(*) AS value
	  FROM ` + table(f.Scope) + ` WHERE 1=1` + base + ` AND http_status >= 400
	 GROUP BY 1 ORDER BY value DESC LIMIT 8`
	if err := db.DB.WithContext(ctx).Raw(errSQL, args...).Scan(&out.Errors).Error; err != nil {
		return nil, err
	}
	var sum int64
	for _, e := range out.Errors {
		sum += e.Value
	}
	if sum > 0 {
		for i := range out.Errors {
			out.Errors[i].Share = float64(out.Errors[i].Value) / float64(sum)
		}
	}
	return out, nil
}

// CSV 按冻结导出列契约（E5，22 列）生成 CSV 文本。
//
// 金额列写成「十进制金额」而不是微：导出的文件是给人看、给脚本对账的，
// 微是内部单位，出现在对账表里只会让每一行都要再除一次 1e6。
func CSV(items []Item) string {
	var sb strings.Builder
	// 表头不带币种后缀，改由独立的「币种」列承载：
	// 一列金额在不同行可能是 ¥ 也可能是 $（多币种并存），
	// 表头写死 USD 等于让文件撒谎，对账脚本按表头取列会算错。
	sb.WriteString("时间,用户,密钥名,模型,模态,事件类型,状态,HTTP," +
		"输入tok,输出tok,缓存tok,图片tok,音频入tok,音频出tok," +
		"渠道,分组,耗时ms,费用,成本,币种,请求ID,上游请求ID,IP\n")
	for _, it := range items {
		// 币种逐行取，不取全局：一份导出里可能同时含人民币与美元时期的记录。
		cur := currencyOr(it.Currency)
		fmt.Fprintf(&sb, "%s,%s,%s,%s,%s,%s,%s,%d,%d,%d,%d,%d,%d,%d,%s,%s,%d,%s,%s,%s,%s,%s,%s\n",
			it.CreatedAt.Format("2006-01-02 15:04:05"),
			Esc(it.Username), Esc(it.KeyName), Esc(it.Model), Esc(it.Modality),
			Esc(it.Kind), Esc(statusText(it)), it.HTTPStatus,
			it.PromptTokens, it.CompletionTokens, it.CacheTokens, it.ImageTokens,
			it.AudioInputTokens, it.AudioOutputTokens,
			Esc(it.ChannelName), Esc(it.Group), it.LatencyMs,
			money.Format(it.Charge, 6), money.Format(it.Cost, 6), cur,
			Esc(it.RequestID), Esc(it.UpstreamRequestID), Esc(it.IP))
	}
	return sb.String()
}

// Esc 做 CSV 转义：先按 RFC 4180 处理引号/逗号/换行，再对首字符为 = + - @ 或 Tab/CR 的单元格前置单引号，
// 防止表格软件把单元格当公式执行（CSV 公式注入防护，P2-11）。导出给其它导出面复用，避免重复实现。
func Esc(s string) string {
	if s != "" {
		switch s[0] {
		case '=', '+', '-', '@', '\t', '\r':
			s = "'" + s
		}
	}
	if strings.ContainsAny(s, ",\"\n\r") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

// statusText 返回状态列文案（派生类型中文）。
func statusText(it Item) string {
	switch it.Kind {
	case "consume":
		return "成功"
	case "client_abort":
		return "客户端断开"
	case "upstream_error":
		return "上游错误"
	case "pending":
		return "进行中"
	case "rate_limited":
		return "预检限流"
	case "auth_error":
		return "预检鉴权失败"
	case "insufficient_balance":
		return "余额不足"
	case "invalid_request":
		return "预检参数错误"
	default:
		if it.HTTPStatus > 0 {
			return fmt.Sprintf("失败(%d)", it.HTTPStatus)
		}
		return "失败"
	}
}

/* ------------------------------ 详情 ------------------------------ */

// IOView 是 call_io 的对外视图（原始报文 + 媒体链接）。
//
// 报文随详情一次性返回而不是另开下载端点：单条上限 256 KiB（gzip 由中间件负责），
// 多一次往返换不来任何收益；「下载原文」在前端用 Blob 本地生成即可。
type IOView struct {
	ReqRaw       string `json:"req_raw,omitempty"`
	ReqBytes     int64  `json:"req_bytes"`
	ReqSHA256    string `json:"req_sha256,omitempty"`
	ReqTruncated bool   `json:"req_truncated"`
	RespRaw      string `json:"resp_raw,omitempty"`
	RespError    string `json:"resp_error,omitempty"`
	RespBytes    int64  `json:"resp_bytes"`
	// DeliveredRaw 是**客户端真正收到**的响应体（媒体直链改写 / 协议转码之后）。
	// 空 = 与 RespRaw 相同（普通透传不存第二份）。门户读它，管理台看 RespRaw。
	DeliveredRaw string            `json:"delivered_raw,omitempty"`
	SSEFrames    int               `json:"sse_frames"`
	Media        []model.MediaItem `json:"media,omitempty"`
	// ReqSource 说明 ReqRaw 的可信度：raw / rebuilt / snapshot。
	// 空值表示 C1 之前的历史行（前端按 call_kind 推断后展示）。
	ReqSource string `json:"req_source,omitempty"`
	// RespSource 说明 RespRaw 的来源：raw / aggregate（流式拼接）/ usage / none。
	RespSource string `json:"resp_source,omitempty"`
	// BodyMode 是本行正文口径：full / meta（空 = 该列上线前的历史行）。
	//
	// 前端据此决定是否提示「这是结构摘要」—— 不再靠猜 JSON 里有没有 _meta。
	BodyMode string `json:"body_mode,omitempty"`

	// ---------- 实际出站报文 ----------
	//
	// 与 ReqRaw（客户端发进来的）分开展示：排障要能一眼看出平台改了什么。
	UpstreamRaw       string `json:"upstream_raw,omitempty"`
	UpstreamBytes     int64  `json:"upstream_bytes"`
	UpstreamTruncated bool   `json:"upstream_truncated"`
	// UpstreamSource：tap / tap-meta / tap-skipped / snapshot。
	UpstreamSource string `json:"upstream_source,omitempty"`
	UpstreamURL    string `json:"upstream_url,omitempty"`
	// UpstreamStatus 为 0 表示未收到响应（连接失败/超时）。
	UpstreamStatus   int `json:"upstream_status"`
	UpstreamAttempts int `json:"upstream_attempts"`
}

// taskViewOf 读取关联的异步任务并投影成**全站统一的任务对象**（任务被清理时返回 nil）。
//
// 用 Unscoped：软删的任务仍应能显示"已删除"，而不是让链路彻底断掉。
//
// 产物清单在这里一起查（只多一次索引查询）：详情是唯一需要 contents 的地方，
// 列表路径根本不调本函数。归一规则只有 model.ArtifactListing 一处
// （跳过过期、稠密重编号、生效 MIME、渲染档）；不能直接调 task.ArtifactsOf ——
// relay → logs → task → relay 是**已存在的**导入环，编译期就会炸。
func taskViewOf(ctx context.Context, taskID uint64, contentBase string, usage taskview.ViewUsage) (*taskview.View, string, error) {
	if taskID == 0 {
		return nil, "", nil
	}
	var row model.Task
	if err := db.DB.WithContext(ctx).Unscoped().
		// 必须带 id：First 依赖主键列回填，漏了它 row.ID 恒 0，视图里的 id 会静默变 "0"。
		// 不带 error_msg 等敏感列：任务对象只下发分类文案（见 taskview.ViewError）。
		Select("id, public_id, status, model, modality, kind, error_msg, created_at, updated_at, deadline_at").
		First(&row, taskID).Error; err != nil {
		return nil, "", err
	}
	v := taskview.ViewOf(&row, taskview.ViewOptions{
		ContentBase: contentBase, Artifacts: artifactRefs(ctx, taskID), Usage: usage,
	})
	// error_msg 只作为**伴随字段**返回，不进 View（见 Detail.TaskErrorMsg 注释）。
	return &v, row.ErrorMsg, nil
}

// Detail 是单条调用记录的完整视图。
type Detail struct {
	Item
	RequestDigest      string `json:"request_digest,omitempty"`
	RawUsage           string `json:"raw_usage,omitempty"`
	UsageSemantic      string `json:"usage_semantic,omitempty"`
	ImageSpec          string `json:"image_spec,omitempty"`
	ImageRatio         string `json:"image_ratio,omitempty"`
	ImageSpecSource    string `json:"image_spec_source,omitempty"`
	ImageQuality       string `json:"image_quality,omitempty"`
	ImageN             int64  `json:"image_n"`
	AudioDurationSecs  int64  `json:"audio_duration_secs"`
	AudioChars         int64  `json:"audio_chars"`
	RerankDocs         int64  `json:"rerank_docs"`
	VideoResolution    string `json:"video_resolution,omitempty"`
	VideoSeconds       int64  `json:"video_seconds"`
	VideoSecondsSource string `json:"video_seconds_source,omitempty"`
	RefImages          int64  `json:"ref_images"`
	RefVideos          int64  `json:"ref_videos"`
	RefAudios          int64  `json:"ref_audios"`
	RefVideoSeconds    int64  `json:"ref_video_seconds"`
	TierPrice          int64  `json:"tier_price"`
	TierSource         string `json:"tier_source,omitempty"`
	ChargeBeforeCap    int64  `json:"charge_before_cap"`
	CapReason          string `json:"cap_reason,omitempty"`
	Frozen             int64  `json:"frozen"`
	// BillingMode 是计费方式的对外口径：tokens / call / spec（与 models.billing_mode 同值）。
	//
	// 由定价快照的 billing_mode 派生后单独下发：门户拿不到快照（含单价等平台定价参数），
	// 但"这笔钱按什么算的"必须能解释 —— 给一个枚举就够了，既够生成结论句，也不泄漏单价。
	BillingMode string `json:"billing_mode,omitempty"`
	// Currency 是行内快照的结算币种（老行按当前结算币种展示）。
	Currency        string          `json:"currency,omitempty"`
	PricingSnapshot json.RawMessage `json:"pricing_snapshot,omitempty"`
	IO              *IOView         `json:"call_io,omitempty"`
	// Task 是关联的异步任务，形状与 /v1/tasks/{id} **同一个结构体**（taskview.View）。
	//
	// 以前这里是一个自造的 TaskRef（id 是数字、时间 RFC3339、error 是裸字符串、
	// contents 没有 url），于是"同一个任务"在调用记录与任务端点里长得不一样。
	// 形状只有 taskview 一处（logs 不能 import task：relay → logs → task → relay）。
	Task *taskview.View `json:"task,omitempty"`
	// TaskErrorMsg 是任务的**上游/平台原文**（仅管理台详情下发，门户投影不复制它）。
	//
	// 为什么单独一个字段而不是塞进 View：View 是**全站共用**的任务对象，
	// 门户/应用面读的是同一个结构体 —— 往里放原文字段等于把「上游域名 + 真实模型名」
	// 一起发给用户。原文只属于"排障视角"，跟着调用记录走。
	TaskErrorMsg string `json:"task_error_msg,omitempty"`
}

// DetailByID 按 id 查单条记录详情（scope=hot/archive）。
// 返回 (nil, nil) 表示不存在（由调用方决定 404 还是别的语义）。
//
// contentBase 是**本次请求所在入口**的任务产物前缀（`/api/admin/tasks`、
// `/api/portal/tasks`、`<origin>/v1/tasks`）：同一份产物在三处下发，
// 写死一个前缀会让另外两处给出必然 401/404 的死链。
func DetailByID(ctx context.Context, scope string, id uint64, base, contentBase string) (*Detail, error) {
	return detailByID(ctx, scope, id, 0, base, contentBase)
}

// DetailByIDForUser 是门户版本：**越权过滤写在 SQL 里**（WHERE user_id = ?）。
//
// 不能"查出来再判断 user_id 是否相等"：那种写法一旦某天有人把判断挪进分支，
// IDOR 就悄悄回来了，而测试通常只覆盖 happy path。
func DetailByIDForUser(ctx context.Context, id, userID uint64, base, contentBase string) (*Detail, error) {
	if userID == 0 {
		return nil, nil
	}
	return detailByID(ctx, ScopeHot, id, userID, base, contentBase)
}

// detailByID 是详情查询的唯一实现（userID=0 表示不按用户过滤）。
func detailByID(ctx context.Context, scope string, id, userID uint64, base, contentBase string) (*Detail, error) {
	if id == 0 {
		return nil, nil
	}
	tbl := table(scope)
	var r struct {
		RawRow
		RequestDigest     string
		RawUsage          string
		UsageSemantic     string
		ImageSpec         string
		ImageRatio        string
		ImageSpecSource   string
		ImageQuality      string
		ImageN            int64
		AudioDurationSecs int64
		AudioChars        int64
		RerankDocs        int64
		VideoResolution   string
		VideoSeconds      int64
		// VideoSecondsSource 等列在两个表里都存在（archive 内嵌 Usage），
		// 所以这里可以一份 struct 通吃 hot/archive。
		VideoSecondsSource string
		RefImages          int64
		RefVideos          int64
		RefAudios          int64
		RefVideoSeconds    int64
		TierPrice          int64
		TierSource         string
		ChargeBeforeCap    int64
		CapReason          string
		Frozen             int64
		PricingSnapshot    []byte
	}
	cols := rawColumns + `, request_digest, raw_usage, usage_semantic,
		image_spec, image_ratio, image_spec_source, image_quality, image_n,
		audio_duration_secs, audio_chars, rerank_docs,
		video_resolution, video_seconds, video_seconds_source,
		ref_images, ref_videos, ref_audios, ref_video_seconds,
		tier_price, tier_source, charge_before_cap, cap_reason, frozen,
		pricing_snapshot`
	cond := " WHERE id = ?"
	args := []any{id}
	if userID > 0 {
		cond += " AND user_id = ?"
		args = append(args, userID)
	}
	if err := db.DB.WithContext(ctx).
		Raw("SELECT "+cols+" FROM "+tbl+cond, args...).Scan(&r).Error; err != nil {
		return nil, err
	}
	if r.ID == 0 {
		return nil, nil
	}
	d := &Detail{
		Item:               r.RawRow.toItem(),
		RequestDigest:      r.RequestDigest,
		RawUsage:           r.RawUsage,
		UsageSemantic:      r.UsageSemantic,
		ImageSpec:          r.ImageSpec,
		ImageRatio:         r.ImageRatio,
		ImageSpecSource:    r.ImageSpecSource,
		ImageQuality:       r.ImageQuality,
		ImageN:             r.ImageN,
		AudioDurationSecs:  r.AudioDurationSecs,
		AudioChars:         r.AudioChars,
		RerankDocs:         r.RerankDocs,
		VideoResolution:    r.VideoResolution,
		VideoSeconds:       r.VideoSeconds,
		VideoSecondsSource: r.VideoSecondsSource,
		RefImages:          r.RefImages,
		RefVideos:          r.RefVideos,
		RefAudios:          r.RefAudios,
		RefVideoSeconds:    r.RefVideoSeconds,
		TierPrice:          r.TierPrice,
		TierSource:         r.TierSource,
		ChargeBeforeCap:    r.ChargeBeforeCap,
		CapReason:          r.CapReason,
		Frozen:             r.Frozen,
	}
	if len(r.PricingSnapshot) > 0 {
		d.PricingSnapshot = json.RawMessage(r.PricingSnapshot)
		// 计费方式在**清空快照之前**派生：门户看不到快照，
		// 但"按什么算"必须能解释，否则金额是个无字天书。
		d.BillingMode, d.Currency = billingFacts(r.PricingSnapshot)
	}
	d.Username = names(ctx, "users", "username", []uint64{r.UserID})[r.UserID]
	d.KeyName = KeyNames(ctx, []uint64{r.KeyID})[r.KeyID]
	d.ChannelName = names(ctx, "channels", "name", []uint64{r.ChannelID})[r.ChannelID]
	// 详情与列表走同一份解析（一行也调 Resolve）：这里手拼 token 就会出现
	// "列表有图标、点进去没有"——而详情页恰恰是最需要认出模型的地方。
	icons := iconindex.Resolve(ctx, []string{r.Model}, []uint64{r.ChannelID})
	d.ModelIconURL = iconref.URL(base, icons.Model(r.Model))
	d.ChannelIconURL = iconref.URL(base, icons.Channel(r.ChannelID))

	if io, err := ioOf(ctx, id); err == nil {
		d.IO = io
	}
	if ref, rawErr, err := taskViewOf(ctx, r.TaskID, contentBase, taskview.ViewUsage{
		Images: r.ImageN, Seconds: r.VideoSeconds, RefImages: r.RefImages,
	}); err == nil {
		d.Task, d.TaskErrorMsg = ref, rawErr
		// 任务对象的 id 就是对外号（taskview 只下发公开号），详情行跟着同源，
		// 不再单独查一次 tasks。
		if ref != nil {
			d.TaskPublicID = ref.ID
		}
	}
	// 没有报文行时用 tasks 兜底：让 C1-R3 之前的历史异步记录也能看到请求参数，
	// 否则那些记录在详情页永远显示"未采集"，用户会以为新功能坏了。
	if d.IO == nil {
		d.IO = taskFallbackIO(ctx, r.TaskID)
	}
	return d, nil
}

// ioOf 读取报文明细（不存在返回 nil，不视为错误：报文可能已被保留策略清掉）。
func ioOf(ctx context.Context, usageID uint64) (*IOView, error) {
	var row model.CallIO
	if err := db.DB.WithContext(ctx).Where("usage_id = ?", usageID).First(&row).Error; err != nil {
		return nil, err
	}
	out := &IOView{
		ReqRaw: row.ReqRaw, ReqBytes: row.ReqBytes, ReqSHA256: row.ReqSHA256,
		ReqTruncated: row.ReqTruncated, ReqSource: row.ReqSource,
		RespRaw: row.RespRaw, RespError: row.RespError, RespBytes: row.RespBytes,
		DeliveredRaw: row.DeliveredRaw,
		SSEFrames:    row.SSEFrames, RespSource: row.RespSource, BodyMode: row.BodyMode,
		UpstreamRaw: row.UpstreamRaw, UpstreamBytes: row.UpstreamBytes,
		UpstreamTruncated: row.UpstreamTruncated, UpstreamSource: row.UpstreamSource,
		UpstreamURL: row.UpstreamURL, UpstreamStatus: row.UpstreamStatus,
		UpstreamAttempts: row.UpstreamAttempts,
	}
	// 历史行没有 resp_source 列值时，按「有正文就是原文」兜底。
	if out.RespSource == "" && row.RespRaw != "" {
		out.RespSource = "raw"
	}
	if len(row.Media) > 0 {
		_ = json.Unmarshal(row.Media, &out.Media)
	}
	return out, nil
}

// billingFacts 从定价快照派生「计费方式」与「结算币种」。
//
// 为什么单独拎出来：快照会被门户裁剪清空，而这两项必须留下 ——
//   - BillingMode 决定前端用哪个结论模板（按量/按次/按规格）；
//   - Currency 决定用哪个符号（$0.12 与 ¥0.86 不是同一个数）。
func billingFacts(raw []byte) (mode string, currency string) {
	var s struct {
		BillingMode string `json:"billing_mode"`
		Currency    string `json:"currency"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return "", ""
	}
	return s.BillingMode, currencyOr(s.Currency)
}

// currencyOr 返回金额展示币种：行为空（老记录、字段缺失）时按当前结算币种。
func currencyOr(c string) string {
	if code, ok := conf.NormalizeCurrency(c); ok {
		return code
	}
	return conf.SiteCurrency()
}

// taskFallbackIO 在没有 call_io 行时，用 tasks 里的参数快照与用量原文拼一个视图。
//
// 存在的意义：C1-R3 之前创建的异步任务没有报文行，但 tasks.request_params
// 从 S7 起就一直有 —— 不给兜底，那些历史记录在详情页永远是"未采集"，
// 而用户会认为是新功能坏了。
//
// ReqSource 标 snapshot 而不是 raw：request_params 是解析后的参数快照，
// 字段顺序、未知字段、原始数字写法都已丢失，不能冒充客户端原文。
func taskFallbackIO(ctx context.Context, taskID uint64) *IOView {
	if taskID == 0 {
		return nil
	}
	var row model.Task
	if err := db.DB.WithContext(ctx).Unscoped().
		Select("request_params, upstream_usage").First(&row, taskID).Error; err != nil {
		return nil
	}
	out := &IOView{ReqSource: "snapshot"}
	if len(row.RequestParams) > 0 && string(row.RequestParams) != "null" {
		out.ReqRaw = string(row.RequestParams)
		out.ReqBytes = int64(len(row.RequestParams))
	}
	if len(row.UpstreamUsage) > 0 && string(row.UpstreamUsage) != "null" {
		out.RespRaw = string(row.UpstreamUsage)
		out.RespSource = "usage"
	}
	if out.ReqRaw == "" && out.RespRaw == "" {
		return nil
	}
	return out
}

// artifactRefs 把任务产物转成对外清单。
//
// 归一规则只有 model.ArtifactListing 一处（跳过过期、稠密重编号、生效 MIME、渲染档）。
// 不能直接调 task.ArtifactsOf：relay → logs → task → relay 是**已存在的**导入环，
// 编译期就会炸 —— 所以这里只查库，规则复用 model。
func artifactRefs(ctx context.Context, taskID uint64) []model.Artifact {
	if taskID == 0 {
		return nil
	}
	var rows []model.File
	if err := db.DB.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("artifact_index ASC").
		Find(&rows).Error; err != nil {
		return nil
	}
	return model.ArtifactListing(rows, time.Now())
}

// PortalDetail 是门户详情：在 Detail 基础上按白名单裁剪。
//
// 裁剪项（与既有两端约定一致）：渠道名 / 适配器 / IP / 上游单号 / 上游 usage 原文 /
// 计费快照（里面含 model_ratio 等平台定价参数）。
// 保留项：原始请求与响应、错误原文、扣费明细、耗时 —— 这些是用户自查需要的。
/* ------------------------------ 展示名解析 ------------------------------ */

// names 批量解析 id → 展示名（列名由调用方指定，避免 SQL 注入面）。
func names(ctx context.Context, tbl, col string, ids []uint64) map[uint64]string {
	out := map[uint64]string{}
	uniq := unique(ids)
	if len(uniq) == 0 {
		return out
	}
	var rows []struct {
		ID   uint64
		Name string
	}
	if err := db.DB.WithContext(ctx).
		Raw("SELECT id, "+col+" AS name FROM "+tbl+" WHERE id IN ?", uniq).Scan(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out
}

// KeyNames 解析 KEY 展示名（name 为空时退化为 key_prefix）。导出供门户导出复用。
func KeyNames(ctx context.Context, ids []uint64) map[uint64]string {
	out := map[uint64]string{}
	uniq := unique(ids)
	if len(uniq) == 0 {
		return out
	}
	var rows []struct {
		ID        uint64
		Name      string
		KeyPrefix string
	}
	if err := db.DB.WithContext(ctx).
		Raw("SELECT id, name, key_prefix FROM api_keys WHERE id IN ?", uniq).Scan(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		if r.Name != "" {
			out[r.ID] = r.Name
		} else {
			out[r.ID] = r.KeyPrefix
		}
	}
	return out
}

// taskPublicIDs 批量取对外任务号（内部 id → `task_xxx`）。
//
// 查不到的行（任务已被保留策略清理）不进 map：调用方按空串处理，
// 展示成"同步调用"而不是猜一个号出来。
func taskPublicIDs(ctx context.Context, ids []uint64) map[uint64]string {
	out := map[uint64]string{}
	uniq := unique(ids)
	if len(uniq) == 0 {
		return out
	}
	var rows []struct {
		ID       uint64
		PublicID string
	}
	if err := db.DB.WithContext(ctx).
		Raw("SELECT id, public_id FROM tasks WHERE id IN ?", uniq).Scan(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = r.PublicID
	}
	return out
}

func unique(ids []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// UsageIDOf 解析字符串 id（供控制器统一处理非法值，避免各处重复 strconv）。
func UsageIDOf(raw string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}
