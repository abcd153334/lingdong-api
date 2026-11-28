package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/relay/adapters"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/channel"
	"lingdong-api/internal/service/channelhealth"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/lifecycle"
	specsvc "lingdong-api/internal/service/spec"
)

// channelReq 是创建/编辑渠道的请求体（编辑时 keys 为空表示不修改密钥池）。
type channelReq struct {
	Name string `json:"name"`
	// Icon 为 nil 表示不修改；显式 "" 表示清空（回到首字占位）。
	Icon      *string `json:"icon"`
	AdapterID string  `json:"adapter_id"`
	BaseURL   string  `json:"base_url"`
	// Website 为 nil 表示不修改；显式 "" 表示清空（回到适配器默认官网）。
	Website        *string  `json:"website"`
	MultiKeyMode   string   `json:"multi_key_mode"`
	Groups         []string `json:"groups"`
	Weight         *int     `json:"weight"`
	Priority       *int     `json:"priority"`
	RPMLimit       *int     `json:"rpm_limit"`
	MaxConcurrency *int     `json:"max_concurrency"`
	AutoBan        *bool    `json:"auto_ban"`
	// SwitchKeyRetry：失败时是否换同渠道另一把密钥重试（不传 = 不修改）。
	SwitchKeyRetry *bool `json:"switch_key_retry"`
	// Keys 只在创建时受理（初始密钥池）；更新走 PATCH /admin/channels/:id/keys。
	Keys []string `json:"keys"`
	// AdapterConfig 是渠道级声明式配置（null = 清空；不传 = 不修改）。
	AdapterConfig json.RawMessage `json:"adapter_config"`
	// AdapterOptions 是适配器私有配置（同语义：不传 = 不修改，null/{} = 清空）。
	AdapterOptions json.RawMessage `json:"adapter_options"`
}

// adapterItem 是适配器能力视图（供 Adapters 页与渠道编辑器下拉）。
type adapterItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Label 是中文展示名（内置来自 adapters.MetaOf，规格取规格的 label）。
	Label  string   `json:"label"`
	Vendor string   `json:"vendor"`
	Caps   []string `json:"caps"`
	Status int      `json:"status"`
	// Source 区分「内置」与「我的」（spec:<slug>），前端按它分两个分组。
	Source string `json:"source"`
	// Editable/Deletable 让前端不必自己判断 spec: 前缀。
	Editable  bool `json:"editable"`
	Deletable bool `json:"deletable"`
	// SpecID / Version 只在规格项上有值（编辑抽屉要带上 version 做乐观锁）。
	SpecID  uint64 `json:"spec_id,omitempty"`
	Version int    `json:"version,omitempty"`
	// ChannelCount 是引用数（删除保护提示与"能不能停用"的判断依据）。
	ChannelCount int64 `json:"channel_count"`
	// SupportsDeclarative 让前端不用硬编码适配器名来判断该不该显示高级配置区。
	SupportsDeclarative bool `json:"supports_declarative"`
	// SupportsOptions 表示该适配器接受渠道私有配置（前端据此渲染自己的配置块，
	// 不认识的适配器就不显示——避免"配了没人读"）。
	SupportsOptions bool `json:"supports_options"`
	// DefaultBaseURL 是该适配器的渠道默认 Base URL（空 = 无默认值，必须由运营填写）。
	// 前端在切换适配器时用它预填，避免运营把 /api/v3 之类的前缀漏掉。
	DefaultBaseURL string `json:"default_base_url"`
	// Website 是该适配器的默认上游官网（空 = 无默认值）。渠道表单用它做占位与跳转兜底。
	Website string `json:"website"`
	// Endpoints 只在规格项上有值：内置适配器没有"声明端点"的能力，
	// 现编一份就是第二套事实，所以内置项恒为空数组。
	Endpoints []string `json:"endpoints"`
	// IconURL 是**可渲染地址**（空 = 首字占位）。
	//
	// 为什么不叫 icon：`icon` 在渠道上是"编辑回填用的令牌"，两者语义不同；
	// 且令牌不该出库（地址形态由 service/iconref 唯一决定）。
	// 只有内置适配器有值：自建规格画供应商的标会让人以为它是官方直连。
	IconURL string `json:"icon_url"`
}

