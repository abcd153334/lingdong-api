// Package pricing 是模型定价目录的组装与内存缓存。
//
// 口径（唯一真相源）：模型价目就是**绝对金额（微）**。
// 不存在倍率、刻度、qpc 这类中间量 —— 库里存什么，计费就用什么，门户就展示什么。
// 旧实现把「价目 ÷ qpc」存成 4 位小数倍率、读回来再「× qpc」，
// 每次改价都在两条换算链上各丢一次精度，7.2 倍的漂移就是这么来的。
package pricing

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/service/configsync"
)

// cacheTTL 是定价快照的默认有效期。
//
// 10s + singleflight：改价后最长 10 秒生效，过期瞬间只有一个协程重建。
// 先上 singleflight 再收 TTL —— 顺序不能反，否则击穿放大。
const cacheTTL = 10 * time.Second

// ModelItem 是定价目录中的单个模型（含供应商展示信息与价目）。
type ModelItem struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Vendor      string `json:"vendor"`
	VendorName  string `json:"vendor_name"`
	VendorColor string `json:"vendor_color"`
	// Icon 是**最终展示值**：模型自己设的图标，没有就沿用所属供应商的图标，
	// 都没有则为空串（前端渲染首字占位）。
	//
	// 派生发生在 build()，前端不再自己拼接回落链 —— 回落规则一旦散到前端，
	// "模型没有图标时显示什么"就变成两份真相源。
	Icon     string `json:"icon"`
	Modality string `json:"modality"`
	// BillingMode 是计费方式（tokens / call / spec），与 models.billing_mode 同值。
	BillingMode string `json:"billing_mode"`
	// Prices 是 token/按次价目，单位**微**（1 货币单位 = 1e6 微）。
	Prices model.Prices `json:"prices"`
	// SpecPrices 是按规格计价的价目（图像/视频/音乐），单位同为微。
	SpecPrices model.SpecPrices `json:"spec_prices"`
	Enabled    bool             `json:"enabled"`
	IsDefault  bool             `json:"is_default"`
	Endpoints  model.JSON       `json:"endpoints"`
	// Capabilities 是模型能力声明（与 models.capabilities 同源）。
	//
	// 为什么要进目录：网关的**预检期**判定只能读这里（那时还没选路）。
	// 「该模型是否支持流式出图」这类判定若放到适配器里按模型名写白名单，
	// 就是第二真相源 —— 运营在管理台改了能力声明，适配器那份不会跟着变。
	Capabilities model.JSON `json:"capabilities"`
	// Tags 是营销标签（已过滤到期项，顺序即展示顺序）。
	//
	// 收进快照而不是让每个出口各自查库/各自判过期：门户、网关、助手都读这一份，
	// 各自再判一次就会出现「门户隐藏了但 /v1/models 还在」。
	Tags []model.ModelTagView `json:"tags"`
	// MaxOutputTokens 是该模型单次输出上限（0 = 未声明），来自 models.max_output_tokens。
	//
	// 网关用它决定「请求没写 max_tokens 时的冻结上限」：写死 1024 会让长输出请求
	// 结算额远超冻结额，被 3 倍截顶后确定性少收。这里只复用模型已有的事实，
	// 不新造配置项 —— 同一个事实两处存储必然对不上。
	MaxOutputTokens int `json:"max_output_tokens"`
	// IsFree 是「价格为 0」的派生事实（保存期由 model.PricingIsFree 唯一派生）。
	IsFree bool `json:"is_free"`
	// Available 表示该条目当前可被真实路由。目录只收「模型启用 + 存在启用绑定 +
	// 该绑定渠道启用」，因此恒为 true；保留字段是为了固定对外形状。
	Available bool `json:"available"`
	// CreatedAt 是模型创建时间（Unix 秒），供 OpenAI list 的 created 字段透出真实值。
	CreatedAt int64 `json:"created_at"`
}

