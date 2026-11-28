// Package presets 写入**内置出厂预设**（渠道 / 模型 / 绑定）。
//
// 数据来源有两条，都落到同一个 presetGroup 结构：
//
//  1. `presets_builtin.json`（`//go:embed`，由 `tools/presetdump` 从现网只读导出）——
//     运营已经调好的那套配置，是新实例唯一可参考的出厂骨架；
//  2. 代码里手写的 `arkPreset()`——历史方舟组，保留是为了让老库的标记与绑定不断链。
//
// 为什么"第三方中转平台"（多米API / 秘塔 / AutoDL）也进种子：它们的渠道与模型已经
// 是运营验证过的现网事实，留空等于让每个新实例重新踩一遍上游路径与价目。
// 供应商展示成什么仍然由运营决定（模型行上的 vendor 是种子建议值，可随时改）。
//
// 为什么单独一个包而不是塞进 internal/db：
// 预设数据要引用 `apidoc` 的能力词表（唯一真相源），而 `apidoc` 反过来 import 了
// `billing`，`billing` 的集成测试又 import 了 `db` —— 一旦 `db` import `apidoc`
// 就形成**测试期**导入环（`go build` 不报，`go test`/`go vet` 才炸）。
// 把这份数据放在 `db` 之上的一层（只被 main 调用）即可彻底断开。
package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/service/billing"
	channelsvc "lingdong-api/internal/service/channel"
	"lingdong-api/internal/service/modelcfg"
)

// 本文件是**内置预设层**：把厂商的模型预置成「停用」状态。
//
// 三条语义，缺一不可：
//
//  1. **默认不启用**（不是"默认可用"）：模型 `enabled=false`、渠道 `status=2`
//     （`model.ChannelStatusDisabled`，不是 0 —— 枚举只有 1/2/3）、绑定
//     `enabled=false`。运营必须先填自己的 API KEY 再启用 —— 内置的密钥是空的，
//     启用一个空密钥渠道等于给用户一个必然失败的模型。
//
//  2. **只写一次，且每组一个独立标记**（`system_config` 的 `builtin_presets.<group>`）。
//     标记存在就**整体跳过**该组，而不是逐行 `OnConflict{DoNothing}` —— 后者的效果是
//     「运营删掉某个模型后重启，模型带着绑定一起复活」，而运营删除是一个明确意图。
//     **标记绝不能跨组共用**：共用等于"老实例已有标记 → 新组永远不写入"，
//     而新组恰恰是老实例最需要的那部分。
//
//  3. **价目是占位值**：真实结算价由运营核定。启用前必须逐档核对 —— 占位值只是
//     "结构完整、能通过保存期校验"的最小集合，不代表任何厂商报价。写入时打一条 warn。
//
// 这一层是"出厂内容"的唯一来源。`db.Seed` 只写基础设施（管理员 / system_config /
// 默认应用 / 供应商 / 套餐），两者互不依赖，开关也只有一个是 `LD_BUILTIN_PRESETS`。

type presetGroup struct {
	// MarkerKey 是幂等标记（system_config 主键）。每组独立，理由见包注释第 2 条。
	MarkerKey string `json:"marker_key"`
	// Version 是预设内容版本。改价目/能力/模型清单时**必须 +1**：
	// 旧实现"标记命中即整体跳过" → 已部署实例永远拿不到新结构；现在版本落后会走
	// 「只补不改」的增量（见 seedGroup）。
	Version int `json:"version"`
	// Vendor 是本组的**默认供应商**（模型没写 Vendor 时用它）。
	Vendor model.Vendor `json:"vendor"`
	// Vendors 是本组额外引用的供应商。聚合渠道（一条渠道横跨 kling/google/openai…）
	// 必须在这里声明，否则模型写入会因 vendor 不存在而失败。
	Vendors []model.Vendor `json:"vendors,omitempty"`
	// Channel 是本组要绑定/新建的渠道事实。
	Channel presetChannel `json:"channel"`
	Models  []presetModel `json:"models"`
}

// RowGroup 返回渠道/模型行上的内置标记（"builtin.<slug>"）。
//
// 与 marker 键同源：marker 是 "builtin_presets.<slug>"（兼容火山方舟的历史键），
// 行标记是 "builtin.<slug>"。不要在别处手写这两个前缀 —— 恢复出厂与徽标都按它筛行。
func (g presetGroup) RowGroup() string {
	return "builtin." + strings.TrimPrefix(g.MarkerKey, "builtin_presets.")
}

