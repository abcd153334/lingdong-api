package relay

import (
	"context"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/keyscope"
	"lingdong-api/internal/pkg/response"
)

// ModelStat 是「按模型聚合」项：请求数 + 输入/输出 token + 消耗额度。
type ModelStat struct {
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Charge           int64  `json:"charge"`
}

// UsageSummary 处理 GET /v1/usage/summary（API KEY 鉴权，设计 §4.3.9 L406）：按模型聚合当前 KEY 用量。
func UsageSummary(c *gin.Context) {
	k := apiKeyOf(c)
	if k == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	f, _ := parseUsageFilter(c, false)
	f.KeyID = 0 // 恒定按当前 KEY 作用域，忽略客户端传入的 key_id
	items, err := modelDist(c.Request.Context(), f, db.DB.Model(&model.Usage{}).Where("key_id = ?", k.ID))
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"items": items})
}

// PortalUsageSummary 处理 GET /api/portal/usage/summary（门户 JWT，设计 §4.3.11 L404）：
// 复用 PortalUsage 的同源筛选口径（f.apply），仅改分组为按模型，含 key_id 二次过滤。
func PortalUsageSummary(c *gin.Context) {
	userID := c.GetUint64("user_id")
	if userID == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	f, ok := parseUsageFilter(c, true)
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 托管密钥只能看**自己这把 KEY** 产生的消费汇总。
	//
	// 与 PortalUsage / PortalUsageExport 同一口径（都用 apiKeyOf + IsHosted 覆盖）：
	// 漏掉这里，应用 A 的托管密钥就能读到该用户整个账号（含应用 B 与平台直连）
	// 的按模型消费 —— 信息越权，且对账时无法区分"我花的"与"别人花的"。
	if s, _ := keyscope.Of(c); s.Hosted {
		f.KeyID = s.KeyID
	}
	items, err := modelDist(c.Request.Context(), f, db.DB.Model(&model.Usage{}).Where("user_id = ?", userID))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"items": items})
}

// modelDist 在 usages 上按当前筛选条件实时聚合（与 detail 列表同源，不做 usage_daily 二次口径），按模型分组。
func modelDist(ctx context.Context, f usageFilter, scope *gorm.DB) ([]ModelStat, error) {
	out := make([]ModelStat, 0, 8)
	err := f.apply(scope.WithContext(ctx)).
		Select("model, COUNT(*) AS requests, COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens, " +
			"COALESCE(SUM(completion_tokens), 0) AS completion_tokens, COALESCE(SUM(charge), 0) AS charge").
		Group("model").Order("charge DESC, model ASC").Scan(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}
