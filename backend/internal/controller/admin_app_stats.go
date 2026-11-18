package controller

// 应用的统计视图（概览图 + 在线状态）。
//
// 为什么不再有「概览」这一个复合接口：品牌页签为了编辑 9 个字符串，原先必须
// 先拉一次含 7 日聚合的概览，而且聚合没算完表单不渲染。四格汇总（消费/充值/
// 密钥/用户）改由列表接口随卡片下发，弹窗直接复用 —— 卡片与弹窗天然同源。

import (
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	appsvc "lingdong-api/internal/service/app"
)

// AdminAppStats 处理 GET /api/admin/apps/{id}/stats。
func AdminAppStats(c *gin.Context) {
	a, ok := appOf(c)
	if !ok {
		return
	}
	v, err := appsvc.StatsOf(c.Request.Context(), db.DB, a.ID, time.Now())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, v)
}