// VendorItem 是供应商分组（与计费分组正交）。
type VendorItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	// Icon 是供应商图标 token（"" | builtin:<key> | upload:<ulid>）：编辑回填用。
	Icon string `json:"icon"`
	// IconURL 是同一个图标的可渲染地址。
	//
	// **快照里恒为空**：地址依赖请求域名（多域名部署各有各的 Host），
	// 而快照有 10s TTL、被所有请求共享。只在控制器响应期填（见
	// controller.withVendorIconURL）——在快照里填过一次，第一个请求的域名
	// 就会被缓存给所有人。
	IconURL    string `json:"icon_url,omitempty"`
	Priority   int    `json:"priority"`
	Sort       int    `json:"sort"`
	Builtin    bool   `json:"builtin"`
	ModelCount int64  `json:"model_count"`
}

// Snapshot 是定价目录快照。
//
// 不变性约定：快照一旦生成即**不可变**。Get 返回外壳的浅拷贝
// （map 已复制、Models/Vendors 共享只读切片），调用方不得写入任何字段。
type Snapshot struct {
	Models  []ModelItem  `json:"models"`
	Vendors []VendorItem `json:"vendors"`
	// GroupDiscounts 是计费分组折扣（**微比例**：1e6 = 不打折，900000 = 九折）。
	// 键 = 计费分组名，与供应商分组（vendor）正交。
	GroupDiscounts map[string]int64 `json:"group_discounts"`
	// Defaults 是「每模态默认模型」的唯一派生结果。
	Defaults map[string]string `json:"defaults"`
	// Scale 是「1 货币单位 = 多少微」，恒定 1e6。
	//
	// 下发而不是写死在客户端：客户端要用它把微格式化成金额，
	// 两边各写一个 1000000 的话，某天改动只会改一边。
	Scale int64 `json:"scale"`
	// Currency 是结算币种（CNY/USD），CurrencySymbol 是对应的展示符号。
	Currency       string `json:"currency"`
	CurrencySymbol string `json:"currency_symbol"`
	// CnyPerUSD 只用于「次币种展示」与「上游美元成本折算」，**永不进入扣费公式**。
	CnyPerUSD float64 `json:"cny_per_usd"`
	// OtherCurrency 是另一种币种（用于「¥72.00 / $10.00」这类并列展示）。
	OtherCurrency string `json:"other_currency"`
}

var (
	mu       sync.RWMutex
	cached   *Snapshot
	cacheAt  time.Time
	inflight *buildCall
	// gen 是"缓存世代"。每次失效 +1，用于识别"跨越失效点的重建"：
	// 若一次 build 开始于改价之前、结束于改价之后，它产出的就是**旧快照**，
	// 绝不能写回缓存（写回等于把旧价再续命一个 TTL，而 cacheTTL 是计费取价依据）。
	gen uint64
)

// buildCall 是一次进行中的重建（singleflight）。
type buildCall struct {
	done chan struct{}
	snap *Snapshot
	err  error
}

// Get 返回定价快照（命中缓存直接返回；过期时**单个**协程重建，其余等待同一结果）。
func Get() (*Snapshot, error) {
	mu.RLock()
	if cached != nil && time.Since(cacheAt) < cacheTTL {
		s := cached.clone()
		mu.RUnlock()
		return s, nil
	}
	mu.RUnlock()

	mu.Lock()
	// 双检：等锁期间可能已有别的协程填好缓存。
	if cached != nil && time.Since(cacheAt) < cacheTTL {
		s := cached.clone()
		mu.Unlock()
		return s, nil
	}
	if c := inflight; c != nil {
		mu.Unlock()
		<-c.done
		return c.snap.clone(), c.err
	}
	c := &buildCall{done: make(chan struct{})}
	inflight = c
	startGen := gen
	mu.Unlock()

	s, err := build()

	mu.Lock()
	// 只在"构建期间没有发生过失效"时写回缓存。否则丢弃结果（等它的调用方
	// 拿到的仍是改价前的快照，但它们本来就是在改价前发起的），下一次 Get 重建。
	if err == nil && gen == startGen {
		cached, cacheAt = s, time.Now()
	}
	c.snap, c.err = s, err
	inflight = nil
	close(c.done)
	mu.Unlock()
	return s.clone(), err
}

