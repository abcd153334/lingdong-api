package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/service/audit"
	iconsvc "lingdong-api/internal/service/icon"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/lifecycle"
	"lingdong-api/internal/service/modelcfg"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/stats"
	"lingdong-api/internal/service/upstream"
)

// vendorPalette 是新建供应商分组的取色盘（对齐原型 admin-models.html）。
var vendorPalette = []string{
	"#6366F1", "#3B82F6", "#8B5CF6", "#F97316",
	"#EC4899", "#06B6D4", "#10B981", "#F59E0B",
}

// adminModelItem 是管理台模型行。
//
// 价格一律按**微**原样下发（`Price` / `SpecPrice`），不在服务端做任何换算：
// 绝对价时代不存在"倍率 ↔ 金额"的两次锚点，读侧用 `scale`（恒 1e6）格式化成
// 展示金额即可。曾经的 model_ratio/input_price(元) 双份展示字段是"写 ×500000、
// 读 ÷qpc"这类错账的来源，已整体删除。
type adminModelItem struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	// Description 必须回显：编辑表单要靠它回填，否则"保存一次就把描述清空"
	// （前端拿不到现值 → 只能发空串 → 后端按显式清空处理）。
	Description string `json:"description"`
	Vendor      string `json:"vendor"`
	VendorName  string `json:"vendor_name"`
	VendorColor string `json:"vendor_color"`
	// Icon 是**最终展示值**（自己设的 or 沿用供应商的），列表/门户都直接渲染它。
	// IconOwn 是模型自己设的原始值（""=未设），只给编辑表单回填 ——
	// 两者分开是为了让"沿用供应商"这件事可表达：表单里显示空、列表里显示图标。
	Icon    string `json:"icon"`
	IconOwn string `json:"icon_own"`
	// IconURL 是 Icon 的可渲染地址（= 最终展示值）。列表直接 `<img src>`；
	// 表单用它给「已经设了自己的图标」这一帧做预览，选中未保存时用图标库回包的 url。
	IconURL     string `json:"icon_url"`
	Modality    string `json:"modality"`
	BillingMode string `json:"billing_mode"`
	Enabled     bool   `json:"enabled"`
	IsDefault   bool   `json:"is_default"`
	// IsFree 是「价格为 0」的派生事实（保存期由价格唯一派生），管理台回显只读展示。
	IsFree bool `json:"is_free"`
	// Prices 是 9 条 token lane + 按次价（微），SpecPrice 是按规格价目。
	Prices     model.Prices     `json:"prices"`
	SpecPrices model.SpecPrices `json:"spec_prices"`
	Endpoints  model.JSON       `json:"endpoints"`
	// Capabilities 回显能力声明（NULL 表示「未声明」，前端据此显示「按事实推荐」按钮）。
	Capabilities model.JSON `json:"capabilities"`
	// Tags 回显营销标签（**含已过期项**，运营要能续期或删除）。
	// 不回显的后果与 description 一样：表单拿不到现值 → 保存一次就清空。
	Tags []adminModelTagView `json:"tags"`
	// ContextWindow / MaxOutputTokens 是模型级覆盖，0 = 继承助手全局设置。
	// 必须回显：不回显的话运营在模型页看到的恒为 0，一保存就把填过的值清成"继承"。
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
	// Version 是乐观锁版本号：编辑保存时必须回传，否则并发改价会静默覆盖。
	Version  int   `json:"version"`
	Bindings int64 `json:"bindings"`
	// Binding 是该模型在**当前筛选渠道**上的绑定；只有请求带 channel_id 时才下发。
	//
	// 为什么放进列表项而不是另开一条「渠道的模型」接口：右栏就是「某渠道的模型列表」，
	// 每行都要显示这条渠道上的上游名/权重/启停。另开接口会把「模型」与「绑定」
	// 拆成两个资源族，读侧出现两个真相源（列表里的 N 个渠道 vs 详情里的绑定集合）。
	Binding *adminModelBindingView `json:"binding,omitempty"`
}

// adminModelBindingView 是列表项里附带的那一条绑定（联表条件见 ListAdminModels）。
type adminModelBindingView struct {
	UpstreamModel string `json:"upstream_model"`
	Weight        int    `json:"weight"`
	Priority      int    `json:"priority"`
	Enabled       bool   `json:"enabled"`
}

// adminModelTagView 是管理台的标签回显。
//
// 比对外多三项，都有明确用途：`color` 用于色块选中态回填（入库形态可能是 key
// 也可能是 #RRGGBB，前端不能靠 color_hex 反推）；`until_date` / `expired` 由
// 服务端按业务时区算好，前端不碰时钟与时区。
type adminModelTagView struct {
	Label     string `json:"label"`
	Color     string `json:"color"`
	ColorHex  string `json:"color_hex"`
	Until     string `json:"until,omitempty"`
	UntilDate string `json:"until_date,omitempty"`
	Expired   bool   `json:"expired"`
}

// adminTagsOf 组装管理台标签（**不过滤过期项**）。
func adminTagsOf(m *model.Model) []adminModelTagView {
	raw := model.DecodeTags(m.Tags)
	out := make([]adminModelTagView, 0, len(raw))
	now := time.Now()
	loc := stats.Location()
	for _, t := range raw {
		out = append(out, adminModelTagView{
			Label:     t.Label,
			Color:     t.Color,
			ColorHex:  model.TagViewOf(t).ColorHex,
			Until:     t.Until,
			UntilDate: model.TagUntilDate(t.Until, loc),
			Expired:   model.TagExpired(t.Until, now),
		})
	}
	return out
}

// adminModelAllLimit 是「不传 page（全量）」时的行数上限。
//
// 保留一个上限而不是真的全表：模型下拉是它的使用者，几百个模型足够；
// 而没有上限时一次误用就能把整张表拉进内存（含全部 JSON 列）。
const adminModelAllLimit = 5000

