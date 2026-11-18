package modelcfg

import (
	"encoding/json"
	"reflect"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/schema"
	"lingdong-api/internal/service/stats"
)

// priceLane 描述一条价格 lane 的读写方式。
//
// 同时给出 get 与 set：价目字段是 `*int64`，"未提交"与"提交了 0"靠 nil 区分，
// 所以补值/清空时必须能**写入指针本身**，只拿 `*int64` 回写会改到副本上。
type priceLane struct {
	name  string
	label string
	get   func(model.Prices) *int64
	set   func(*model.Prices, *int64)
}

// laneCN 是错误文案用的中文字段名（同一份口径只在价格校验里出现一次）。
var laneCN = []priceLane{
	{"input", "输入单价", func(p model.Prices) *int64 { return p.Input }, func(p *model.Prices, v *int64) { p.Input = v }},
	{"output", "输出单价", func(p model.Prices) *int64 { return p.Output }, func(p *model.Prices, v *int64) { p.Output = v }},
	{"cache", "缓存读取单价", func(p model.Prices) *int64 { return p.Cache }, func(p *model.Prices, v *int64) { p.Cache = v }},
	{"cache_create", "缓存写入单价", func(p model.Prices) *int64 { return p.CacheCreate }, func(p *model.Prices, v *int64) { p.CacheCreate = v }},
	{"cache_create_1h", "1 小时缓存写入单价", func(p model.Prices) *int64 { return p.CacheCreate1h }, func(p *model.Prices, v *int64) { p.CacheCreate1h = v }},
	{"image", "图像单价", func(p model.Prices) *int64 { return p.Image }, func(p *model.Prices, v *int64) { p.Image = v }},
	{"audio", "音频输入单价", func(p model.Prices) *int64 { return p.Audio }, func(p *model.Prices, v *int64) { p.Audio = v }},
	{"audio_output", "音频输出单价", func(p model.Prices) *int64 { return p.AudioOutput }, func(p *model.Prices, v *int64) { p.AudioOutput = v }},
	{"call", "按次单价", func(p model.Prices) *int64 { return p.Call }, func(p *model.Prices, v *int64) { p.Call = v }},
}

// Build 把入参编译成 models 行（不落库）。
//
// 规则顺序刻意固定：先判模态/计费方式，再判必填，最后判符号与量级。
func Build(in Input) (*model.Model, error) {
	if err := ValidateModality(in.Modality); err != nil {
		return nil, err
	}
	if err := ValidateBillingMode(in.Modality, in.BillingMode); err != nil {
		return nil, err
	}
	// 展示名：nil（未提交）或空串都收敛到 ID —— 与创建期的兜底同一口径。
	// 空串不是"错误"，而是"我不想单独起名"，所以不报错、用 ID 顶上。
	displayName := in.ID
	if in.DisplayName != nil {
		if v := strings.TrimSpace(*in.DisplayName); v != "" {
			displayName = v
		}
	}
	var description string
	if in.Description != nil {
		description = *in.Description
	}
	// 图标：nil = 本次没提交（FillMissing 已用库内值补上），非 nil 才归一。
	// 归一放这里而不是 controller：AI 编排的 apply 也走 Build，只在校验一处做，
	// 才不会出现「管理台存得进、AI 提案存不进」这种分叉。
	var icon string
	if in.Icon != nil {
		v, err := brandicon.Normalize(*in.Icon)
		if err != nil {
			return nil, fe("icon", "图标不合法：只支持内置图标或站内上传的图片")
		}
		icon = v
	}
	m := &model.Model{
		ID:          in.ID,
		DisplayName: displayName,
		Description: description,
		Icon:        icon,
		Vendor:      in.Vendor,
		Modality:    in.Modality,
		BillingMode: in.BillingMode,
	}
	in.fillEnabled(true)
	m.Enabled = *in.Enabled
	// 上下文窗口与输出上限：nil 已在 FillMissing 里补成库内值；新建时 nil 视为 0（继承全局）。
	//
	// 必须走指针：`Updates` 是**全列写**，非指针的 int 在"只改价格"的提交里是 0，
	// 会把运营填好的窗口静默清零，而界面上刚显示过一次正确的值。
	if in.ContextWindow != nil {
		m.ContextWindow = *in.ContextWindow
	}
	if in.MaxOutputTokens != nil {
		m.MaxOutputTokens = *in.MaxOutputTokens
	}
	if f, reason := model.AgentLimitProblem(m.ContextWindow, m.MaxOutputTokens); f != "" {
		return nil, fe(f, "%s", reason)
	}

	if in.EndpointsSet {
		if in.Endpoints == nil {
			m.Endpoints = nil
		} else {
			eps := make([]string, 0, len(in.Endpoints))
			for _, e := range in.Endpoints {
				if k := apidoc.NormalizeEndpointKey(e); k != "" {
					eps = append(eps, k)
				}
			}
			m.Endpoints = model.MustJSON(eps)
		}
	}
	// 能力：nil = 未提交（沿用库内），非 nil 才归一。
	// `[]` 必须原样落成 `[]`，不能被"按模态兜底"重新打开运营关掉的 TAB。
	if in.CapabilitiesSet {
		if in.Capabilities == nil {
			m.Capabilities = nil
		} else {
			raw := json.RawMessage(model.MustJSON(in.Capabilities))
			caps := apidoc.NormalizeCapabilities(in.Modality, raw)
			m.Capabilities = model.MustJSON(apidoc.CapKeys(caps))
		}
	}
	// 标签：`[]` 与 NULL 语义相同（没有"未声明标签"这回事），一律落成 `[]`。
	if in.TagsSet {
		tags, err := normalizeTags(in.Tags, stats.Location())
		if err != nil {
			return nil, err
		}
		m.Tags = model.MustJSON(tags)
	}

	if err := applyPrices(m, in); err != nil {
		return nil, err
	}
	m.IsFree = m.PricingIsFree()
	return m, nil
}