// clone 复制快照外壳与可变 map（Models/Vendors 作为只读切片共享）。
func (s *Snapshot) clone() *Snapshot {
	if s == nil {
		return nil
	}
	out := *s
	out.GroupDiscounts = copyInt64Map(s.GroupDiscounts)
	out.Defaults = copyStringMap(s.Defaults)
	return &out
}

func copyInt64Map(in map[string]int64) map[string]int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// IsModalityDefault 判断模型是否为某模态的默认模型（管理台/门户共用同一份派生结果）。
func IsModalityDefault(modelID string) bool {
	if modelID == "" {
		return false
	}
	s, err := Get()
	if err != nil || s == nil {
		return false
	}
	for _, id := range s.Defaults {
		if id == modelID {
			return true
		}
	}
	return false
}

// ConfiguredDefaults 返回 system_config.modality_defaults 的原始配置（可能含已失效项）。
func ConfiguredDefaults() map[string]string {
	out := map[string]string{}
	_ = conf.System().Unmarshal("modality_defaults", &out)
	return out
}

// deriveDefaults 按唯一优先级派生每模态默认模型：
// 配置项（必须存在、启用且模态匹配）→ 该模态第一个启用模型。
//
// items 已按 id ASC 且只含启用模型（见 build），因此「第一个」是确定的。
func deriveDefaults(items []ModelItem, configured map[string]string) map[string]string {
	out := make(map[string]string, len(configured)+4)
	for modality, want := range configured {
		if want == "" {
			continue
		}
		for i := range items {
			if items[i].ID == want && items[i].Modality == modality {
				out[modality] = want
				break
			}
		}
	}
	for i := range items {
		if _, ok := out[items[i].Modality]; !ok {
			out[items[i].Modality] = items[i].ID
		}
	}
	return out
}

// InvalidatePricingCache 清空本机缓存**并广播**给其它实例（多副本改价一致）。
func InvalidatePricingCache() {
	InvalidateLocalCache()
	configsync.NotifyPricing()
}

// InvalidateLocalCache 只清本机缓存，不广播（广播接收端与内部重载使用）。
//
// 同时递增世代号：清空 `cached` 挡不住"已经在跑"的那次重建，它会带着旧价
// 回来并把 cacheAt 刷新 —— 见 Get 里的 gen 判断。不清 inflight 是因为
// 等待中的调用方还握着那个 channel，丢弃它只会让同一份数据被构建两次。
func InvalidateLocalCache() {
	mu.Lock()
	cached, cacheAt = nil, time.Time{}
	gen++
	mu.Unlock()
}