// ListAdminModels 处理 GET /api/admin/models（关键词/供应商过滤，非分页）。
func ListAdminModels(c *gin.Context) {
	// 同上：计数与取数各自构建条件链，避免 GORM statement 状态复用。
	// channel_id / enabled 是「模型服务」右栏的两个维度：
	//   - channel_id：只看这个渠道上的模型（存在一条绑定即算，**不看绑定启停** ——
	//     启停是列表内的筛选条件，不是可见性条件，否则停用一条绑定模型会凭空消失）；
	//   - enabled：模型自身的启停。
	chID, ok := optionalUintQuery(c, "channel_id")
	if !ok {
		response.FailDef(c, errs.InvalidParam.WithMessage("channel_id 必须是正整数"))
		return
	}
	var enabledFilter *bool
	if raw := strings.TrimSpace(c.Query("enabled")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			response.FailDef(c, errs.InvalidParam.WithMessage("enabled 只能是 true 或 false"))
			return
		}
		enabledFilter = &v
	}
	scope := func() *gorm.DB {
		qy := db.DB.Model(&model.Model{})
		if kw := strings.TrimSpace(c.Query("keyword")); kw != "" {
			qy = qy.Where("id ILIKE ? OR display_name ILIKE ?", "%"+kw+"%", "%"+kw+"%")
		}
		if v := c.Query("vendor"); v != "" {
			qy = qy.Where("vendor = ?", v)
		}
		if chID > 0 {
			qy = qy.Where("EXISTS (SELECT 1 FROM model_bindings b WHERE b.model_id = models.id"+
				" AND b.channel_id = ? AND b.deleted_at IS NULL)", chID)
		}
		if enabledFilter != nil {
			qy = qy.Where("enabled = ?", *enabledFilter)
		}
		return qy
	}
	// B3：分页 + 排序同时下推（page 缺省/0 = 全量，配置页的模型下拉依赖该行为）。
	//
	// 非法 page 必须 400：曾经用 `Atoi` 忽略错误，于是 page=abc 与 page 缺省
	// 都是 0 —— 一次笔误就把分页查询变成"最多 5000 行的全表扫描"，
	// 前端还按 page=1 显示，两边都不报错。
	p, all, err := pagination.ParseOptional(c)
	if err != nil {
		response.FailDefMsg(c, errs.InvalidParam, err.Error())
		return
	}
	order, err := adminModelOrder(c.Query("sort"), c.Query("order"))
	if err != nil {
		response.FailDefMsg(c, errs.InvalidParam, "不支持的排序字段")
		return
	}

	var total int64
	limit := adminModelAllLimit
	if !all {
		if err := scope().Count(&total).Error; err != nil {
			response.FailDef(c, errs.Internal)
			return
		}
		limit = p.Limit()
	}

	var models []model.Model
	sel := scope().Order(order).Limit(limit)
	if !all {
		sel = sel.Offset(p.Offset())
	}
	if err := sel.Find(&models).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if all {
		total = int64(len(models))
	}

	vendors, err := vendorList()
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	vendorByID := map[string]pricing.VendorItem{}
	for _, v := range vendors {
		vendorByID[v.ID] = v
	}

	ids := make([]string, 0, len(models))
	for i := range models {
		ids = append(ids, models[i].ID)
	}
	counts := bindingCounts(ids)
	// base 只在这里取一次：多域名部署下它跟着请求走，快照里不带任何地址。
	base := siteurl.Base(c)
	// 指定渠道时一次查出这批模型在该渠道上的绑定 —— 右栏每行要显示上游名/权重/启停。
	// 逐行查会变成 N+1（20 行 = 20 次往返）。
	onChannel := map[string]adminModelBindingView{}
	if chID > 0 && len(ids) > 0 {
		var rows []model.ModelBinding
		if err := db.DB.Where("channel_id = ? AND model_id IN ?", chID, ids).Find(&rows).Error; err != nil {
			response.FailDef(c, errs.Internal)
			return
		}
		for _, b := range rows {
			onChannel[b.ModelID] = adminModelBindingView{
				UpstreamModel: b.UpstreamModel, Weight: b.Weight,
				Priority: b.Priority, Enabled: b.Enabled,
			}
		}
	}
	items := make([]adminModelItem, 0, len(models))
	for i := range models {
		it := toAdminModelItem(&models[i], vendorByID[models[i].Vendor], counts[models[i].ID], base)
		if bv, ok := onChannel[models[i].ID]; ok {
			bound := bv
			it.Binding = &bound
		}
		items = append(items, it)
	}
	resp := gin.H{
		"items": items, "vendors": vendorViews(vendors, base), "total": total,
		// 色板由后端下发：前端自己维护一份 12 色 = 第二张色表，改色时两边必然漂移。
		"tag_palette": model.TagPalette,
	}
	if !all {
		resp["page"] = p.Page
		resp["page_size"] = p.PageSize
	}
	response.OK(c, resp)
}

// adminModelSortable 是模型列表的服务端排序白名单（值直接进 ORDER BY，必须映射）。
var adminModelSortable = map[string]string{
	"id":           "id",
	"display_name": "display_name",
	"vendor":       "vendor",
	"billing_mode": "billing_mode",
	"enabled":      "enabled",
	"created_at":   "created_at",
}

// adminModelOrder 翻译排序参数（缺省 id ASC）。
func adminModelOrder(field, dir string) (string, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "id ASC", nil
	}
	col, ok := adminModelSortable[field]
	if !ok {
		return "", fmt.Errorf("unsupported sort: %s", field)
	}
	if strings.EqualFold(strings.TrimSpace(dir), "desc") {
		return col + " DESC", nil
	}
	return col + " ASC", nil
}

