package controller

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/modality"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/pricing"
)

// gatewayModel 是 OpenAI list 中的单个模型对象（含 §4.5.7 pricing 扩展）。
//
// Modality / Endpoints / Available 是 B2-6 增补的网关自有字段：
// OpenAI 标准没有它们，但客户端要判断「这个模型该往哪个端点发」只能靠猜。
// 全部 omitempty 之外的字段必须显式给值，避免客户端把「缺字段」误判为「不支持」。
type gatewayModel struct {
	ID        string   `json:"id"`
	Object    string   `json:"object"`
	Created   int64    `json:"created"`
	OwnedBy   string   `json:"owned_by"`
	Modality  string   `json:"modality"`
	Endpoints []string `json:"endpoints"`
	Available bool     `json:"available"`
	// IconURL 是模型生效图标的绝对地址（模型自有 → 供应商回落）。
	// OpenAI 标准没有它，客户端会忽略未知字段；给绝对地址是因为第三方拿到的
	// 是 JSON 而不是本站页面，根相对路径在跨域场景下直接 404。
	IconURL string `json:"icon_url,omitempty"`
	// Tags 是营销标签（文案 + 渲染色）。**不带 omitempty**：无标签时必须是 `[]`，
	// 客户端 `x.tags.length` 遇到 null 会直接抛错。
	Tags    []model.ModelTagView `json:"tags"`
	Pricing modelPricing         `json:"pricing"`
}

// catalogModalities 之外的新增：music 已由 systemconfig 的 modalities 列表统一维护，
// 这里补上以保持「目录遍历顺序」与前端一致（B2-7）。
// catalogModalities 是 /v1/models 的模态归一表（唯一词表在 pkg/modality）。
var catalogModalities = modality.All

// modelPricing 是 /v1/models 的价格透出结构。
//
// **刻度是美元，与站点结算币种无关**：这是 OpenAI 生态客户端的协议契约
// （很多客户端把金额直接按美元展示）。跟随站点币种会让人民币部署下
// 客户端显示的余额整体放大 7.2 倍。汇率换算只在本文件发生。
type modelPricing struct {
	BillingMode string            `json:"billing_mode"`
	Currency    string            `json:"currency"` // 恒为 USD
	Prices      model.Prices      `json:"prices"`   // 美元微 / 百万 tokens 或 / 次
	SpecPrices  *model.SpecPrices `json:"spec_prices,omitempty"`
	Default     bool              `json:"default"`
}

// defaultModelEntry 是模态默认模型的下发结构（§4.3.11 L403 每项附 allowed）。
type defaultModelEntry struct {
	Model   string `json:"model"`
	Allowed bool   `json:"allowed"`
}

// GatewayModels 处理 GET /v1/models（API KEY 鉴权，设计 §4.3.11 L402）。
//
// **按鉴权头选择返回形状**（与兼容实现的判定逐字一致），因为同一个 URL 会被三类
// SDK 打：
//   - Anthropic SDK 的 `client.models.list()` 带 `x-api-key` + `anthropic-version`，
//     它按 `{data:[{id,display_name,created_at}],first_id,last_id,has_more}` 解析；
//   - google-genai 在 base_url 填了 `/v1` 时带 `x-goog-api-key`（或 `?key=`），
//     它按 `{models:[{name,displayName}]}` 解析；
//   - 其余按 OpenAI 的 `{object:"list",data:[...]}`。
//
// 不分支的后果不是报错而是**静默解析失败**：SDK 拿到 200 却得到空列表或字段缺失，
// 用户看到的是"模型列表是空的"，而服务端日志里是一次成功请求。
//
// 三个形状各自由一个具名入口产出（本函数 + GatewayModelsOpenAI + GeminiModelList），
// 不靠 FullPath 猜：`/v1beta/openai/models` 必须恒为 OpenAI 形状，
// 它一旦跟着 header 漂走，那一层的存在前提（OpenAI SDK 只改 base_url）就没了。
func GatewayModels(c *gin.Context) {
	switch {
	case c.GetHeader("x-api-key") != "" && c.GetHeader("anthropic-version") != "":
		writeAnthropicModels(c)
	case c.GetHeader("x-goog-api-key") != "" || strings.TrimSpace(c.Query("key")) != "":
		body, def, ok := geminiModelPage(c)
		if !ok {
			gateway.FailDef(c, def)
			return
		}
		gateway.OK(c, body)
	default:
		GatewayModelsOpenAI(c)
	}
}

