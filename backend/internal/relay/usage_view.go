// 调用记录的列表投影与分页（门户 /v1/usage 与 /api/portal/usage 共用）。
//
// 两条铁律：
//  1. **列表只取需要的列**：usages 带 pricing_snapshot（jsonb，几 KB），
//     整行 Find 会让每页多传几十 KB，且新增列后越来越慢（P1-8）。
//  2. **耗时由服务端算**：started_at / finished_at 都是应用侧写入的，
//     用应用时钟求差才自洽；用数据库 now() 会在两机时钟有偏差时算出负数或跳变。
//
// 对外结构体是 logs.PortalItem（白名单投影）：本文件只做「查库 → 填 Item → 投影」，
// 不再自己定义一套 view —— 两套 view 并存必然漂移（一套删了字段、另一套没删）。

package relay

import (
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/sorting"
	"lingdong-api/internal/service/iconindex"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/logs"
)

// usageListColumns 是列表投影的列清单（与 usageRow 字段严格对应）。
//
// 必须带上 currency：门户列表要按**当时的**币种显示金额。金额本身是微（绝对金额），
// 不存在"再乘一次刻度"的换算，所以只需要币种符号，不需要任何基准列。
// 缺了 currency 的话历史行会按当前币种符号展示 —— 用户会认为系统在改数。
//
// **刻意不取** error_msg / upstream_request_id / cap_reason / usage_source：
// 它们不参与用户端任何展示（错误只给分类文案），取了就是「先拿到敏感数据再指望别泄漏」。
const usageListColumns = `id, key_id, created_at, model, modality, status, http_status,
	prompt_tokens, completion_tokens, cache_tokens, image_tokens,
	audio_input_tokens, audio_output_tokens, charge, latency_ms,
	upstream_ms, first_token_ms, attempts, started_at, finished_at,
	is_stream, request_id, call_kind, task_id,
	video_resolution, video_seconds, tier_price, charge_before_cap,
	anomaly, currency`

// usageRow 是列表的数据库投影。
type usageRow struct {
	ID                uint64
	KeyID             uint64
	CreatedAt         time.Time
	Model             string
	Modality          string
	Status            string
	HTTPStatus        int
	PromptTokens      int64
	CompletionTokens  int64
	CacheTokens       int64
	ImageTokens       int64
	AudioInputTokens  int64
	AudioOutputTokens int64
	Charge            int64
	LatencyMs         int
	UpstreamMs        int
	FirstTokenMs      int
	Attempts          int
	StartedAt         *time.Time
	FinishedAt        *time.Time
	IsStream          bool
	RequestID         string
	CallKind          string
	TaskID            uint64
	VideoResolution   string
	VideoSeconds      int64
	TierPrice         int64
	ChargeBeforeCap   int64
	Anomaly           string
	Currency          string
}

// toView 转成用户端视图。
//
// elapsed_ms 的口径：
//   - 终态（finished_at 有值）→ 与 latency_ms 同源，避免两个数字打架；
//   - 进行中 → now − started_at（started_at 为空的历史行回落 created_at）。
//
// 基准全部来自服务端，前端只做「本地单调时钟 +1s」的增量，
// 所以刷新页面能接着涨、客户端改系统时间也不会跳变。
func (r usageRow) toItem(now time.Time, keyNames map[uint64]string) logs.Item {
	start := r.CreatedAt
	if r.StartedAt != nil && !r.StartedAt.IsZero() {
		start = *r.StartedAt
	}
	elapsed := int64(0)
	switch {
	case r.FinishedAt != nil && !r.FinishedAt.IsZero():
		if d := r.FinishedAt.Sub(start).Milliseconds(); d > 0 {
			elapsed = d
		}
	case r.Status == "pending":
		if d := now.Sub(start).Milliseconds(); d > 0 {
			elapsed = d
		}
	default:
		// 终态但没写 finished_at（升级前的存量行）：用 latency_ms 兜底，
		// 否则"已完成"的记录会显示成一直在进行。
		elapsed = int64(r.LatencyMs)
	}
	// 先落成内部 Item 再一次性投影：投影逻辑只有 logs.PortalItemOf 一处，
	// 列表与详情不会各自裁剪出两套口径。
	//
	// 为什么分两步而不是直接返回 PortalItem：对外任务号必须**整页批量**补
	// （logs.FillTaskPublicIDs），逐行补会让 100 行变 100 次查询；而"补号"
	// 这一步在控制面列表里也存在 —— 两边必须调同一个函数，否则又会出现
	// 「列表没有 task_id、详情有」。
	//
	// KeyName 必须在这里补：logs.PortalItemOf 会原样拷贝它，前端「调用记录」也有 KEY 列，
	// 而本函数此前没传 —— 表现为门户那一列恒显示 "—"（对外面更是无法把消费归到应用）。
	// 取值走调用方批量查好的 map，不在循环里查库（N≤100 会变成 100 次查询）。
	return logs.Item{
		ID: r.ID, KeyID: r.KeyID, KeyName: keyNames[r.KeyID], CreatedAt: r.CreatedAt,
		Model: r.Model, Modality: r.Modality, Status: r.Status, HTTPStatus: r.HTTPStatus,
		PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
		CacheTokens: r.CacheTokens, ImageTokens: r.ImageTokens,
		AudioInputTokens: r.AudioInputTokens, AudioOutputTokens: r.AudioOutputTokens,
		Charge: r.Charge, LatencyMs: r.LatencyMs, ElapsedMs: elapsed,
		UpstreamMs: r.UpstreamMs, FirstTokenMs: r.FirstTokenMs, Attempts: r.Attempts,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, IsStream: r.IsStream,
		RequestID: r.RequestID, CallKind: r.CallKind, TaskID: r.TaskID,
		Anomaly: r.Anomaly, Currency: r.Currency,
	}
}