// adminModelReq 是创建/编辑模型的请求体。
//
// 价格一律按**微**提交（1 货币单位 = 1e6 微，`money.Scale`），与 AI 提案的
// `agent.ModelData` 完全同一套字段名与单位 —— 人工保存与 AI apply 共用
// `modelcfg.Build`，如果两边单位不同，就必然出现"AI 建的模型价格是人工的
// 百万分之一"这类只有对账才发现的错。
type adminModelReq struct {
	ID string `json:"id"`
	// InitialBinding 只在**创建**时生效：一次事务里建模型并绑到这一个渠道。
	//
	// 为什么是单对象而不是数组、为什么叫 initial_binding 而不叫 bindings：
	// 前者匹配「在渠道下新建模型」这个唯一场景（多渠道请建完再逐条绑），
	// 后者这个键名在本结构里已经被 `Bindings int64`（绑定**数量**）占着，
	// 复用会让数组塞进 int64，表现为 400 或静默当 0。
	InitialBinding *bindingInput `json:"initial_binding"`
	// Version 是乐观锁：0 = 不校验（脚本/AI 提案的老调用方式），>0 时版本不匹配直接 409。
	//
	// 人工路径原来固定传 0，两个管理员同时改价会「后写覆盖前写」且没有任何痕迹 ——
	// 价格争议时只能看到最终值，看不到被覆盖的那一版。
	Version int `json:"version"`
	// DisplayName / Description 必须是指针：nil = 不修改（沿用库内现值）。
	//
	// 用字符串零值时，任何"只改价格"的保存都会把展示名与描述写成空串：
	// 2026-10-29 审计实测 `PUT /api/admin/models/{id}` 带 `{}` 返回 200，
	// `display_name` 从 "Qwen3 Reranker" 变空、version +1，无任何日志。
	DisplayName *string `json:"display_name"`
	Vendor      string  `json:"vendor"`
	Modality    string  `json:"modality"`
	// BillingMode 必须是指针：nil = 不修改（沿用库内现值）。
	// 用字符串零值时，"只改价格"的保存会把按次模型静默改成按量 —— 永久 0 元。
	BillingMode *string `json:"billing_mode"`
	// Enabled 用指针：**缺省不改**。原实现是 bool 零值，于是「只改价格、没带 enabled」
	// 会把在售模型静默下架（PUT 语义与前端表单耦合，任何少传一个字段的调用方都会踩）。
	Enabled *bool `json:"enabled"`
	// 以下九条 lane 与 models.prices 一一对应，单位**微**。
	CallPrice          *int64 `json:"call_price"`
	InputPrice         *int64 `json:"input_price"`
	OutputPrice        *int64 `json:"output_price"`
	CachePrice         *int64 `json:"cache_price"`
	CacheCreatePrice   *int64 `json:"cache_create_price"`
	CacheCreatePrice1h *int64 `json:"cache_create_price_1h"`
	ImagePrice         *int64 `json:"image_price"`
	AudioPrice         *int64 `json:"audio_price"`
	AudioOutputPrice   *int64 `json:"audio_output_price"`
	// SpecPrices 必须是指针：nil = 不修改（沿用库内现值），non-nil = 整块覆盖。
	// 类型是 model.SpecPrices（自带 UnmarshalJSON + DisallowUnknownFields）：
	// 旧键名（resolution_prices_quota 之类）会直接 400，而不是被静默丢成"未配置"。
	SpecPrices *model.SpecPrices `json:"spec_prices"`
	// ClearPrices 列出要**显式清空**（写 NULL）的 price lane 名。
	//
	// 单独一份名单是必要的：lane 用 `*int64` 表达，nil 同时意味着"本次没提交"
	// 与"要清空"，而前者会被 FillMissing 用库内值补齐 —— 于是「把某个通道的
	// 单独定价删掉」永远保存不进去（删了保存，价格又回来了）。名单把两种意图分开。
	ClearPrices []string `json:"clear_prices"`
	// Description 过去**根本没有这个入参** → 模型描述永远改不了（列表里恒为空）。
	// 现在也是指针：nil = 不修改，`""` = 显式清空。
	Description *string `json:"description"`
	// Icon 是模型图标 token："" = 清空（回到沿用供应商图标），nil = 不修改。
	// 与 display_name / description 同类，必须是指针：modelcfg 的 Updates 是全列写。
	Icon *string `json:"icon"`
	// Endpoints 必须是指针：nil = 不修改，non-nil = 整组覆盖（`[]` = 明确清空）。
	// 用非指针时，任何"只改价格"的保存都会把端点写成 NULL，
	// 门户端点列表与文档右栏随之消失，而且不报错。
	Endpoints *model.JSON `json:"endpoints"`
	// Capabilities 是模型能力声明（封闭词表，见 apidoc/capability.go）。
	//
	// **必须是指针**：nil = 不修改（保持库内现值），non-nil = 整组覆盖
	// （`[]` 表示明确声明「没有额外能力」）。用切片的零值当「未提供」会让每个
	// 只改价格的保存把能力清成 `[]`，而 `[]` 与 NULL 语义不同 —— 启动期补齐
	// 只补 NULL，于是「编辑一次模型，文档右栏的 TAB 全部消失」且没有任何报错。
	Capabilities *model.JSON `json:"capabilities"`
	// Tags 是营销标签（数组顺序即展示顺序，最多 3 个）。
	//
	// **必须是指针**：nil = 不修改，non-nil = 整组覆盖（`[]` = 明确清空）。
	// 用切片零值当"未提供"会让每次"只改价格"的保存把标签清空。
	Tags *[]model.ModelTag `json:"tags"`
	// ContextWindow / MaxOutputTokens 是模型级覆盖（0 = 继承助手全局设置）。
	//
	// **必须是指针**：nil = 本次不改。用 int 零值时，任何"只改价格"的保存都会
	// 把它们写成 0（= 静默退回继承），而运营刚在页面上填过 128000。
	ContextWindow   *int `json:"context_window"`
	MaxOutputTokens *int `json:"max_output_tokens"`
}

// CreateAdminModel 处理 POST /api/admin/models。
func CreateAdminModel(c *gin.Context) {
	var req adminModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		// 带上原文：spec_prices 的严格解码会指出"哪个键/哪个字段不合法"
		// （价格写成小数、沿用已废弃的 _quota 键名），只回"参数错误"等于让运营盲试。
		response.FailDef(c, errs.InvalidParam.WithMessage("请求体不合法："+err.Error()))
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		response.FailDef(c, errs.InvalidParam.WithMessage("模型 ID 不能为空"))
		return
	}
	// 展示名的兜底已下沉到 modelcfg.Build（nil/空 → ID），这里不再重复设置：
	// 两处兜底必然分叉，而创建与更新必须共用同一条路径。
	in, err := modelInputOf(req)
	if err != nil {
		response.FailDef(c, errs.InvalidParam.WithMessage(err.Error()))
		return
	}
	// 写库只走 modelcfg.CreateTx：vendor 存在性、墓碑物理清理、8 条价格 lane
	// 的换算与符号校验、落库列清单都在它里面。这里再写一遍就是第二真相源
	// —— 历史缺陷（负价不拦、1h 列名写错、只改价把能力写没）全是这么来的。
	// 创建时附带的那一条绑定：走与单条绑定完全相同的校验（CreateTx 内部调
	// NormalizeBindings，严格口径）。任一不合法整个事务回滚 —— 不允许留下
	// 「模型建好了但绑定没建成」的半截状态（那会让模型在目录里不可路由）。
	var initial []modelcfg.Binding
	if b := req.InitialBinding; b != nil {
		initial = []modelcfg.Binding{{
			ChannelID: b.ChannelID, UpstreamModel: strings.TrimSpace(b.UpstreamModel),
			Weight: b.Weight, Priority: b.Priority,
			Enabled: b.Enabled == nil || *b.Enabled,
		}}
	}
	var created *model.Model
	if err := db.DB.Transaction(func(tx *gorm.DB) error {
		var e error
		created, e = modelcfg.CreateTx(tx, in, initial)
		return e
	}); err != nil {
		respondModelErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), "model.create", "model", created.ID, gin.H{"vendor": created.Vendor})
	// 新模型的出厂文档由重算补齐（Reconcile 会给新建的组补一份事实模板并直接发布）：
	// 单独再调一次「按模型补文案」，判据与组级不同，迟早出现「补了又被组的规则盖掉」的拉锯。
	reconcileDocs("model.create", created.ID)
	snap, _ := pricing.Get()
	// 绑定数用与列表同一个口径（`bindingCounts` = 启用绑定 ∩ 启用渠道）：
	// 新建时带了 `initial_binding` 却回 0，前端刚建的模型立刻显示"未绑定"。
	response.OK(c, gin.H{
		"item":     toAdminModelItem(created, vendorOf(snap, created.Vendor), bindingCounts([]string{created.ID})[created.ID], siteurl.Base(c)),
		"warnings": specPriceWarnings(created),
	})
}