// adaptersView 汇总全部可用的适配器：内置（随进程启动注册）+ 我的（adapter_specs）。
//
// 两件事必须一起做，否则运营会撞上同一类问题：
//   - 数据源换成 relay.Builtins()：注册表里同时住着"内置身份"和"运行期缓存"，
//     拿它当清单会让刚被调用过的规格混进内置卡片；
//   - 把规格列表并进来：新建的规格在**被解析过之前**不在注册表里，
//     渠道编辑页的下拉就永远选不到它（保存必然报"适配器不存在"）。
func adaptersView(base string) []adapterItem {
	items := make([]adapterItem, 0, 8)
	// 一次 GROUP BY 拿到全部引用数：内置与自定义共用同一个数字来源，
	// 逐个适配器查会变成 N+1（渠道下拉每次打开都要调这个接口）。
	counts, err := channel.CountByAdapter()
	if err != nil {
		logger.L().Warn("统计渠道引用数失败（引用数将显示 0）", zap.Error(err))
		counts = map[string]int64{}
	}
	for _, a := range relay.Builtins() {
		meta, _ := adapters.MetaOf(a.Name())
		label := meta.Label
		if label == "" {
			label = a.Name()
		}
		items = append(items, adapterItem{
			ID: a.Name(), Name: a.Name(), Label: label, Vendor: meta.Vendor,
			Caps: a.Caps(), Status: 1, Source: "builtin",
			Editable: false, Deletable: false,
			ChannelCount:        counts[a.Name()],
			SupportsDeclarative: relay.SupportsDeclarativeConfig(a.Name()),
			SupportsOptions:     relay.SupportsAdapterOptions(a.Name()),
			DefaultBaseURL:      meta.DefaultBaseURL,
			Website:             meta.Website,
			Endpoints:           []string{},
			IconURL:             iconref.URL(base, meta.Icon),
		})
	}
	specs, err := specsvc.List(context.Background())
	if err != nil {
		logger.L().Warn("读取规格清单失败（渠道下拉将缺少规格项）", zap.Error(err))
	}
	for _, sp := range specs {
		items = append(items, adapterItem{
			ID: sp.AdapterID, Name: sp.AdapterID, Label: sp.Label, Vendor: sp.Vendor,
			Caps: sp.Caps, Status: sp.Status, Source: "custom",
			Editable: true, Deletable: true,
			SpecID: sp.ID, Version: sp.Version, ChannelCount: sp.ChannelCount,
			SupportsDeclarative: true, SupportsOptions: false,
			Endpoints: sp.Endpoints,
		})
	}
	return items
}

// ListAdminChannels 处理 GET /api/admin/channels（keyword/status 过滤，非分页）。
func ListAdminChannels(c *gin.Context) {
	status := 0
	if s := strings.TrimSpace(c.Query("status")); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			status = n
		}
	}
	// B3：分页 + 排序同时下推。**page 缺省/0** 时返回全量（适配器页的渠道计数依赖该行为）。
	//
	// 传了但不是合法页码必须 400，不能静默当成 0：0 就是"全量"，于是一个笔误
	// （page=1x）会把"分页查询"变成"把全部渠道一次性拉出来"，而调用方看不出差异。
	p, all, perr := pagination.ParseOptional(c)
	if perr != nil {
		response.FailDefMsg(c, errs.InvalidParam, perr.Error())
		return
	}
	page := p.Page
	if all {
		page = 0
	}
	items, total, err := channel.List(channel.ListQuery{
		Ctx:     c.Request.Context(),
		Keyword: c.Query("keyword"),
		Status:  status,
		Page:    page,
		Size:    p.PageSize,
		Sort:    c.Query("sort"),
		Order:   c.Query("order"),
	}, siteurl.Base(c))
	if err != nil {
		if errors.Is(err, channel.ErrInvalidSort) {
			response.FailDefMsg(c, errs.InvalidParam, "不支持的排序字段")
			return
		}
		response.FailDef(c, errs.Internal)
		return
	}
	resp := gin.H{
		"items":    items,
		"adapters": adaptersView(siteurl.Base(c)),
		"groups":   channel.Groups(),
		"total":    total,
		// P2-22：熔断策略随列表下发。渠道的 health 列只显示「测试累计」，
		// 没有阈值/窗口/最小样本，运营无法解释「失败率 2% 却被自动禁用」。
		"monitor": channelhealth.CurrentPolicy(),
	}
	if page > 0 {
		// 全量分支不回显页码：前端据此保持「未分页」的展示，与改造前一致。
		resp["page"] = page
		resp["page_size"] = p.PageSize
	}
	response.OK(c, resp)
}