// usageSortColumns 是调用记录列表的排序白名单：**对外字段名 → SQL 列**。
//
// 键集合必须与 apicontract 里 `usage.list` 的 Sortable 逐字一致，
// 由门禁测试双向比对（多一个 = 文档没写但能传，少一个 = 文档写了但会 400）。
var usageSortColumns = map[string]string{
	"created_at": "created_at",
	"latency_ms": "latency_ms",
	"charge":     "charge",
}

// UsageSortColumns 返回排序白名单的**副本**（门禁测试用）。
//
// 给副本而不是直接暴露变量：白名单是 SQL 拼串的唯一防线，谁都能改的全局 map
// 等于没有防线。handler 内部仍用上面那份，这里只服务"契约与实现是否一致"的比对。
func UsageSortColumns() map[string]string {
	out := make(map[string]string, len(usageSortColumns))
	for k, v := range usageSortColumns {
		out[k] = v
	}
	return out
}

// usagePage 执行调用记录的分页查询（q 需已完成筛选）。
//
// order 由调用方解析后传入（而不是在这里读 c.Query("sort")）：
// 同一个函数还被 /v1/usage 兼容端点复用，而那条端点的契约在 internal/apidoc 里
// **没有** sort 参数 —— 在这里统一读会让它悄悄多出一个未声明的能力。
func usagePage(c *gin.Context, q *gorm.DB, order sorting.Order, p pagination.Params) ([]logs.PortalItem, int64, int, int, error) {
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, 0, 0, err
	}
	// 参数已由调用方用 pagination.Parse 严格校验（非法即 400）：
	// 曾经这里用「非法回落第 1 页」，于是 page=abc 静默返回第一页，
	// 而调用方以为翻到了自己请求的那一页。
	page, size := p.Page, p.PageSize
	var rows []usageRow
	if err := q.Select(usageListColumns).Order(order.ClauseOr("id DESC")).
		Offset(p.Offset()).Limit(p.Limit()).Scan(&rows).Error; err != nil {
		return nil, 0, 0, 0, err
	}
	now := time.Now()
	// 一次批量取名：页内最多 pagination.MaxPageSize 条，去重后一条 IN 查询。
	keyIDs := make([]uint64, 0, len(rows))
	for _, r := range rows {
		keyIDs = append(keyIDs, r.KeyID)
	}
	keyNames := logs.KeyNames(c.Request.Context(), keyIDs)
	// 这里是**另一条**数据源（不走 logs.List）：用户端调用记录直接由 usage 表扫出来，
	// 所以图标必须在这里单独补一次 —— 漏了的表现是"管理台有图、门户没有"。
	modelIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		modelIDs = append(modelIDs, r.Model)
	}
	icons := iconindex.Resolve(c.Request.Context(), modelIDs, nil)
	base := siteurl.Base(c)
	raw := make([]logs.Item, 0, len(rows))
	for _, r := range rows {
		it := r.toItem(now, keyNames)
		it.ModelIconURL = iconref.URL(base, icons.Model(r.Model))
		raw = append(raw, it)
	}
	// 补号与投影各一次：调用记录列表/详情/导出用的是同一对函数。
	logs.FillTaskPublicIDs(c.Request.Context(), raw)
	items := make([]logs.PortalItem, 0, len(raw))
	for i := range rows {
		items = append(items, logs.PortalItemOf(raw[i],
			rows[i].VideoResolution, rows[i].VideoSeconds, rows[i].TierPrice, rows[i].ChargeBeforeCap))
	}
	return items, total, page, size, nil
}