// specPriceWarnings 返回按规格计价模型的**非阻断**价格告警（保存成功后随响应回前端）。
//
// 为什么在这里重算而不是让 modelcfg.Build 透传：Build 的返回签名被 presets、
// agent 提案两条路径复用，为了一条提示改签名，波及面远大于收益；
// ValidateSpecPrices 是纯函数，重算一次的成本可以忽略。
//
// 为什么必须回前端：这些告警描述的是「配置合法但会长期少收/多收」的事实
// （兜底价不在档位区间、素材价缺失、单位疑似填错）。只写日志等于没发生 ——
// 运营不会去看服务端日志，月底对账才会发现差额。
func specPriceWarnings(m *model.Model) []string {
	out := make([]string, 0, 4)
	if m == nil || m.BillingMode != model.BillingModeSpec {
		return out
	}
	_, warnings, err := modelcfg.ValidateSpecPrices(m.Modality, m.SpecPrices)
	if err != nil {
		return out
	}
	return append(out, warnings...)
}

// UpdateAdminModel 处理 PUT /api/admin/models/{id}（改价后必须失效定价缓存）。
func UpdateAdminModel(c *gin.Context) {
	id := c.Param("id")
	m, ok := requireModel(c, id)
	if !ok {
		return
	}
	var req adminModelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		// 与创建同一口径：带上原文，价目结构错误要能指出字段。
		response.FailDef(c, errs.InvalidParam.WithMessage("请求体不合法："+err.Error()))
		return
	}
	req.ID = id
	in, err := modelInputOf(req)
	if err != nil {
		response.FailDef(c, errs.InvalidParam.WithMessage(err.Error()))
		return
	}
	// 与创建同一条路径（UpdateTx 内含 FillMissing + Build + 全列写 + version+1）。
	// 人工路径不启用 CAS（expectVersion=0），但**仍然 version+1**：
	// 否则 AI 提案拿着过期 version 也能通过乐观锁，把运营刚做的修改静默覆盖。
	var next *model.Model
	if err := db.DB.Transaction(func(tx *gorm.DB) error {
		var e error
		next, e = modelcfg.UpdateTx(tx, in, req.Version)
		if e != nil {
			return e
		}
		// 价目只做**自洽**校验（档位键可落库、单价非负、默认价能解释），
		// 不再校验「上游能不能出这个档」：那是厂商官方文档的事实，
		// 代码替它判定只会在上游扩容时误杀可用组合（且型号换上游后旧上限更荒谬）。
		return nil
	}); err != nil {
		respondModelErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), "model.update", "model", id, gin.H{"vendor": next.Vendor, "billing_mode": next.BillingMode})
	// P2-20：改价必须留 before/after。原实现只记了 vendor/quota_type，价格争议发生时
	// 无法回答「上一版是多少」，等于没有审计价值（ActionModelRatioUpdate 早已定义却从未使用）。
	if before, after := modelPriceAudit(m), modelPriceAudit(next); !samePriceAudit(before, after) {
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionModelRatioUpdate, "model", id,
			gin.H{"before": before, "after": after})
	}
	reconcileDocs("model.update", id)
	response.OK(c, gin.H{"item": toAdminModelItem(next, vendorOf2(next.Vendor), countBindings(id), siteurl.Base(c)), "warnings": specPriceWarnings(next)})
}

// ToggleAdminModel 处理 PATCH /api/admin/models/{id}（仅切换启用态）。
func ToggleAdminModel(c *gin.Context) {
	m, ok := requireModel(c, c.Param("id"))
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 走 modelcfg：唯一区别是它会把 version 一起 +1，
	// 否则 AI 提案的乐观锁看不见「运营刚把模型下架」，apply 时静默覆盖。
	if err := modelcfg.SetEnabledTx(db.DB, m.ID, *req.Enabled); err != nil {
		respondModelErr(c, err)
		return
	}
	pricing.InvalidatePricingCache()
	// 单条启停属状态变更：设计 §4.9.8 模型域状态动作即 model.batch_status（无独立 toggle 动作）
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionModelBatchStatus, "model", m.ID, gin.H{"enabled": *req.Enabled})
	reconcileDocs("model.toggle", m.ID)
	response.OK(c, gin.H{"enabled": *req.Enabled})
}

// DeleteAdminModel 处理 DELETE /api/admin/models/{id}（绑定/默认引用保护）。
func DeleteAdminModel(c *gin.Context) {
	m, ok := requireModel(c, c.Param("id"))
	if !ok {
		return
	}
	// E6：计数必须**含停用绑定**。原实现只数 enabled=true，于是「绑定全停用」的模型
	// 可以通过删除校验，删完后 model_bindings 里留下指向不存在模型的孤儿行。
	var bindings int64
	if err := db.DB.Model(&model.ModelBinding{}).Where("model_id = ?", m.ID).Count(&bindings).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if bindings > 0 {
		response.FailDef(c, errs.ModelProtected)
		return
	}
	// E4：默认模型判定改用定价快照的派生结果（与网关/门户展示同一份口径）。
	if pricing.IsModalityDefault(m.ID) {
		response.FailDef(c, errs.ModelProtected)
		return
	}
	if err := db.DB.Where("id = ?", m.ID).Delete(&model.Model{}).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	pricing.InvalidatePricingCache()
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), "model.delete", "model", m.ID, nil)
	reconcileDocs("model.delete", m.ID)
	response.OK(c, nil)
}

