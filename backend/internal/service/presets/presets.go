// Package presets 写入**内置厂商预设**（渠道 / 模型 / 绑定），当前只有一组：
// 火山方舟（ark）。
//
// 为什么只内置**官方厂商**：预设会写入 `vendors` / `channels` 并在门户按供应商分组展示，
// 等于把"上游是哪一家"公开给终端用户。第三方中转平台（多米API）因此只做**适配器档案**
// （协议识别、路径、族表），不建预设组 —— 模型与渠道由运营在管理台自建，
// 供应商展示成什么是运营的决定，不是代码的默认值。
//
// 为什么单独一个包而不是塞进 internal/db：
// 预设数据要引用 `apidoc` 的能力词表（唯一真相源），而 `apidoc` 反过来 import 了
// `billing`，`billing` 的集成测试又 import 了 `db` —— 一旦 `db` import `apidoc`
// 就形成**测试期**导入环（`go build` 不报，`go test`/`go vet` 才炸）。
// 把这份数据放在 `db` 之上的一层（只被 main 调用）即可彻底断开。
package presets

import (
	"errors"
	"fmt"
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
)

// 本文件是**内置预设层**：把厂商的模型预置成「停用」状态。
//
// 三条语义，缺一不可：
//
//  1. **默认不启用**（不是"默认可用"）：模型 `enabled=false`、渠道 `status=0`（停用）、
//     绑定 `enabled=false`。运营必须先填自己的 API KEY 再启用 —— 内置的密钥是假的，
//     启用一个假密钥渠道等于给用户一个必然失败的模型。
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
// 这一层与演示层（`LD_SEED_ENABLED`）互不依赖：关掉演示数据不该丢掉可用模型清单，
// 反之亦然。开关是 `LD_BUILTIN_PRESETS`。

// presetGroup 是一个「内置预设组」的完整声明（一组 = 一组厂商事实 + 一批模型）。
//
// 为什么抽象而不是给每组复制一份 writer：写库逻辑（建模型行 + 置停用 + 建渠道 +
// 建绑定 + 写标记）逐字相同，复制一份等于把上面三条语义各复制一遍 ——
// 漏一份都不会报错，只会静默少一条语义（例如某组忘了 forceDisabled，模型出厂即上线）。
type presetGroup struct {
	// MarkerKey 是幂等标记（system_config 主键）。每组独立，理由见包注释第 2 条。
	MarkerKey string
	// Version 是预设内容版本。改价目/能力/模型清单时**必须 +1**，
	// 语义：标记命中即整体跳过。
	Version int
	// Vendor 是本组的供应商行（模型归组展示用）。
	Vendor model.Vendor
	// Channel 是本组要绑定/新建的渠道事实。
	Channel presetChannel
	Models  []presetModel
}

// presetChannel 是内置渠道的固定事实（不含密钥：内置渠道永远是空密钥）。
type presetChannel struct {
	AdapterID      string
	Name           string
	BaseURL        string
	Icon           string
	MaxConcurrency int
	RPMLimit       int
	Weight         int
	Priority       int
}

// presetModel 是一个内置模型的完整声明。
//
// 用结构体而不是直接拼 model.Model：同组模型的差异只在 5 个维度
// （id / 名称 / 描述 / 能力 / 价目），逐条手写必然漏一处能力或档位，
// 而漏了不会报错 —— 表现是"文档里少一个 TAB"或"某个档位点提交就 400"。
type presetModel struct {
	ID          string
	DisplayName string
	// Description 是模型描述（门户可见）。它补的是"接口文档不能写具体模型名"的空位：
	// `apidoc.TestOnlyModelsGeneric` 禁止在接口文档里写模型名，所以「这个模型只能这样调」
	// 的事实只能落在模型描述与接入手册里。
	Description string
	Modality    string
	// Endpoints 是该模型在本平台实际承载的端点（如 video/generations）。
	Endpoints []string
	Caps      []apidoc.Cap
	Spec      model.SpecPrices
}

// presetGroups 是全部内置预设组（顺序即写入顺序，互不依赖）。
func presetGroups() []presetGroup { return []presetGroup{arkPreset()} }

