// Package spec 是 L2 规格（adapter_specs）的读写与运行时解析入口。
//
// 三层能力模型的定位（改代码前先读）：
//   - L1：channel.adapter_config（声明式请求改写），能力上限是"改字段"，写不了协议；
//   - L2：本包 —— 一份受限 DSL 描述「某家上游的 API 形状」，渠道以 `spec:<slug>` 引用；
//   - L3：内置 Go 适配器（multipart、非 JSON 协议、流式特殊处理）。
//
// 本包只做三件事：**校验并落库**、**给 relay 提供惰性解析**、**在写库后让注册表失效**。
// 它不参与选路、不参与计费，也不缓存"哪个渠道在用哪份规格"。
package spec

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/relay"
	specdsl "lingdong-api/internal/relay/spec"
	"lingdong-api/internal/service/configsync"
	"lingdong-api/internal/service/iconindex"
	"lingdong-api/internal/service/iconref"
)

// AdapterPrefix 是规格在 channels.adapter_id 里的前缀（唯一拼装点）。
const AdapterPrefix = "spec:"

var (
	ErrNotFound  = errors.New("spec: 规格不存在")
	ErrSlugTaken = errors.New("spec: 规格标识已被占用")
	// ErrSlugImmutable 改 slug。slug 已经进了 channels.adapter_id，
	// 允许改名等于让引用它的渠道集体变成"适配器未注册"，且无法回滚。
	ErrSlugImmutable = errors.New("spec: 规格标识创建后不可修改")
	// ErrInUse 仍有渠道引用该规格。
	//
	// 为什么禁止强删：删掉规格后引用它的渠道不会报错配置，而是在**每次请求**时
	// 变成"适配器未注册"（500）。这种"配置看起来正常、调用全失败"的状态最难排查，
	// 所以宁可让运营先去改渠道。
	ErrInUse = errors.New("spec: 仍有渠道引用该规格，禁止删除")
	// ErrConflict 乐观锁失败（提案 apply 期间被人工改过）。
	ErrConflict = errors.New("spec: 规格已被他人修改，请刷新后重试")
)

// Item 是管理台的规格视图（列表与详情共用，详情多带 Spec 原文）。
type Item struct {
	ID     uint64 `json:"id"`
	Slug   string `json:"slug"`
	Label  string `json:"label"`
	Vendor string `json:"vendor"`
	// AdapterID 是渠道侧引用的值（`spec:<slug>`），前端直接回填进渠道表单。
	AdapterID string `json:"adapter_id"`
	Status    int    `json:"status"`
	Version   int    `json:"version"`
	// Endpoints 是已声明的端点键（含 @poll 轮询端点），按字典序。
	Endpoints []string `json:"endpoints"`
	// Caps 是由端点派生的能力矩阵（text/image/video/...），不接受手工声明。
	Caps []string `json:"caps"`
	// ChannelCount 是引用该规格的渠道数（不含软删渠道），列表用于删除保护提示。
	ChannelCount int64 `json:"channel_count"`
	// OwnerID 是创建人（0 = 系统预置）。
	OwnerID uint64 `json:"owner_id"`
	// Warnings 是「配置看起来合法、但某个开关不会生效」的提示（读期计算，不落库）。
	Warnings  []string  `json:"warnings"`
	UpdatedAt time.Time `json:"updated_at"`
	// Spec 只在详情下发：列表带上 32 份 64KB 的 DSL 是纯浪费，且前端表格用不到。
	Spec json.RawMessage `json:"spec,omitempty"`
}

// AdapterID 把 slug 拼成渠道侧的适配器标识。
func AdapterID(slug string) string { return AdapterPrefix + slug }

// SlugOf 从适配器标识还原 slug。
func SlugOf(adapterID string) (string, bool) {
	if !strings.HasPrefix(adapterID, AdapterPrefix) {
		return "", false
	}
	slug := adapterID[len(AdapterPrefix):]
	if slug == "" {
		return "", false
	}
	return slug, true
}