// UpsertModelBinding 处理 PUT /api/admin/models/{id}/bindings/{channelId}：
// 新增或更新**一条**绑定（绑定编辑的唯一写入口）。
//
// 为什么不再是整组 PUT：整组要求调用方先读全部渠道、改一处、再整组回传，
// 两名管理员同时改同一模型的不同渠道会互相覆盖；且整组是硬删重建，
// 版本与创建时间全变，任何基于它的乐观锁立即失效。
func UpsertModelBinding(c *gin.Context) {
	m, ok := requireModel(c, c.Param("id"))
	if !ok {
		return
	}
	chID, ok := uintParam(c, "channelId")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	var req bindingInput
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// 渠道一律以路径参数为准：body 里的 channel_id 只用于兼容旧前端，不参与定位。
	b := modelcfg.Binding{
		ChannelID: chID, UpstreamModel: req.UpstreamModel,
		Weight: req.Weight, Priority: req.Priority,
		Enabled: req.Enabled == nil || *req.Enabled,
	}
	var notes []modelcfg.BindingNote
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		n, err := modelcfg.UpsertBindingTx(tx, m, b)
		notes = n
		return err
	})
	if err != nil {
		failBindingErr(c, err)
		return
	}
	pricing.InvalidatePricingCache()
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionModelBindingsUpdate, "model",
		m.ID, gin.H{"channel_id": chID, "enabled": b.Enabled})
	// 绑定决定「模型是否在售」（目录口径 = 启用且有启用绑定），文档归属必须跟着重算。
	reconcileDocs("model.binding", m.ID)
	// warnings 恒为数组（空时为 []）：前端与 AI 都会直接取 .length。
	response.OK(c, gin.H{"bindings": countBindings(m.ID), "warnings": notes})
}

// DeleteModelBinding 处理 DELETE /api/admin/models/{id}/bindings/{channelId}。
func DeleteModelBinding(c *gin.Context) {
	m, ok := requireModel(c, c.Param("id"))
	if !ok {
		return
	}
	chID, ok := uintParam(c, "channelId")
	if !ok {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	err := db.DB.Transaction(func(tx *gorm.DB) error {
		return modelcfg.DeleteBindingTx(tx, m, chID)
	})
	if err != nil {
		failBindingErr(c, err)
		return
	}
	pricing.InvalidatePricingCache()
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionModelBindingsUpdate, "model",
		m.ID, gin.H{"channel_id": chID, "removed": true})
	reconcileDocs("model.binding", m.ID)
	response.OK(c, gin.H{"bindings": countBindings(m.ID)})
}

// failBindingErr 把绑定校验的领域错误映射成对外错误码。
//
// 不能一律 500，也不能一律 400：
//   - FieldError（上游名必填 / 渠道已停用 / 在售不能零可用）是运营当场能改的
//     配置问题，原文回给管理台才有意义；
//   - ErrBindingNotFound 是「并发解除」或「前端拿了旧列表」，要 404 让前端重取，
//     提示"配置错误"会让运营去改一个其实没问题的表单。
func failBindingErr(c *gin.Context, err error) {
	var fe *modelcfg.FieldError
	switch {
	case errors.As(err, &fe):
		response.FailDef(c, errs.InvalidParam.WithMessage(textx.TruncateRunes(fe.Message, 200)))
	case errors.Is(err, modelcfg.ErrBindingNotFound):
		response.FailDef(c, errs.NotFound)
	default:
		response.FailDef(c, errs.Internal)
	}
}

// ListAdminVendors 处理 GET /api/admin/vendors。
func ListAdminVendors(c *gin.Context) {
	vendors, err := vendorList()
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// palette 随列表下发：取色盘是**后端的业务常量**（新建时未传 color 就从这里随机取），
	// 前端再抄一份徽标色就出现两个真相源 —— 抄错一位的表现是"色盘选中了 A、落库成了 B"。
	// 必须非 nil（对外数组铁律）。
	response.OK(c, gin.H{"items": vendorViews(vendors, siteurl.Base(c)), "palette": vendorPalette})
}

// CreateAdminVendor 处理 POST /api/admin/vendors（自定义分组）。
//
// icon / color 可选：不传时 icon=""（首字占位）、color 从取色盘随机。
func CreateAdminVendor(c *gin.Context) {
	var req struct {
		Name  string  `json:"name"`
		Icon  *string `json:"icon"`
		Color *string `json:"color"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 12 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	color := vendorPalette[randInt(len(vendorPalette))]
	if req.Color != nil {
		v, err := normalizeHexColor(*req.Color)
		if err != nil {
			response.FailDef(c, errs.InvalidParam.WithMessage("品牌色需为 #RRGGBB 格式"))
			return
		}
		color = v
	}
	icon := ""
	if req.Icon != nil {
		v, ok := validateIcon(c, *req.Icon)
		if !ok {
			return
		}
		icon = v
	}
	var n int64
	if err := db.DB.Model(&model.Vendor{}).Where("name = ?", req.Name).Count(&n).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if n > 0 {
		response.FailDef(c, errs.VendorExists)
		return
	}
	v := model.Vendor{
		ID:      "vg_" + randHex(4),
		Name:    req.Name,
		Color:   color,
		Icon:    icon,
		Builtin: false,
		Sort:    100,
	}
	if err := db.DB.Create(&v).Error; err != nil {
		// 并发建同名分组：预检查（上面的 COUNT）在并发下必然漏，唯一索引才是最终判据。
		if pgerr.IsUnique(err) {
			response.FailDef(c, errs.VendorExists)
			return
		}
		response.FailDef(c, errs.Internal)
		return
	}
	pricing.InvalidatePricingCache()
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), "vendor.create", "vendor", v.ID, gin.H{"name": v.Name, "color": v.Color})
	response.OK(c, gin.H{"item": vendorView(vendorItemOf(v, 0), siteurl.Base(c))})
}

// UpdateAdminVendor 处理 PUT /api/admin/vendors/{id}。
//
// 三项都可改，包括**内置分组**（名称/图标/品牌色）：
//   - 改名安全：models.vendor 引用的是 vendors.id，不是名字；
//   - 图标/颜色出厂只是初始值，不允许改就只能发版；
//   - 只有**删除**仍受保护（见 DeleteAdminVendor）：内置分组被删后 seed 不会重建
//     （软删行仍占主键），而 pricing.build 读不到它 → 该组下所有模型的供应商名与
//     图标全变空，且 UI 上无法修复。
func UpdateAdminVendor(c *gin.Context) {
	v, ok := requireVendor2(c, c.Param("id"))
	if !ok {
		return
	}
	var req struct {
		Name  *string `json:"name"`
		Icon  *string `json:"icon"`
		Color *string `json:"color"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if req.Name == nil && req.Icon == nil && req.Color == nil {
		// 空请求体不该被当成"成功"，否则前端会以为保存生效。
		response.FailDef(c, errs.InvalidParam)
		return
	}

	updates := map[string]any{}
	auditLog := gin.H{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len([]rune(name)) > 12 {
			response.FailDef(c, errs.InvalidParam)
			return
		}
		var n int64
		if err := db.DB.Model(&model.Vendor{}).Where("name = ? AND id <> ?", name, v.ID).Count(&n).Error; err != nil {
			response.FailDef(c, errs.Internal)
			return
		}
		if n > 0 {
			response.FailDef(c, errs.VendorExists)
			return
		}
		updates["name"] = name
		auditLog["old"] = v.Name
		auditLog["new"] = name
	}
	if req.Icon != nil {
		icon, ok := validateIcon(c, *req.Icon)
		if !ok {
			return
		}
		updates["icon"] = icon
		auditLog["icon"] = icon
	}
	if req.Color != nil {
		color, err := normalizeHexColor(*req.Color)
		if err != nil {
			response.FailDef(c, errs.InvalidParam.WithMessage("品牌色需为 #RRGGBB 格式"))
			return
		}
		updates["color"] = color
		auditLog["color"] = color
	}

	if err := db.DB.Model(&model.Vendor{}).Where("id = ?", v.ID).Updates(updates).Error; err != nil {
		if pgerr.IsUnique(err) {
			response.FailDef(c, errs.VendorExists)
			return
		}
		response.FailDef(c, errs.Internal)
		return
	}
	// 管理层列表读的是定价快照（10s TTL），不失效就会"改完还是旧的"。
	pricing.InvalidatePricingCache()
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionVendorUpdate, "vendor", v.ID, auditLog)
	// 回包必须是**更新后**的行。`v` 是更新前读出来的，`Updates` 不会回填它 ——
	// 直接回 `*v` 的表现是「保存成功但图标/名称没变」，前端若拿回包刷新草稿
	// 还会把旧值写回去。model_count 也重新数：回 0 会让左栏「N 个模型」瞬间归零。
	var fresh model.Vendor
	if err := db.DB.Where("id = ?", v.ID).First(&fresh).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	var modelCount int64
	if err := db.DB.Model(&model.Model{}).Where("vendor = ?", fresh.ID).Count(&modelCount).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"item": vendorView(vendorItemOf(fresh, modelCount), siteurl.Base(c))})
}