// Seed 写入内置厂商预设。
//
// 与 `db.Seed` 分开调用而不是塞进它：`db.Seed` 的第二层受 `LD_SEED_ENABLED` 控制，
// 而"有没有这些模型"不该由"要不要演示数据"决定（开关是 `LD_BUILTIN_PRESETS`）。
func Seed(gdb *gorm.DB) error {
	if !conf.EnvConfig().BuiltinPresets {
		return nil
	}
	// 逐组独立事务：一组写坏不该把另一组也已写好的内容回滚掉（两组互不依赖）。
	// 但错误必须全部带出去 —— 预设写不进去时服务要停下，否则运营看到的是一份缺模型的列表。
	var errs []error
	for _, g := range presetGroups() {
		if err := seedGroup(gdb, g); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.MarkerKey, err))
		}
	}
	return errors.Join(errs...)
}

// seedGroup 写入一组预设（一次）。
func seedGroup(gdb *gorm.DB, g presetGroup) error {
	var row model.SystemConfig
	err := gdb.Where("key = ?", g.MarkerKey).First(&row).Error
	switch {
	case err == nil:
		return nil // 写过：不再触碰（删掉的模型不复活）
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return fmt.Errorf("db: 读取内置预设标记(%s)失败: %w", g.MarkerKey, err)
	}

	return gdb.Transaction(func(tx *gorm.DB) error {
		// ① **先抢标记**：多副本同时启动时只有一行能插入成功。
		//
		// 与业务行在同一事务里，后续任一步失败会一起回滚 —— 不会留下"标记在、行不在"
		// 的半成品。旧实现最后写标记，多副本会撞 uq_channels_name 直接让启动失败。
		marker := model.SystemConfig{
			Key: g.MarkerKey,
			Value: model.MustJSON(map[string]any{
				"version": g.Version, "at": time.Now().UTC().Format(time.RFC3339),
			}),
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker)
		if res.Error != nil {
			return fmt.Errorf("db: 内置预设标记写入失败(%s): %w", g.MarkerKey, res.Error)
		}
		if res.RowsAffected == 0 {
			return nil // 另一个副本先写了
		}

		// ② 供应商：先按 ID、再按名字（复用运营已建的同一家）、最后才新建。
		vendorID, err := upsertPresetVendor(tx, g.Vendor)
		if err != nil {
			return err
		}

		// ③ 模型行（跳过运营已建的同 ID 模型）
		rows, err := createPresetModels(tx, g, vendorID)
		if err != nil {
			return err
		}

		// ④ 渠道（含「已有同适配器渠道则复用、同名异构则不碰」的规则）
		ch, err := resolveGroupChannel(tx, g.Channel)
		if err != nil {
			return err
		}

		// ⑤ 绑定：只有「本次新建的模型」×「可绑定的渠道」才建，且一律停用。
		if ch != nil && len(rows) > 0 {
			ids := make([]string, 0, len(rows))
			bindings := make([]model.ModelBinding, 0, len(rows))
			for i := range rows {
				ids = append(ids, rows[i].ID)
				bindings = append(bindings, model.ModelBinding{
					ModelID: rows[i].ID, ChannelID: ch.ID,
					Weight: 10, Priority: 1, Enabled: false,
				})
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&bindings).Error; err != nil {
				return fmt.Errorf("db: 内置预设绑定写入失败(%s): %w", g.MarkerKey, err)
			}
			// 作用域必须限定到**本次插入的模型**：渠道可能是运营已在跑的存量渠道，
			// 按 channel_id 全量置停用会把他们的在跑绑定一起关掉（现象是"某天开始模型全调不通"）。
			if err := forceDisabled(tx, "model_bindings", "channel_id = ? AND model_id IN ?", ch.ID, ids); err != nil {
				return err
			}
		}

		logger.L().Warn("已写入内置预设（全部停用）",
			zap.String("group", g.MarkerKey),
			zap.String("adapter", g.Channel.AdapterID),
			zap.Int("models", len(rows)),
			zap.String("channel", g.Channel.BaseURL),
			zap.String("note", "价目为占位值：启用前请在管理台逐档核定"))
		return nil
	})
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

// createPresetModels 写入本组**尚不存在**的模型行，并把它们显式置为停用。
//
// 已存在的同 id 模型属于「运营自建」：不覆盖它的能力/价目，不改它的启用态，
// 更不给它绑到内置渠道上（否则等于把运营在跑流量的模型悄悄挪到一条空密钥渠道）。
func createPresetModels(tx *gorm.DB, g presetGroup, vendorID string) ([]model.Model, error) {
	allIDs := make([]string, 0, len(g.Models))
	for _, m := range g.Models {
		allIDs = append(allIDs, m.ID)
	}
	var taken []string
	if len(allIDs) > 0 {
		if err := tx.Model(&model.Model{}).Where("id IN ?", allIDs).Pluck("id", &taken).Error; err != nil {
			return nil, fmt.Errorf("db: 读取既有内置同名模型失败: %w", err)
		}
	}
	takenSet := make(map[string]bool, len(taken))
	for _, id := range taken {
		takenSet[id] = true
	}

	rows := make([]model.Model, 0, len(g.Models))
	for _, m := range g.Models {
		if takenSet[m.ID] {
			continue
		}
		rows = append(rows, model.Model{
			ID: m.ID, DisplayName: m.DisplayName, Description: m.Description,
			Vendor: vendorID, Modality: m.Modality,
			BillingMode: model.BillingModeSpec,
			// Enabled=false 是这一层的核心：预置 ≠ 开通。
			// 注意它**不会**靠这一次 Create 落库，见下面 forceDisabled 的说明。
			Enabled:      false,
			Endpoints:    model.MustJSON(m.Endpoints),
			Capabilities: model.MustJSON(apidoc.CapKeys(m.Caps)),
			SpecPrices:   m.Spec,
		})
	}
	for i := range rows {
		rows[i].IsFree = rows[i].PricingIsFree()
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if err := tx.Create(&rows).Error; err != nil {
		return nil, fmt.Errorf("db: 内置预设模型写入失败: %w", err)
	}
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	if err := forceDisabled(tx, "models", "id IN ?", ids); err != nil {
		return nil, err
	}
	return rows, nil
}

// resolveGroupChannel 找到本组应绑定的渠道；返回 nil 表示"本次不建绑定"。
//
// 三条规则，任何一条都**不修改运营已有渠道的字段**（名称/BaseURL/密钥/状态/并发/图标）：
//  1. `adapter_id` 命中 → **复用**（只往它身上加停用绑定）。这是唯一不会丢功能的路径：
//     重复建一条同名渠道会撞 `uq_channels_name`，直接让服务起不来。
//  2. `name` 命中但 adapter 不同 → **不碰**，只写模型行、不建绑定。
//     把模型绑到别的适配器上，一启用就是"能调但上游 404"，比没有绑定更难查。
//  3. 都没有 → 新建一条**停用 + 空密钥**的渠道（与原口径一致）。
func resolveGroupChannel(tx *gorm.DB, c presetChannel) (*model.Channel, error) {
	var ch model.Channel
	err := tx.Where("adapter_id = ?", c.AdapterID).Order("id").First(&ch).Error
	if err == nil {
		logger.L().Warn("内置预设复用了已有渠道（不修改其任何配置）",
			zap.String("adapter", c.AdapterID), zap.Uint64("channel_id", ch.ID), zap.String("name", ch.Name))
		return &ch, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("db: 查询既有预设渠道失败: %w", err)
	}

	var byName model.Channel
	err = tx.Where("name = ?", c.Name).First(&byName).Error
	if err == nil {
		logger.L().Warn("内置预设渠道名已被占用（适配器不同），本次只写模型行、不建绑定",
			zap.String("name", c.Name), zap.String("want_adapter", c.AdapterID),
			zap.String("got_adapter", byName.AdapterID))
		return nil, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("db: 查询同名渠道失败: %w", err)
	}

	newCh := model.Channel{
		Name: c.Name, AdapterID: c.AdapterID, BaseURL: c.BaseURL, Icon: c.Icon,
		// Keys 写空数组而不是假密钥：假密钥会让「探活」把渠道判成"密钥无效"，
		// 运营看到的是一条故障记录，而真实原因是他还没填自己的 KEY。
		Keys: model.MustJSON([]string{}), MultiKeyMode: "polling",
		Groups: model.MustJSON([]string{"default"}),
		Weight: c.Weight, Priority: c.Priority,
		MaxConcurrency: c.MaxConcurrency, RPMLimit: c.RPMLimit,
		// Status 必须用 `ChannelStatusDisabled`（2，人工禁用）而不是 0：
		// 0 不在状态词表里，而「人工禁用」是唯一"绝不自动恢复"的状态 ——
		// 写 0 会让渠道落在三不像的位置（不进路由，也不进探活队列）。
		Status: model.ChannelStatusDisabled, AutoBan: true,
	}
	if err := tx.Create(&newCh).Error; err != nil {
		return nil, fmt.Errorf("db: 内置预设渠道写入失败: %w", err)
	}
	return &newCh, nil
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
			Icon: "builtin:volcengine",
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
