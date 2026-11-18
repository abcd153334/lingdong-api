package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/redeem"
)

// 兑换码导出上限：单次最多 5 万行（与日志/审计导出一致）。
const redeemExportLimit = 50000

// redeemGenerateReq 是批量生成请求体。
type redeemGenerateReq struct {
	Batch     string `json:"batch"`
	Amount    int64  `json:"amount"`
	Count     int    `json:"count"`
	ExpiresAt string `json:"expires_at"` // YYYY-MM-DD，空 = 永不过期
}

// parseExpiresAt 解析「有效期至」日期：语义为「该日 23:59:59 前有效」，故按当日末刻落库。
// 返回 (exp, "") 表示合法（exp 为 nil 表示永不过期）；返回非空 msg 表示该字段不合法。
func parseExpiresAt(raw string) (*time.Time, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	d, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil {
		return nil, "有效期格式应为 YYYY-MM-DD"
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	if d.Before(today) {
		return nil, "有效期不得早于今日"
	}
	end := d.AddDate(0, 0, 1).Add(-time.Nanosecond)
	return &end, ""
}

// AdminRedeemGenerate 处理 POST /api/admin/redeems：批量生成兑换码（单事务 + 唯一性重试）。
func AdminRedeemGenerate(c *gin.Context) {
	var req redeemGenerateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	req.Batch = strings.TrimSpace(req.Batch)
	if req.Batch == "" || len(req.Batch) > 64 {
		response.FailDef(c, errs.InvalidParam.WithMessage("批次名必填且不超过 64 字符"))
		return
	}
	// 面额是**金额**（微），必须是正整数：0 或负数表示「这张码不给钱」，
	// 那不该通过生成接口产生（要发免费码请用 1 微，语义明确）。
	if req.Amount <= 0 {
		response.FailDef(c, errs.InvalidParam.WithMessage("面额必须大于 0"))
		return
	}
	if cap := conf.BalanceLimitFor(0); cap > 0 && req.Amount > cap {
		response.FailDef(c, errs.InvalidParam.WithMessage(fmt.Sprintf(
			"面额不得超过余额上限 %s（超过会立刻触发上限截断，等于白送一张用不完的码）",
			money.Format(cap, 2))))
		return
	}
	if req.Count < 1 || req.Count > redeem.MaxGenerateCount {
		response.FailDef(c, errs.InvalidParam.WithMessage(fmt.Sprintf("数量取值范围为 1-%d", redeem.MaxGenerateCount)))
		return
	}
	expiresAt, msg := parseExpiresAt(req.ExpiresAt)
	if msg != "" {
		response.FailDef(c, errs.InvalidParam.WithMessage(msg))
		return
	}
	codes, err := redeem.New(db.DB).Generate(c.Request.Context(), redeem.GenerateInput{
		Batch: req.Batch, Amount: req.Amount, Count: req.Count, ExpiresAt: expiresAt,
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionRedeemGenerate, "redeem_code", req.Batch,
		gin.H{"batch": req.Batch, "amount": req.Amount, "count": len(codes),
			"expires_at": req.ExpiresAt})
	response.OK(c, gin.H{"created": len(codes), "codes": codes})
}

// redeemStatuses 是合法的派生状态枚举（其余值按「不过滤」处理，见 §4.3）。
var redeemStatuses = map[string]bool{
	redeem.StatusAvailable: true, redeem.StatusUsed: true, redeem.StatusExpired: true,
}

// redeemListInputOf 解析列表/导出共用的筛选条件（batch/status）。
func redeemListInputOf(c *gin.Context) redeem.ListInput {
	batch := strings.TrimSpace(c.Query("batch"))
	status := strings.TrimSpace(c.Query("status"))
	if !redeemStatuses[status] {
		status = ""
	}
	return redeem.ListInput{Batch: batch, Status: status}
}

// AdminRedeemList 处理 GET /api/admin/redeems：兑换码分页查询（batch/status 可选筛选）。
func AdminRedeemList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	in := redeemListInputOf(c)
	in.Offset, in.Limit = p.Offset(), p.Limit()
	items, total, err := redeem.New(db.DB).List(c.Request.Context(), in)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}

type redeemDeleteReq struct {
	IDs []uint64 `json:"ids"`
}

// AdminRedeemDelete 处理 POST /api/admin/redeems/delete：批量作废（仅未使用的码，幂等跳过）。
func AdminRedeemDelete(c *gin.Context) {
	var req redeemDeleteReq
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	deleted, skipped, err := redeem.New(db.DB).DeleteBatch(c.Request.Context(), req.IDs)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if deleted > 0 {
		target := fmt.Sprintf("%d 条", len(req.IDs))
		if len(req.IDs) == 1 {
			target = strconv.FormatUint(req.IDs[0], 10)
		}
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionRedeemDelete, "redeem_code", target,
			gin.H{"ids": req.IDs, "deleted": deleted, "skipped": skipped})
	}
	response.OK(c, gin.H{"deleted": deleted, "skipped": skipped})
}

// AdminRedeemExport 处理 GET /api/admin/redeems/export：按当前筛选导出 CSV（无数据返回空表头）。
func AdminRedeemExport(c *gin.Context) {
	in := redeemListInputOf(c)
	in.Limit = redeemExportLimit
	items, err := redeem.New(db.DB).ExportRows(c.Request.Context(), in, redeemExportLimit)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	name := fmt.Sprintf("redeem-codes-%s.csv", time.Now().Format("20060102-150405"))
	writeCSV(c, name, redeem.CSV(items))
}