// GatewayModelsOpenAI 处理 GET /v1beta/openai/models：**恒为 OpenAI 形状**。
//
// 单独一个入口而不是给 GatewayModels 传参：这一层是 Gemini 的 OpenAI 兼容层，
// 换成别的形状等于改了它存在的前提。
func GatewayModelsOpenAI(c *gin.Context) {
	items, err := allowedCatalog(c)
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	base := siteurl.Base(c)
	data := make([]gatewayModel, 0, len(items))
	for i := range items {
		data = append(data, gatewayModelOf(&items[i], base))
	}
	gateway.OK(c, gin.H{"success": true, "object": "list", "data": data})
}

// anthropicModel 是 Anthropic 的模型对象。
//
// created_at 是 **RFC3339 字符串**（不是 Unix 秒）—— 这是 Anthropic 与 OpenAI
// 在这一处的硬差别，给成数字会让 SDK 的时间解析直接抛错。
type anthropicModel struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// writeAnthropicModels 写 Anthropic 形状的模型列表（GET /v1/models 的 header 分支）。
func writeAnthropicModels(c *gin.Context) {
	items, err := allowedCatalog(c)
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	data := make([]anthropicModel, 0, len(items))
	for i := range items {
		it := &items[i]
		name := it.DisplayName
		if name == "" {
			name = it.ID
		}
		created := ""
		if it.CreatedAt > 0 {
			created = time.Unix(it.CreatedAt, 0).UTC().Format(time.RFC3339)
		}
		data = append(data, anthropicModel{Type: "model", ID: it.ID, DisplayName: name, CreatedAt: created})
	}
	first, last := "", ""
	if n := len(data); n > 0 {
		first, last = data[0].ID, data[n-1].ID
	}
	// has_more 恒 false：本平台的目录是一次性全量（几百条），不做游标分页。
	// 给 true 会诱导 SDK 继续发 after_id，而我们不认那个参数 → 空列表。
	gateway.OK(c, gin.H{"data": data, "has_more": false, "first_id": first, "last_id": last})
}

