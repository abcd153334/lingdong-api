package controller

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/pricing"
)

// Pricing 处理 GET /api/pricing（公开）：返回**用户端**定价目录（模型 + 供应商）。
//
// 响应体是 pricing.PublicSnapshot（白名单投影），不是内部 Snapshot：
//   - 不下发 `bindings`（该模型挂了几条渠道 —— 直接暴露供应链规模）；
//   - 不下发 `group_ratios`（全站折扣表，等于摸清平台的分层定价）；
//   - 不下发 `enabled` / `available`（目录只收可路由模型，这两个值恒为真，下发即双真相源）；
//   - `vendors` 只保留当前在售模型引用的分组，且只有 id / name / color。
//
// 内部 Snapshot 仍在 relay.groupRatio / 管理台使用，白名单只作用于本端点。
func Pricing(c *gin.Context) {
	snap, err := pricing.Public(siteurl.Base(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, snap)
}