// ValidateModality 校验模态（唯一真相源是 service/schema）。
//
// 对外暴露的原因：提案期的结构校验要**先**给出带 operations[i] 的精确错误，
// 但它不能自己再写一份模态列表 —— 只能调这里。
func ValidateModality(modality string) error {
	if !schema.IsModality(modality) {
		return fe("modality", "未知模态 %q（可选值：chat/image/video/audio/music/embedding/rerank/moderation）", modality)
	}
	return nil
}

// ValidateBillingMode 校验「模态 × 计费方式」的合法组合。
//
// 关键一条：audio **不允许按 token** —— TTS/ASR 的响应是二进制或纯文本，
// ParseUsage 永远解析不出 token，配成按 token 就是**永久 0 元**。
// 允许的是「按次」与「按秒」（后者要求时长有真实来源，见 billing.perSecondCharge）。
func ValidateBillingMode(modality, mode string) error {
	if !schema.IsModality(modality) {
		return fe("billing_mode", "未知模态 %q", modality)
	}
	if !schema.AllowsBillingMode(modality, mode) {
		return fe("billing_mode",
			"模态 %s 不支持计费方式 %q（图像/视频/音乐只能按规格，音频可按次或按规格/按秒）；可用取值：%s",
			modality, mode, strings.Join(schema.AllowedBillingModes(modality), " / "))
	}
	return nil
}

// ValidateSpecPrices 归一化并校验按规格价目（唯一实现：billing）。
//
// 返回**归一化后**的结构：只校验不归一化是历史上的真实错价来源
// （提交 1080P 能过校验，运行期按 1080p 查档位永不命中）。
func ValidateSpecPrices(modality string, sp model.SpecPrices) (model.SpecPrices, []string, error) {
	normalized, err := billing.NormalizeSpecPrices(modality, sp)
	if err != nil {
		return sp, nil, fe("spec_prices", "%v", err)
	}
	warnings, err := billing.ValidateSpecPrices(modality, normalized)
	if err != nil {
		return sp, nil, fe("spec_prices", "%v", err)
	}
	return normalized, warnings, nil
}

