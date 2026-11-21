package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/logs"
	"lingdong-api/internal/service/taskview"
)

// 导出上限：单次导出最多 5 万行（按当前筛选结果，S11 §6 E5）。
const logExportLimit = 50000

// logFilterOf 解析日志筛选条件（scope/用户/模型/渠道/模态/类型/状态/时间）。
func logFilterOf(c *gin.Context) (logs.Filter, bool) {
	f := logs.Filter{
		Scope:    strings.TrimSpace(c.Query("scope")),
		Model:    strings.TrimSpace(c.Query("model")),
		Modality: strings.TrimSpace(c.Query("modality")),
		Kind:     strings.TrimSpace(c.Query("kind")),
		Status:   strings.TrimSpace(c.Query("status")),
		CallKind: strings.TrimSpace(c.Query("call_kind")),
		// 精确检索项：排障时手上最先有的就是 request_id 或上游单号。
		RequestID:         strings.TrimSpace(c.Query("request_id")),
		UpstreamRequestID: strings.TrimSpace(c.Query("upstream_request_id")),
		// 上游密钥指纹：多 key 渠道里「是不是就这一把在挂」只能靠它筛。
		// 值走占位符（见 logs.where），非法值只是查不到行，不需要额外校验。
		ChannelKeyID: strings.TrimSpace(c.Query("channel_key_id")),
	}
	if f.Scope != logs.ScopeArchive {
		f.Scope = logs.ScopeHot
	}
	for _, p := range []struct {
		key string
		dst *uint64
	}{{"user_id", &f.UserID}, {"channel_id", &f.ChannelID}, {"usage_id", &f.UsageID}} {
		if v := strings.TrimSpace(c.Query(p.key)); v != "" {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return f, false
			}
			*p.dst = n
		}
	}
	// 应用三态：未传不过滤、0=平台直连、n=某应用（app_id=0 必须能筛，
	// 这是「平台直连的调用有多少」唯一可用的口径）。
	appID, appScoped, ok := appScopeQuery(c)
	if !ok {
		return f, false
	}
	f.AppID, f.AppScoped = appID, appScoped
	f.From, f.To = timeRange(c)
	return f, true
}

// AdminLogList 处理 GET /api/admin/logs：明细/归档分页查询（T21 + 多条件）。
func AdminLogList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f, ok := logFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, total, err := logs.List(c.Request.Context(), f, p.Page, p.PageSize, siteurl.Base(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}

// AdminLogDetail 处理 GET /api/admin/logs/:id：单条调用记录详情（含原始报文与扣费明细）。
//
// scope=archive 时查 usages_archive：归档后的记录 id 不变，详情必须还能打开，
// 否则"归档"在运营眼里等于"数据被删了"。
func AdminLogDetail(c *gin.Context) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	if scope != logs.ScopeArchive {
		scope = logs.ScopeHot
	}
	// contentBase 用管理台自己的任务路由组：调用详情里的产物链接要在本路由组内可点，
	// 给门户前缀会得到一个必然 401 的死链。
	d, derr := logs.DetailByID(c.Request.Context(), scope, id, siteurl.Base(c), taskview.ContentBaseFor(c.FullPath()))
	if derr != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if d == nil {
		response.FailDef(c, errs.NotFound)
		return
	}
	response.OK(c, d)
}

// AdminLogAnalytics 处理 GET /api/admin/logs/analytics：类型分布 + 错误排行（与筛选联动）。
func AdminLogAnalytics(c *gin.Context) {
	f, ok := logFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	a, err := logs.AnalyticsData(c.Request.Context(), f)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, a)
}

// AdminLogExport 处理 GET /api/admin/logs/export：按当前筛选结果导出 CSV（列契约 E5）。
func AdminLogExport(c *gin.Context) {
	f, ok := logFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, err := logs.Export(c.Request.Context(), f, logExportLimit, siteurl.Base(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	name := fmt.Sprintf("logs-%s.csv", time.Now().Format("20060102-150405"))
	// 金额口径由 logs.CSV 内部按行处理（微 + 逐行币种）：
	// 刻度不再是可配的 qpc，调用方传一个换算系数只会引入第二套口径。
	writeCSV(c, name, logs.CSV(items))
}

// writeCSV 以 UTF-8 BOM 写出 CSV 附件（BOM 保证 Excel 正确识别中文）。
func writeCSV(c *gin.Context, filename, body string) {
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Data(200, "text/csv; charset=utf-8", append([]byte{0xEF, 0xBB, 0xBF}, []byte(body)...))
}

// timeRange 解析时间范围：优先 from/to（RFC3339 或 2006-01-02），否则按 range 预设（默认 24h）。
func timeRange(c *gin.Context) (time.Time, time.Time) {
	from := parseTime(c.Query("from"))
	to := parseTime(c.Query("to"))
	if !from.IsZero() || !to.IsZero() {
		return from, to
	}
	switch strings.TrimSpace(c.Query("range")) {
	case "1h":
		return time.Now().Add(-time.Hour), time.Time{}
	case "3h":
		return time.Now().Add(-3 * time.Hour), time.Time{}
	case "7d":
		return time.Now().AddDate(0, 0, -7), time.Time{}
	case "30d":
		return time.Now().AddDate(0, 0, -30), time.Time{}
	case "all":
		return time.Time{}, time.Time{}
	default:
		return time.Now().Add(-24 * time.Hour), time.Time{}
	}
}

// parseTime 解析单侧时间边界（RFC3339 / 日期 / 日期时间）。
func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}
