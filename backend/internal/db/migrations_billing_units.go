package db

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/money"
)

// MigrationNameBillingUnits 是「模型价目迁移到绝对金额」的数据动作标识。
//
// 为什么必须有这一条：计费整改把 models 的**记账单位**从「额度」换成了「微」
// （1 结算货币单位 = 1e6 微），同时把 `quota_type` 换成 `billing_mode`、
// 把 `spec_prices` 的键名统一去掉 `_quota` 后缀、把 9 条倍率列替换成 9 条价格列。
// 这些改动**只有结构、没有数据**时，存量模型的价目会全部读不出来：
//   - `billing_mode` 落在新列的默认值 `tokens` 上 → 图片/视频根本不读 spec_prices；
//   - `price_*` 全为 NULL → token 计费直接 ErrPriceMissing（全站 400）；
//   - `spec_prices` 的旧键被严格解码拒绝 → 整包价目归零，档位/素材全部失效。
//
// 换算基准（**只在这一处出现，折完即从系统消失**）：
//   - 旧的 1 额度 = 1/500000 美元（设计文档 F1：QuotaPerUnit=500000）；
//   - 因此 微 = 额度 ÷ 500000 × 汇率 × 1e6 = 额度 × 汇率 × 2；
//   - token 倍率：`model_ratio` 的定义是「输入价($/1M) ÷ 2」，
//     故 输入价(微/1M) = ratio × 2 × 汇率 × 1e6（例：gpt-4o ratio=1.25、汇率 7.2 →
//     $2.5/1M = ¥18/1M = 18,000,000 微，与种子数据完全一致）。
const MigrationNameBillingUnits = "m28_billing_units"

// legacyQuotaPerUSD 是旧记账单位的锚（1 美元 = 500000 额度）。
//
// 写死在这里而不是读配置：它是**历史常量**，值一旦随配置漂移就无法还原旧价目。
const legacyQuotaPerUSD = 500000