// applyPrices 是**唯一**的价格落库实现（按 token 8 条 lane / 按次 / 按规格）。
//
// 绝对价下不存在换算：入参已经是微，直接落列。
// 旧实现把"价格 × 刻度"换算成 ratio 存库，写入与回读用了两个不同的锚
// （×500000 写入、÷qpc 回读），导致人民币站每编辑一次价格就放大 7.2 倍。
func applyPrices(m *model.Model, in Input) error {
	// 清空名单先校验：名字拼错必须报错，静默忽略等于"点了清除但价格还在"。
	for _, name := range in.ClearPrices {
		if _, ok := laneByName(name); !ok {
			return fe("clear_prices", "未知价格通道 %q（可选值：input/output/cache/cache_create/cache_create_1h/image/audio/audio_output/call）", name)
		}
	}
	// 所有 lane 一律非负且不越界（不区分计费方式，任何一列脏数据都会污染账单）。
	for _, l := range laneCN {
		v := l.get(in.Prices)
		if v == nil {
			continue
		}
		if *v < 0 {
			return fe(l.name, "%s不能为负数", l.label)
		}
		if err := money.Check(*v); err != nil {
			return fe(l.name, "%s超出范围", l.label)
		}
	}
	// 计费方式只选中一套价目：把另一套**显式**写成空（不是"靠 m 是零值"）。
	//
	// `Updates` 写全列，所以 tokens/call 分支不写 spec_prices 时它也照样落库 ——
	// 落的是 `m` 的零值。那属于"隐式正确"：任何人把 Build 改成复用 cur 行
	// （这是个很自然的优化）就会让旧档位价复活，而复活后的表现是门户上
	// 「按次」模型显示 ¥/秒 的价目。显式清空 = 把这条不变量钉在本地。
	switch in.BillingMode {
	case model.BillingModeTokens:
		if in.Prices.Input == nil {
			return fe("input", "按 token 计费必须提供输入单价")
		}
		// 半免费：输入价是 0、其它 lane 有价。这类账单无法向用户解释
		// （"为什么我只发了 10 个字却被收钱"），且在实操里 100% 是漏填输入价。
		// 真要免费就把所有 lane 都填 0 —— 那时 is_free 派生为真，门户会打「免费」徽标。
		if in.Prices.Input != nil && *in.Prices.Input == 0 && !in.Prices.AllConfiguredFree() {
			return fe("input", "输入价为 0 时其它价格必须也为 0（半免费账单无法解释；要免费请全部填 0）")
		}
		m.Prices = in.Prices
		m.SpecPrices = model.SpecPrices{}
		return nil
	case model.BillingModeCall:
		if in.Prices.Call == nil {
			return fe("call", "按次计费必须提供单次价格")
		}
		m.Prices = in.Prices
		m.SpecPrices = model.SpecPrices{}
		return nil
	case model.BillingModeSpec:
		normalized, _, err := ValidateSpecPrices(in.Modality, in.SpecPrices)
		if err != nil {
			return err
		}
		m.SpecPrices = normalized
		return nil
	default:
		return fe("billing_mode", "未知计费方式 %q", in.BillingMode)
	}
}

// FillMissing 用库内现值为更新补齐"本次没提交"的字段。
//
// 为什么必须逐 lane 补齐（而不是"只要 input 给了就整体跳过"）：
// Updates 写全列，未提交的 lane 会落成 NULL —— 一条「只改输入价」的提案
// 会把 output/cache/image/audio 全部抹成 NULL，模型静默变成"输出不收费"。
func FillMissing(in *Input, cur *model.Model) {
	// 展示名与描述必须最先补：Updates 是全列写，漏一条就是"改价把名字清空"
	// （2026-10-29 审计 A-01 的实际现场）。
	if in.DisplayName == nil {
		v := cur.DisplayName
		in.DisplayName = &v
	}
	if in.Description == nil {
		v := cur.Description
		in.Description = &v
	}
	// icon 与展示名同类：不补就被全列写清空，而"只改价格"是最常见的提交形态。
	if in.Icon == nil {
		v := cur.Icon
		in.Icon = &v
	}
	// vendor 未提交时沿用库内：它不是"提案要改的东西"，而是一个引用。
	if in.Vendor == "" {
		in.Vendor = cur.Vendor
	}
	if in.Modality == "" {
		in.Modality = cur.Modality
	}
	if !in.BillingModeSet {
		in.BillingMode = cur.BillingMode
		in.BillingModeSet = true
	}
	// 价格与"事实字段"必须分开补：价格段有三处合理的提前返回，
	// 但那些返回**只该跳出价格段**。写成函数级 return 会顺手跳过下面的
	// endpoints / capabilities 回填，而 Updates 是写全列 —— 于是一条"按 token 改按次"
	// 的提案会把端点与能力写成 NULL：门户端点消失、文档右栏 TAB 全没，全程不报错。
	in.fillPrices(cur)
	in.clearPrices()
	in.fillFacts(cur)
	// 模型级覆盖也必须补：漏了它，全列写会把窗口清成 0（= 回到全局继承），
	// 而"只改价格"是最常见的提交形态，所以这个 bug 会天天发生。
	if in.ContextWindow == nil {
		v := cur.ContextWindow
		in.ContextWindow = &v
	}
	if in.MaxOutputTokens == nil {
		v := cur.MaxOutputTokens
		in.MaxOutputTokens = &v
	}
}

