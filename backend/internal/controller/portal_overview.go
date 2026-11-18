package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/stats"
)

// PortalOverview 处理 GET /api/portal/overview?days=7|30：门户主页（Bento 工作台）聚合数据（S15）。
//
// 口径见 stats.UserOverview：今日实时直查明细（与管理台看板同源）、本月 = 聚合表(月初~昨日) + 今日、
// 充值走 transactions 净额。generated_at 是**取数时刻**（页面不轮询，这个时间就是数字的真实年龄）。
//
// user_id 一律取自 JWT 上下文，不接受任何请求参数（防越权读取他人用量）；
// days 非法值由 service 回落为 7。
func PortalOverview(c *gin.Context) {
	days := 7
	if raw := c.Query("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			days = n
		}
	}
	view, err := stats.UserOverview(c.Request.Context(), c.GetUint64("user_id"), days)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, view)
}