// CreateAdminChannel 处理 POST /api/admin/channels。
func CreateAdminChannel(c *gin.Context) {
	req, ok := parseChannelReq(c, true)
	if !ok {
		return
	}
	item, err := channel.Create(req, siteurl.Base(c))
	if err != nil {
		respondChannelErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionChannelCreate, "channel", strconv.FormatUint(item.ID, 10),
		gin.H{"name": item.Name, "adapter_id": item.AdapterID})
	// 渠道是"在售集合"的一部分（见 pricing.OnSaleModels）：新建后必须让目录与文档知道。
	afterChange(c.Request.Context(), "channel.create")
	response.OK(c, gin.H{"item": item})
}

// UpdateAdminChannel 处理 PUT /api/admin/channels/{id}。
//
// **不受理 keys**：已有渠道的密钥池只有一条写路径 —— `PATCH /admin/channels/:id/keys`。
// 带了 keys 就 400，而不是静默忽略：静默忽略会让"我在编辑页改了密钥"变成
// 一次什么都没发生的保存，而运营要过很久才会发现密钥还是旧的。
func UpdateAdminChannel(c *gin.Context) {
	id, ok := channelID(c)
	if !ok {
		return
	}
	req, ok := parseChannelReq(c, false)
	if !ok {
		return
	}
	item, err := channel.Update(id, req, siteurl.Base(c))
	if err != nil {
		respondChannelErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionChannelUpdate, "channel", strconv.FormatUint(id, 10),
		gin.H{"name": item.Name})
	// base_url / adapter_id / 分组都可能改变可路由性：目录与文档要跟着重算。
	afterChange(c.Request.Context(), "channel.update")
	response.OK(c, gin.H{"item": item})
}

// DeleteAdminChannel 处理 DELETE /api/admin/channels/{id}（仍被绑定时拒绝）。
func DeleteAdminChannel(c *gin.Context) {
	id, ok := channelID(c)
	if !ok {
		return
	}
	if err := channel.Delete(id); err != nil {
		respondChannelErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionChannelDelete, "channel", strconv.FormatUint(id, 10), nil)
	afterChange(c.Request.Context(), "channel.delete")
	response.OK(c, nil)
}