// laneByName 按 lane 名找读写器（`ClearPrices` 与错误文案共用 laneCN 这一份口径）。
func laneByName(name string) (priceLane, bool) {
	for _, l := range laneCN {
		if l.name == name {
			return l, true
		}
	}
	return priceLane{}, false
}

// clearPrices 把 ClearPrices 名单里的 lane 置空（写 NULL）。
//
// 必须在 fillPrices **之后**执行：先清后补会被 FillMissing 立刻用库内值填回去，
// 表现就是"界面上删掉了价格，保存后又出现"。
func (in *Input) clearPrices() {
	for _, name := range in.ClearPrices {
		if l, ok := laneByName(name); ok {
			l.set(&in.Prices, nil)
		}
	}
}

// fillPrices 只补 Prices 与 SpecPrices（允许内部提前返回）。
//
// **绝不按比例重算**：旧实现用「新输入价 × 旧倍率」补其它 lane，
// 于是运营"只把输入价从 2 调到 3"会让输出价从 8 静默变成 12。
// 绝对价下逐 lane 用库内原值补齐，改一个只动一个。
func (in *Input) fillPrices(cur *model.Model) {
	if cur.BillingMode != in.BillingMode {
		// 计费方式变了就不补：把 token 价当按次价会差几个数量级，
		// 宁可让 Build 报"必须提供单价"也不猜。
		return
	}
	switch in.BillingMode {
	case model.BillingModeTokens, model.BillingModeCall:
		curPrices := cur.Prices
		for _, l := range laneCN {
			if l.get(in.Prices) != nil {
				continue
			}
			if v := l.get(curPrices); v != nil {
				cp := *v
				l.set(&in.Prices, &cp)
			}
		}
	case model.BillingModeSpec:
		// 只改显示名的按规格模型必须沿用库内 spec_prices，
		// 否则校验会报"必须至少配置一个档位"，改名都改不了。
		if !in.SpecPricesSet {
			in.SpecPrices = cur.SpecPrices
			in.SpecPricesSet = true
		}
	}
}

// fillFacts 无条件补"非价格事实"，任何路径都不许跳过。
func (in *Input) fillFacts(cur *model.Model) {
	if !in.EndpointsSet {
		in.EndpointsSet = true
		in.Endpoints = decodeStrings(cur.Endpoints)
	}
	if !in.CapabilitiesSet {
		in.CapabilitiesSet = true
		// nil 与 [] 都要保留：cur 为 NULL 时写回 NULL，为 [] 时写回 []。
		if cur.Capabilities != nil {
			var caps []string
			if json.Unmarshal(cur.Capabilities, &caps) == nil {
				in.Capabilities = caps
			}
		}
	}
	// 标签同样是无条件回填：不过滤过期项 —— 过滤等于"保存一次就把过期标签从库里洗掉"，
	// 而运营恰恰需要打开旧项续期。
	if !in.TagsSet {
		in.TagsSet = true
		in.Tags = model.DecodeTags(cur.Tags)
	}
	// enabled 同理：nil = 本次没提交（保留库内），非 nil 才按提交值写。
	if in.Enabled == nil {
		v := cur.Enabled
		in.Enabled = &v
	}
}