// OnSaleModels 返回「当前在售」的模型行，是「哪些模型对外存在」的唯一判定。
//
// 为什么导出：文档目录必须与它一致 —— 文档里出现一个模型广场点不到的模型，
// 用户照着文档写完代码才发现没有权限。
//
// 返回完整模型行（不是目录条目）：文档投影需要 endpoints / spec_prices / prices，
// 从条目上再复原一次就是第二份事实。
func OnSaleModels() ([]model.Model, error) {
	var models []model.Model
	if err := onSaleQuery(nil).Order("id ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// OnSaleModelsByIDs 是 OnSaleModels 的窄版本：只判定给定模型里哪些在售。
//
// 为什么要有它：文档投影每次只关心「这个节点的几个成员」，而 Markdown 导出
// 会逐接口调用投影 —— 用全表版本会把一次导出变成上百次全表扫 + EXISTS。
// 判据必须与 OnSaleModels 逐字同源，否则文档会出现目录里买不到的模型。
func OnSaleModelsByIDs(ids []string) ([]model.Model, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var models []model.Model
	if err := onSaleQuery(ids).Order("id ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

// onSaleQuery 是「在售」唯一判据的构造点（ids 为空 = 全量）。
//
// 目录必须与「能不能真的路由」一致：模型启用 + 存在启用绑定 + 该绑定指向
// 启用且未删的渠道。否则目录就是在骗人（选中后必然 503）。
func onSaleQuery(ids []string) *gorm.DB {
	q := db.DB.Model(&model.Model{}).Where("enabled = ?", true)
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	}
	if catalogRequireBinding() {
		sub := db.DB.Model(&model.ModelBinding{}).Select("1").
			Joins("JOIN channels c ON c.id = model_bindings.channel_id AND c.deleted_at IS NULL").
			Where("model_bindings.model_id = models.id AND model_bindings.enabled = ? AND c.status = ?",
				true, model.ChannelStatusEnabled)
		q = q.Where("EXISTS (?)", sub)
	}
	return q
}

func build() (*Snapshot, error) {
	var vendors []model.Vendor
	if err := db.DB.Order("sort ASC, id ASC").Find(&vendors).Error; err != nil {
		return nil, err
	}
	vendorByID := make(map[string]model.Vendor, len(vendors))
	for _, v := range vendors {
		vendorByID[v.ID] = v
	}

	models, err := OnSaleModels()
	if err != nil {
		return nil, err
	}

	items := make([]ModelItem, 0, len(models))
	// 整个快照共用一个时刻：否则同一批模型可能落在到期边界两侧（快照内自相矛盾）。
	now := time.Now()
	for _, m := range models {
		v := vendorByID[m.Vendor]
		items = append(items, ModelItem{
			ID:              m.ID,
			DisplayName:     m.DisplayName,
			Vendor:          m.Vendor,
			VendorName:      v.Name,
			VendorColor:     v.Color,
			Icon:            brandicon.Effective(m.Icon, v.Icon),
			Modality:        m.Modality,
			BillingMode:     m.BillingMode,
			Prices:          m.Prices,
			SpecPrices:      m.EffectiveSpecPrices(),
			Enabled:         m.Enabled,
			Endpoints:       m.Endpoints,
			Capabilities:    m.Capabilities,
			Tags:            model.ActiveTags(model.DecodeTags(m.Tags), now),
			MaxOutputTokens: m.MaxOutputTokens,
			IsFree:          m.IsFree,
			Available:       true,
			CreatedAt:       m.CreatedAt.Unix(),
		})
	}
	if catalogRequireBinding() {
		var total int64
		if err := db.DB.Model(&model.Model{}).Where("enabled = ?", true).Count(&total).Error; err == nil {
			reportHiddenModels(len(items), int(total))
		}
	}

	// IsDefault 由「每模态默认模型」唯一派生，不读 models.is_default。
	defaults := deriveDefaults(items, ConfiguredDefaults())
	for i := range items {
		items[i].IsDefault = defaults[items[i].Modality] == items[i].ID
	}

	vItems := make([]VendorItem, 0, len(vendors))
	vendorCount := map[string]int64{}
	for _, m := range models {
		vendorCount[m.Vendor]++
	}
	for _, v := range vendors {
		vItems = append(vItems, VendorItem{
			ID: v.ID, Name: v.Name, Color: v.Color, Icon: v.Icon,
			Priority: v.Priority, Sort: v.Sort, Builtin: v.Builtin,
			ModelCount: vendorCount[v.ID],
		})
	}

	currency := conf.SiteCurrency()
	return &Snapshot{
		Models:         items,
		Vendors:        vItems,
		GroupDiscounts: readGroupDiscounts(),
		Defaults:       defaults,
		Scale:          money.Scale,
		Currency:       currency,
		CurrencySymbol: conf.SiteCurrencySymbol(),
		CnyPerUSD:      conf.CnyPerUsd(),
		OtherCurrency:  conf.OtherCurrency(currency),
	}, nil
}

// GroupDiscount 返回某计费分组的折扣（微比例）；未配置的分组按「不打折」，并告警。
//
// 「未知分组按不打折」是刻意的：配置漏一个分组时，用户不该被多收钱。
// 反向（未知按 0 = 全免费）会让新增分组变成免费通道，那是更贵的错。
func GroupDiscount(group string) int64 {
	if group == "" {
		group = "default"
	}
	table := readGroupDiscounts()
	if v, ok := table[group]; ok {
		return v
	}
	reportUnknownGroup(group)
	return money.Scale
}

var (
	unknownMu   sync.Mutex
	unknownSeen = map[string]bool{}
)

func reportUnknownGroup(group string) {
	unknownMu.Lock()
	seen := unknownSeen[group]
	unknownSeen[group] = true
	unknownMu.Unlock()
	if seen {
		return
	}
	logger.L().Warn("计费分组没有折扣配置，按不打折处理", zap.String("group", group))
}

// maxGroupDiscount 是分组折扣的上限（10 倍）：正常折扣区间是 0.5–1.5，
// 企业独占很少超过 10 倍。它同时是「手滑多打两个 0」的兜底线。
const maxGroupDiscount = 10 * money.Scale

// ConfiguredGroupDiscounts 返回 system_config 里**实际配置**的分组折扣表（微比例）。
//
// 返回 nil 表示「没有真相源」（配置缺失 / JSON 损坏 / 全部取值非法）。
// 这个三态（nil / 空 / 非空）是 channel 保存校验需要的：它只在真相源存在时才
// 比对「渠道声明的分组是否存在」，配置还没加载时不该把渠道判成非法。
//
// 非法值一律丢弃而不是夹紧：0 或负数会让整组免费，那是「配置写错 = 白送」的方向。
func ConfiguredGroupDiscounts() map[string]int64 {
	raw, ok := conf.System().Get("group_discount")
	if !ok {
		return nil
	}
	var parsed map[string]int64
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed) == 0 {
		return nil
	}
	out := make(map[string]int64, len(parsed))
	for k, v := range parsed {
		if strings.TrimSpace(k) == "" || v <= 0 || v > maxGroupDiscount {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// readGroupDiscounts 返回**生效**的分组折扣表（恒含 default：缺省为不打折）。
func readGroupDiscounts() map[string]int64 {
	out := map[string]int64{"default": money.Scale}
	for k, v := range ConfiguredGroupDiscounts() {
		out[k] = v
	}
	return out
}

// GroupNames 返回已配置折扣的计费分组名（升序，恒含 default）。
//
// 供渠道保存校验与选路使用：分组的唯一真相源是 group_discount 的键集，
// 在别处再解析一遍就会漂移（运营新加一个分组，渠道却仍判它不存在）。
func GroupNames() []string {
	table := readGroupDiscounts()
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// catalogRequireBinding 判断目录是否只收「存在启用绑定」的模型
// （LD_CATALOG_REQUIRE_BINDING，默认开）。
func catalogRequireBinding() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_CATALOG_REQUIRE_BINDING"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

var (
	hiddenMu   sync.Mutex
	hiddenSeen = -1
)

// reportHiddenModels 在被过滤的模型数量变化时打一次 Warn（避免每 10s 重建都刷一条）。
func reportHiddenModels(shown, total int) {
	hidden := total - shown
	if hidden < 0 {
		hidden = 0
	}
	hiddenMu.Lock()
	changed := hiddenSeen != hidden
	hiddenSeen = hidden
	hiddenMu.Unlock()
	if !changed {
		return
	}
	if hidden == 0 {
		logger.L().Info("定价目录已无隐藏模型", zap.Int("shown", shown))
		return
	}
	logger.L().Warn("定价目录隐藏了无启用绑定的模型（门户与 /v1/models 不可见）",
		zap.Int("hidden", hidden), zap.Int("shown", shown), zap.Int("enabled_total", total))
}