// DeleteAdminVendor 处理 DELETE /api/admin/vendors/{id}（内置/在用保护）。
func DeleteAdminVendor(c *gin.Context) {
	v, ok := requireVendor2(c, c.Param("id"))
	if !ok {
		return
	}
	if v.Builtin {
		response.FailDef(c, errs.VendorProtect)
		return
	}
	var n int64
	if err := db.DB.Model(&model.Model{}).Where("vendor = ?", v.ID).Count(&n).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if n > 0 {
		response.FailDef(c, errs.VendorInUse)
		return
	}
	if err := db.DB.Where("id = ?", v.ID).Delete(&model.Vendor{}).Error; err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	pricing.InvalidatePricingCache()
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), "vendor.delete", "vendor", v.ID, gin.H{"name": v.Name})
	response.OK(c, nil)
}

// ListUpstreamSources 处理 GET /api/admin/upstream/sources。
func ListUpstreamSources(c *gin.Context) {
	response.OK(c, gin.H{"sources": upstream.Sources()})
}

// PreviewUpstream 处理 POST /api/admin/upstream/preview（真实拉取 → 与本地模型 diff，只读不写入）。
func PreviewUpstream(c *gin.Context) {
	var req struct {
		Source string `json:"source"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Source == "" {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	items, err := upstream.Preview(c.Request.Context(), req.Source)
	if err != nil {
		if errors.Is(err, upstream.ErrSourceUnavailable) {
			logger.L().Warn("上游价目拉取失败", zap.String("source", req.Source), zap.Error(err))
			response.FailDef(c, errs.ServiceUnavailable)
			return
		}
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"items": items})
}

// ---- 内部工具 ----

// capabilitiesOf 校验并归一请求里的能力声明。
//
// 返回 (值, 是否提供, 错误)：**未提供**与**提供了非法值**必须分开 ——
// 前者保持库内现值（改个价格不该动能力），后者 400（写进去就是永久错的文档）。
func capabilitiesOf(req adminModelReq) ([]string, bool, error) {
	if req.Capabilities == nil {
		return nil, false, nil
	}
	raw := json.RawMessage(*req.Capabilities)
	if err := apidoc.ValidateCapabilities(req.Modality, raw); err != nil {
		return nil, true, err
	}
	caps := apidoc.NormalizeCapabilities(req.Modality, raw)
	return apidoc.CapKeys(caps), true, nil
}

// reconcileDocs 是模型/绑定侧的统一后置入口（与渠道侧同一个管线）。
//
// 只剩「定价快照失效」：那份缓存 10s，不失效的话同一份配置在两个读路径上
// 会给出不同答案。**文档不再跟着模型变** —— 族是静态白名单，新增或下架一个模型
// 不会多出也不会少掉任何一页；「适用模型」在每次投影时按在售集合现算。
func reconcileDocs(...any) { lifecycle.AfterChange() }

// modelInputOf 把请求体搬成 modelcfg.Input。
//
// 刻意"只搬运、不做判断"：价格换算、符号校验、半免费一致性、计费方式与模态
// 的互斥、is_free 派生都在 modelcfg.Build —— 这也是 AI 编排走的那条路径。
// 这里一旦开始做判断，就又变成"人工一套、AI 一套"。
func modelInputOf(req adminModelReq) (modelcfg.Input, error) {
	in := modelcfg.Input{
		ID:              strings.TrimSpace(req.ID),
		DisplayName:     req.DisplayName,
		Description:     req.Description,
		Icon:            req.Icon,
		Vendor:          req.Vendor,
		Modality:        req.Modality,
		Enabled:         req.Enabled,
		ContextWindow:   req.ContextWindow,
		MaxOutputTokens: req.MaxOutputTokens,
	}
	if req.BillingMode != nil {
		in.BillingMode = strings.TrimSpace(*req.BillingMode)
		in.BillingModeSet = true
	}
	if req.Endpoints != nil {
		in.EndpointsSet = true
		if raw := []byte(*req.Endpoints); len(raw) > 0 {
			var eps []string
			if err := json.Unmarshal(raw, &eps); err != nil {
				return in, errors.New("端点列表格式不正确（应为字符串数组）")
			}
			in.Endpoints = eps
		}
	}
	if req.Capabilities != nil {
		caps, provided, err := capabilitiesOf(req)
		if err != nil {
			return in, err
		}
		if provided {
			in.CapabilitiesSet = true
			in.Capabilities = caps
		}
	}
	if req.Tags != nil {
		in.TagsSet = true
		in.Tags = *req.Tags
	}
	in.Prices = model.Prices{
		Input: req.InputPrice, Output: req.OutputPrice,
		Cache: req.CachePrice, CacheCreate: req.CacheCreatePrice,
		CacheCreate1h: req.CacheCreatePrice1h,
		Image:         req.ImagePrice, Audio: req.AudioPrice,
		AudioOutput: req.AudioOutputPrice,
		Call:        req.CallPrice,
	}
	in.ClearPrices = req.ClearPrices
	// 去掉空串（前端「没勾任何通道」会下发空数组或空串，空串不该被当成非法通道名）。
	if len(in.ClearPrices) > 0 {
		names := make([]string, 0, len(in.ClearPrices))
		for _, n := range in.ClearPrices {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		in.ClearPrices = names
	}
	if req.SpecPrices != nil {
		in.SpecPrices = *req.SpecPrices
		in.SpecPricesSet = true
	}
	return in, nil
}

// respondModelErr 把 modelcfg 的错误翻成对外的错误码。
//
// 字段级错误必须带原文透出：运营看到"输出价不能为负数"才知道改哪，
// 只回 invalid_param 等于让他自己猜。
func respondModelErr(c *gin.Context, err error) {
	var fe *modelcfg.FieldError
	switch {
	case errors.As(err, &fe):
		// 图标错误单独给码：选择器要据此提示"重新选/重新上传"，与"价格填错了"是
		// 两种完全不同的处置动作。
		if fe.Field == "icon" {
			response.FailDef(c, errs.IconInvalid)
			return
		}
		response.FailDef(c, errs.InvalidParam.WithMessage(fe.Message))
	case errors.Is(err, modelcfg.ErrModelExists):
		response.FailDef(c, errs.ModelExists)
	case errors.Is(err, modelcfg.ErrNotFound):
		response.FailDef(c, errs.ModelNotFound)
	case errors.Is(err, modelcfg.ErrVersionConflict):
		response.FailDef(c, errs.InvalidParam.WithMessage(modelcfg.ErrVersionConflict.Error()))
	case errors.Is(err, modelcfg.ErrNoUsableBinding):
		// 启用一致性是**配置**问题而不是服务端故障：必须 400 并把「怎么做」说出来，
		// 否则运营只会看到"启用失败"，然后在模型页反复点。
		response.FailDef(c, errs.InvalidParam.WithMessage(modelcfg.ErrNoUsableBinding.Error()))
	case errors.Is(err, modelcfg.ErrNoEnabledBinding):
		// 创建时带了一条 enabled=false 的初始绑定会走到这里。哨兵自带的文案带包名前缀，
		// 直接下发对运营是噪音，所以用固定中文（信息量不变）。
		response.FailDef(c, errs.InvalidParam.WithMessage("至少要有一条启用的绑定，否则模型不会进入目录"))
	default:
		response.FailDef(c, errs.Internal)
	}
}

// normalizeSpecPrices 归一化并按规格价目校验（唯一实现：modelcfg → billing）。
//
// 保留本函数只为「不落库、只校验」的复用方（测试/预览）；落库路径由
// modelcfg.Build 内部再校验一次，两边共用同一个 ValidateSpecPrices。
func normalizeSpecPrices(modality string, sp model.SpecPrices) (model.SpecPrices, error) {
	normalized, _, err := modelcfg.ValidateSpecPrices(modality, sp)
	if err != nil {
		return sp, err
	}
	return normalized, nil
}

// validSpecPrices 保留为「只判合法、不关心归一化结果」的入口（测试与复用方使用）。
func validSpecPrices(modality string, sp model.SpecPrices) error {
	_, err := normalizeSpecPrices(modality, sp)
	return err
}

// 模态与计费方式的合法性校验已下沉到 modelcfg.Build（schema.AllowsQuotaType 是
// 唯一真相源）：人工保存与 AI 提案必须过同一道闸，而不是各判一次。

// requireModel 取路径模型；不存在返回 404。
func requireModel(c *gin.Context, id string) (*model.Model, bool) {
	var m model.Model
	if err := db.DB.Where("id = ?", id).First(&m).Error; err != nil {
		response.FailDef(c, errs.NotFound)
		return nil, false
	}
	return &m, true
}

// requireVendor 校验供应商 ID 存在，不存在返回 400。
func requireVendor(c *gin.Context, id string) (model.Vendor, bool) {
	var v model.Vendor
	if err := db.DB.Where("id = ?", id).First(&v).Error; err != nil {
		response.FailDef(c, errs.InvalidParam)
		return v, false
	}
	return v, true
}

func requireVendor2(c *gin.Context, id string) (*model.Vendor, bool) {
	var v model.Vendor
	if err := db.DB.Where("id = ?", id).First(&v).Error; err != nil {
		response.FailDef(c, errs.NotFound)
		return nil, false
	}
	return &v, true
}

// toAdminModelItem 组装管理台模型行。
//
// deliverable 由调用方**批量**算好传进来（列表页逐模型算就是 N+1）；单模型路径
// 传 dialectsOfModel 的结果即可。stale 直接从价目派生，不查库。
func toAdminModelItem(m *model.Model, v pricing.VendorItem, bindings int64, base string) adminModelItem {
	icon := brandicon.Effective(m.Icon, v.Icon)
	return adminModelItem{
		ID: m.ID, DisplayName: m.DisplayName, Description: m.Description, Vendor: m.Vendor,
		VendorName: v.Name, VendorColor: v.Color,
		Icon: icon, IconOwn: m.Icon, IconURL: iconref.URL(base, icon),
		Modality: m.Modality, BillingMode: m.BillingMode,
		Enabled: m.Enabled, IsDefault: m.IsDefault, IsFree: m.IsFree,
		Prices: m.Prices, SpecPrices: m.EffectiveSpecPrices(),
		Endpoints: m.Endpoints, Capabilities: m.Capabilities,
		Tags:          adminTagsOf(m),
		ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
		Version:  m.Version,
		Bindings: bindings,
	}
}

// validateIcon 是管理台写路径的图标校验唯一入口：形状 + upload 对象存在性。
//
// 失败时已写响应（icon_invalid / 500），调用方直接 return。
// 用 icon.Validate 而不是 brandicon.Normalize：后者只看形状，会放行一个指向
// 已被清理对象的 upload:<ulid>，落库后对外就是一张 404 坏图。
func validateIcon(c *gin.Context, raw string) (string, bool) {
	v, err := iconsvc.Validate(c.Request.Context(), raw)
	if err == nil {
		return v, true
	}
	if errors.Is(err, brandicon.ErrInvalid) || errors.Is(err, iconsvc.ErrNotFound) {
		response.FailDef(c, errs.IconInvalid)
		return "", false
	}
	response.FailDef(c, errs.Internal)
	return "", false
}

// vendorView 给单个供应商行补上图标地址。
//
// 值拷贝：pricing.VendorItem 来自 10s TTL 的共享快照（见该字段的说明），
// 直接改切片元素就是把本次请求的域名写进了全站共享状态。
func vendorView(v pricing.VendorItem, base string) pricing.VendorItem {
	v.IconURL = iconref.URL(base, v.Icon)
	return v
}

// vendorViews 是 vendorView 的切片版本（新建切片，不改入参）。
func vendorViews(in []pricing.VendorItem, base string) []pricing.VendorItem {
	out := make([]pricing.VendorItem, 0, len(in))
	for _, v := range in {
		out = append(out, vendorView(v, base))
	}
	return out
}

func vendorList() ([]pricing.VendorItem, error) {
	snap, err := pricing.Get()
	if err != nil {
		return nil, err
	}
	return snap.Vendors, nil
}

func vendorOf(snap *pricing.Snapshot, id string) pricing.VendorItem {
	if snap == nil {
		return pricing.VendorItem{ID: id}
	}
	for _, v := range snap.Vendors {
		if v.ID == id {
			return v
		}
	}
	return pricing.VendorItem{ID: id}
}

func vendorOf2(id string) pricing.VendorItem {
	var v model.Vendor
	if err := db.DB.Where("id = ?", id).First(&v).Error; err != nil {
		return pricing.VendorItem{ID: id}
	}
	return vendorItemOf(v, 0)
}

func vendorItemOf(v model.Vendor, count int64) pricing.VendorItem {
	return pricing.VendorItem{
		ID: v.ID, Name: v.Name, Color: v.Color, Icon: v.Icon,
		Priority: v.Priority, Sort: v.Sort, Builtin: v.Builtin, ModelCount: count,
	}
}

// normalizeHexColor 保留为委托：实现已收敛到 model.NormalizeHexColor。
//
// 供应商品牌色与模型标签色必须共用同一份 hex 归一 —— 两套实现必然在某个边界上
// 分叉（一个接受不带 `#`、另一个不接受），而它们都会进入前端同一个相等判断。
func normalizeHexColor(s string) (string, error) {
	return model.NormalizeHexColor(s)
}

// modelPriceAudit 提取与计费直接相关的字段（改价审计的 before/after 快照）。
func modelPriceAudit(m *model.Model) gin.H {
	if m == nil {
		return gin.H{}
	}
	return gin.H{
		"billing_mode": m.BillingMode,
		"enabled":      m.Enabled,
		"prices":       m.Prices,
		"spec_prices":  m.SpecPrices,
	}
}

// samePriceAudit 判断两份价格快照是否等价（等价就不写「改价」审计，避免刷无意义的行）。
func samePriceAudit(a, b gin.H) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ja) == string(jb)
}