// Input 是新建/更新入参。
//
// Slug/Label/Vendor 与 Spec 里的同名字段是**同一份数据**，服务端以入参为准并校验一致性：
// 若允许两处各说各话，管理台改了 label 但列表显示旧值这种「看不到症状」的错误必然出现。
type Input struct {
	Slug   string
	Label  string
	Vendor string
	// Status 为 0 时按"启用"处理（新建默认启用；更新时 0 表示不改）。
	Status int
	// Spec 是规格 DSL 原文；允许在其中省略 slug/label/vendor（由入参补齐）。
	Spec json.RawMessage
	// Version > 0 时做乐观锁校验（仅 Update 使用）。
	Version int
	// OwnerID 只在新建时写入（更新不改 owner：owner 是创建人，改动人由审计回答）。
	OwnerID uint64
}

// prepare 解码 + 补齐投影字段 + 强校验，返回校验通过的规格与规范化后的 JSON。
//
// 落库的是**重新序列化后**的 JSON：字段顺序稳定、未知键已在 Decode 阶段被拒，
// 因此不会出现"运营提交的原文与服务端理解的原文不一致"。
func prepare(in Input, slug string) (*specdsl.Spec, json.RawMessage, error) {
	raw := in.Spec
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage(`{}`)
	}
	s, err := specdsl.Decode(raw)
	if err != nil {
		return nil, nil, err
	}
	// 一致性校验（只对 slug 报错，label/vendor 以入参为准）。
	//
	// 为什么 slug 必须报错：slug 已经进 channels.adapter_id，两处不一致说明
	// 提交的是一份"别人的规格"，静默采用入参会让运营以为改的是另一份规格。
	//
	// 为什么 label/vendor 不报错：它们只是展示信息，没有外部引用。管理台表单
	// 与服务端投影本来就是同一份数据的不同入口，报错只会让"改个名字"这种操作
	// 变成"先改 JSON 再改表单"。入参非空即为准，空则沿用 DSL 里的值。
	if s.Slug != "" && s.Slug != slug {
		return nil, nil, &specdsl.FieldError{Field: "slug", Message: "规格内容里的 slug 与标识不一致"}
	}
	if in.Label != "" {
		s.Label = in.Label
	}
	if in.Vendor != "" {
		s.Vendor = in.Vendor
	}
	s.Slug = slug
	if err := s.Validate(); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(s)
	if err != nil {
		return nil, nil, err
	}
	return s, out, nil
}

// List 返回全部规格（按 slug 排序）。
//
// 不分页：规格是"每种上游 API 一份"，现实上限是几十条；分页只会让
//
//	「下拉选规格」的调用方拿不到全量。
func List(ctx context.Context) ([]Item, error) {
	var rows []model.AdapterSpec
	if err := db.DB.WithContext(ctx).Order("slug ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	counts, err := channelCounts(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for i := range rows {
		items = append(items, toItem(&rows[i], counts[rows[i].Slug], false))
	}
	return items, nil
}

// Get 返回单个规格详情（含 DSL 原文）。
func Get(ctx context.Context, id uint64) (*Item, error) {
	var row model.AdapterSpec
	if err := db.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	n, err := countChannels(ctx, row.Slug)
	if err != nil {
		return nil, err
	}
	it := toItem(&row, n, true)
	return &it, nil
}

// Create 新建规格。
func Create(ctx context.Context, in Input) (*Item, error) { return CreateTx(ctx, db.DB, in) }

