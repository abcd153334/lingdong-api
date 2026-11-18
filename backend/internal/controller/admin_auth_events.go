package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/authevent"
)

// 认证事件导出上限：单次最多 5 万行（与调用日志/审计导出同口径）。
const authEventExportLimit = 50000

// authEventFilterOf 解析认证事件筛选条件（事件/结果/用户/IP/时间）。
func authEventFilterOf(c *gin.Context) (authevent.Filter, bool) {
	f := authevent.Filter{
		Event:   strings.TrimSpace(c.Query("event")),
		Result:  strings.TrimSpace(c.Query("result")),
		IP:      strings.TrimSpace(c.Query("ip")),
		Account: strings.TrimSpace(c.Query("account")),
	}
	uid, ok := optionalUintQuery(c, "uid")
	if !ok {
		return f, false
	}
	f.UID = uid
	if f.Result != "" && f.Result != authevent.ResultSuccess && f.Result != authevent.ResultFail {
		return f, false
	}
	f.From, f.To = timeRange(c)
	return f, true
}

// AdminAuthEventList 处理 GET /api/admin/auth-events：认证事件分页查询。
func AdminAuthEventList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	f, ok := authEventFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, total, err := authevent.List(c.Request.Context(), f, p.Page, p.PageSize)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}

// AdminAuthEventExport 处理 GET /api/admin/auth-events/export：按当前筛选导出 CSV。
func AdminAuthEventExport(c *gin.Context) {
	f, ok := authEventFilterOf(c)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, err := authevent.Export(c.Request.Context(), f, authEventExportLimit)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	name := fmt.Sprintf("auth-events-%s.csv", time.Now().Format("20060102-150405"))
	writeCSV(c, name, authevent.CSV(items))
}

// AdminAuthEventMeta 处理 GET /api/admin/auth-events/meta：事件选项 + 保留天数。
//
// 事件选项取自 `authevent` 的白名单（编译期常量），不从库里 GROUP BY：
// 库里只有「发生过的事件」，而筛选器需要「能筛的所有事件」——
// 一次攻击没发生后就没法按它筛，正是最需要排查的时候。
func AdminAuthEventMeta(c *gin.Context) {
	response.OK(c, gin.H{
		"events":         authevent.Options(),
		"retention_days": authcfg.EventRetentionDays(),
	})
}