// bindingInput 是保存绑定请求里的单条绑定（通道 + 权重 + 上游模型名 + 启停 + 优先级）。
//
// Priority 是 P3-02 的落地：此前落库时硬编码为 1，选路完全不看它，
// 运营在界面上填了也没用。零值 = 未设置，落库归一为 1。
type bindingInput struct {
	ChannelID     uint64 `json:"channel_id"`
	Weight        int    `json:"weight"`
	Priority      int    `json:"priority"`
	UpstreamModel string `json:"upstream_model"`
	Enabled       *bool  `json:"enabled"`
}

// displayModalityName 把模态枚举翻成中文，用于绑定校验的错误提示。
func displayModalityName(m string) string {
	switch strings.TrimSpace(m) {
	case "chat":
		return "对话"
	case "image":
		return "图像"
	case "video":
		return "视频"
	case "audio":
		return "音频"
	case "music":
		return "音乐"
	case "embedding":
		return "向量"
	case "rerank":
		return "重排"
	case "moderation":
		return "审核"
	}
	if m == "" {
		return "未设置"
	}
	return m
}

// countBindings 返回模型**可路由**的绑定数（保存绑定后回给抽屉做提示）。
//
// 直接复用 modelcfg.CountUsableBindings：这是「模型能不能被路由」的唯一判据，
// 在这里另写一份 SQL 迟早会与启用闸门算出不同答案（列表说 1、启用说 0）。
func countBindings(modelID string) int64 {
	n, err := modelcfg.CountUsableBindings(db.DB, modelID)
	if err != nil {
		return 0
	}
	return n
}