// CreateTx 在指定连接/事务上新建规格（AI 提案 apply 用）。
func CreateTx(ctx context.Context, gdb *gorm.DB, in Input) (*Item, error) {
	slug := strings.TrimSpace(in.Slug)
	s, specJSON, err := prepare(in, slug)
	if err != nil {
		return nil, err
	}
	status := in.Status
	if status == 0 {
		status = model.SpecStatusEnabled
	}
	if status != model.SpecStatusEnabled && status != model.SpecStatusDisabled {
		return nil, &specdsl.FieldError{Field: "status", Message: "状态取值不合法"}
	}
	// 先查重给友好提示；并发下靠 uq_adapter_specs_slug 兜底（23505 → ErrSlugTaken）。
	var exists int64
	if err := gdb.WithContext(ctx).Model(&model.AdapterSpec{}).Where("slug = ?", slug).Count(&exists).Error; err != nil {
		return nil, err
	}
	if exists > 0 {
		return nil, ErrSlugTaken
	}
	row := model.AdapterSpec{
		Slug:    slug,
		Label:   s.Label,
		Vendor:  s.Vendor,
		Status:  int16(status),
		Version: 1,
		OwnerID: in.OwnerID,
		Spec:    model.JSON(specJSON),
	}
	if err := gdb.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicate(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	// 事务内不广播（广播不可回滚）：apply 提交后由调用方统一失效。
	if gdb == db.DB {
		Invalidate(slug)
	}
	it := toItem(&row, 0, true)
	return &it, nil
}

// Update 修改规格（slug 不可改）。
func Update(ctx context.Context, id uint64, in Input) (*Item, error) {
	return UpdateTx(ctx, db.DB, id, in)
}

// UpdateTx 在指定连接/事务上修改规格（AI 提案 apply 用）。
func UpdateTx(ctx context.Context, gdb *gorm.DB, id uint64, in Input) (*Item, error) {
	var row model.AdapterSpec
	if err := gdb.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if slug := strings.TrimSpace(in.Slug); slug != "" && slug != row.Slug {
		return nil, ErrSlugImmutable
	}
	s, specJSON, err := prepare(in, row.Slug)
	if err != nil {
		return nil, err
	}
	// 编辑把端点删掉时，仍声明该端点的在用模型会在运行期直接 400/501，
	// 而保存本身"成功"——所以必须在这里挡住，强制先调整模型或绑定。
	if missing, merr := missingEndpoints(ctx, gdb, row.Slug, s); merr != nil {
		return nil, merr
	} else if len(missing) > 0 {
		return nil, &specdsl.FieldError{
			Field:   "endpoints",
			Message: "以下端点仍被在用模型声明，先调整模型端点再删除：" + strings.Join(missing, "、"),
		}
	}
	// 用 map 而不是 struct：GORM 对带 default 的列会丢掉零值，
	// 而这里每个字段都可能被显式改成零值语义（如 vendor 清空）。
	updates := map[string]any{
		"label":      s.Label,
		"vendor":     s.Vendor,
		"spec":       model.JSON(specJSON),
		"version":    gorm.Expr("version + 1"),
		"updated_at": time.Now(),
	}
	if in.Status != 0 {
		if in.Status != model.SpecStatusEnabled && in.Status != model.SpecStatusDisabled {
			return nil, &specdsl.FieldError{Field: "status", Message: "状态取值不合法"}
		}
		updates["status"] = in.Status
	}
	q := gdb.WithContext(ctx).Model(&model.AdapterSpec{}).Where("id = ?", id)
	if in.Version > 0 {
		q = q.Where("version = ?", in.Version)
	}
	res := q.Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrConflict
	}
	if gdb == db.DB {
		Invalidate(row.Slug)
	}
	// 重新读一次：version 是数据库侧自增的，客户端要拿到新值才能继续编辑。
	if err := gdb.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	n, err := countChannels(ctx, row.Slug)
	if err != nil {
		return nil, err
	}
	it := toItem(&row, n, true)
	return &it, nil
}

// Impact 是「改或删这份规格会影响谁」（管理台确认框用）。
type Impact struct {
	ChannelCount     int64           `json:"channel_count"`
	Channels         []ImpactChannel `json:"channels"`
	Models           []ImpactModel   `json:"models"`
	MissingEndpoints []string        `json:"missing_endpoints"`
}

// ImpactChannel 是引用该规格的渠道。
type ImpactChannel struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	// IconURL 是渠道图标地址（"" = 首字占位）。影响面弹窗列出的是"要改哪几个渠道"，
	// 只有名字时运营得回渠道页逐个对号。
	IconURL string `json:"icon_url"`
	Status  int    `json:"status"`
}

// ImpactModel 是受影响模型（含它声明的端点，便于判断能不能删端点）。
type ImpactModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// IconURL 是模型生效图标地址（已回落供应商，走 iconindex 统一解析）。
	IconURL   string   `json:"icon_url"`
	Endpoints []string `json:"endpoints"`
}

// ImpactOf 返回引用影响面（两次查询，不做 N+1）。
func ImpactOf(ctx context.Context, id uint64, base string) (*Impact, error) {
	var row model.AdapterSpec
	if err := db.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	chans, models, err := referencing(ctx, db.DB, row.Slug, base)
	if err != nil {
		return nil, err
	}
	return &Impact{
		ChannelCount: int64(len(chans)),
		Channels:     chans,
		Models:       models,
		// 当前状态下没有"缺失端点"：它只在编辑提交时才可能出现。
		MissingEndpoints: []string{},
	}, nil
}