// presetChannel 是内置渠道的固定事实（不含密钥：内置渠道永远是空密钥）。
type presetChannel struct {
	AdapterID string `json:"adapter_id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	// Website 是上游官网（展示/跳转）。内置渠道必须显式配置：
	// 新用户第一眼看到的就是它，留空等于给了一个"没有出处"的渠道。
	Website string `json:"website"`
	Icon    string `json:"icon"`
	// AdapterOptions 只放**行为开关**（如 metaso fast_mode）。
	// 租户私有值（AutoDL 的 workflows ID）绝不能进种子：那是账号里的对象，不是产品事实。
	AdapterOptions json.RawMessage `json:"adapter_options,omitempty"`
	MaxConcurrency int             `json:"max_concurrency"`
	RPMLimit       int             `json:"rpm_limit"`
	Weight         int             `json:"weight"`
	Priority       int             `json:"priority"`
}

// presetModel 是一个内置模型的完整声明。
type presetModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	// Description 是模型描述（门户可见）。
	Description string `json:"description"`
	// Vendor 为空 = 用组默认 Vendor；聚合渠道必须逐条指定（duomi 一组 5 家）。
	Vendor string `json:"vendor"`
	Icon   string `json:"icon"`
	// Modality 如 chat / image / video / music。
	Modality string `json:"modality"`
	// BillingMode 是 tokens | call | spec；空 = spec（火山方舟的历史写法）。
	BillingMode string `json:"billing_mode"`
	// Prices 是 tokens/call 的九条 lane（单位微）；Spec 是规格计价。
	Prices model.Prices     `json:"prices"`
	Spec   model.SpecPrices `json:"spec"`
	// Endpoints 是该模型在本平台实际承载的端点（如 video/generations）。
	Endpoints []string         `json:"endpoints"`
	Caps      []apidoc.Cap     `json:"caps"`
	Tags      []model.ModelTag `json:"tags,omitempty"`
	// ContextWindow / MaxOutputTokens：0 = 继承全局。
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
	// BindingUpstream 是该模型在本渠道上的上游名（空 = 与模型 ID 同名）。
	// 必须能表达：秘塔的上游叫 MiniMax-H3，不是本地 ID。
	BindingUpstream string `json:"binding_upstream,omitempty"`
	BindingWeight   int    `json:"binding_weight"`
	BindingPriority int    `json:"binding_priority"`
}

// presetGroups 是全部内置预设组（顺序即写入顺序，互不依赖）。
func presetGroups() []presetGroup {
	// 顺序即写入顺序：**先现网导出的组，再代码里的历史组（方舟）**。
	// 同一模型 ID 出现在两组时（如 duomi 与方舟都提供 seedance），先写入的那组
	// 决定全局价目与能力；后一组只补自己渠道的停用绑定（见 createPresetModels）。
	// 把现网数据放前面，是为了让"当前配置"成为唯一价目真相，而不是被代码里的
	// 占位价覆盖。
	groups := builtinPresetGroups()
	return append(groups, arkPreset())
}

// Seed 写入内置厂商预设。
//
// 与 `db.Seed` 分开调用而不是塞进它：那一层是"没有它系统跑不起来"的基础设施，
// 无条件执行；本层是"可关、可恢复"的出厂内容（开关 `LD_BUILTIN_PRESETS`）。
// 两者混在一个函数里，就会重演"想关演示数据却连带丢了模型清单"的老问题。
func Seed(gdb *gorm.DB) error {
	if !conf.EnvConfig().BuiltinPresets {
		return nil
	}
	groups := presetGroups()
	// 先校验声明再写库：拼错一个 vendor / 计价方式，应该在启动时报出组名，
	// 而不是写了一半才被 modelcfg 拒掉（那时已留下部分行）。
	for _, g := range groups {
		if err := g.validate(); err != nil {
			return fmt.Errorf("内置预设声明不合法(%s): %w", g.MarkerKey, err)
		}
	}
	// 逐组独立事务：一组写坏不该把另一组也已写好的内容回滚掉（两组互不依赖）。
	// 但错误必须全部带出去 —— 预设写不进去时服务要停下，否则运营看到的是一份缺模型的列表。
	var errs []error
	for _, g := range groups {
		if err := seedGroup(gdb, g); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.MarkerKey, err))
		}
	}
	return errors.Join(errs...)
}

// seedGroup 写入/补齐一组预设。
//
// 版本语义：marker 里的 version 落后时走「只补不改」——只插入缺失的模型与绑定，
// 绝不 UPDATE 运营已改过的行。要回到出厂值走管理台的「恢复出厂」（显式动作）。
func seedGroup(gdb *gorm.DB, g presetGroup) error {
	cur, found, err := markerVersion(gdb, g.MarkerKey)
	if err != nil {
		return err
	}
	if found && cur >= g.Version {
		return nil
	}

	siblings := siblingGroupCount(g)
	return gdb.Transaction(func(tx *gorm.DB) error {
		// ① 先占位再工作：多副本同时启动时只有一行能插入成功。
		//    占位版本写 0（不是目标版本）：事务失败回滚后不会留下"标记在、行不在"。
		claim := model.SystemConfig{
			Key:   g.MarkerKey,
			Value: model.MustJSON(map[string]any{"version": 0}),
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim).Error; err != nil {
			return fmt.Errorf("db: 内置预设标记写入失败(%s): %w", g.MarkerKey, err)
		}
		// ② 锁住标记行再读版本：并发的第二个副本会等第一个提交，然后看到新版本直接返回。
		var row model.SystemConfig
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("key = ?", g.MarkerKey).First(&row).Error; err != nil {
			return fmt.Errorf("db: 读取内置预设标记(%s)失败: %w", g.MarkerKey, err)
		}
		var cur struct {
			Version int `json:"version"`
		}
		_ = json.Unmarshal(row.Value, &cur)
		if cur.Version >= g.Version {
			return nil
		}

		// ③ 供应商：先按 ID、再按名字（复用运营已建的同一家）、最后才新建。
		vendorIDs, err := upsertPresetVendors(tx, g)
		if err != nil {
			return err
		}

		// ④ 渠道（八态匹配，见 resolveGroupChannel）必须在模型之前：
		//    模型与绑定要同事务创建，绑定需要 channel_id。
		ch, adopted, err := resolveGroupChannel(tx, g, siblings)
		if err != nil {
			return err
		}
		if adopted && ch != nil {
			// 认领运营行：只补标记，不改任何业务字段。
			if err := tx.Model(&model.Channel{}).Where("id = ?", ch.ID).
				Update("preset_group", g.RowGroup()).Error; err != nil {
				return fmt.Errorf("db: 认领渠道 #%d 失败: %w", ch.ID, err)
			}
		}

		// ⑤ 模型 + 绑定。只处理**尚不存在**的模型（软删墓碑也跳过：删除是明确意图）。
		models, err := createPresetModels(tx, g, ch, vendorIDs)
		if err != nil {
			return err
		}

		// ⑥ 标记升版：只在这里写目标版本，前五步任一失败都会整体回滚。
		val := model.MustJSON(map[string]any{
			"version": g.Version, "at": time.Now().UTC().Format(time.RFC3339),
		})
		if err := tx.Model(&model.SystemConfig{}).Where("key = ?", g.MarkerKey).
			Update("value", val).Error; err != nil {
			return fmt.Errorf("db: 更新内置预设标记(%s)失败: %w", g.MarkerKey, err)
		}

		chDesc := "(跳过：同名冲突或匹配歧义)"
		if ch != nil {
			chDesc = ch.BaseURL
		}
		logger.L().Warn("已写入内置预设（全部停用）",
			zap.String("group", g.MarkerKey),
			zap.String("adapter", g.Channel.AdapterID),
			zap.Int("models_new", len(models)),
			zap.String("channel", chDesc),
			zap.String("note", "价目为参考值：启用前请在管理台逐档核定"))
		return nil
	})
}

// markerVersion 读取组标记里的内容版本（不存在 = 0/false）。
func markerVersion(gdb *gorm.DB, key string) (int, bool, error) {
	var row model.SystemConfig
	err := gdb.Where("key = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("db: 读取内置预设标记(%s)失败: %w", key, err)
	}
	var v struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(row.Value, &v)
	return v.Version, true, nil
}

// siblingGroupCount 返回与本组「同 adapter + 同归一化 base」的种子组数量。
//
// 用途只有一个：判断"两条渠道是不是同一个上游的兄弟组"（Metaso 标准/优先）。
// 兄弟组存在时，**不允许**把"base 相同、名字不同"当成运营改名去复用 ——
// 那会把第二组绑到第一组的渠道上（同一条渠道挂两套价，谁都不知道流量走了哪）。
func siblingGroupCount(g presetGroup) int {
	base := channelsvc.NormalizeBaseURL(g.Channel.BaseURL)
	n := 0
	for _, other := range presetGroups() {
		if other.Channel.AdapterID != g.Channel.AdapterID {
			continue
		}
		if channelsvc.NormalizeBaseURL(other.Channel.BaseURL) == base {
			n++
		}
	}
	return n
}

// validate 是纯函数校验（不连库）：拼错的 vendor / 计价方式 / 空端点必须在写库前报出来。
func (g presetGroup) validate() error {
	if !strings.HasPrefix(g.MarkerKey, "builtin_presets.") {
		return fmt.Errorf("marker key 必须以 builtin_presets. 开头：%q", g.MarkerKey)
	}
	if g.Channel.AdapterID == "" || g.Channel.Name == "" || g.Channel.BaseURL == "" {
		return errors.New("渠道的 adapter/name/base_url 必填")
	}
	if g.Channel.Website != "" {
		u, err := url.Parse(g.Channel.Website)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("官网页不是合法 http(s) 地址：%q", g.Channel.Website)
		}
	}
	vendors := map[string]bool{}
	if g.Vendor.ID != "" {
		vendors[g.Vendor.ID] = true
	}
	for _, v := range g.Vendors {
		if v.ID != "" {
			vendors[v.ID] = true
		}
	}
	seen := map[string]bool{}
	for _, m := range g.Models {
		if m.ID == "" || seen[m.ID] {
			return fmt.Errorf("模型 ID 为空或重复：%q", m.ID)
		}
		seen[m.ID] = true
		if m.Vendor != "" && !vendors[m.Vendor] {
			return fmt.Errorf("模型 %s 引用了未声明的 vendor %q", m.ID, m.Vendor)
		}
		if len(m.Endpoints) == 0 {
			return fmt.Errorf("模型 %s 缺端点", m.ID)
		}
		mode := m.BillingMode
		if mode == "" {
			mode = model.BillingModeSpec
		}
		if mode != model.BillingModeSpec && mode != model.BillingModeTokens && mode != model.BillingModeCall {
			return fmt.Errorf("模型 %s 的计费方式不合法：%q", m.ID, mode)
		}
		if m.BindingWeight < 0 || m.BindingPriority < 0 {
			return fmt.Errorf("模型 %s 的绑定权重/优先级不能为负", m.ID)
		}
	}
	return nil
}

// upsertPresetVendor 解析本组应使用的供应商 ID（返回给模型行用）。
//
// 三段：按 ID → 按名字 → 新建。
//
// 为什么不能只按 ID 查再插入：`vendors` 有 `uq_vendors_name`，运营若先手工建过
// "多米API"（ID 不同），插入会撞唯一约束 —— 那会让整个 Seed 失败、服务起不来。
// 而若直接跳过不插入，模型行的 vendor 就指向一个不存在的行：门户按 `vendors` 表投影
// 分组（pricing 的 vendorByID 取不到时名称为空），表现是"模型在，但不属于任何供应商"。
func upsertPresetVendor(tx *gorm.DB, v model.Vendor) (string, error) {
	var row model.Vendor
	err := tx.Where("id = ?", v.ID).First(&row).Error
	if err == nil {
		return row.ID, nil // 已有同 ID：不覆盖运营改过的名称/图标/颜色
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("db: 读取内置预设供应商失败: %w", err)
	}

	err = tx.Where("name = ?", v.Name).First(&row).Error
	if err == nil {
		logger.L().Warn("内置预设复用了同名供应商（不修改其任何字段）",
			zap.String("want_id", v.ID), zap.String("got_id", row.ID), zap.String("name", row.Name))
		return row.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("db: 按名称查找内置预设供应商失败: %w", err)
	}

	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&v).Error; err != nil {
		return "", fmt.Errorf("db: 内置预设供应商写入失败: %w", err)
	}
	return v.ID, nil
}

// upsertPresetVendors 写入本组声明的全部供应商，返回「声明 ID → 实际 ID」。
//
// 实际 ID 可能与声明不同（运营先手工建了同名、ID 不同的行）：模型行的 vendor
// 必须指向实际 ID，否则门户按 vendors 投影时名称为空。
func upsertPresetVendors(tx *gorm.DB, g presetGroup) (map[string]string, error) {
	out := make(map[string]string, len(g.Vendors)+1)
	all := make([]model.Vendor, 0, len(g.Vendors)+1)
	if g.Vendor.ID != "" {
		all = append(all, g.Vendor)
	}
	all = append(all, g.Vendors...)
	for _, v := range all {
		if v.ID == "" {
			continue
		}
		id, err := upsertPresetVendor(tx, v)
		if err != nil {
			return nil, err
		}
		out[v.ID] = id
	}
	return out, nil
}

// createPresetModels 写入本组**尚不存在**的模型行（含绑定），并标记为内置。
//
// 三条规则：
//  1. 已存在的同 id 模型属于「运营自建」：不覆盖、不绑到内置渠道、不改启停；
//  2. 软删墓碑（主键仍被占）也视为"运营删过"：跳过，避免 Create 撞主键让启动失败；
//  3. 写入走 modelcfg.CreateTx（唯一写入口）：价目/规格校验、is_free 派生、
//     vendor 存在性、map 落库（不丢 enabled=false）都在那里，不在这里复刻。
func createPresetModels(tx *gorm.DB, g presetGroup, ch *model.Channel, vendorIDs map[string]string) ([]model.Model, error) {
	allIDs := make([]string, 0, len(g.Models))
	for _, m := range g.Models {
		allIDs = append(allIDs, m.ID)
	}
	var existing []struct {
		ID          string
		PresetGroup string
		Deleted     bool
	}
	if len(allIDs) > 0 {
		// Unscoped：软删行仍占主键。命中即跳过（删除是明确意图，复活只能走恢复出厂）。
		if err := tx.Unscoped().Model(&model.Model{}).
			Select("id, preset_group, deleted_at IS NOT NULL AS deleted").
			Where("id IN ?", allIDs).Scan(&existing).Error; err != nil {
			return nil, fmt.Errorf("db: 读取既有内置同名模型失败: %w", err)
		}
	}
	// 三态：运营自建（preset_group='') → 不碰；其他预设组 → 只补本渠道的停用绑定；
	// 软删墓碑 → 视为运营删过，跳过。
	operatorOwned := map[string]bool{}
	presetOwned := map[string]bool{}
	for _, e := range existing {
		switch {
		case e.Deleted:
			// 跳过
		case e.PresetGroup == "":
			operatorOwned[e.ID] = true
		default:
			presetOwned[e.ID] = true
		}
	}

	created := make([]model.Model, 0, len(g.Models))
	for _, m := range g.Models {
		if operatorOwned[m.ID] {
			continue
		}
		enabled := false
		bw, bp := m.BindingWeight, m.BindingPriority
		if bw <= 0 {
			bw = 1
		}
		if bp <= 0 {
			bp = 1
		}
		upstream := strings.TrimSpace(m.BindingUpstream)
		if upstream == "" {
			upstream = m.ID
		}

		// 另一个预设组已经建过同 ID 模型（如 ark 与 duomi 都提供 seedance）：
		// 模型是全局的，这里只补一条本渠道的停用绑定，绝不改模型本身。
		if presetOwned[m.ID] {
			if ch != nil {
				if err := addDisabledBinding(tx, m.ID, ch.ID, upstream, bw, bp); err != nil {
					return nil, err
				}
			}
			continue
		}

		in := presetModelInput(m, vendorIDs, g.Vendor.ID, &enabled)
		// 绑定**不交给 modelcfg**：它的 NormalizeBindings 要求"至少一条启用绑定"，
		// 而出厂预设的全部绑定必须是停用的（预置 ≠ 开通）。模型本身仍走唯一写入口，
		// 绑定在这里按同一张表直接落库，并显式置停用（见下）。
		if _, err := modelcfg.CreateTx(tx, in, nil); err != nil {
			return nil, fmt.Errorf("db: 内置模型 %s 写入失败: %w", m.ID, err)
		}
		// 行标记与模型同事务写：恢复出厂按它筛行，丢了就等于该行永远不进工厂清单。
		if err := tx.Model(&model.Model{}).Where("id = ?", m.ID).
			Update("preset_group", g.RowGroup()).Error; err != nil {
			return nil, fmt.Errorf("db: 标记内置模型 %s 失败: %w", m.ID, err)
		}
		if ch != nil {
			if err := addDisabledBinding(tx, m.ID, ch.ID, upstream, bw, bp); err != nil {
				return nil, err
			}
		}
		var row model.Model
		if err := tx.Where("id = ?", m.ID).First(&row).Error; err != nil {
			return nil, fmt.Errorf("db: 回读内置模型 %s 失败: %w", m.ID, err)
		}
		created = append(created, row)
	}
	return created, nil
}

// presetModelInput 把预设声明翻成 modelcfg 的唯一写入口入参。
//
// create 与 restore 共用：两处各拼一份字段清单，迟早出现"新建的能写、恢复的漏一列"。
// enabled 为 nil = 不改变启用态（恢复出厂默认保留启停）；非 nil = 显式写入。
func presetModelInput(m presetModel, vendorIDs map[string]string, defaultVendor string, enabled *bool) modelcfg.Input {
	vendorID := m.Vendor
	if vendorID == "" {
		vendorID = defaultVendor
	}
	if actual, ok := vendorIDs[vendorID]; ok {
		vendorID = actual
	}
	mode := m.BillingMode
	if mode == "" {
		mode = model.BillingModeSpec
	}
	display, desc, icon := m.DisplayName, m.Description, m.Icon
	cw, mo := m.ContextWindow, m.MaxOutputTokens
	return modelcfg.Input{
		ID: m.ID, DisplayName: &display, Description: &desc, Icon: &icon,
		Vendor: vendorID, Modality: m.Modality,
		BillingMode: mode, BillingModeSet: true, Enabled: enabled,
		Endpoints: m.Endpoints, EndpointsSet: true,
		Capabilities: apidoc.CapKeys(m.Caps), CapabilitiesSet: true,
		Tags: m.Tags, TagsSet: true,
		Prices: m.Prices,
		// 规格价目只在 spec 计价时整块写入；tokens/call 写零值会被当成"清空规格"。
		SpecPrices: m.Spec, SpecPricesSet: mode == model.BillingModeSpec,
		ContextWindow: &cw, MaxOutputTokens: &mo,
	}
}

// addDisabledBinding 插入一条**停用**绑定；已存在则原样保留。
//
// 为什么只在"本次真的插入"时补 UPDATE：GORM 对 `default:true` 的 enabled 会把
// 结构体里的 false 写成 true；但无条件 UPDATE 会把运营在复用渠道上已经启用的绑定关掉
// —— 出厂预设只该新增停用绑定，不该改运营的启停决定。
func addDisabledBinding(tx *gorm.DB, modelID string, channelID uint64, upstream string, weight, priority int) error {
	b := model.ModelBinding{
		ModelID: modelID, ChannelID: channelID, UpstreamModel: upstream,
		Weight: weight, Priority: priority, Enabled: false,
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&b)
	if res.Error != nil {
		return fmt.Errorf("db: 内置绑定 %s 写入失败: %w", modelID, res.Error)
	}
	if res.RowsAffected == 0 {
		return nil
	}
	if err := tx.Model(&model.ModelBinding{}).
		Where("model_id = ? AND channel_id = ?", modelID, channelID).
		Update("enabled", false).Error; err != nil {
		return fmt.Errorf("db: 停用内置绑定 %s 失败: %w", modelID, err)
	}
	return nil
}

// channelReqOf 把预设声明翻成渠道写入口入参（create 与 restore 共用）。
func channelReqOf(c presetChannel) channelsvc.Req {
	website, icon := c.Website, c.Icon
	req := channelsvc.Req{
		Name: c.Name, AdapterID: c.AdapterID, BaseURL: c.BaseURL,
		Website: &website, MultiKeyMode: channelsvc.ModePolling,
		Groups: []string{"default"}, Weight: c.Weight, Priority: c.Priority,
		RPMLimit: c.RPMLimit, MaxConcurrency: c.MaxConcurrency,
		AutoBan:        true,
		AdapterOptions: c.AdapterOptions,
	}
	if icon != "" {
		req.Icon = &icon
	}
	return req
}

// resolveGroupChannel 找到本组应绑定的渠道。返回 (nil,false,nil) = 本次跳过（不建绑定）。
//
// 八态匹配（按顺序短路；候选只含 preset_group 为空的活渠道）：
//  1. 本组已认领（preset_group 命中）→ 复用；
//  2. adapter+归一化 base+名称 全等 → 复用；同 base 有兄弟组也安全（名称是精确的）；
//  3. 同 base 唯一、名称不同、该 base 只有本组一个种子 → 视为运营改名，复用；
//  4. 同 base 命中多条，或该 base 有兄弟组 → 跳过（不猜）；
//  5. 同名但 adapter/base 不同 → 跳过（绑上去就是必然 404）；
//  6. 仅软删行命中 → 不复用（删除是明确意图），按新建处理（部分唯一索引允许同名）；
//  7. 全不命中 → 新建（空密钥 + 停用），并写行标记。
//
// 任何一条复用/跳过路径都**不修改**运营渠道的名称/BaseURL/密钥/状态/并发/图标；
// 认领只补 preset_group，由 seedGroup 负责。
func resolveGroupChannel(tx *gorm.DB, g presetGroup, siblings int) (*model.Channel, bool, error) {
	c := g.Channel

	// 1. 本组已认领的渠道（上一轮写过标记，或标记行丢了但行还在）。
	var own model.Channel
	err := tx.Where("preset_group = ?", g.RowGroup()).Order("id").First(&own).Error
	if err == nil {
		return &own, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("db: 查询已认领渠道失败: %w", err)
	}

	// 2. 同 adapter 的运营行（preset_group=''）；再按归一化 base 过滤。
	var rows []model.Channel
	if err := tx.Where("adapter_id = ? AND preset_group = ?", c.AdapterID, "").
		Order("id").Find(&rows).Error; err != nil {
		return nil, false, fmt.Errorf("db: 查询既有预设渠道失败: %w", err)
	}
	base := channelsvc.NormalizeBaseURL(c.BaseURL)
	sameBase := make([]model.Channel, 0, len(rows))
	for i := range rows {
		if base != "" && channelsvc.NormalizeBaseURL(rows[i].BaseURL) == base {
			sameBase = append(sameBase, rows[i])
		}
	}

	// 2a. 名称精确匹配：同 base 的兄弟组也能安全复用（Metaso 标准/优先）。
	for i := range sameBase {
		if sameBase[i].Name == c.Name {
			return &sameBase[i], true, nil
		}
	}
	// 2b. 唯一候选 + 没有兄弟组 → 视为改名。兄弟组存在时不能猜：
	//     同一 adapter+base 的多条渠道各自对应不同价目，复用会把两套配置混在一条线上。
	if len(sameBase) == 1 && siblings <= 1 {
		logger.L().Warn("内置预设复用疑似改名的渠道（不修改其任何配置）",
			zap.String("group", g.MarkerKey),
			zap.Uint64("channel_id", sameBase[0].ID),
			zap.String("want_name", c.Name),
			zap.String("got_name", sameBase[0].Name))
		return &sameBase[0], true, nil
	}
	if len(sameBase) > 0 {
		logger.L().Warn("内置预设跳过渠道匹配：同上游存在多条候选或兄弟组",
			zap.String("group", g.MarkerKey),
			zap.String("adapter", c.AdapterID),
			zap.String("base", c.BaseURL),
			zap.Int("candidates", len(sameBase)),
			zap.Int("siblings", siblings))
		return nil, false, nil
	}

	// 3. 名称已被别的 adapter/base 占用 → 只写模型行、不建绑定。
	var byName model.Channel
	err = tx.Where("name = ?", c.Name).Order("id").First(&byName).Error
	if err == nil {
		logger.L().Warn("内置预设渠道名已被占用（适配器/上游不同），本次只写模型行、不建绑定",
			zap.String("group", g.MarkerKey),
			zap.String("name", c.Name),
			zap.String("want_adapter", c.AdapterID),
			zap.String("got_adapter", byName.AdapterID),
			zap.String("got_base", byName.BaseURL))
		return nil, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("db: 查询同名渠道失败: %w", err)
	}

	// 4. 新建：走 CreateFactoryTx（校验复用 + 空密钥 + 停用）。
	item, err := channelsvc.CreateFactoryTx(tx, channelReqOf(c))
	if err != nil {
		return nil, false, fmt.Errorf("db: 内置预设渠道写入失败: %w", err)
	}
	var created model.Channel
	if err := tx.Where("id = ?", item.ID).First(&created).Error; err != nil {
		return nil, false, fmt.Errorf("db: 回读内置预设渠道失败: %w", err)
	}
	if err := tx.Model(&model.Channel{}).Where("id = ?", created.ID).
		Update("preset_group", g.RowGroup()).Error; err != nil {
		return nil, false, fmt.Errorf("db: 标记内置渠道失败: %w", err)
	}
	return &created, false, nil
}

// forceDisabled 把刚插入的行**在插入之后**显式置为停用。
//
// 为什么不靠结构体字段表达：GORM 构造 INSERT 值时会（`callbacks/create.go`
// `ConvertToCreateValues`）把「零值 + 带 default 标签」的列**替换成列默认值**
// （`isZero && field.DefaultValueInterface != nil` → 用默认值）。
// `Select("*")` 只决定"列是否进入 INSERT 语句"，拦不住这层替换 ——
// 写 `Enabled: false` 落库就是 `true`，而错误永远是静默的。
// 本仓库已有两处因此绕开结构体插入（`modelcfg/write.go` 用 map、
// `relay/select_candidates_test.go` 用 map），这里没有走 map 是因为
// models 一行 20+ 列、列清单手写必然漏一列；「插入后置位」是三处里最少代码的一种。
//
// 调用方必须把作用域限定到**本次插入的行**：按表全量刷会把运营自己的行一起停掉。
func forceDisabled(tx *gorm.DB, table, cond string, args ...any) error {
	if err := tx.Table(table).Where(cond, args...).
		Update("enabled", false).Error; err != nil {
		return fmt.Errorf("db: 内置预设 %s 置停用失败: %w", table, err)
	}
	return nil
}

// ---------------------------------------------------------------- 火山方舟

// arkPresetMarkerKey 是火山方舟组的幂等标记。
const arkPresetMarkerKey = "builtin_presets.volcengine_ark"

// arkPresetVersion 是预设内容版本。改价目/能力/模型清单时**必须 +1**，
// 否则已写过的实例永远不会拿到新结构（标记命中即跳过）。
const arkPresetVersion = 2

// arkChannelSeed 是内置渠道的固定事实。
//
// BaseURL 必须写到 `/api/v3` 为止：Ark 数据面的所有路径都挂在 `/api/v3` 下
// （`/api/v3/images/generations`），少写一段会让所有请求 404，而 404 在平台侧
// 表现为"渠道可用但模型全挂"，很难看出来。
const (
	arkAdapterID   = "volcengine-ark"
	arkChannelName = "火山方舟"
	arkChannelBase = "https://ark.cn-beijing.volces.com/api/v3"
	// arkVideoDefaultTier 是 Ark 视频模型的默认分辨率档（官方默认 720p）。
	//
	// 枚举值写成小写的 720p：价目表的档位键就是这一串（归一化会把 720P 折成 720p），
	// 写成 "720P" 会在 ValidateSpecPrices 里被判"档位不受支持"。
	arkVideoDefaultTier = "720p"
	// arkImageDefaultTier 是 Ark 图像模型的默认尺寸档（不传 size 时上游按 2K 出图）。
	//
	// 三个内置图像模型的档位集合不同（pro/flash 是 1K/2K，lite 是 2K/3K/4K），
	// 但**默认档都写 2K** —— 它直接决定"没传 size"的请求按哪一档计费，
	// 与档位单价无关（不再有"兜底价必须等于某档价"这种价格当选择器的耦合）。
	arkImageDefaultTier = "2K"
)

// arkImageEndpoints / arkVideoEndpoints 是这两个模态在本平台实际承载的端点。
//
// 只声明一个：Seedream 的图生图与图层拆分都走同一个 `images/generations`（用参数区分），
// 平台没有 `images/edits` 的 multipart 入口可用（Ark 数据面只收 JSON）。
var (
	arkImageEndpoints = []string{"images/generations"}
	arkVideoEndpoints = []string{"video/generations"}
)

// arkPreset 返回火山方舟预设组。
func arkPreset() presetGroup {
	return presetGroup{
		MarkerKey: arkPresetMarkerKey,
		Version:   arkPresetVersion,
		// 供应商：复用「字节豆包」而不是新建「火山引擎」。
		//
		// 为什么：模型广场是按 vendor 分组展示的，同一家公司在同一个列表里出现两栏
		// （「字节豆包」下已有 Seedream 4.0）会让用户以为这是两家不同的供应商。
		Vendor: model.Vendor{
			ID: "bytedance", Name: "字节豆包", Color: "#325AB4", Icon: "builtin:bytedance-doubao",
			Builtin: true, Sort: 6,
		},
		Channel: presetChannel{
			AdapterID: arkAdapterID, Name: arkChannelName, BaseURL: arkChannelBase,
			Website: "https://www.volcengine.com/product/ark",
			Icon:    "builtin:volcengine",
			// 并发 50（P1-6）：图片流式请求在整个响应期间占用 1 个并发额度
			// （dispatch 只在流结束时统一释放），而一次 4K 组图流式可能跑几分钟。
			// 20 槽在高峰期会被几路流式吃光，之后连 2 秒的同步图都拿不到槽，
			// 终局报 429「渠道限流」——用户完全看不出是被自己的长流挤掉的。
			//
			// 注意：这是**单进程**上限。多副本部署时实际并发 = 50 × 副本数，
			// 管理台渠道卡片展示的「本机在飞」也只代表本机。
			MaxConcurrency: 50, RPMLimit: 300, Weight: 10, Priority: 1,
		},
		Models: arkModelSeeds(),
	}
}

// arkModelSeeds 是 7 个内置模型（顺序即管理台列表顺序：先图后视频、由强到弱）。
//
// 能力声明的依据是官方文档的「模型支持」清单，逐条可查：
//   - Seedream 5.0 pro / flash：支持图层拆分（layer_decomposition），**不支持**流式与组图；
//   - Seedream 5.0（lite）：支持流式（stream）与联网搜索（tools:web_search），档位上探到 4K；
//   - Seedance 2.5 / 2.0 系列：支持首尾帧、参考图/视频/音频、有声视频与原生 content[]。
func arkModelSeeds() []presetModel {
	// defTier 是**默认档键**（不传 size / auto 时按它计费），不是金额：
	// 金额只在 tier_price 里，默认档只回答"按哪一档"，两者不会再因为"价格恰好相等"耦合。
	img := func(id, name string, caps []apidoc.Cap, tiers map[string]int64, defTier string, maxN int64) presetModel {
		return presetModel{
			ID: id, DisplayName: name, Modality: "image", Endpoints: arkImageEndpoints,
			Caps: caps,
			Spec: model.SpecPrices{
				Kind: "per_image", TierPrice: tiers, ResolutionFixed: defTier, MaxN: ptr(maxN),
			},
		}
	}
	// 视频：秒价 + 时长上限 + 智能时长开关 + 固定档位。
	//
	// `AllowAutoDuration` 必须显式为 true 的模型才允许 `seconds:-1`：
	// 其它厂商上游会直接拒 -1，那会被记成渠道故障 → 误熔断健康渠道。
	//
	// `ResolutionFixed` 必须是 720p：Ark 各模型的 `resolution` 默认值就是 720p，
	// 而平台的 `resolveVideoTier` 在「请求没给档位且模型没配固定档」时**直接 400**。
	// 不配的表现是"照官方文档不传分辨率就提交，平台报'必须指定 resolution'"——
	// 用户看不出这是平台自己的缺口。配上之后出站体会显式带 `resolution:720p`，
	// 与上游默认值等价，计价也不随上游默认值变化而漂移。
	vid := func(id, name string, tiers map[string]int64, maxSeconds int64) presetModel {
		yes := true
		return presetModel{
			ID: id, DisplayName: name, Modality: "video", Endpoints: arkVideoEndpoints,
			Caps: []apidoc.Cap{
				apidoc.CapT2V, apidoc.CapI2V, apidoc.CapFirstLast,
				apidoc.CapRefAudio, apidoc.CapRefVideo, apidoc.CapVideoAudio, apidoc.CapVideoNative,
			},
			Spec: model.SpecPrices{
				Kind: "per_video", SecondPrice: tiers,
				MaxSeconds: ptr(maxSeconds), AllowAutoDuration: &yes,
				ResolutionFixed: arkVideoDefaultTier,
			},
		}
	}
	return []presetModel{
		// —— 图像 ——
		//
		// 图层拆分的 max_n 必须是 17（1 张底图 + 最多 16 个图层，见
		// billing.ImageLayerDecompMax）：冻结侧按 17 张预估，收费上限必须容得下它，
		// 否则冻结阶段直接 ErrInvalidTier，图层拆分一次也跑不起来。
		img("doubao-seedream-5-0-pro-260628", "Seedream 5.0 pro", []apidoc.Cap{
			apidoc.CapT2I, apidoc.CapI2I, apidoc.CapImageLayer, apidoc.CapImageNative,
		}, map[string]int64{
			// 1.5K 与 1K 同价（官方口径），是 Seedream 5.0 pro 的正式档位。
			"1K": money.MustParse("0.30"), "1.5K": money.MustParse("0.30"), "2K": money.MustParse("0.45"),
		}, "2K", billing.ImageLayerDecompMax),
		img("doubao-seedream-5-0-flash-260915", "Seedream 5.0 flash", []apidoc.Cap{
			apidoc.CapT2I, apidoc.CapI2I, apidoc.CapImageLayer, apidoc.CapImageNative,
		}, map[string]int64{
			"1K": money.MustParse("0.20"), "1.5K": money.MustParse("0.20"), "2K": money.MustParse("0.30"),
		}, "2K", billing.ImageLayerDecompMax),
		// lite 的档位是 2K/3K/4K，默认档写 2K：写 4K 会让"没传 size"的普通请求
		// 按 4K 收费（永远多收一倍）。上游默认也是 2K，账与交付才对得上。
		img("doubao-seedream-5-0-260128", "Seedream 5.0", []apidoc.Cap{
			apidoc.CapT2I, apidoc.CapI2I, apidoc.CapImageStream, apidoc.CapImageSearch, apidoc.CapImageNative,
		}, map[string]int64{
			"2K": money.MustParse("0.30"), "3K": money.MustParse("0.45"), "4K": money.MustParse("0.60"),
		}, "2K", 15),
		// —— 视频 ——
		vid("doubao-seedance-2-5-260628", "Seedance 2.5", map[string]int64{
			"480p": money.MustParse("0.30"), "720p": money.MustParse("0.60"), "1080p": money.MustParse("1.20"),
		}, 30),
		vid("doubao-seedance-2-0-260128", "Seedance 2.0", map[string]int64{
			"480p": money.MustParse("0.20"), "720p": money.MustParse("0.40"),
			"1080p": money.MustParse("0.80"), "4k": money.MustParse("1.60"),
		}, 15),
		vid("doubao-seedance-2-0-fast-260128", "Seedance 2.0 fast", map[string]int64{
			"480p": money.MustParse("0.10"), "720p": money.MustParse("0.25"),
		}, 15),
		vid("doubao-seedance-2-0-mini-260615", "Seedance 2.0 mini", map[string]int64{
			"480p": money.MustParse("0.08"), "720p": money.MustParse("0.18"),
		}, 15),
	}
}

// ---------------------------------------------------------------- 导出

// MediaModelIDs 返回全部内置媒体模型的 id（副本）。
//
// 导出给「说明页文案对账」用：出厂说明页里的示例请求直接写了模型 id，
// 拼错一个字符的表现是用户复制后必然 400，而没有任何测试会失败。
// 让文案反过来对账这份清单，模型改名/加模型时文案漏改会被测试抓住。
//
// 返回副本而不是内部切片：调用方 append 一次就会改写下一轮 Seed 的数据。
func MediaModelIDs() []string {
	var out []string
	for _, g := range presetGroups() {
		for _, m := range g.Models {
			out = append(out, m.ID)
		}
	}
	return out
}

// ptr 返回值的指针（价目里 max_n / max_seconds / allow_auto_duration 都是可选字段）。
func ptr[T any](v T) *T { return &v }