// bindingCounts 统计模型**可路由**的绑定数；传入 ids 时只统计这批模型（分页页内聚合）。
//
// 判据必须与 modelcfg.CountUsableBindings（单模型版）完全一致，否则会出现
// 「列表显示 1 个可用渠道、点启用却报没有可用绑定」这种自相矛盾：
// 启用绑定 ∩ 启用渠道 ∩ 两边都没软删。
func bindingCounts(modelIDs []string) map[string]int64 {
	out := map[string]int64{}
	if modelIDs != nil && len(modelIDs) == 0 {
		return out
	}
	var rows []struct {
		ModelID string
		Cnt     int64
	}
	sel := db.DB.Table("model_bindings AS b").
		Select("b.model_id AS model_id, count(*) AS cnt").
		Joins("JOIN channels AS c ON c.id = b.channel_id AND c.deleted_at IS NULL").
		Where("b.enabled = ? AND b.deleted_at IS NULL AND c.status = ?", true, model.ChannelStatusEnabled)
	if modelIDs != nil {
		sel = sel.Where("b.model_id IN ?", modelIDs)
	}
	_ = sel.Group("b.model_id").Scan(&rows).Error
	for _, r := range rows {
		out[r.ModelID] = r.Cnt
	}
	return out
}

func randHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	var b [1]byte
	_, _ = rand.Read(b[:])
	return int(b[0]) % n
}