// referencing 返回引用某规格的渠道与它们的绑定模型。
func referencing(ctx context.Context, gdb *gorm.DB, slug, base string) ([]ImpactChannel, []ImpactModel, error) {
	var chans []ImpactChannel
	if err := gdb.WithContext(ctx).Model(&model.Channel{}).
		Select("id, name, status").
		Where("adapter_id = ?", AdapterID(slug)).
		Order("id ASC").Scan(&chans).Error; err != nil {
		return nil, nil, err
	}
	if len(chans) == 0 {
		return chans, []ImpactModel{}, nil
	}
	ids := make([]uint64, 0, len(chans))
	for _, c := range chans {
		ids = append(ids, c.ID)
	}
	var rows []struct {
		ID        string
		Name      string
		Endpoints string
	}
	if err := gdb.WithContext(ctx).Raw(`
		SELECT DISTINCT m.id AS id, COALESCE(m.display_name, m.id) AS name, m.endpoints::text AS endpoints
		FROM model_bindings mb
		JOIN models m ON m.id = mb.model_id
		WHERE mb.channel_id IN ? AND mb.deleted_at IS NULL AND m.deleted_at IS NULL
		ORDER BY m.id ASC`, ids).Scan(&rows).Error; err != nil {
		return nil, nil, err
	}
	modelIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		modelIDs = append(modelIDs, r.ID)
	}
	// 图标统一走 iconindex：渠道与模型两种情况都在同一个快照里，
	// 回落规则（模型 → 供应商）不必在这里再实现一遍。
	icons := iconindex.Resolve(ctx, modelIDs, ids)
	for i := range chans {
		chans[i].IconURL = iconref.URL(base, icons.Channel(chans[i].ID))
	}
	models := make([]ImpactModel, 0, len(rows))
	for _, r := range rows {
		models = append(models, ImpactModel{
			ID: r.ID, Name: r.Name, IconURL: iconref.URL(base, icons.Model(r.ID)),
			Endpoints: decodeEndpoints(r.Endpoints),
		})
	}
	return chans, models, nil
}

// missingEndpoints 返回「在用模型声明了、但新规格不再提供」的端点。
func missingEndpoints(ctx context.Context, gdb *gorm.DB, slug string, s *specdsl.Spec) ([]string, error) {
	_, models, err := referencing(ctx, gdb, slug, "")
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, nil
	}
	have := make(map[string]bool, len(s.Endpoints))
	for k := range s.Endpoints {
		if specdsl.IsPollKey(k) {
			continue
		}
		have[k] = true
	}
	missing := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, m := range models {
		for _, e := range m.Endpoints {
			if have[e] || seen[e] {
				continue
			}
			seen[e] = true
			missing = append(missing, e)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// decodeEndpoints 解析 models.endpoints 的 JSON 数组（脏值返回空，不报错）。
func decodeEndpoints(raw string) []string {
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// SetStatus 启用/停用规格。
//
// 停用不检查引用：运营明确要求"先停掉，再逐个改渠道"。但它会让引用渠道的请求
// 立刻失败，所以停用后必须让注册表失效（否则旧实例还能跑最多 5s 负缓存/TTL）。
func SetStatus(ctx context.Context, id uint64, status int) error {
	return SetStatusTx(ctx, db.DB, id, status)
}

// SetStatusTx 在指定连接上启停规格（apply 的事务路径用）。
//
// 与 SetStatus 的唯一区别是连接与"提交后失效"：事务内不失效注册表
// （回滚后失效已生效会制造 5s 的假故障），由调用方在提交后调 Invalidate。
func SetStatusTx(ctx context.Context, gdb *gorm.DB, id uint64, status int) error {
	if status != model.SpecStatusEnabled && status != model.SpecStatusDisabled {
		return &specdsl.FieldError{Field: "status", Message: "状态取值不合法"}
	}
	var row model.AdapterSpec
	if err := gdb.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if int(row.Status) == status {
		return nil
	}
	if err := gdb.WithContext(ctx).Model(&model.AdapterSpec{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":     status,
			"version":    gorm.Expr("version + 1"),
			"updated_at": time.Now(),
		}).Error; err != nil {
		return err
	}
	if gdb == db.DB {
		Invalidate(row.Slug)
	}
	return nil
}

// Delete 软删规格；有渠道引用时拒绝。
// Delete 删除一份规格（仍被渠道引用时拒绝）。
//
// 为什么整段包在事务里并 `SELECT ... FOR UPDATE` 住那一行：
// 「数引用」与「删」之间如果有别的事务刚好建了一条指向本规格的渠道，
// 就会留下一条**指向已删规格的渠道** —— 它保存时校验通过、管理台显示启用，
// 而每次调用都是 503，且没有任何提示告诉你原因。
// 渠道保存侧对 spec:<slug> 取 SHARE 锁（channel.AdapterResolvable），
// 这里取 UPDATE 锁，两者互斥，竞态窗口被关掉。
func Delete(ctx context.Context, id uint64) error {
	var row model.AdapterSpec
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, id).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		n, cerr := countChannelsTx(ctx, tx, row.Slug)
		if cerr != nil {
			return cerr
		}
		if n > 0 {
			return ErrInUse
		}
		return tx.Delete(&model.AdapterSpec{}, id).Error
	})
	if err != nil {
		return err
	}
	Invalidate(row.Slug)
	return nil
}

