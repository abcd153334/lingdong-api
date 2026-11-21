package controller

// 应用选择器的选项端点。
//
// 为什么需要独立端点：`GET /admin/apps` 返回的是**卡片视图**（每行带密钥数/用户数/
// 消费/充值/近 7 日调用/公告数/最近版本，见 adminAppItem），一次查询要打四五条聚合。
// 而筛选下拉只需要 id/名字。把卡片接口当选项源，等于每开一个页面就为一个下拉
// 付一次全量聚合的代价（管理台的订单/日志/任务/用户四个页面都要用它）。
//
// 路径放在 `/apps/options`（而不是 `/app-options`）：gin 的 addChild 在 wildChild
// 为真时会把静态子节点插到通配符**之前**（gin@v1.10.0/tree.go:85-92），所以
// 「已存在 /apps/:id/*」与「新增静态 /apps/options」可以同级共存。
// 但"能注册"不等于"能匹配"—— 顺序一旦被写坏，`/apps/:id/*` 会静默 404，
// 所以 router 包里有一条真的发请求的回归用例守着（见 TestAdminAppOptionsRouting）。

import (
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
)

// appOptionsLimit 是选项上限。
//
// 500 的依据：应用是"接入方"，量级与租户/项目不同，真实部署通常在个位数到几十。
// 超过上限时**不截断成静默的部分结果**，而是回 truncated=true 让前端提示
// 「结果过多，请输入关键字」—— 一个悄悄少了几项的下拉，比一个明确要求你输入的
// 下拉更容易让人得出错误结论（"这个应用没有订单"）。
const appOptionsLimit = 500

// adminAppOption 是下拉项（只包含渲染与筛选必需的四项，不含任何聚合）。
type adminAppOption struct {
	ID   uint64 `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// DisplayName 是**对外品牌名**（可能为空）；前端按 `display_name || name`
	// 渲染，与后台卡片、日志/任务的应用列同一口径。
	DisplayName string `json:"display_name"`
	// Status：1 启用 / 2 停用。**停用应用也要下发** —— 历史订单/日志指向它们，
	// 筛不出来等于对账断链。前端据 status 标注「（已停用）」。
	Status int16 `json:"status"`
}

// AdminAppOptions 处理 GET /api/admin/apps/options。
//
// 排序 `status, name, id`：启用的排前面、同状态按名字、最后用 id 兜底 ——
// 必须给全序，否则同名的两个应用在不同实例/不同请求里顺序可能不同，
// 前端拿着 id 列表做 diff 时会莫名其妙地抖动。
func AdminAppOptions(c *gin.Context) {
	ctx := c.Request.Context()
	q := db.DB.WithContext(ctx).Model(&model.App{})
	if kw := strings.TrimSpace(c.Query("keyword")); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("slug ILIKE ? OR name ILIKE ?", like, like)
	}
	var rows []model.App
	if err := q.Select("id, slug, name, display_name, status").
		Order("status ASC, name ASC, id ASC").
		Limit(appOptionsLimit + 1).
		Find(&rows).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 多取一条判截断：COUNT 一次只为了说"还有更多"不值得（应用表很小，
	// 但这张表被四个页面并发读取，能省一次扫描就省）。
	truncated := len(rows) > appOptionsLimit
	if truncated {
		rows = rows[:appOptionsLimit]
	}
	items := make([]adminAppOption, 0, len(rows))
	for _, a := range rows {
		items = append(items, adminAppOption{
			ID: a.ID, Slug: a.Slug, Name: a.Name, DisplayName: a.DisplayName, Status: a.Status,
		})
	}
	// items 恒为数组（空也回 []，不是 null）：前端直接 .map，不必处理 null。
	response.OK(c, gin.H{"items": items, "truncated": truncated})
}