// SetChannelStatus 处理 PUT /api/admin/channel/status（手动启停）。
func SetChannelStatus(c *gin.Context) {
	var req struct {
		ID     uint64 `json:"id"`
		Status *int   `json:"status"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ID == 0 || req.Status == nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	item, err := channel.SetStatus(req.ID, *req.Status, siteurl.Base(c))
	if err != nil {
		respondChannelErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionChannelBatchStatus, "channel", strconv.FormatUint(req.ID, 10),
		gin.H{"status": *req.Status, "reason": strings.TrimSpace(req.Reason)})
	// 启停直接决定「这个渠道上的模型还能不能被路由」：必须重算在售与文档。
	afterChange(c.Request.Context(), "channel.status")
	response.OK(c, gin.H{"item": item})
}

// TestChannel 处理 POST /api/admin/channel/test：返回测试结果对象（连通失败不视为请求错误）。
func TestChannel(c *gin.Context) {
	var req struct {
		ID uint64 `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ID == 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	res, err := channel.Test(c.Request.Context(), req.ID)
	if err != nil {
		respondChannelErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionChannelTest, "channel", strconv.FormatUint(req.ID, 10),
		gin.H{"ok": res.OK, "message": res.Message})
	response.OK(c, gin.H{"result": res})
}

// ListAdminAdapters 处理 GET /api/admin/adapters（内置适配器与能力矩阵，只读）。
func ListAdminAdapters(c *gin.Context) {
	response.OK(c, gin.H{"items": adaptersView(siteurl.Base(c))})
}

// parseChannelReq 解析并组装渠道入参（缺省值对齐原型 admin-channels.html）。
// adapterRegistered 判断适配器标识是否对应一个已注册适配器（内置或已启用的规格）。
//
// 注册名就是 relay.Adapter.Name()：内置是 `openai`，规格是 `spec:<slug>`。
// **不接受** `builtin:openai` 这类前缀写法——它不会被任何注册项匹配。
//
// 用 CanResolve 而不是 GetAdapter：后者会把失败写进 5s 负缓存，而这里正是
// 「刚建完规格、立刻保存渠道」的时序，负缓存会让保存误判为「适配器不存在」。
// afterChange 走统一后置管线（只剩定价失效：文档是内置契约，不随配置变）。
//
// 参数只用来在调用点自述「什么变了」（排障时读代码用），所以用 `...any` 接收：
// 每个 handler 多写一个字符串参数没有任何逻辑价值，改了还要全站跟着改。
func afterChange(...any) { lifecycle.AfterChange() }

func adapterRegistered(id string) bool {
	return channel.AdapterResolvable(db.DB, id)
}

func parseChannelReq(c *gin.Context, forCreate bool) (channel.Req, bool) {
	var body channelReq
	if err := c.ShouldBindJSON(&body); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return channel.Req{}, false
	}
	// 密钥池只有一条写路径：创建时收（初始池），此后一律走 PATCH /keys。
	//
	// 拒绝而不是忽略：忽略会让"我在编辑页填了密钥"变成一次什么都没发生的保存。
	if !forCreate && len(body.Keys) > 0 {
		response.FailDefMsg(c, errs.InvalidParam,
			"已有渠道的密钥池请用密钥池接口单独修改（PUT 不再受理 keys）")
		return channel.Req{}, false
	}
	// P2-9：adapter_id 必须命中注册表。原实现只校验「格式」，于是 `builtin:openai` 这种
	// 「格式合法但从未注册」的值能保存成功：管理台显示「启用」，但每次选路 GetAdapter 都失败，
	// 该渠道的每次调用都会在选路期失败（对外 503 service_unavailable）。这类配置错误只能靠保存时拦截。
	if !adapterRegistered(body.AdapterID) {
		response.FailDefMsg(c, errs.AdapterNotRegistered,
			errs.AdapterNotRegistered.Message+"："+strings.TrimSpace(body.AdapterID))
		return channel.Req{}, false
	}

	// 出厂值只有一个来源（channel.Defaults）：这里再写一份字面量，
	// 就会出现"管理台建的渠道"与"AI 建的渠道"默认限流不同。
	req := channel.Defaults()
	req.Name = body.Name
	// 不从 Defaults() 取：出厂默认里没有图标，nil 必须保持 nil（=不修改）。
	req.Icon = body.Icon
	req.AdapterID = body.AdapterID
	req.BaseURL = body.BaseURL
	// nil 透传 = 不修改；显式 "" = 清空（回到适配器默认）。
	req.Website = body.Website
	req.MultiKeyMode = body.MultiKeyMode
	req.Groups = body.Groups
	req.Keys = body.Keys
	// nil 透传 = service 层视为"不修改"；显式 null/{} 才清空。
	req.AdapterConfig = body.AdapterConfig
	req.AdapterOptions = body.AdapterOptions
	if body.Weight != nil {
		req.Weight = *body.Weight
	}
	if body.Priority != nil {
		req.Priority = *body.Priority
	}
	if body.RPMLimit != nil {
		req.RPMLimit = *body.RPMLimit
	}
	if body.MaxConcurrency != nil {
		req.MaxConcurrency = *body.MaxConcurrency
	}
	if body.SwitchKeyRetry != nil {
		req.SwitchKeyRetry = *body.SwitchKeyRetry
	}
	if body.AutoBan != nil {
		req.AutoBan = *body.AutoBan
	}
	return req, true
}

// channelID 解析路径 {id}。
func channelID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return 0, false
	}
	return id, true
}

// respondChannelErr 把 service 层错误映射为统一错误码（校验类回落 invalid_param）。
func respondChannelErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, channel.ErrNotFound):
		response.FailDef(c, errs.NotFound)
	case errors.Is(err, channel.ErrNameExists):
		response.FailDef(c, errs.ChannelNameExists.WithMessage(err.Error()))
	case errors.Is(err, channel.ErrConflict):
		// 409 而不是 400：这不是"参数填错了"，是"你手上的版本过期了"，
		// 前端据此决定"刷新后重试"而不是"改表单"。
		response.FailDef(c, errs.ChannelConflict)
	case errors.Is(err, channel.ErrUnknownKey):
		response.FailDef(c, errs.InvalidParam.WithMessage(err.Error()))
	case errors.Is(err, channel.ErrLastEnabledKey):
		response.FailDef(c, errs.InvalidParam.WithMessage(err.Error()))
	case errors.Is(err, channel.ErrIconInvalid):
		response.FailDef(c, errs.IconInvalid)
	case errors.Is(err, channel.ErrBound):
		response.FailDef(c, errs.ChannelBound)
	case errors.Is(err, channel.ErrNoAdapter):
		response.FailDef(c, errs.AdapterUnavailable)
	default:
		// 配置类错误必须把「哪个字段错了」带给运营，否则保存失败只会显示"参数不合法"。
		var cfgErr *relay.ConfigError
		if errors.As(err, &cfgErr) {
			response.FailDef(c, errs.InvalidParam.WithMessage(cfgErr.Error()))
			return
		}
		response.FailDef(c, errs.InvalidParam)
	}
}
