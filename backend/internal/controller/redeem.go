package controller

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/redeem"
)

type redeemReq struct {
	Code string `json:"code"`
}

// PortalRedeem 处理 POST /api/portal/redeem：兑换码核销入账。
func PortalRedeem(c *gin.Context) {
	var req redeemReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	credited, err := redeem.New(db.DB).Redeem(c.Request.Context(), c.GetUint64("user_id"), req.Code)
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"amount": credited})
}

// UserRedeemList 处理 GET /api/user/redeems：我的兑换记录（仅会话票）。
//
// 为什么只给会话票：兑换记录是账户级账单视图，托管密钥代表的是"某个应用"，
// 让它读用户在平台直连或其他应用下的兑换历史，等同于把账单摊开给接入方。
// 应用侧对账走 /api/user/transactions（已按需脱敏余额）。
func UserRedeemList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam.WithMessage(err.Error()))
		return
	}
	items, total, err := redeem.New(db.DB).ListByUser(c.Request.Context(), c.GetUint64("user_id"), p.Offset(), p.Limit())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}
