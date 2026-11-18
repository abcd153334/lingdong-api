package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/audit"
)

// 审计导出上限：单次最多 5 万行。
const auditExportLimit = 50000

// auditFilterOf 解析审计筛选条件（动作/操作人/关键字/时间）。
func auditFilterOf(c *gin.Context) (audit.Filter, bool) {
	f := audit.Filter{
		Action:  strings.TrimSpace(c.Query("action")),
		Keyword: strings.TrimSpace(c.Query("keyword")),
	}
	adminID, ok := optionalUintQuery(c, "admin_id")
	if !ok {
		return f, false
	}
	f.AdminID = adminID
	f.From, f.To = timeRange(c)
	return f, true
}

// AdminAuditList 处理 GET /api/admin/audits：审计明细分页（动作/操作人/时间组合过滤）。
func AdminAuditList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f, ok := auditFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, total, err := audit.List(c.Request.Context(), f, p.Page, p.PageSize)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}

// AdminAuditExport 处理 GET /api/admin/audits/export：按当前筛选结果导出 CSV。
func AdminAuditExport(c *gin.Context) {
	f, ok := auditFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, _, err := audit.List(c.Request.Context(), f, 1, auditExportLimit)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	name := fmt.Sprintf("audit-%s.csv", time.Now().Format("20060102-150405"))
	writeCSV(c, name, audit.CSV(items))
}

// AdminAuditMeta 处理 GET /api/admin/audits/meta：动作集合（数据驱动筛选）+ 保留天数。
func AdminAuditMeta(c *gin.Context) {
	actions, err := audit.Actions(c.Request.Context())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{
		"actions":        actions,
		"retention_days": conf.System().Int("audit_retention_days", 180),
	})
}