// UsageCount 返回引用该规格的渠道数（供控制器生成错误提示，不参与判定）。
func UsageCount(ctx context.Context, slug string) (int64, error) {
	return countChannels(ctx, slug)
}

// ---- 运行时解析 ----

// cache 是进程内的规格缓存。
//
// 为什么需要：relay 首次遇到 `spec:<slug>` 时的解析会查库并反序列化最多 64KB JSON。
// relay 侧的注册表会把**成功解析的实例**留在表里（见 relay.resolveAdapterLazily），
// 所以这里真正要挡的是「规格被改了之后第一波并发」——它们会同时穿透到库。
var cache = specdsl.NewCache(30*time.Second, 64)

// ResolveBySlug 是注入给 relay 的 Loader：库里读一份启用的规格。
//
// 只解析 status=启用的规格：停用语义就是"不再参与任何选路"，
// 若这里放行，停用开关等于只是给管理台看的。
func ResolveBySlug(slug string) (*specdsl.Spec, error) {
	if s, ok := cache.Get(slug); ok {
		return s, nil
	}
	var row model.AdapterSpec
	err := db.DB.Where("slug = ?", slug).First(&row).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			logger.L().Warn("读取规格失败", zap.String("slug", slug), zap.Error(err))
		}
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveNotFound, Name: "spec:" + slug}
	}
	// 停用不是「不存在」：台账与渠道页必须能说清是哪一个问题，
	// 否则运营会去反复检查渠道配置。
	if row.Status != model.SpecStatusEnabled {
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveDisabled, Name: "spec:" + slug}
	}
	s, err := specdsl.Decode(row.Spec)
	if err != nil {
		// 手工改库/迁移残留导致的不合法规格：不静默 New() 一个半成品，
		// 那样会把"配置脏了"表现成"上游莫名其妙 400"。
		logger.L().Warn("规格内容不合法，已拒绝解析",
			zap.String("slug", slug), zap.Uint64("spec_id", row.ID), zap.Error(err))
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveInvalid, Name: "spec:" + slug, Detail: err.Error()}
	}
	cache.Put(slug, s)
	return s, nil
}

// InvalidateLocal 只清本机运行时状态（写库后必须调）。
//
// 两件事都不能省：
//   - cache.Purge：本包自己的缓存；
//   - relay.ForgetAdapter：relay 注册表里的实例 + **5s 负缓存**。只清前者的话，
//     新建规格后保存渠道仍会报"适配器未注册"（负缓存命中），最长 5s 才自愈。
//
// 名字带 Local 是因为它**不广播**：广播版见 Invalidate，钩子注入的必须是本函数
// （注入广播版会让两个实例互相触发死循环）。
func InvalidateLocal(slug string) {
	cache.Purge(slug)
	relay.ForgetAdapter(AdapterID(slug))
}

// Invalidate 清本机状态并广播给其它副本（写库路径用这个）。
func Invalidate(slug string) {
	InvalidateLocal(slug)
	configsync.NotifySpec(slug)
}

// Watch 是规格的多副本兜底巡检（ctx 取消即停）。
//
// 为什么还需要它：relay 注册表把成功解析的实例长期留在表里，而 pub/sub 在
// Redis 重启/重连期间会丢消息。丢一条的后果不是"晚 30s 生效"，而是该副本
// **永久**用旧规格——与 P1-8 同一类"没有报错的不一致"。这里定期把本机缓存
// 全部失效，让下一次请求重新解析（代价是每个在用规格每分钟一次读库）。
func Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refreshAll()
			}
		}
	}()
}

