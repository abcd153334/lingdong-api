package controller

import (
	"context"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/affiliate"
)

// bindAffiliate 在注册成功后绑定邀请关系（唯一入口，两个注册用例共用）。
//
// 错误**不阻断注册**：邀请码非法、达每日上限、返利未开启都是正常分支，
// 而 DB 故障也不该把「账号已创建」变成「注册失败」。
// 但必须留 WARN：静默丢弃曾经让「绑不上」永远查不出来（具体原因由 service 打点）。
//
// ctx 必须与请求生命周期解耦：调用点在注册事务**已提交之后**，此时客户端断开
// （或超时）会取消 ctx，于是账号建好了、邀请关系却永久丢失，而且没有任何重试。
func bindAffiliate(ctx context.Context, userID uint64, code string) {
	ctx = context.WithoutCancel(ctx)
	if err := affiliate.New(db.DB).OnRegister(ctx, userID, code); err != nil {
		logger.L().Warn("邀请关系绑定失败（注册已成功，不影响登录）",
			zap.Uint64("user_id", userID), zap.Error(err))
	}
}

// PortalAff 处理 GET /api/portal/aff：邀请码、受邀人数与返利额度。
func PortalAff(c *gin.Context) {
	info, err := affiliate.New(db.DB).Info(c.Request.Context(), c.GetUint64("user_id"))
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, info)
}

// PortalAffTransfer 处理 POST /api/portal/aff/transfer：返利额度转入余额。
func PortalAffTransfer(c *gin.Context) {
	credited, err := affiliate.New(db.DB).Transfer(c.Request.Context(), c.GetUint64("user_id"))
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, gin.H{"amount": credited})
}

// PortalAffRecords 处理 GET /api/portal/aff/records：直邀记录（分页）。
func PortalAffRecords(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, total, err := affiliate.New(db.DB).Records(c.Request.Context(), c.GetUint64("user_id"), p.Offset(), p.Limit())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, pagination.New(items, total, p))
}