// BillingMigrationRow 是单行迁移结果（供 dry-run 报告与核对表使用）。
type BillingMigrationRow struct {
	ID       string   `json:"id"`
	Modality string   `json:"modality"`
	Mode     string   `json:"billing_mode"`
	Lanes    []string `json:"lanes"`     // 写入的 token/按次 lane
	SpecKeys []string `json:"spec_keys"` // 迁移的 spec_prices 键
	// Prices 是折算后的 lane 金额（微）；键与 lane 同名。dry-run 核对用。
	Prices map[string]int64 `json:"prices,omitempty"`
	// Spec 是折算后的规格价目（微）；仅 spec 计费有值。
	Spec     *model.SpecPrices `json:"spec,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
}

// BillingMigrationReport 是一次迁移的完整结果。
type BillingMigrationReport struct {
	Skipped  bool                  `json:"skipped"` // true = 已迁移过（data_migrations 有标记）
	DryRun   bool                  `json:"dry_run"` // true = 只算不写
	Fx       float64               `json:"cny_per_usd"`
	Rows     []BillingMigrationRow `json:"rows"`
	Warnings []string              `json:"warnings,omitempty"`
}

// MigrateBillingUnits 把存量模型价目从「额度」搬迁到「微」，幂等。
//
// 幂等策略（三重）：
//  1. data_migrations 标记（跑过就不再跑）；
//  2. 逐行判据：只有「还没迁移」的行才处理（billing_mode 仍是旧默认值
//     且 price_* 全空、或 spec_prices 里仍带 `_quota` 键）；
//  3. 已经迁过的行即使被强制重跑，也会因为判据不成立而跳过。
func MigrateBillingUnits(gdb *gorm.DB, dryRun bool) (BillingMigrationReport, error) {
	rep := BillingMigrationReport{DryRun: dryRun}
	if !gdb.Migrator().HasTable(&model.Model{}) {
		return rep, nil
	}
	// 全新库（没有 quota_type 列）= 没有需要搬迁的存量，直接返回。
	// 这一步让迁移对「新建环境 + 种子数据」完全无副作用。
	if !gdb.Migrator().HasColumn(&model.Model{}, "quota_type") {
		rep.Skipped = true
		return rep, nil
	}
	if !dryRun {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", MigrationNameBillingUnits).Count(&n).Error; err != nil {
			return rep, fmt.Errorf("db: 查询迁移标记失败: %w", err)
		}
		if n > 0 {
			rep.Skipped = true
			return rep, nil
		}
	}

	fx, err := readCNYPerUSD(gdb)
	if err != nil {
		return rep, err
	}
	rep.Fx = fx

	rows, err := loadLegacyModels(gdb)
	if err != nil {
		return rep, err
	}
	for i := range rows {
		item, changed, warn, err := migrateOneLegacyModel(gdb, rows[i], fx, dryRun)
		rep.Warnings = append(rep.Warnings, warn...)
		if err != nil {
			return rep, fmt.Errorf("db: 迁移模型 %s 失败: %w", rows[i].ID, err)
		}
		if !changed {
			continue
		}
		item.Warnings = warn
		rep.Rows = append(rep.Rows, item)
	}

	if !dryRun {
		if err := gdb.Create(&model.DataMigration{
			Name: MigrationNameBillingUnits, AppliedAt: time.Now(),
		}).Error; err != nil {
			return rep, fmt.Errorf("db: 写迁移标记失败: %w", err)
		}
	}
	return rep, nil
}

// readCNYPerUSD 读汇率（system_config 的 jsonb 数字），缺失回落 1:1。
//
// 迁移在 LoadSystemConfig 之前执行，不能走 conf.System()，只能读库。
func readCNYPerUSD(gdb *gorm.DB) (float64, error) {
	var raw string
	err := gdb.Raw(
		"SELECT value::text FROM system_config WHERE key = 'cny_per_usd' LIMIT 1",
	).Scan(&raw).Error
	if err != nil {
		return 0, fmt.Errorf("db: 读 cny_per_usd 失败: %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 1, nil
	}
	var fx float64
	if err := json.Unmarshal([]byte(raw), &fx); err != nil || fx <= 0 {
		return 1, nil
	}
	return fx, nil
}

// legacyModel 是迁移所需的旧列视图（新结构字段一并带上，用于判断"是否已迁移"）。
type legacyModel struct {
	ID                   string   `gorm:"column:id"`
	Modality             string   `gorm:"column:modality"`
	BillingMode          string   `gorm:"column:billing_mode"`
	QuotaType            int      `gorm:"column:quota_type"`
	ModelRatio           *float64 `gorm:"column:model_ratio"`
	CompletionRatio      *float64 `gorm:"column:completion_ratio"`
	CacheRatio           *float64 `gorm:"column:cache_ratio"`
	CreateCacheRatio     *float64 `gorm:"column:create_cache_ratio"`
	CreateCacheRatio1h   *float64 `gorm:"column:create_cache_ratio_1h"`
	ImageRatio           *float64 `gorm:"column:image_ratio"`
	AudioRatio           *float64 `gorm:"column:audio_ratio"`
	AudioCompletionRatio *float64 `gorm:"column:audio_completion_ratio"`
	ModelPriceQuota      *int64   `gorm:"column:model_price_quota"`
	SpecRaw              *string  `gorm:"column:spec_raw"`
	HasPrice             bool     `gorm:"column:has_price"`
}

func loadLegacyModels(gdb *gorm.DB) ([]legacyModel, error) {
	var out []legacyModel
	err := gdb.Raw(`
		SELECT id, modality, billing_mode, quota_type,
		       model_ratio, completion_ratio, cache_ratio, create_cache_ratio,
		       COALESCE(create_cache_ratio_1h, create_cache_ratio1h) AS create_cache_ratio_1h,
		       image_ratio, audio_ratio, audio_completion_ratio,
		       model_price_quota,
		       spec_prices::text AS spec_raw,
		       (price_input IS NOT NULL OR price_call IS NOT NULL) AS has_price
		FROM models
		WHERE deleted_at IS NULL
		ORDER BY modality, id`).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("db: 读存量模型失败: %w", err)
	}
	return out, nil
}

// migrateOneLegacyModel 迁移单个模型；changed=false 表示该行已经是新结构。
func migrateOneLegacyModel(gdb *gorm.DB, m legacyModel, fx float64, dryRun bool) (
	row BillingMigrationRow, changed bool, warnings []string, err error,
) {
	sp, specKeys, specWarn, err := migrateLegacySpec(m.SpecRaw, fx)
	if err != nil {
		return row, false, nil, err
	}
	lanes := buildLegacyLanes(m, fx)
	warnings = append(warnings, specWarn...)

	// 判据：已有新价格 且 价目里没有旧键 → 已迁移，跳过。
	if m.HasPrice && len(specKeys) == 0 {
		return row, false, nil, nil
	}

	mode := legacyBillingMode(m.QuotaType, sp.Kind, m.Modality)

	upd := map[string]any{"billing_mode": mode}
	if specKeys != nil {
		// 价目有旧键才覆盖；纯空价目不动（避免把「运营刚填好的新结构」写回空）。
		upd["spec_prices"] = sp
	}
	// 只写「算得出值」的 lane：NULL 表示未单独配置（按输入价/同价），
	// 这是新结构的合法语义，不能拿 0 去填（0 = 显式免费，会改价）。
	names := make([]string, 0, len(lanes))
	for _, l := range lanes {
		upd[l.Column] = l.Micro
		names = append(names, l.Name)
	}

	row = BillingMigrationRow{ID: m.ID, Modality: m.Modality, Mode: mode, Lanes: names, SpecKeys: specKeys}
	warnings = append(warnings, sanityWarnings(mode, lanes, sp)...)
	row.Prices = make(map[string]int64, len(lanes))
	for _, l := range lanes {
		if l.Micro != nil {
			row.Prices[l.Name] = *l.Micro
		}
	}
	if len(specKeys) > 0 {
		spCopy := sp
		row.Spec = &spCopy
	}

	if dryRun {
		return row, true, warnings, nil
	}

	// is_free 必须跟着新价目重算：它是展示投影，不重算会让「全部为 0」的模型
	// 继续带着旧徽标，运营看到"免费"却按正价扣费（或反之）。
	free := recomputeIsFree(mode, lanes, sp)
	upd["is_free"] = free
	// version +1：改价审计与 AI 提案的 CAS 依赖它，不 +1 会让"迁移后立刻改价"
	// 命中旧版本号，触发一次莫名其妙的冲突。
	if err := gdb.Exec(`UPDATE models SET version = version + 1 WHERE id = ?`, m.ID).Error; err != nil {
		return row, false, nil, err
	}
	if err := gdb.Model(&model.Model{}).Where("id = ?", m.ID).Updates(upd).Error; err != nil {
		return row, false, nil, err
	}
	return row, true, warnings, nil
}

// legacyLane 是单个价格 lane 的计算结果。
type legacyLane struct {
	Name   string // 审计/报告用（与 laneCN 同名）
	Column string // 列名
	Micro  *int64
}

// buildLegacyLanes 把旧倍率列算成微。
//
// 只有「旧列有值」才产出 lane：倍率为 NULL 在新结构里的语义就是 NULL（按输入价/同价），
// 补成输入价会把"未配置"变成"显式同价"，之后运营再想区分就分不出来了。
func buildLegacyLanes(m legacyModel, fx float64) []legacyLane {
	var out []legacyLane
	input := microOfRatio(m.ModelRatio, fx)
	if input != nil {
		out = append(out, legacyLane{"input", "price_input", input})
	}
	rel := func(name, col string, r *float64) {
		if input == nil || r == nil {
			return
		}
		out = append(out, legacyLane{name, col, mulMicro(*input, *r)})
	}
	rel("output", "price_output", m.CompletionRatio)
	rel("cache", "price_cache", m.CacheRatio)
	rel("cache_create", "price_cache_create", m.CreateCacheRatio)
	rel("cache_create_1h", "price_cache_create_1h", m.CreateCacheRatio1h)
	rel("image", "price_image", m.ImageRatio)
	rel("audio", "price_audio", m.AudioRatio)
	// 音频输出价相对**音频输入价**（旧定义），不是相对文本输入价。
	if input != nil && m.AudioRatio != nil && m.AudioCompletionRatio != nil {
		audio := mulMicro(*input, *m.AudioRatio)
		if audio != nil {
			out = append(out, legacyLane{"audio_output", "price_audio_output", mulMicro(*audio, *m.AudioCompletionRatio)})
		}
	}
	if m.ModelPriceQuota != nil {
		v := quotaToMicro(*m.ModelPriceQuota, fx)
		out = append(out, legacyLane{"call", "price_call", &v})
	}
	return out
}

// microOfRatio 把 ratio（= 输入价$/1M ÷ 2）算成「微 / 1M tokens」。
func microOfRatio(r *float64, fx float64) *int64 {
	if r == nil || *r < 0 {
		return nil
	}
	v := int64(math.Round(*r * 2 * fx * float64(money.Scale)))
	return &v
}

// quotaToMicro 把旧额度（1 美元 = 500000 额度）折成微。
func quotaToMicro(q int64, fx float64) int64 {
	return int64(math.Round(float64(q) * fx * float64(money.Scale) / legacyQuotaPerUSD))
}

// mulMicro 按倍率放大金额（四舍五入到 1 微）。
func mulMicro(base int64, ratio float64) *int64 {
	v := int64(math.Round(float64(base) * ratio))
	return &v
}

// legacyBillingMode 由旧 quota_type 派生计费方式（0 按量 / 1 按次 / 2 按规格）。
//
// 模态兜底优先于 quota_type：旧的 0 值在新结构里对图片/视频/音乐是**非法组合**，
// 直接照搬会让这些模型走 token 公式（图片没有 token → 几乎 0 元），
// 这正是本次事故的核心表现，必须在迁移里按模态纠正。
func legacyBillingMode(quotaType int, specKind, modality string) string {
	switch modality {
	case "image", "video", "music":
		if specKind != "" {
			return model.BillingModeSpec
		}
		if quotaType == 1 {
			return model.BillingModeCall
		}
		return model.BillingModeSpec
	}
	switch quotaType {
	case 1:
		return model.BillingModeCall
	case 2:
		return model.BillingModeSpec
	default:
		return model.BillingModeTokens
	}
}

// legacySpecKeys 是旧 spec_prices 的键 → 新键。值里的 `_quota` 后缀一并去掉。
var legacySpecKeys = map[string]string{
	"resolution_prices_quota":      "tier_price",
	"default_price_quota":          "default_price",
	"n_max":                        "max_n",
	"second_price_quota":           "second_price",
	"per_task_price_quota":         "per_task_price",
	"ref_image_price_quota":        "ref_image_price",
	"free_ref_images":              "ref_image_free",
	"ref_audio_price_quota":        "ref_audio_price",
	"ref_video_price_quota":        "ref_video_price",
	"ref_video_second_price_quota": "ref_video_second_price",
}

// migrateLegacySpec 把旧 spec_prices 转成新结构（值折算成微）。
//
// 返回的 specKeys 为 nil 表示「该行没有旧键」（已迁移或空价目），调用方据此决定是否覆盖。
func migrateLegacySpec(raw *string, fx float64) (model.SpecPrices, []string, []string, error) {
	var sp model.SpecPrices
	if raw == nil || strings.TrimSpace(*raw) == "" || strings.TrimSpace(*raw) == "null" {
		return sp, nil, nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		// 坏 JSON 不是 migrate 的事：交给启动体检（UnparsableSpecModels）点名，
		// 这里跳过而不是让整个迁移失败（一行坏数据不该拦住 30 个模型。
		return sp, nil, []string{fmt.Sprintf("价目 JSON 无法解析，已跳过（请人工重填）：%v", err)}, nil
	}
	var keys, warns []string
	var w string
	var legacyDefault int64
	var hasLegacyDefault bool
	for legacyKey, newKey := range legacySpecKeys {
		v, ok := m[legacyKey]
		if !ok || v == nil {
			continue
		}
		keys = append(keys, newKey)
		switch newKey {
		case "tier_price":
			sp.TierPrice, w = legacyTierMap(v, fx, true, newKey)
			warns = appendWarn(warns, w)
		case "second_price":
			sp.SecondPrice, w = legacyTierMap(v, fx, false, newKey)
			warns = appendWarn(warns, w)
		case "ref_video_second_price":
			sp.RefVideoSecondPrice, w = legacyTierMap(v, fx, false, newKey)
			warns = appendWarn(warns, w)
		case "default_price":
			// 旧兜底价不再直接落字段（字段已删）：先存局部变量，
			// **循环结束后**再折算成默认档。原因：legacySpecKeys 是 map，
			// 遍历顺序随机 —— default_price 可能先于 tier_price 被处理，
			// 那一刻还没有档位表，反推不出档位，只能静默落成"最低档"。
			if amt, w := legacyAmountChecked(v, fx, "default_price"); amt != nil {
				legacyDefault, hasLegacyDefault = *amt, true
			} else if w != "" {
				warns = appendWarn(warns, w)
			}
		case "max_n":
			sp.MaxN = legacyCount(v)
		case "per_task_price":
			sp.PerTaskPrice, w = legacyAmountChecked(v, fx, "per_task_price")
			warns = appendWarn(warns, w)
		case "ref_image_price":
			sp.RefImagePrice, w = legacyAmountChecked(v, fx, "ref_image_price")
			warns = appendWarn(warns, w)
		case "ref_image_free":
			// 旧键叫 free_ref_images，0 的旧语义是「全部免费」还是「不免费」已无从考证
			// （注释与实现历史上不一致）。这里按「不免费」迁移并点名，让运营核对 ——
			// 宁可少收，也不要把原本免费的参考图默默改成收费。
			n := legacyCount(v)
			if n != nil && *n == 0 {
				warns = append(warns, "free_ref_images=0 语义不明，已按「不免费」迁移，请核对参考图是否应免费")
			}
			sp.RefImageFree = n
		case "ref_audio_price":
			sp.RefAudioPrice, w = legacyAmountChecked(v, fx, "ref_audio_price")
			warns = appendWarn(warns, w)
		case "ref_video_price":
			sp.RefVideoPrice, w = legacyAmountChecked(v, fx, "ref_video_price")
			warns = appendWarn(warns, w)
		}
	}
	// 与新键同名的字段（kind / max_seconds / resolution_fixed / long_*）直接沿用。
	if v, ok := m["kind"].(string); ok {
		sp.Kind = v
	}
	sp.MaxSeconds = legacyCount(m["max_seconds"])
	sp.LongAfterSeconds = legacyCount(m["long_after_seconds"])
	sp.LongVideoMultiplier = legacyMultiplier(m["long_video_multiplier"])
	if v, ok := m["resolution_fixed"].(string); ok {
		sp.ResolutionFixed = strings.ToLower(strings.TrimSpace(v))
	}
	// 旧兜底价 → 默认档。规则与改造前的 DefaultImageTier 逐字一致：
	//   取与兜底价同价的最低档；没有同价档则取最低价档。
	// 这样存量模型「不传尺寸」的金额一分不变。
	if strings.TrimSpace(sp.ResolutionFixed) == "" && hasLegacyDefault && len(sp.TierPrice) > 0 {
		sp.ResolutionFixed = defaultTierFromAmount(sp.TierPrice, legacyDefault)
	}
	sort.Strings(keys)
	return sp, keys, warns, nil
}

// defaultTierFromAmount 把旧兜底价折算成默认档（与旧 DefaultImageTier 同规则）。
//
// 只在迁移里用：历史数据的"默认档"是一个价格，而新结构要的是档位键。
// 同价多档取清晰度低者 —— 金额一致，且不会挑到上游更难交付的高档。
func defaultTierFromAmount(tiers map[string]int64, amount int64) string {
	best, bestRank := "", 0
	lowest, lowestUnit := "", int64(0)
	for k, v := range tiers {
		if v == amount && (best == "" || tierRankOf(k) < bestRank) {
			best, bestRank = k, tierRankOf(k)
		}
		if lowest == "" || v < lowestUnit {
			lowest, lowestUnit = k, v
		}
	}
	if best != "" {
		return best
	}
	return lowest
}

// tierRankOf 是档位清晰度的粗排（`N×1024` 的 N）。只用于迁移里的"取最低档"，
// 认不出的键返回 0（排最前）。
func tierRankOf(tier string) int {
	t := strings.ToUpper(strings.TrimSpace(tier))
	if !strings.HasSuffix(t, "K") {
		return 0
	}
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSuffix(t, "K"), "%f", &f); err != nil {
		return 0
	}
	return int(f * 100)
}

// legacyTierMap 把旧档位表折成微，并按模态归一化键的大小写。
//
// 非整数原值（旧库里把「元」写成小数，如 0.08）一律**跳过并告警**：
// 把它当额度折会得到 1 微（≈ 免费），那是比"未配置"更危险的错 ——
// 未配置会被计费层拒绝，1 微会真的按 1 微扣款并且看起来"配过了"。
func legacyTierMap(v any, fx float64, upper bool, field string) (map[string]int64, string) {
	src, ok := v.(map[string]any)
	if !ok {
		return nil, ""
	}
	out := make(map[string]int64, len(src))
	var bad []string
	for k, raw := range src {
		key := strings.TrimSpace(k)
		if upper {
			key = strings.ToUpper(key)
		} else {
			key = strings.ToLower(key)
		}
		if isFractional(raw) {
			bad = append(bad, key)
			continue
		}
		amt := legacyAmount(raw, fx)
		if amt == nil {
			continue
		}
		out[key] = *amt
	}
	var warn string
	if len(bad) > 0 {
		sort.Strings(bad)
		warn = fmt.Sprintf("%s 的档位 %s 原值带小数（旧数据把「元」写成了额度），已跳过，必须人工重填",
			field, strings.Join(bad, "/"))
	}
	if len(out) == 0 {
		return nil, warn
	}
	return out, warn
}

// legacyAmountChecked 单值版本的折算 + 坏数据点名。
func legacyAmountChecked(v any, fx float64, field string) (*int64, string) {
	if isFractional(v) {
		return nil, fmt.Sprintf("%s 原值带小数（旧数据把「元」写成了额度），已跳过，必须人工重填", field)
	}
	return legacyAmount(v, fx), ""
}

// isFractional 报告旧值是否为非整数（合法的旧额度一定是整数）。
func isFractional(v any) bool {
	f, ok := toFloat(v)
	if !ok {
		return false
	}
	return f != math.Trunc(f)
}

func appendWarn(dst []string, w string) []string {
	if w == "" {
		return dst
	}
	return append(dst, w)
}

// legacyAmount 旧额度 → 微（负数/非数字返回 nil）。
func legacyAmount(v any, fx float64) *int64 {
	f, ok := toFloat(v)
	if !ok || f < 0 {
		return nil
	}
	micro := int64(math.Round(f * fx * float64(money.Scale) / legacyQuotaPerUSD))
	return &micro
}

// legacyCount 计数类字段（张数/秒数）原样搬，只做整数化。
func legacyCount(v any) *int64 {
	if v == nil {
		return nil
	}
	f, ok := toFloat(v)
	if !ok || f < 0 {
		return nil
	}
	n := int64(math.Round(f))
	return &n
}

// legacyMultiplier 旧倍率 → 微比例。
//
// 旧值存的是「倍数」（2 = 2 倍），新结构存微比例（2_000_000）。
// 判据用 <=100：倍数不会超过 100，而微比例最小是 1_000_001（必须 > 1 倍）。
func legacyMultiplier(v any) *int64 {
	f, ok := toFloat(v)
	if !ok || f <= 1 {
		return nil
	}
	var out int64
	if f <= 100 {
		out = int64(math.Round(f * float64(money.Scale)))
	} else {
		out = int64(math.Round(f))
	}
	return &out
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// recomputeIsFree 与 model.Model.PricingIsFree 同一判据（迁移期不复用是因为
// 此刻还没写库，只能按算出来的 lane 判断）。
func recomputeIsFree(mode string, lanes []legacyLane, sp model.SpecPrices) bool {
	switch mode {
	case model.BillingModeCall:
		for _, l := range lanes {
			if l.Name == "call" && l.Micro != nil {
				return *l.Micro == 0
			}
		}
		return false
	case model.BillingModeSpec:
		return sp.AllConfiguredZero()
	default:
		var input *int64
		allZero := true
		any := false
		for _, l := range lanes {
			if l.Name == "input" {
				input = l.Micro
			}
			if l.Micro != nil {
				any = true
				if *l.Micro != 0 {
					allZero = false
				}
			}
		}
		return input != nil && any && allZero
	}
}

// sanityWarnings 点名「折算出来明显不像真钱」的价目。
//
// 折算只保证等值，不保证原值是对的：联调期把 50 填进视频秒价、把 0.08 填进图片档位
// 这类数据，折完会得到 ¥0.0007/秒、1 微/张 —— 它们在库里看起来"已配置"，
// 计费层也不会拒绝，只会安静地少收钱。所以必须在报告里点名，让运营重填。
//
// 阈值取"正常定价的下限再低一档"，宁可多报几条也不要漏：
//
//	图片档位/兜底 < ¥0.01/张、视频秒价 < ¥0.01/秒、按任务 < ¥0.01/次、token 输入 < ¥0.05/1M
func sanityWarnings(mode string, lanes []legacyLane, sp model.SpecPrices) []string {
	var out []string
	low := func(name string, v int64, floor int64, unit string) {
		if v > 0 && v < floor {
			out = append(out, fmt.Sprintf("%s = %s%s，明显低于正常定价，请核对原值是否填错",
				name, money.Format(v, 4), unit))
		}
	}
	switch mode {
	case model.BillingModeSpec:
		for _, tier := range sortedKeys(sp.TierPrice) {
			low("图片档位 "+tier, sp.TierPrice[tier], 10_000, "/张")
		}
		for _, tier := range sortedKeys(sp.SecondPrice) {
			low("视频 "+tier, sp.SecondPrice[tier], 10_000, "/秒")
		}
		for _, tier := range sortedKeys(sp.RefVideoSecondPrice) {
			low("参考视频 "+tier, sp.RefVideoSecondPrice[tier], 10_000, "/秒")
		}
		if sp.PerTaskPrice != nil {
			low("按任务价", *sp.PerTaskPrice, 10_000, "/次")
		}
		if sp.RefImagePrice != nil {
			low("参考图价", *sp.RefImagePrice, 10_000, "/张")
		}
		if sp.RefAudioPrice != nil {
			low("参考音频价", *sp.RefAudioPrice, 10_000, "/个")
		}
	case model.BillingModeTokens:
		for _, l := range lanes {
			if l.Name == "input" && l.Micro != nil {
				low("输入价", *l.Micro, 50_000, "/1M tokens")
			}
		}
	}
	return out
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LogBillingMigration 把迁移结果打到启动日志（每行一条，便于运营核对）。
func LogBillingMigration(rep BillingMigrationReport) {
	if rep.Skipped {
		return
	}
	for _, w := range rep.Warnings {
		logger.L().Warn("M28 价目迁移告警", zap.String("warning", w))
	}
	for _, r := range rep.Rows {
		logger.L().Info("M28 价目迁移",
			zap.String("model", r.ID),
			zap.String("modality", r.Modality),
			zap.String("billing_mode", r.Mode),
			zap.Strings("lanes", r.Lanes),
			zap.Strings("spec_keys", r.SpecKeys),
		)
		for _, w := range r.Warnings {
			logger.L().Warn("M28 价目迁移告警", zap.String("model", r.ID), zap.String("warning", w))
		}
	}
	if len(rep.Rows) > 0 {
		logger.L().Info("M28 存量价目已折算为绝对金额",
			zap.Int("models", len(rep.Rows)),
			zap.Float64("cny_per_usd", rep.Fx),
			zap.Bool("dry_run", rep.DryRun),
		)
	}
}