func decodeStrings(raw model.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// updateSkipColumns 是不参与「全列写」的列：主键、时间戳、软删、乐观锁版本号。
const updateSkipColumnsX = "id,version,created_at,updated_at,deleted_at"

var updateSkip = func() map[string]bool {
	out := map[string]bool{}
	for _, c := range strings.Split(updateSkipColumnsX, ",") {
		out[c] = true
	}
	return out
}()

// Updates 把模型行转成更新 map（避免 GORM 对带 default 的列丢零值）。
//
// **列清单由反射生成**：唯一来源是 model.Model（含内嵌 model.Prices）的 gorm column tag。
// 历史教训：旧实现手写 19 列，新增价格列而不加进去 → 保存成功但值永远回滚成旧值；
// 加了但忘改测试 → 测试也不拦。反射生成后"新增列不可能漏写"。
//
// 全列写是刻意的：只写部分列会让"没提到的列"保持旧值，看起来更安全，
// 但一旦某次改动把这些列算成 nil，就变成静默清空。全列写 + FillMissing 补齐，
// 语义才可预测：库里看到的就是 Build 算出来的。
func Updates(m *model.Model) map[string]any {
	out := make(map[string]any, 24)
	v := reflect.ValueOf(m).Elem()
	var walk func(rv reflect.Value, rt reflect.Type)
	walk = func(rv reflect.Value, rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(rv.Field(i), f.Type)
				continue
			}
			col := gormColumnOf(f)
			if col == "" || updateSkip[col] {
				continue
			}
			out[col] = rv.Field(i).Interface()
		}
	}
	walk(v, v.Type())
	return out
}

// gormColumnOf 读 struct tag 里的显式 column 名。
//
// 刻意**不**回落到 GORM 的默认命名：`CreateCache1h` 的默认列是 `create_cache1h`，
// 而库列是 `create_cache_1h` —— 两边一分叉，症状是"保存成功但价格永远不生效"。
// 没有显式 column tag 的字段一律被忽略（由 columns_test 守住）。
func gormColumnOf(f reflect.StructField) string {
	for _, part := range strings.Split(f.Tag.Get("gorm"), ";") {
		if strings.HasPrefix(part, "column:") {
			return strings.TrimPrefix(part, "column:")
		}
	}
	return ""
}

// Data 把库内行还原成提案载荷（回滚与 model_get 共用）。
//
// 绝对价下**不再反解**：直接返回库内价格原值。
// 旧实现"ratio → price"的反解再 Build 回去会二次取整，改一次价漂一次。
func Data(m *model.Model) Input {
	in := Input{
		ID:             m.ID,
		Vendor:         m.Vendor,
		Modality:       m.Modality,
		BillingMode:    m.BillingMode,
		BillingModeSet: true,
		Prices:         m.Prices,
		SpecPrices:     m.SpecPrices,
		SpecPricesSet:  true,
	}
	dn, de := m.DisplayName, m.Description
	in.DisplayName, in.Description = &dn, &de
	// 图标必须一起还原：AI 提案的快照/回滚都走 Data，漏了它，
	// 「回滚一次改价」会把模型图标一并清空。
	ic := m.Icon
	in.Icon = &ic
	cw, mo := m.ContextWindow, m.MaxOutputTokens
	in.ContextWindow, in.MaxOutputTokens = &cw, &mo
	enabled := m.Enabled
	in.Enabled = &enabled
	in.EndpointsSet = true
	in.Endpoints = decodeStrings(m.Endpoints)
	in.CapabilitiesSet = true
	if m.Capabilities != nil {
		in.Capabilities = decodeStrings(m.Capabilities)
	}
	// 标签必须一起还原：AI 提案的快照/回滚都走 Data，漏了它，
	// 「回滚一次改价」会把营销标签一并清空（与图标同一条教训）。
	in.TagsSet = true
	in.Tags = model.DecodeTags(m.Tags)
	return in
}