// allowedCatalog 返回「本 KEY 可用且在售」的模型（/v1/models、/v1beta/models、
// /v1beta/openai/models 共用这一个过滤点）。
//
// 为什么必须是唯一入口：三处各写一遍白名单过滤，漂移时的表现是
// 「OpenAI 面能看到、Gemini 面看不到」—— 两边都"正常"，只有用户发现少了一个模型。
//
// 与兼容协议一致：只列本 KEY 能用的模型。不过滤时客户端会从列表里自动挑一个，
// 然后调用 403 —— 它没有任何办法从「列表里明明有」推回「我的钥没这个权限」。
func allowedCatalog(c *gin.Context) ([]pricing.ModelItem, error) {
	cat, err := pricing.Get()
	if err != nil {
		return nil, err
	}
	k := relay.APIKeyOf(c)
	out := make([]pricing.ModelItem, 0, len(cat.Models))
	for i := range cat.Models {
		it := cat.Models[i]
		if k != nil && it.Enabled && !apikey.AllowsModel(k, it.ID, it.Modality) {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// geminiModel 是 Gemini 的模型对象。
//
// `name` 必须带 `models/` 前缀：google-genai 客户端把它原样回填到
// `models/<name>:generateContent`，少了前缀就是 404。
type geminiModel struct {
	Name                       string   `json:"name"`
	DisplayName                string   `json:"displayName,omitempty"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

// GeminiModelList 处理 GET /v1beta/models（Gemini 协议面）。
//
// 与 /v1/models 共用 allowedCatalog：两份列表各自过滤时会出现
// 「OpenAI 面有、Gemini 面没有」这种谁也说不清的差异。
//
// 只列**真的能用**的方法：chat 模型给 generateContent / streamGenerateContent，
// embedding 模型给 embedContent。`countTokens` 本平台返回 501，因此不列 ——
// 列了等于骗 SDK 去调一个必然失败的接口。其它模态（图片/音频/视频）不出现：
// Gemini 协议这边不承接它们（走各自的专用端点）。
func GeminiModelList(c *gin.Context) {
	body, def, ok := geminiModelPage(c)
	if !ok {
		gateway.FailDef(c, def)
		return
	}
	gateway.OK(c, body)
}

// geminiModelPage 产出 Gemini 形状的模型分页体（/v1beta/models 与
// /v1/models 的 x-goog-api-key 分支共用）。
//
// 返回 (body, def, ok) 而不是直接写响应：两个调用方的**信封不同**
// （本平台都是 gateway.OK，但未来任一处换信封不必改这里），
// 更关键的是分页与过滤规则只有一份 —— 抄第二份的漂移表现是两边条数不一致。
func geminiModelPage(c *gin.Context) (gin.H, errs.Def, bool) {
	items, err := allowedCatalog(c)
	if err != nil {
		return nil, errs.Internal, false
	}
	// 分页：pageSize 缺省 50、上限 1000；pageToken 是十进制 offset。
	// 两者都必须有上界与校验，否则 pageToken=999999999 会让 OFFSET 溢出成 0
	// 却回显"已翻页"（与分页事故同款形态）。
	pageSize := 50
	if v := strings.TrimSpace(c.Query("pageSize")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > 1000 {
			return nil, errs.InvalidParam.WithMessage("pageSize 必须是 1..1000 的整数"), false
		}
		pageSize = n
	}
	offset := 0
	if v := strings.TrimSpace(c.Query("pageToken")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 || n > pagination.MaxPage*pagination.MaxPageSize {
			return nil, errs.InvalidParam.WithMessage("pageToken 非法（应为上一页返回的十进制偏移）"), false
		}
		offset = n
	}

	out := make([]geminiModel, 0, len(items))
	for i := range items {
		it := &items[i]
		method := ""
		switch strings.TrimSpace(it.Modality) {
		case "chat", "":
			method = "chat"
		case "embedding":
			method = "embedding"
		default:
			continue
		}
		m := geminiModel{Name: "models/" + it.ID, DisplayName: it.DisplayName}
		if m.DisplayName == "" {
			m.DisplayName = it.ID
		}
		if method == "chat" {
			m.SupportedGenerationMethods = []string{"generateContent", "streamGenerateContent"}
		} else {
			m.SupportedGenerationMethods = []string{"embedContent"}
		}
		out = append(out, m)
	}
	// 切片而不是重新查询：目录本身是内存快照（pricing.Get），offset 只是视图。
	start := offset
	if start > len(out) {
		start = len(out)
	}
	end := start + pageSize
	next := ""
	if end < len(out) {
		next = strconv.Itoa(end)
	} else {
		end = len(out)
	}
	body := gin.H{"models": out[start:end]}
	if next != "" {
		body["nextPageToken"] = next
	}
	return body, errs.Def{}, true
}

// gatewayModelOf 组装单个模型对象（list 与 retrieve 共用，避免两处字段不一致）。
//
// created 必须取 models.created_at 的真实 Unix 秒：改造前硬编码 time.Now()，
// 同一模型每次请求 created 都不同，客户端的模型缓存永远失效（且看起来像上游在频繁更新）。
func gatewayModelOf(it *pricing.ModelItem, base string) gatewayModel {
	if it == nil {
		return gatewayModel{Object: "model"}
	}
	modality := strings.TrimSpace(it.Modality)
	if modality == "" {
		modality = "chat"
	}
	tags := it.Tags
	if tags == nil {
		tags = make([]model.ModelTagView, 0)
	}
	return gatewayModel{
		ID:        it.ID,
		Object:    "model",
		Created:   it.CreatedAt,
		OwnedBy:   it.Vendor,
		Modality:  modality,
		Endpoints: modelEndpointList(it),
		Available: it.Available,
		IconURL:   iconref.URL(base, it.Icon),
		Tags:      tags,
		Pricing:   buildPricing(it, it.IsDefault),
	}
}

// modelEndpointList 返回模型可用的端点路径列表。
//
// 优先用 models.endpoints 的显式声明（跨模态复用的唯一真相）；未声明时按 modality
// 派生一份**默认**清单——客户端（NextChat / LobeChat 之类）没法靠 modality 猜端点，
// 不给默认值它们只能硬编码一张跟我们永远不同步的表。
func modelEndpointList(it *pricing.ModelItem) []string {
	var declared []string
	if len(it.Endpoints) > 0 {
		_ = json.Unmarshal(it.Endpoints, &declared)
	}
	out := make([]string, 0, len(declared))
	seen := make(map[string]struct{}, len(declared))
	for _, e := range declared {
		p := strings.TrimPrefix(strings.TrimSpace(e), "/")
		p = strings.TrimPrefix(p, "v1/")
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) > 0 {
		return out
	}
	for _, pr := range relay.ModalityProbes(it.Modality) {
		if pr.Path == "" {
			continue
		}
		if _, dup := seen[pr.Path]; dup {
			continue
		}
		seen[pr.Path] = struct{}{}
		out = append(out, pr.Path)
	}
	return out
}

// GatewayModelDefaults 处理 GET /v1/models/defaults（API KEY 鉴权，设计 §4.3.11 L403）：
// 按模态返回默认模型，allowed 按当前调用 KEY 的白名单计算（复用 relay/apikey 判定）。
func GatewayModelDefaults(c *gin.Context) {
	k := relay.APIKeyOf(c)
	if k == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	cat, err := pricing.Get()
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	defs := modalityDefaultMap(cat)
	out := make(map[string]defaultModelEntry, len(catalogModalities))
	for _, m := range catalogModalities {
		id := defs[m]
		entry := defaultModelEntry{Model: id}
		if id != "" {
			entry.Allowed = apikey.AllowsModel(k, id, m)
		}
		out[m] = entry
	}
	response.OK(c, out)
}

// GatewayModelGet 处理 GET /v1/models/{model}（D4 对齐 new-api / OpenAI 标准 models.retrieve）。
func GatewayModelGet(c *gin.Context) {
	id := c.Param("model")
	cat, err := pricing.Get()
	if err != nil {
		gateway.FailDef(c, errs.Internal)
		return
	}
	k := relay.APIKeyOf(c)
	for i := range cat.Models {
		it := &cat.Models[i]
		if it.ID == id && it.Enabled && (k == nil || apikey.AllowsModel(k, it.ID, it.Modality)) {
			gateway.OK(c, gatewayModelOf(it, siteurl.Base(c)))
			return
		}
	}
	// 未命中按兼容协议的方式答：**HTTP 200 + error 体**，而不是 404。
	//
	// 这是对方的一个已知缺陷（OpenAI SDK 拿到 200 会按模型对象反序列化，
	// 得到一堆零值而不是抛错），但既有客户端的重试/降级分支是按它写的：
	// 给 404 会让它们认定"接口版本不对"进而改写 base_url。文档里已注明。
	gateway.OK(c, gin.H{"error": gateway.ErrorObject{
		Message: "model not found: " + id,
		Type:    "invalid_request_error",
		Param:   "model",
		Code:    errs.CodeModelNotFound,
	}})
}

// usdCentsOf 把结算币种微换算为 OpenAI billing 口的「0.01 美元」整数。
//
// 推导：cents = micro × 100 / (1e6 × fx)，而 fx 以微比例 F 表示时
// cents = micro × 100 / F —— 一次整数乘除，无浮点、无中间量。
// 站点是 USD 时 F = 1e6，退化为 cents = micro / 1e4。
func usdCentsOf(micro int64) int64 {
	f := usdFxMicro()
	v, err := money.MulDiv(micro, money.CentsPerUnit, f)
	if err != nil {
		return 0
	}
	return v
}

// usdFxMicro 返回「1 美元 = 多少微结算币种」的微比例（人民币站 = 7.2e6）。
func usdFxMicro() int64 {
	if conf.SiteCurrency() != conf.CurrencyCNY {
		return money.Scale
	}
	fx := conf.CnyPerUsd()
	if fx <= 0 {
		fx = conf.DefaultCnyPerUsd
	}
	return int64(math.Round(fx * float64(money.Scale)))
}

// toUSDPrices 把结算币种的价目换算成美元微（只用于 OpenAI 协议透出）。
func toUSDPrices(p model.Prices) model.Prices {
	f := usdFxMicro()
	conv := func(v *int64) *int64 {
		if v == nil {
			return nil
		}
		out, err := money.MulDiv(*v, money.Scale, f)
		if err != nil {
			return nil
		}
		return &out
	}
	return model.Prices{
		Input: conv(p.Input), Output: conv(p.Output), Cache: conv(p.Cache),
		CacheCreate: conv(p.CacheCreate), CacheCreate1h: conv(p.CacheCreate1h),
		Image: conv(p.Image), Audio: conv(p.Audio), AudioOutput: conv(p.AudioOutput),
		Call: conv(p.Call),
	}
}

// toUSDSpec 把结算币种的规格价目换算成美元微。
//
// 必须与 toUSDPrices 成对存在：只换算 `prices` 而把 `spec_prices` 原样透出，
// 人民币站点下同一份 pricing 里会出现「token 价是美元、图像价是人民币」的混合口径，
// 客户端按美元展示时图像价会正好小 7.2 倍（看起来像打了 1.4 折）。
//
// 键（档位名）不换算，只换算金额；map 一律新建，避免改动定价目录快照里的共享数据。
func toUSDSpec(sp model.SpecPrices) model.SpecPrices {
	f := usdFxMicro()
	conv := func(v *int64) *int64 {
		if v == nil {
			return nil
		}
		out, err := money.MulDiv(*v, money.Scale, f)
		if err != nil {
			return nil
		}
		return &out
	}
	convMap := func(m map[string]int64) map[string]int64 {
		if len(m) == 0 {
			return nil
		}
		out := make(map[string]int64, len(m))
		for k, v := range m {
			if c, err := money.MulDiv(v, money.Scale, f); err == nil {
				out[k] = c
			}
		}
		return out
	}
	return model.SpecPrices{
		Kind:                sp.Kind,
		TierPrice:           convMap(sp.TierPrice),
		MaxN:                sp.MaxN,
		SecondPrice:         convMap(sp.SecondPrice),
		MaxSeconds:          sp.MaxSeconds,
		ResolutionFixed:     sp.ResolutionFixed,
		LongAfterSeconds:    sp.LongAfterSeconds,
		LongVideoMultiplier: sp.LongVideoMultiplier,
		RefImagePrice:       conv(sp.RefImagePrice),
		RefImageFree:        sp.RefImageFree,
		RefAudioPrice:       conv(sp.RefAudioPrice),
		RefVideoPrice:       conv(sp.RefVideoPrice),
		RefVideoSecondPrice: convMap(sp.RefVideoSecondPrice),
		PerTaskPrice:        conv(sp.PerTaskPrice),
		// 按条计费（音乐/歌词）：漏掉这一行，/v1/models 的 pricing.spec_prices 里
		// 只剩 kind 与 max_n —— 客户端拿不到单价，只能显示"按条计费"却算不出金额。
		PerClipPrice: conv(sp.PerClipPrice),
	}
}

// BillingSubscription 处理 GET /dashboard/billing/subscription 与 /v1/dashboard/billing/subscription
// （D1 对齐 new-api：NextChat / LobeChat 等生态客户端的余额显示依赖）。
func BillingSubscription(c *gin.Context) {
	u := relay.UserOf(c)
	k := relay.APIKeyOf(c)
	if u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	// 生态客户端（NextChat / LobeChat）用它显示余额，所以**结构不能变**：
	// 关掉额度查询时这三个字段回 0 而不是 403 —— 客户端会把 403 当"密钥失效"，
	// 直接弹"请重新登录"，而用户其实只是关了一个隐私开关。
	//
	// 语义必须是「总额度 = 剩余 + 已用」：这些客户端一律按
	// `余额 = hard_limit_usd - total_usage` 展示。只填剩余时它们会把已用金额
	// 再减一次 —— 显示金额偏小甚至为负，而账单本身是对的，用户只会认为平台算错。
	//
	// access_until 取密钥过期时间（0 = 不限）：这是 OpenAI 契约里唯一表达
	// 「凭据何时失效」的字段，写死「十年后」等于告诉客户端永远不会过期。
	usd := 0.0
	if quotaVisible(c) {
		usd = float64(usdCentsOf(u.Balance+u.Used)) / 100
	}
	accessUntil := int64(0)
	if k.ExpiredAt != nil {
		accessUntil = k.ExpiredAt.Unix()
	}
	gateway.OK(c, gin.H{
		"object":                "billing_subscription",
		"has_payment_method":    true,
		"soft_limit_usd":        usd,
		"hard_limit_usd":        usd,
		"system_hard_limit_usd": usd,
		"access_until":          accessUntil,
	})
}

// BillingUsage 处理 GET /v1/dashboard/billing/usage 与 /dashboard/billing/usage。
//
// 口径与兼容协议一致：**账号级累计已用**，不按 KEY、不按日期窗口。
// 两个取舍都必须写清，因为它们看起来"更像 bug"：
//   - 忽略 start_date/end_date：兼容协议的实现从来不读这两个参数（客户端会传，
//     但只有 OpenAI 官方按它切窗口）。在这里按日期过滤会让同一账号在不同客户端
//     显示两个不同的「已用」，而 hard_limit 是账号级的，相减就不是余额。
//   - 取 users.used 而不是 SUM(usages.charge)：前者是结算时累加的累计列（与
//     /v1/account 的 used 同源），后者要扫全历史 —— 这个端点被客户端高频轮询，
//     一次全表聚合就是把已知性能坑搬到对外兼容口上。
func BillingUsage(c *gin.Context) {
	u := relay.UserOf(c)
	if u == nil {
		gateway.FailDef(c, errs.InvalidAPIKey)
		return
	}
	gateway.OK(c, gin.H{"object": "list", "total_usage": usdCentsOf(u.Used)})
}

// PortalDefaults 处理 GET /api/portal/defaults（门户 JWT，设计 §4.3.11... §4.3.8 L474）：
// 按模态返回默认模型，any_key_allowed 表示用户任一 KEY 的白名单含该模型。
func PortalDefaults(c *gin.Context) {
	uid := c.GetUint64("user_id")
	if uid == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	cat, err := pricing.Get()
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	defs := modalityDefaultMap(cat)
	keys, err := userKeys(uid)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	out := make(map[string]gin.H, len(catalogModalities))
	for _, m := range catalogModalities {
		id := defs[m]
		allowed := false
		if id != "" {
			for i := range keys {
				if apikey.AllowsModel(&keys[i], id, m) {
					allowed = true
					break
				}
			}
		}
		out[m] = gin.H{"model": id, "any_key_allowed": allowed}
	}
	response.OK(c, out)
}

// userKeys 读取用户全部（未软删）KEY，用于 any_key_allowed 判定。
func userKeys(userID uint64) ([]model.APIKey, error) {
	var keys []model.APIKey
	if err := db.DB.Where("user_id = ?", userID).Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// modalityDefaultMap 直接复用定价快照里已派生的 Defaults（E4：默认模型唯一真相源）。
//
// 优先级（system_config.modality_defaults[modality] → 该模态首个启用模型）在
// pricing.deriveDefaults 实现一次，这里只做「按目录遍历顺序投影」，不再有第二份判定逻辑。
// 原实现的三级回落（models.is_default → configured → 首个启用）已删除：
// models.is_default 没有写入口（只在 seed 写过），把它当第一优先级会让运营改完默认模型后
// 界面上显示的仍是旧值。
func modalityDefaultMap(snap *pricing.Snapshot) map[string]string {
	out := make(map[string]string, len(catalogModalities))
	if snap == nil {
		return out
	}
	for _, m := range catalogModalities {
		if id, ok := snap.Defaults[m]; ok && id != "" {
			out[m] = id
		}
	}
	return out
}

// buildPricing 由定价目录条目派生 pricing：**一律换算成美元口径**。
//
// 为什么不能直接下发 settlement 币种的价目：这份结构是 OpenAI 生态客户端的
// 计价契约，客户端按美元 × 100 的整数展示。跟随站点币种会导致
// 人民币部署下客户端显示的价格放大 7.2 倍。
func buildPricing(it *pricing.ModelItem, isDefault bool) modelPricing {
	p := modelPricing{
		BillingMode: it.BillingMode,
		Currency:    conf.CurrencyUSD,
		Prices:      toUSDPrices(it.Prices),
		Default:     isDefault,
	}
	if it.BillingMode == model.BillingModeSpec && !it.SpecPrices.IsZero() {
		sp := toUSDSpec(it.SpecPrices)
		p.SpecPrices = &sp
	}
	return p
}