// refreshAll 清掉本机所有规格的缓存与注册实例（slug 为空 = 全清）。
func refreshAll() {
	cache.Purge("")
	var slugs []string
	if err := db.DB.Model(&model.AdapterSpec{}).Pluck("slug", &slugs).Error; err != nil {
		logger.L().Warn("规格巡检读取列表失败", zap.Error(err))
		return
	}
	for _, slug := range slugs {
		relay.ForgetAdapter(AdapterID(slug))
	}
}

// Install 把规格解析器接进 relay 注册表与配置广播（main 在 db 初始化后调用一次）。
//
// 放在这里而不是 main 里直接写三行：三个注入点必须成对出现，
// 少注入一个的后果分别是「永远解析不到」「解析到但没有数据源」「改了不生效也不报错」，
// 都属于不报错、只表现为行为不对的那类问题。
func Install() {
	specdsl.SetLoader(ResolveBySlug)
	relay.SetAdapterResolver(specdsl.Resolve)
	// 钩子必须是 Local 版：广播版会在此处再次 Publish，形成两实例互踢的死循环。
	configsync.SetSpecInvalidator(InvalidateLocal)
}

// ---- 内部 ----

func toItem(row *model.AdapterSpec, channelCount int64, withSpec bool) Item {
	keys := make([]string, 0, 4)
	// caps 必须初始化成空切片：Decode 失败时 nil 会序列化成 null，
	// 前端 `.length` 直接把管理台整页替换成错误页。
	caps := make([]string, 0, 4)
	warnings := make([]string, 0, 2)
	if s, err := specdsl.Decode(row.Spec); err == nil {
		for k := range s.Endpoints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		caps = s.Caps()
		warnings = append(warnings, specWarnings(s)...)
	}
	it := Item{
		ID:           row.ID,
		Slug:         row.Slug,
		Label:        row.Label,
		Vendor:       row.Vendor,
		AdapterID:    AdapterID(row.Slug),
		Status:       int(row.Status),
		Version:      row.Version,
		Endpoints:    keys,
		Caps:         caps,
		ChannelCount: channelCount,
		OwnerID:      row.OwnerID,
		Warnings:     warnings,
		UpdatedAt:    row.UpdatedAt,
	}
	if withSpec {
		it.Spec = json.RawMessage(row.Spec)
	}
	return it
}

// specWarnings 找出「配置合法、但某个开关不会生效」的情况。
//
// 第一条来自真实故障：`omit_model: true` 且 path 里没有 {upstream_model} 时，
// 绑定上填的上游型号名**永远不会被使用**，而管理台看起来一切正常。
func specWarnings(s *specdsl.Spec) []string {
	out := make([]string, 0, 2)
	for key, ep := range s.Endpoints {
		if specdsl.IsPollKey(key) || !ep.OmitModel {
			continue
		}
		if strings.Contains(ep.Path, "{upstream_model}") {
			continue
		}
		out = append(out, "端点 "+key+
			"：omit_model=true 且 path 里没有 {upstream_model}，绑定中的上游型号名不会被使用")
	}
	sort.Strings(out)
	return out
}

func countChannels(ctx context.Context, slug string) (int64, error) {
	return countChannelsTx(ctx, db.DB, slug)
}

// countChannelsTx 用指定连接计数（删除需要在事务里数，见 Delete 的注释）。
func countChannelsTx(ctx context.Context, gdb *gorm.DB, slug string) (int64, error) {
	var n int64
	err := gdb.WithContext(ctx).Model(&model.Channel{}).
		Where("adapter_id = ?", AdapterID(slug)).Count(&n).Error
	return n, err
}

// channelCounts 一次取全部规格的引用数（避免列表 N+1）。
func channelCounts(ctx context.Context) (map[string]int64, error) {
	var rows []struct {
		AdapterID string
		Cnt       int64
	}
	if err := db.DB.WithContext(ctx).Model(&model.Channel{}).
		Select("adapter_id, count(*) AS cnt").
		Where("adapter_id LIKE ?", AdapterPrefix+"%").
		Group("adapter_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		if slug, ok := SlugOf(r.AdapterID); ok {
			out[slug] = r.Cnt
		}
	}
	return out, nil
}

// isDuplicate 判定唯一约束冲突（slug 重名）。
func isDuplicate(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "23505") || strings.Contains(msg, "duplicate key")
}
