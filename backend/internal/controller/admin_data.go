package controller

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/stats"
)

// AdminBoard 处理 GET /api/admin/data/board：经营看板聚合（今日指标 + 近 7 日趋势 + d7/d30 双档）。
func AdminBoard(c *gin.Context) {
	b, err := stats.BoardData(c.Request.Context(), siteurl.Base(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, b)
}
