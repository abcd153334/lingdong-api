package controller

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/packages"
)

// packageWriteReq 是套餐写接口的请求体。
//
// 全部业务字段用指针：`sort_order: 0`（置顶）与 `badge: ""`（去掉角标）都是合法值，
// 值类型无法与「未传」区分。Enabled 只在创建时生效（更新走 PATCH /status）。
type packageWriteReq struct {
	Name      string  `json:"name"`
	PriceCent *int64  `json:"price_cent"`
	Bonus     *int64  `json:"bonus"`
	Enabled   *bool   `json:"enabled"`
	SortOrder *int    `json:"sort_order"`
	Badge     *string `json:"badge"`
}

// packageInputOf 把请求体转成 service 入参（缺省值按「必填」处理，交由 service 校验报错）。
func packageInputOf(req packageWriteReq) packages.Input {
	in := packages.Input{
		Name: req.Name, Enabled: req.Enabled, SortOrder: req.SortOrder, Badge: req.Badge,
	}
	if req.PriceCent != nil {
		in.PriceCent = *req.PriceCent
	}
	if req.Bonus != nil {
		in.Bonus = *req.Bonus
	}
	return in
}

// packageIDOf 解析 :id。0 与非数字一律 400 —— 否则会拿 id=0 去查库并回 404，
// 把「参数写错」伪装成「资源不存在」。
func packageIDOf(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam.WithMessage("套餐 ID 不合法"))
		return 0, false
	}
	return id, true
}

// packageSnap 是审计用的字段快照。改价是资金敏感操作，只记「谁改了哪个套餐」
// 无法事后追责，必须能看出改前改后的价格与赠送额度。
func packageSnap(it *packages.Item) gin.H {
	if it == nil {
		return gin.H{}
	}
	return gin.H{
		"name": it.Name, "price_cent": it.PriceCent, "bonus": it.Bonus,
		"enabled": it.Enabled, "sort_order": it.SortOrder, "badge": it.Badge,
	}
}

// adminPackagesResult 是列表响应：除分页外把「计算所需的配置」一并下发，
// 前端不硬编码任何常量（表单 max、额度预览、超上限判定的口径都来自这里）。
type adminPackagesResult struct {
	Items    []packages.Item `json:"items"`
	Total    int64           `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
	// BalanceLimit 是生效的余额上限（微），0 = 不限制。
	BalanceLimit int64 `json:"balance_limit"`
	// Scale 是金额的内部刻度（1 单位货币 = Scale 微），**下发给前端而不是让 JS 写死**：
	// 前端用它把「微」格式化成金额，刻度一旦调整，前后端不会各算各的。
	Scale          int64  `json:"scale"`
	Currency       string `json:"currency"`
	CurrencySymbol string `json:"currency_symbol"`
	MaxPriceCent   int64  `json:"max_price_cent"`
	MinPriceCent   int64  `json:"min_price_cent"`
	MaxBonus       int64  `json:"max_bonus"`
}

// AdminPackageList 处理 GET /api/admin/packages。
func AdminPackageList(c *gin.Context) {
	p, err := pagination.Parse(c)
	if err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	enabled := strings.TrimSpace(c.Query("enabled"))
	if enabled != "0" && enabled != "1" {
		enabled = ""
	}
	items, total, err := packages.New(db.DB).List(c.Request.Context(), packages.ListInput{
		Keyword: strings.TrimSpace(c.Query("keyword")), Enabled: enabled,
		Offset: p.Offset(), Limit: p.Limit(), SortField: p.Field, Desc: p.Desc,
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	response.OK(c, adminPackagesResult{
		Items: items, Total: total, Page: p.Page, PageSize: p.PageSize,
		BalanceLimit: conf.BalanceLimitFor(0),
		Scale:        money.Scale,
		Currency:     conf.SiteCurrency(), CurrencySymbol: conf.SiteCurrencySymbol(),
		MaxPriceCent: packages.MaxPriceCent, MinPriceCent: packages.MinPriceCent,
		MaxBonus: packages.MaxBonusAmount,
	})
}

// AdminPackageCreate 处理 POST /api/admin/packages。
func AdminPackageCreate(c *gin.Context) {
	var req packageWriteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	item, warn, err := packages.New(db.DB).Create(c.Request.Context(), packageInputOf(req))
	if err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionPackageCreate, "package",
		strconv.FormatUint(item.ID, 10), gin.H{"after": packageSnap(item)})
	response.OK(c, gin.H{"item": item, "warning": warn})
}

// AdminPackageUpdate 处理 PUT /api/admin/packages/:id。
//
// 刻意不接收 enabled：启停只走 PATCH /status，避免同一字段两条写路径、两种审计动作，
// 出问题时无法判断是哪条路径改的。
func AdminPackageUpdate(c *gin.Context) {
	id, ok := packageIDOf(c)
	if !ok {
		return
	}
	var req packageWriteReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	svc := packages.New(db.DB)
	before, err := svc.Get(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	in := packageInputOf(req)
	in.Enabled = nil
	item, warn, err := svc.Update(c.Request.Context(), id, in)
	if err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionPackageUpdate, "package",
		strconv.FormatUint(id, 10), gin.H{"before": packageSnap(before), "after": packageSnap(item)})
	response.OK(c, gin.H{"item": item, "warning": warn})
}

// AdminPackageStatus 处理 PATCH /api/admin/packages/:id/status。
func AdminPackageStatus(c *gin.Context) {
	id, ok := packageIDOf(c)
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		response.FailDef(c, errs.InvalidParam.WithMessage("enabled 必填"))
		return
	}
	svc := packages.New(db.DB)
	before, err := svc.Get(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	item, err := svc.SetEnabled(c.Request.Context(), id, *req.Enabled)
	if err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionPackageStatus, "package",
		strconv.FormatUint(id, 10), gin.H{"enabled_before": before.Enabled, "enabled_after": item.Enabled})
	response.OK(c, gin.H{"item": item})
}

// AdminPackageDelete 处理 DELETE /api/admin/packages/:id（软删；有订单引用则 409）。
func AdminPackageDelete(c *gin.Context) {
	id, ok := packageIDOf(c)
	if !ok {
		return
	}
	svc := packages.New(db.DB)
	before, err := svc.Get(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	if err := svc.Delete(c.Request.Context(), id); err != nil {
		respondErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionPackageDelete, "package",
		strconv.FormatUint(id, 10), gin.H{"before": packageSnap(before)})
	response.OK(c, nil)
}
