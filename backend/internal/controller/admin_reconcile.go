package controller

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/affiliate"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/reconcile"
)

// AdminReconcile 对账（只读）：GET /api/admin/data/reconcile。
//
// 返回余额链、冻结、用量汇总、运行态、索引健康度与返利凭据一致性；
// 不修改任何业务数据。返利段的 missing_rows 用下面的补发入口修。
func AdminReconcile(c *gin.Context) {
	rep, err := reconcile.Run(c.Request.Context())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, rep)
}

// AdminAffiliateBackfill 补发「已入账但没有任何返利行」的订单。
//
// POST /api/admin/data/reconcile/affiliate/backfill
//
// 为什么必须有这个入口：返利发放现在是「尽力而为」——失败不回滚充值、只记 ERROR。
// 那批订单不会自己长回来，而对账只能告诉你缺了多少。没有补发通道，一次故障就是
// 永久少发（用户少拿钱，且平台无法自证已经补过）。
//
// 动钱但**不动余额**：补发只写 affiliate_rewards 凭据，用户要自己点「转入余额」。
// 因此它比"直接改余额"的补发安全得多，仍要写审计。
func AdminAffiliateBackfill(c *gin.Context) {
	var body struct {
		Days   int  `json:"days"`
		Limit  int  `json:"limit"`
		DryRun bool `json:"dry_run"`
	}
	// 空请求体合法：按默认 7 天、上限 500 单执行。
	_ = c.ShouldBindJSON(&body)

	res, err := affiliate.New(db.DB).BackfillMissing(
		c.Request.Context(), affiliate.BackfillSince(body.Days), body.Limit, body.DryRun)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if body.DryRun {
		response.OKMsg(c, "试算完成（未写入）", res)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAffiliateBackfill,
		"affiliate_rewards", "", map[string]any{
			"scanned": res.Scanned, "rewarded": res.Rewarded, "skipped": res.Skipped,
			"days": body.Days,
		})
	response.OKMsg(c, "补发完成", res)
}
