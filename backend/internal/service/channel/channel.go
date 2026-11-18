// Package channel 是渠道配置的读写与健康度单一入口（step-04 / v2.0 §4.2.4 / §4.6）。
// 密钥池以 Fernet 密文落 channels.keys（与 api_keys 的 AES-GCM 不混用，§4.14.1）；
// 连通测试通过 relay 适配器注册表执行，并把结果写入 channels.health 快照。
package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/channelhealth"
	"lingdong-api/internal/service/channellimit"
	iconsvc "lingdong-api/internal/service/icon"
	"lingdong-api/internal/service/iconindex"
	"lingdong-api/internal/service/iconref"
	"lingdong-api/internal/service/keyhealth"
	"lingdong-api/internal/service/pricing"
	specsvc "lingdong-api/internal/service/spec"
)

// 渠道状态枚举（G23 冻结，引用 model 唯一真相源）：1 启用 / 2 手动禁用 / 3 自动禁用。
const (
	StatusEnabled  = model.ChannelStatusEnabled
	StatusDisabled = model.ChannelStatusDisabled
)

// 多密钥调度模式（对齐 mock-data.js L172-L178：polling / random）。
const (
	ModePolling = "polling"
	ModeRandom  = "random"
)

// adapterIDPattern 允许两种形态：内置无前缀（openai）、规格（spec:<slug>）。
// `builtin:` 前缀从未有注册项匹配，历史数据已随插件清理迁走，故不再接受。
var adapterIDPattern = regexp.MustCompile(`^(?:[a-z][a-z0-9_-]*|spec:[a-z][a-z0-9-]{1,31})$`)

var (
	ErrNotFound  = errors.New("channel: 渠道不存在")
	ErrBound     = errors.New("channel: 渠道仍绑定模型，禁止删除")
	ErrNoAdapter = errors.New("channel: 适配器不可用")
	// ErrNameExists 渠道名重复（数据库层无唯一约束，应用层兜住）。
	ErrNameExists = errors.New("channel: 渠道名已存在")
	// ErrConflict 乐观锁冲突（期间被他人修改）。
	ErrConflict = errors.New("channel: 该渠道已被他人修改，请刷新后重试")
	// ErrIconInvalid 图标 token 不合法（controller 映射成 icon_invalid）。
	ErrIconInvalid = errors.New("channel: 图标不合法")
)

// Health 是 channels.health JSONB 快照。
type Health struct {
	FailRate     float64    `json:"fail_rate"`
	SuccessCount int        `json:"success_count"`
	FailCount    int        `json:"fail_count"`
	LastTestAt   *time.Time `json:"last_test_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
}

// 说明：Health（channels.health）记录的是**连通测试**的累计成功/失败，
// 熔断判定用的是 Redis 滑窗里的**真实流量**样本（channelhealth.Window），两者不同源，
// 因此 Item 同时携带 health 与 window，UI 必须分开表述（P2-22）。

// BoundModel 是渠道绑定视图项（派生自 model_bindings，编辑入口唯一在模型管理）。
type BoundModel struct {
	Model string `json:"model"`
	// IconURL 是模型**生效**图标的地址（自有 → 供应商回落）。
	// 绑定行只显示模型名时，一个渠道挂十几个模型全靠读字符串区分 —— 图标是这里
	// 唯一能一眼扫完的信息，所以它必须与模型表格里那张图完全一致。
	IconURL string `json:"icon_url"`
	Alias   string `json:"alias"`
	Weight  int    `json:"weight"`
	// Enabled 是**绑定自己的**启停，与渠道启停是两件事。
	//
	// 必须下发：删除渠道的拦截判据是「任意绑定（含停用）」，而这里若只给启用绑定，
	// 运营会看到「0 个模型」却删不掉（后端 409），且左栏的「N 个模型」与右栏
	// （含停用）对不上。前端按它显示「启用数/总数」。
	Enabled bool `json:"enabled"`
}

// Item 是渠道对外视图（不含密钥明文）。
type Item struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	// Icon 是图标 token（"" | builtin:<key> | upload:<ulid>）：**编辑回填**用，
	// 表单要把它原样提交回来，所以这里必须保留令牌而不是地址。
	Icon string `json:"icon"`
	// IconURL 是同一个图标的可渲染地址（空 = 首字占位）。
	// 列表、抽屉、绑定行都直接 `<img src>`，不再自己拼路径。
	IconURL        string   `json:"icon_url"`
	AdapterID      string   `json:"adapter_id"`
	BaseURL        string   `json:"base_url"`
	MultiKeyMode   string   `json:"multi_key_mode"`
	Groups         []string `json:"groups"`
	Weight         int      `json:"weight"`
	Priority       int      `json:"priority"`
	RPMLimit       int      `json:"rpm_limit"`
	MaxConcurrency int      `json:"max_concurrency"`
	// Inflight / InflightLimit 是**本进程**当前的并发占用与信号量上限。
	//
	// 它是进程内信号量的快照，多副本部署时**只代表本机**：3 副本 × 20 的真实
	// 上限是 60 并发，拿这个数字当"全局已用"会让人按 20 去调却怎么都调不动；
	// 反过来「本机满了」也不等于「整条渠道满了」。前端文案必须写「本机」。
	Inflight      int      `json:"inflight"`
	InflightLimit int      `json:"inflight_limit"`
	Status        int      `json:"status"`
	AutoBan       bool     `json:"auto_ban"`
	BalanceCNY    *float64 `json:"balance_cny"`
	// KeyCount 是物理条数（含无法解密的条目）；KeyEnabledCount 是选路实际会用的条数。
	// 两个数并存不是冗余：前者回答"配了几把"，后者回答"现在能用几把"，
	// 只看前者时「5 把 key 全坏」看起来完全正常。
	KeyCount        int  `json:"key_count"`
	KeyEnabledCount int  `json:"key_enabled_count"`
	SwitchKeyRetry  bool `json:"switch_key_retry"`
	// Version 是乐观锁版本（密钥面板改完要带回来；没有它，CAS 永远成立）。
	Version int    `json:"version"`
	Health  Health `json:"health"`
	// AdapterConfig 不加 omitempty：零值必须是 {}（前端按必填处理，靠 {} 表示"未配置"）。
	AdapterConfig relay.AdapterConfig `json:"adapter_config"`
	// AdapterOptions 是适配器私有配置原文（nil = 未配置）。脏数据回 null，不带病下发给前端。
	AdapterOptions json.RawMessage `json:"adapter_options"`
	// Window 是熔断滑窗内的实时样本（仅当该渠道窗口内有流量时下发）
	Window      *channelhealth.Window `json:"window,omitempty"`
	BoundModels []BoundModel          `json:"bound_models"`
}

// Req 是创建/编辑渠道的入参（编辑时 Keys 为空表示不修改密钥池）。
type Req struct {
	Name string
	// Icon 为 nil 表示"本次不修改"（AI 的增量补丁不带它）；
	// 显式 `""` 表示清空（回到首字占位）。用 *string 才能区分这两件事。
	Icon           *string
	AdapterID      string
	BaseURL        string
	MultiKeyMode   string
	Groups         []string
	Weight         int
	Priority       int
	RPMLimit       int
	MaxConcurrency int
	AutoBan        bool
	// SwitchKeyRetry：失败时是否换同渠道另一把 key 重试（默认 false = 维持原行为）。
	SwitchKeyRetry bool
	Keys           []string
	// AdapterConfig 为 nil 表示"本次不修改"（旧调用方不带该字段时不能把已有配置清掉）；
	// 显式传 null/{} 才表示清空。
	AdapterConfig json.RawMessage
	// AdapterOptions 语义同上：nil = 不修改；显式 null/{} = 清空（回到适配器内置默认）。
	AdapterOptions json.RawMessage
}

// TestResult 是渠道连通测试结果。
type TestResult struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	LatencyMS int64  `json:"latency_ms"`
	// KeyIndex 是**原始数组下标**（不是"可用池里的第几个"）：面板按它定位到具体一行。
	KeyIndex       int    `json:"key_index"`
	KeyCount       int    `json:"key_count"`
	KeyID          string `json:"key_id"`
	KeyAlias       string `json:"key_alias"`
	KeyFingerprint string `json:"key_fingerprint"`
}

// Groups 返回计费分组名（来源 system_config.group_discount，与 vendor 正交）。
//
// 分组键集只有一个读取点：pricing.GroupNames()。这里若自己解析一遍 JSON，
// 两边对「什么算合法分组」的判定就会漂移（例如 pricing 丢弃 0 值键、这里不丢）。
// CountByAdapter 返回每个适配器被多少条渠道引用（适配器页与渠道下拉的唯一计数来源）。
//
// 后端算而不是前端从渠道列表里数：适配器页的「内置」分组必须与「我的」分组
// （数字来自 adapter_specs 的统计）用同一套口径，前端各数一遍迟早对不上。
func CountByAdapter() (map[string]int64, error) {
	var rows []struct {
		AdapterID string
		N         int64
	}
	err := db.DB.Model(&model.Channel{}).
		Select("adapter_id, COUNT(*) AS n").
		Group("adapter_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.AdapterID] = r.N
	}
	return out, nil
}

func Groups() []string {
	return pricing.GroupNames()
}

// ErrInvalidSort 排序字段不在白名单。
var ErrInvalidSort = errors.New("channel: 排序字段不支持")

// sortableColumns 是服务端排序白名单（值直接拼进 ORDER BY，故必须走白名单映射，
// 绝不能用请求里的字符串——那是 SQL 注入面）。
var sortableColumns = map[string]string{
	"id":         "id",
	"name":       "name",
	"priority":   "priority",
	"weight":     "weight",
	"status":     "status",
	"created_at": "created_at",
}

// ListQuery 是渠道列表的查询条件（B3：分页 + 排序同时下推）。
//
// Page=0 表示「不分页」，返回全部匹配行（上限 allPageSize）。这是给下拉/计数类
// 消费方保留的兼容路径：`/admin/channels` 同时服务于渠道管理页（分页）与适配器页
// 的渠道计数（全量）。分页页面必须显式传 Page。
type ListQuery struct {
	// Ctx 仅用于读取熔断滑窗（Redis）；为空时用 Background。
	Ctx     context.Context
	Keyword string
	Status  int
	Page    int
	Size    int
	Sort    string
	Order   string // asc | desc
}

// 分页上限：单页最多 100 行（约定与其余分页接口一致），不分页路径最多 5000 行。
const (
	maxPageSize = 100
	allPageSize = 5000
)

// List 返回渠道列表与匹配总数（keyword 命中 name/adapter_id；status>0 时按状态过滤）。
func List(q ListQuery, base string) ([]Item, int64, error) {
	// 每次调用都重新构建条件链：GORM 的 *gorm.DB 带 statement 状态，
	// 「先 Count 再 Find 复用同一个 db」在部分版本会带上上一次的 Select/Offset，
	// 计数与取数分叉是最难察觉的一类分页 bug。
	scope := func() *gorm.DB {
		qy := db.DB.Model(&model.Channel{})
		if kw := strings.TrimSpace(q.Keyword); kw != "" {
			qy = qy.Where("name ILIKE ? OR adapter_id ILIKE ?", "%"+kw+"%", "%"+kw+"%")
		}
		if q.Status > 0 {
			qy = qy.Where("status = ?", q.Status)
		}
		return qy
	}

	var total int64
	if q.Page > 0 {
		if err := scope().Count(&total).Error; err != nil {
			return nil, 0, err
		}
	}

	order, err := orderClause(q.Sort, q.Order)
	if err != nil {
		return nil, 0, err
	}

	rows := make([]model.Channel, 0)
	sel := scope().Order(order)
	if q.Page > 0 {
		size := q.Size
		if size <= 0 {
			size = 20
		}
		if size > maxPageSize {
			size = maxPageSize
		}
		sel = sel.Offset(pagination.OffsetOf(q.Page, size)).Limit(size)
	} else {
		sel = sel.Limit(allPageSize)
	}
	if err := sel.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	if q.Page == 0 {
		total = int64(len(rows))
	}

	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	bounds := boundModelsByChannel(ids, base)
	items := make([]Item, 0, len(rows))
	for i := range rows {
		items = append(items, toItem(&rows[i], bounds[rows[i].ID], base))
	}
	// P2-22：附带熔断滑窗（真实流量口径），让「为什么还没熔断 / 为什么已熔断」可解释。
	ctx := q.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	wins := channelhealth.WindowStats(ctx, ids)
	for i := range items {
		if w, ok := wins[items[i].ID]; ok {
			win := w
			items[i].Window = &win
		}
	}
	return items, total, nil
}

// orderClause 把请求里的排序字段/方向翻译成 ORDER BY 片段（白名单 + 兜底 id ASC）。
func orderClause(field, dir string) (string, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "id ASC", nil
	}
	col, ok := sortableColumns[field]
	if !ok {
		return "", fmt.Errorf("%w：%s", ErrInvalidSort, field)
	}
	if strings.EqualFold(strings.TrimSpace(dir), "desc") {
		return col + " DESC", nil
	}
	return col + " ASC", nil
}

// Create 新建渠道（至少 1 个密钥 + 至少 1 个分组）。
// ensureNameFree 校验渠道名未被占用（排除自身与已软删行：软删渠道的名字允许复用）。
// 为什么不用唯一索引：历史库里已存在同名渠道（实测 2 组），直接建唯一索引会失败并阻断启动；
// 应用层查重能在不改数据的前提下阻止新增重复。
func ensureNameFree(name string, selfID uint64) error {
	return ensureNameFreeTx(db.DB, name, selfID)
}

func ensureNameFreeTx(gdb *gorm.DB, name string, selfID uint64) error {
	var n int64
	q := gdb.Model(&model.Channel{}).Where("name = ?", name)
	if selfID > 0 {
		q = q.Where("id <> ?", selfID)
	}
	if err := q.Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w：%s", ErrNameExists, name)
	}
	return nil
}

func Create(req Req, base string) (*Item, error) { return CreateTx(db.DB, req, base) }

// CreateTx 在指定连接/事务上创建渠道（AI 提案 apply 用）。
//
// 只把 `db.DB` 换成参数，业务规则一行未改 —— 这一点是刻意的：
// 任何"顺手优化"都会让人工保存与 AI 应用走两条不同的校验路径。
func CreateTx(gdb *gorm.DB, req Req, base string) (*Item, error) {
	if err := req.normalize(); err != nil {
		return nil, err
	}
	if err := req.resolveIcon(gdb.Statement.Context); err != nil {
		return nil, err
	}
	if err := req.validateWith(gdb, true); err != nil {
		return nil, err
	}
	// 必须用 tx 版本：同一条提案里连建两个渠道时，走 db.DB 的重名检查
	// 看不到本事务刚插入的那一行，两个同名渠道会一起通过校验。
	if err := ensureNameFreeTx(gdb, req.Name, 0); err != nil {
		return nil, err
	}
	keys, err := EncryptKeys(req.Keys)
	if err != nil {
		return nil, err
	}
	groups, err := model.NewJSON(req.Groups)
	if err != nil {
		return nil, err
	}
	adapterConfig, err := normalizeAdapterConfig(req.AdapterConfig, req.AdapterID)
	if err != nil {
		return nil, err
	}
	adapterOptions, err := normalizeAdapterOptions(req.AdapterOptions, req.AdapterID)
	if err != nil {
		return nil, err
	}
	c := model.Channel{
		Name:           req.Name,
		Icon:           iconValue(req.Icon),
		AdapterID:      req.AdapterID,
		BaseURL:        req.BaseURL,
		Keys:           keys,
		MultiKeyMode:   req.MultiKeyMode,
		Groups:         groups,
		Weight:         req.Weight,
		Priority:       req.Priority,
		RPMLimit:       req.RPMLimit,
		MaxConcurrency: req.MaxConcurrency,
		Status:         StatusEnabled,
		AutoBan:        req.AutoBan,
		SwitchKeyRetry: req.SwitchKeyRetry,
		Health:         model.MustJSON(Health{}),
		AdapterConfig:  adapterConfig,
		AdapterOptions: adapterOptions,
	}
	// auto_ban 默认 true、weight 默认 1：GORM 不会把 false/0 写进 INSERT，
	// 且 Create 后会把列默认值回填到结构体——因此补写必须用 req 里的原始值。
	autoBan, weight, keyRetry := req.AutoBan, req.Weight, req.SwitchKeyRetry
	err = gdb.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&c).Error; err != nil {
			return err
		}
		return tx.Model(&model.Channel{}).Where("id = ?", c.ID).
			Updates(map[string]any{"auto_ban": autoBan, "weight": weight, "switch_key_retry": keyRetry}).Error
	})
	if err != nil {
		return nil, err
	}
	item := toItem(&c, nil, base)
	return &item, nil
}

// Update 整体替换渠道的可编辑字段（**不含密钥池**）。
//
// 密钥池只有一条写路径：`PatchKeys`（PATCH /admin/channels/:id/keys）。
// 原因：`keys` 与 `key_meta` 必须落在同一次 UPDATE 里，两个写者必然分叉成
// 两套「去重 + 属性重建 + 最后一把校验」——历史实现就是这样，
// 而"编辑页留空 = 不改"的语义又让人以为自己在追加，实际是整池替换。
func Update(id uint64, req Req, base string) (*Item, error) { return UpdateTx(db.DB, id, req, 0, base) }

// UpdateTx 在指定连接/事务上更新渠道；expectVersion>0 时做乐观锁（AI 提案 apply 用）。
//
// expectVersion==0 表示"无条件更新"：人工编辑页不传版本号（前端表单可能已经打开很久），
// 强行要求带版本会把"编辑十分钟后保存"变成一次失败的保存。
func UpdateTx(gdb *gorm.DB, id uint64, req Req, expectVersion int, base string) (*Item, error) {
	c, err := getTx(gdb, id)
	if err != nil {
		return nil, err
	}
	if err := req.normalize(); err != nil {
		return nil, err
	}
	if err := req.resolveIcon(gdb.Statement.Context); err != nil {
		return nil, err
	}
	if err := req.validateWith(gdb, false); err != nil {
		return nil, err
	}
	if err := ensureNameFreeTx(gdb, req.Name, id); err != nil {
		return nil, err
	}
	groups, err := model.NewJSON(req.Groups)
	if err != nil {
		return nil, err
	}
	// nil = 调用方没提供 → 保持原值（不能因为旧客户端不带该字段就把配置清掉）。
	// 显式传 null/{} 才表示清空。
	var adapterConfig model.JSON
	if req.AdapterConfig != nil {
		adapterConfig, err = normalizeAdapterConfig(req.AdapterConfig, req.AdapterID)
		if err != nil {
			return nil, err
		}
	}
	var adapterOptions model.JSON
	if req.AdapterOptions != nil {
		adapterOptions, err = normalizeAdapterOptions(req.AdapterOptions, req.AdapterID)
		if err != nil {
			return nil, err
		}
	}
	updates := map[string]any{
		"name":             req.Name,
		"adapter_id":       req.AdapterID,
		"base_url":         req.BaseURL,
		"multi_key_mode":   req.MultiKeyMode,
		"groups":           groups,
		"weight":           req.Weight,
		"priority":         req.Priority,
		"rpm_limit":        req.RPMLimit,
		"max_concurrency":  req.MaxConcurrency,
		"auto_ban":         req.AutoBan,
		"switch_key_retry": req.SwitchKeyRetry,
		// 所有写路径都要 version+1：否则提案的 CAS 永远成立，会静默覆盖他人改动。
		"version": gorm.Expr("version + 1"),
	}
	if req.AdapterConfig != nil {
		// 漏掉这行就是「编辑保存后配置不生效」——这是最容易漏的一处。
		updates["adapter_config"] = adapterConfig
	}
	if req.AdapterOptions != nil {
		// 同上：漏掉就是「改完工作流映射，保存后还是旧值」。
		updates["adapter_options"] = adapterOptions
	}
	if req.Icon != nil {
		// 同 adapter_config 一类坑：漏掉这行就是「换了图标，保存后还是旧的」。
		updates["icon"] = *req.Icon
	}
	// 换适配器必须清空私有配置。
	//
	// 旧适配器的键在新适配器解析时是未知字段（各适配器 DisallowUnknownFields），
	// 结果不是「这个键不生效」，而是**整段配置**解析失败 → 该渠道绑定的一切模型 5xx
	// （2026-11-04 多米渠道 12 个模型全挂即此形态，见 migrate_options.go）。
	// 所以无论调用方有没有传，adapter_id 变了就一律覆盖成 NULL —— 留着的旧配置一定是错的。
	if req.AdapterID != c.AdapterID {
		updates["adapter_config"] = nil
		updates["adapter_options"] = nil
	}
	q := gdb.Model(&model.Channel{}).Where("id = ?", id)
	if expectVersion > 0 {
		q = q.Where("version = ?", expectVersion)
	}
	res := q.Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if expectVersion > 0 && res.RowsAffected == 0 {
		// 版本不符 = 期间被别人改过。必须明确报冲突，否则人工/AI 都以为保存成功。
		return nil, ErrConflict
	}
	c, err = getTx(gdb, id)
	if err != nil {
		return nil, err
	}
	item := toItem(c, boundModelsByChannel([]uint64{id}, base)[id], base)
	return &item, nil
}

// Delete 删除渠道；仍被模型绑定时拒绝（对齐原型「有绑定模型拒绝删除」）。
func Delete(id uint64) error {
	if _, err := get(id); err != nil {
		return err
	}
	var n int64
	if err := db.DB.Model(&model.ModelBinding{}).Where("channel_id = ?", id).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return ErrBound
	}
	return db.DB.Delete(&model.Channel{}, id).Error
}

// SetStatus 手动启停（仅允许 1 启用 / 2 手动禁用；自动禁用由 S11 熔断写 3）。
func SetStatus(id uint64, status int, base string) (*Item, error) {
	return SetStatusTx(db.DB, id, status, base)
}

// SetStatusTx 在指定连接/事务上启停渠道（AI 提案 apply 用）。
func SetStatusTx(gdb *gorm.DB, id uint64, status int, base string) (*Item, error) {
	if status != StatusEnabled && status != StatusDisabled {
		return nil, fmt.Errorf("channel: 非法状态 %d", status)
	}
	c, err := getTx(gdb, id)
	if err != nil {
		return nil, err
	}
	if status == StatusEnabled {
		// 三种"启用了也一把都用不了"的形态必须在源头拦住，且文案要指向不同的动作：
		//
		// 放行的后果是一串无法自愈的连锁反应：每次请求都在 PickKey 处失败 →
		// 记渠道健康 → 10 次内自锁熔断（status=3）→ 模型从目录里消失；
		// 而熔断恢复探测同样要先拿到 key，于是永远探不通，只能人工改库。
		total, enabled, broken := keyStats(c)
		switch {
		case total == 0:
			return nil, &relay.ConfigError{Field: "keys", Scope: "渠道", Msg: "请先填写 API KEY 再启用"}
		case enabled == 0 && broken > 0:
			return nil, &relay.ConfigError{Field: "keys", Scope: "渠道",
				Msg: "全部密钥无法解密（加密主密钥可能已变更），请重新填写 API KEY 后再启用"}
		case enabled == 0:
			return nil, &relay.ConfigError{Field: "keys", Scope: "渠道", Msg: "全部密钥已被禁用，请至少启用一把再启用渠道"}
		}
	}
	if err := gdb.Model(&model.Channel{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "version": gorm.Expr("version + 1")}).Error; err != nil {
		return nil, err
	}
	c.Status = int16(status)
	item := toItem(c, boundModelsByChannel([]uint64{id}, base)[id], base)
	return &item, nil
}

// Test 执行连通测试（启用态与停用态都可测）。
//
// 为什么允许测停用渠道：运营的正常顺序是「填密钥 → 测通 → 再启用」。
// 要求先启用才能测，等于让一个自己都没验证过的渠道先接上流量；
// 而「不启用就不给测」还会把空 key 的配置问题藏到他点启用的那一刻。
func Test(ctx context.Context, id uint64) (*TestResult, error) {
	c, err := get(id)
	if err != nil {
		return nil, err
	}
	// 手动测试保持原语义：整池逐把试，用请求自己的 ctx（HTTP 超时会兜住）。
	return connectivity(ctx, c, 0, 0)
}

// Probe 执行连通探测（供熔断恢复 Worker 使用）：不做启用态前置校验，其余同 Test。
func Probe(ctx context.Context, id uint64) (*TestResult, error) {
	c, err := get(id)
	if err != nil {
		return nil, err
	}
	return connectivity(ctx, c, 0, 0)
}

// probeKeyTimeout 是熔断恢复探测的单次上游超时；ProbeRoundBudget 是一轮探测的总预算。
const (
	probeKeyTimeout  = 5 * time.Second
	ProbeRoundBudget = 2 * time.Minute
)

// probeMaxKeys 是一轮探测最多尝试的密钥数。
//
// 为什么不是 1 把：原来的 5s 总超时 + 只试第一把，等于「渠道一旦因为某把 key 失效
// 被熔断，就再也探不通」—— 熔断期间没有真实流量，那枚坏 key 的冷却早已过期，
// 于是一直排在第一位、一直被选中、一直失败，只能人工改库。
//
// 为什么不是全池：一把超时 5s，10 把全坏就是 50s，会白占一轮预算里的并发槽。
// 配合 scanEntries（冷却中的排后）与「鉴权失败即冷」，最坏 ceil(n/5)+1 轮收敛。
const probeMaxKeys = 5

// ProbeOne 用最多 probeMaxKeys 枚密钥探测渠道可达性。
func ProbeOne(ctx context.Context, id uint64) (*TestResult, error) {
	c, err := get(id)
	if err != nil {
		return nil, err
	}
	// 预算按「每把 5s × 最多 5 把」给：原来用 5s 罩整条，与"遍历可用 key"直接矛盾 ——
	// 第一把超时就把整轮判死，剩下的 key 一次都没试过。
	ctx, cancel := context.WithTimeout(ctx, probeKeyTimeout*probeMaxKeys)
	defer cancel()
	return connectivity(ctx, c, probeMaxKeys, probeKeyTimeout)
}

// connectivity 是 Test/Probe 共用主体：遍历密钥池测试并把结果落入 channels.health。
// maxKeys>0 时最多尝试 maxKeys 枚；perKey>0 时每枚单独限时（0 = 共用调用方 ctx）。
func connectivity(ctx context.Context, c *model.Channel, maxKeys int, perKey time.Duration) (*TestResult, error) {
	adapter, ok := relay.GetAdapter(c.AdapterID)
	if !ok {
		return nil, ErrNoAdapter
	}
	// **先过滤再截断**（顺序不能反）：先截断的话，maxKeys=1 的熔断恢复探测
	// 永远只拿到池子里第一把 —— 它可能正好是禁用/坏掉的那把，于是探测永远失败，
	// 渠道一旦熔断就再也恢复不了（只能人工改库）。
	usable := usableEntries(c)
	if len(usable) == 0 {
		// 必须让运营看到是哪种"没有可用密钥"（空/全禁用/全坏），
		// 这个错误在启用时会被再拦一次（SetStatusTx），两处文案必须指同一个动作。
		total, _, broken := keyStats(c)
		msg := "密钥池为空，请先填写 API KEY"
		switch {
		case total > 0 && broken > 0:
			msg = "全部密钥无法解密（加密主密钥可能已变更），请重新填写 API KEY"
		case total > 0:
			msg = "全部密钥已被禁用，请至少启用一把"
		}
		return nil, &relay.ConfigError{Field: "keys", Scope: "渠道", Msg: msg}
	}

	order := scanEntries(ctx, c.ID, usable, c.MultiKeyMode)
	if maxKeys > 0 && len(order) > maxKeys {
		order = order[:maxKeys]
	}
	start := time.Now()
	var lastErr error
	hit := -1
	var hitEntry channelkeys.Entry
	for _, e := range order {
		// 必须走统一构造函数：否则连通测试不走自定义鉴权头 / 路径覆盖，
		// 配了声明式配置的渠道会「测试一直失败」，运营会误判渠道坏了。
		cfg := relay.ChannelConfigFor(c, e.Plain)
		kctx, kcancel := ctx, context.CancelFunc(func() {})
		if perKey > 0 {
			kctx, kcancel = context.WithTimeout(ctx, perKey)
		}
		err := adapter.Test(kctx, cfg)
		kcancel()
		if err != nil {
			// 只有「上游明确说这枚 key 没权限」才冷它。
			//
			// 探活是熔断渠道唯一的观察者：不冷的话，一枚坏 key 会让每一轮都从它开始，
			// 试满 probeMaxKeys 把就整轮判死，渠道永不自愈。
			relay.CoolKeyIfAuth(c, relay.KeyRef{Key: e.Plain, Index: e.Index, ID: e.ID}, err)
			lastErr = err
			continue
		}
		// 探通 = 这枚 key 已被证明可用：清掉冷却与退避，让它立刻回到选路池。
		keyhealth.Clear(ctx, c.ID, e.ID)
		hit, hitEntry = e.Index, e
		break
	}

	// P2-10：健康快照是「读-改-写」的 JSONB，原先没有任何并发保护：
	// 手动测试与熔断探测 Worker 同时命中同一渠道时后写者会覆盖前者的计数
	// （表现为 fail_rate 忽大忽小、探测恢复判断被吃掉）。这里用行锁把更新串行化。
	if err := db.DB.Transaction(func(tx *gorm.DB) error {
		var row model.Channel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", c.ID).First(&row).Error; err != nil {
			return err
		}
		h := applyTestResult(decodeHealth(row.Health), lastErr, time.Now())
		return tx.Model(&model.Channel{}).Where("id = ?", c.ID).Update("health", model.MustJSON(h)).Error
	}); err != nil {
		return nil, err
	}

	res := &TestResult{OK: hit >= 0, KeyIndex: hit, KeyCount: keyCount(c.Keys),
		LatencyMS: time.Since(start).Milliseconds()}
	if res.OK {
		alias, _ := channelkeys.ParseMeta(c.KeyMeta).Effective(hitEntry.ID)
		res.KeyID, res.KeyAlias, res.KeyFingerprint = hitEntry.ID, alias,
			channelkeys.Fingerprint(hitEntry.Plain)
		name := alias
		if name == "" {
			name = "#" + strconv.Itoa(hit+1)
		}
		res.Message = fmt.Sprintf("连通正常（密钥 %s）", name)
	} else {
		res.Message = lastErr.Error()
	}
	return res, nil
}

// applyTestResult 是纯函数：按测试结果更新健康度快照（便于单测）。
func applyTestResult(h Health, err error, now time.Time) Health {
	h.LastTestAt = &now
	if err == nil {
		h.SuccessCount++
		h.LastError = ""
	} else {
		h.FailCount++
		h.LastError = err.Error()
	}
	if total := h.SuccessCount + h.FailCount; total > 0 {
		h.FailRate = math.Round(float64(h.FailCount)/float64(total)*10000) / 10000
	}
	return h
}

// usableEntries 返回可尝试的密钥条目（解密成功且未被禁用），保持原始顺序。
func usableEntries(c *model.Channel) []channelkeys.Entry {
	dec := channelkeys.Decrypt(c.Keys)
	meta := channelkeys.ParseMeta(c.KeyMeta)
	out := make([]channelkeys.Entry, 0, len(dec.Entries))
	for _, e := range dec.Entries {
		if !e.OK() {
			continue
		}
		if _, enabled := meta.Effective(e.ID); !enabled {
			continue
		}
		out = append(out, e)
	}
	return out
}

// scanEntries 给出探测/测试的尝试顺序。
//
// 两条规则，都是为了把有限的尝试次数花在更可能成功的 key 上：
//  1. 未冷却的排在前面 —— 冷却中的那几把大概率仍然是坏的（冷却正是"刚刚失败过"的标记）；
//  2. random 模式整体洗牌（与选路口径一致），polling 保持池序（与轮转口径一致）。
//
// 为什么不做游标轮转：那会引入第二份"轮到第几把"的状态，重启即丢，
// 而且与 relay.PickKey 的 Redis 游标形成两个真相源。靠"鉴权失败即冷却"
// 让坏 key 自己沉到队尾，是最小且收敛的做法。
func scanEntries(ctx context.Context, channelID uint64, entries []channelkeys.Entry,
	mode string) []channelkeys.Entry {
	if len(entries) < 2 {
		return entries
	}
	out := append([]channelkeys.Entry(nil), entries...)
	if mode == ModeRandom {
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	ids := make([]string, 0, len(out))
	for _, e := range out {
		ids = append(ids, e.ID)
	}
	cooling := keyhealth.CoolingState(ctx, channelID, ids)
	if len(cooling) == 0 {
		return out
	}
	// 稳定分区：保序，只把冷却中的挪到后面。
	warm := make([]channelkeys.Entry, 0, len(out))
	cold := make([]channelkeys.Entry, 0, len(cooling))
	for _, e := range out {
		if _, ok := cooling[e.ID]; ok {
			cold = append(cold, e)
			continue
		}
		warm = append(warm, e)
	}
	return append(warm, cold...)
}

// EncryptKeys 把密钥明文池加密为 Fernet token 数组（落 channels.keys）。
//
// 唯一实现在 pkg/channelkeys；这里保留薄包装，是为了让创建/更新/提案应用
// 三条写路径都只面对 channel 包，不必各自 import 加解密。
func EncryptKeys(keys []string) (model.JSON, error) { return channelkeys.Encrypt(keys) }

// DecryptKeys 返回**解密成功**的密钥明文（不含启用态过滤）。
//
// 为什么不过滤启用态：调用方（AI 上下文、dry-run、连通性测试）有的只
// Select 了 keys 列、没取 key_meta，按启用态过滤会得到"看起来全启用"的
// 错误结果。需要"可用数"时用 EnabledKeyCount，它显式要求在完整行上调用。
func DecryptKeys(raw model.JSON) []string {
	res := channelkeys.Decrypt(raw)
	out := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		if e.OK() {
			out = append(out, e.Plain)
		}
	}
	return out
}

func get(id uint64) (*model.Channel, error) { return getTx(db.DB, id) }

// getTx 在指定连接/事务上取渠道（tx 变体共用）。
func getTx(gdb *gorm.DB, id uint64) (*model.Channel, error) {
	var c model.Channel
	if err := gdb.Where("id = ?", id).First(&c).Error; err != nil {
		return nil, ErrNotFound
	}
	return &c, nil
}

func toItem(c *model.Channel, bounds []BoundModel, base string) Item {
	if bounds == nil {
		bounds = []BoundModel{}
	}
	// 并发占用：单副本读本机信号量，多副本读 Redis 共享计数（真值）。
	// 多副本下上限不在计数键里，由配置补 —— 否则列表页会显示 "12 / 0"。
	inflight, inflightLimit := channellimit.Inflight(c.ID)
	if inflightLimit == 0 {
		inflightLimit = c.MaxConcurrency
	}
	return Item{
		ID:              c.ID,
		Name:            c.Name,
		Icon:            c.Icon,
		IconURL:         iconref.URL(base, c.Icon),
		AdapterID:       c.AdapterID,
		BaseURL:         c.BaseURL,
		MultiKeyMode:    c.MultiKeyMode,
		Groups:          decodeStrings(c.Groups),
		Weight:          c.Weight,
		Priority:        c.Priority,
		RPMLimit:        c.RPMLimit,
		MaxConcurrency:  c.MaxConcurrency,
		Inflight:        inflight,
		InflightLimit:   inflightLimit,
		Status:          int(c.Status),
		AutoBan:         c.AutoBan,
		BalanceCNY:      c.BalanceCNY,
		KeyCount:        keyCount(c.Keys),
		KeyEnabledCount: func() int { _, n, _ := keyStats(c); return n }(),
		SwitchKeyRetry:  c.SwitchKeyRetry,
		Version:         c.Version,
		Health:          decodeHealth(c.Health),
		// 解析失败给零值：脏数据不该让列表页整页 500（运行期由 DeclErr fail-closed）。
		AdapterConfig: func() relay.AdapterConfig {
			decl, _ := relay.ParseAdapterConfig([]byte(c.AdapterConfig))
			return decl
		}(),
		// 私有配置原样回显（不含密钥）：脏数据回 null，避免前端拿到半截 JSON 崩掉。
		AdapterOptions: func() json.RawMessage {
			raw := bytes.TrimSpace([]byte(c.AdapterOptions))
			if len(raw) == 0 || string(raw) == "null" || !json.Valid(raw) {
				return nil
			}
			return json.RawMessage(raw)
		}(),
		BoundModels: bounds,
	}
}

// normalize 归一所有自由文本字段，并把密钥池收敛成「可直接落库」的形态。
//
// 密钥的三件事（逐条归一、去重、容量）必须在这里做完，而且**顺序固定**：
//  1. 归一在去重之前 —— `" sk-a"` 与 `"sk-a"` 只有 trim 后才相等；
//  2. 去重在校验容量之前 —— 反过来的话「101 行里有 2 行重复」会被误判超限。
//
// 放这里而不是 validateWith：validate 是值接收者（`r Req`），改不了 r.Keys，
// 而这三个动作都会改写池子本身。
func (r *Req) normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	r.AdapterID = strings.TrimSpace(r.AdapterID)
	r.BaseURL = strings.TrimSpace(r.BaseURL)
	if r.Icon != nil {
		v := strings.TrimSpace(*r.Icon)
		r.Icon = &v
	}
	if r.MultiKeyMode == "" {
		r.MultiKeyMode = ModeRandom
	}
	r.Groups = trimAll(r.Groups)
	r.Keys = dropEmpty(trimAll(r.Keys))
	for i, k := range r.Keys {
		norm, err := channelkeys.NormalizeKey(k)
		if err != nil {
			return fmt.Errorf("channel: 第 %d 个密钥：%w", i+1, err)
		}
		r.Keys[i] = norm
	}
	r.Keys, _ = channelkeys.Dedupe(r.Keys)
	if len(r.Keys) > channelkeys.MaxKeysPerChannel {
		return fmt.Errorf("channel: 密钥池最多 %d 把，当前 %d 把",
			channelkeys.MaxKeysPerChannel, len(r.Keys))
	}
	return nil
}

// resolveIcon 校验并归一图标 token（nil 保持 nil = 不修改）。
//
// 走 icon.Validate 而不是 brandicon.Normalize：后者只看形状，放行一个对象已被
// 清理的 upload:<ulid>，结果是「保存成功、对外 404」。三处写路径（模型/渠道/
// 供应商）共用同一个入口，才不会出现「管理台存得进、AI 提案存不进」这种分叉。
//
// ErrNotFound 与形状错误对外同为 ErrIconInvalid：用户视角的答案都是「重选一张」。
func (r *Req) resolveIcon(ctx context.Context) error {
	if r.Icon == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	v, err := iconsvc.Validate(ctx, *r.Icon)
	if err != nil {
		if errors.Is(err, brandicon.ErrInvalid) || errors.Is(err, iconsvc.ErrNotFound) {
			return ErrIconInvalid
		}
		return err
	}
	r.Icon = &v
	return nil
}

// iconValue 取 Req 的图标值（nil 视为空串，供新建用）。
func iconValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// configuredGroups 读取当前**实际配置**的分组折扣表（与计费侧同一真相源）。
//
// 返回 nil 表示「无真相源可比对」（配置未加载或 JSON 损坏），调用方据此跳过校验，
// 而不是把渠道判成非法——配置故障不该表现成「渠道保存不了」。
func configuredGroups() map[string]int64 { return pricing.ConfiguredGroupDiscounts() }

func (r Req) validate(isCreate bool) error { return r.validateWith(db.DB, isCreate) }

// validateWith 用指定连接做校验。
//
// 为什么要传 gdb：`adapter_id` 必须命中注册表，而规格渠道（spec:<slug>）在
// 同一事务里刚建、尚未提交时，relay 的解析器（查 db.DB）是看不见它的。
// 提案试跑与"建规格 + 立刻建渠道"都发生在同一事务里，用全局连接会误判成
// "适配器不存在"。
func (r Req) validateWith(gdb *gorm.DB, isCreate bool) error {
	if r.Name == "" || len([]rune(r.Name)) > 64 {
		return errors.New("channel: 渠道名称必填且不超过 64 字")
	}
	if !adapterIDPattern.MatchString(r.AdapterID) {
		return errors.New("channel: 适配器标识不合法")
	}
	// 适配器必须在保存期就命中注册表。
	//
	// 只校验格式（正则）是不够的：`builtin:openai` 这种写法格式合法但从未注册，
	// 保存后管理台显示"启用"，而每次选路 GetAdapter 都失败 → 该渠道的每个调用
	// 都是 503。这类配置错误无法在运行期自愈，只能在这里拦。
	if !AdapterResolvable(gdb, r.AdapterID) {
		return &relay.ConfigError{
			Field: "adapter_id",
			Msg:   "适配器不存在或未启用：" + r.AdapterID,
		}
	}
	// 出站地址统一走 netguard：拒非 http(s)、拒内嵌凭据、拒私网/回环（自建上游需 LD_UPSTREAM_ALLOW_PRIVATE=1）。
	if err := netguard.ValidateURL(r.BaseURL, netguard.PolicyFromEnv()); err != nil {
		return errors.New("channel: Base URL 不合法（需为 http/https 公网地址；自建内网需开启 LD_UPSTREAM_ALLOW_PRIVATE）")
	}
	if r.MultiKeyMode != ModePolling && r.MultiKeyMode != ModeRandom {
		return errors.New("channel: 多密钥调度模式不合法")
	}
	if len(r.Groups) == 0 {
		return errors.New("channel: 至少选择一个可用分组")
	}
	// 分组必须真的存在于 group_discount：选路按「模型分组 ∩ 渠道分组」命中折扣，
	// 存进一个不存在的分组名不会有任何报错，但这个渠道**永远不会被选中**——
	// 管理台显示「启用」，调用方却一直 503，是排障成本最高的一类配置错误。
	// 配置未加载（单测/极早期启动）时跳过：那时没有可比对的真相源，不该拦保存。
	if known := configuredGroups(); known != nil {
		for _, g := range r.Groups {
			if g == "" {
				return errors.New("channel: 分组名不能为空")
			}
			if _, ok := known[g]; !ok {
				return fmt.Errorf("channel: 分组 %q 不存在，请先在系统配置的分组折扣（group_discount）中添加", g)
			}
		}
	}
	if r.Weight < 0 || r.Priority < 1 || r.RPMLimit < 0 || r.MaxConcurrency < 0 {
		return errors.New("channel: 数值字段不合法（权重≥0、优先级≥1、上限≥0）")
	}
	if isCreate && len(r.Keys) == 0 {
		return errors.New("channel: 新建渠道至少填 1 个密钥")
	}
	// 声明式配置必须在保存期校验完：留到运行期会被记成渠道失败（可能自动熔断），
	// 而 JSONB 存进去不解析就永远不会报错，直到第一个真实请求。
	if len(r.AdapterConfig) > 0 {
		cfg, err := relay.ParseAdapterConfig(r.AdapterConfig)
		if err != nil {
			return err
		}
		if !cfg.IsZero() && !relay.SupportsDeclarativeConfig(r.AdapterID) {
			return &relay.ConfigError{
				Field: "adapter_config",
				Msg:   "适配器 " + r.AdapterID + " 不使用渠道级声明式配置（插件渠道请在插件清单里声明鉴权头与允许域名）",
			}
		}
	}
	// 私有配置同理：保存期必须校验完，否则"存进去了但没人读"或"第一次真实请求才报错"。
	if len(r.AdapterOptions) > 0 && !isClearingJSON(r.AdapterOptions) {
		if !relay.SupportsAdapterOptions(r.AdapterID) {
			return &relay.ConfigError{
				Scope: "渠道适配器配置错误",
				Field: "adapter_options",
				Msg:   "适配器 " + r.AdapterID + " 不使用渠道私有配置",
			}
		}
		if err := relay.ValidateAdapterOptions(r.AdapterID, r.AdapterOptions); err != nil {
			return err
		}
	}
	return nil
}

// isClearingJSON 判断原始 JSON 是否表示「清空」（null 或空对象）。
func isClearingJSON(raw json.RawMessage) bool {
	s := string(bytes.TrimSpace(raw))
	return s == "null" || s == "{}"
}

// normalizeAdapterConfig 解析校验并规范化，返回要落库的 JSONB 值（nil = 清空）。
func normalizeAdapterConfig(raw json.RawMessage, adapterID string) (model.JSON, error) {
	cfg, err := relay.ParseAdapterConfig(raw)
	if err != nil {
		return nil, err
	}
	if cfg.IsZero() {
		return nil, nil
	}
	if !relay.SupportsDeclarativeConfig(adapterID) {
		return nil, &relay.ConfigError{
			Field: "adapter_config",
			Msg:   "适配器 " + adapterID + " 不使用渠道级声明式配置（插件渠道请在插件清单里声明鉴权头与允许域名）",
		}
	}
	enc, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return model.JSON(enc), nil
}

// normalizeAdapterOptions 解析校验并规范化适配器私有配置，返回要落库的 JSONB 值（nil = 清空）。
//
// 与 normalizeAdapterConfig 的区别：形状完全由适配器定义，这里只做「能力门禁 + 交给适配器校验」，
// 不认识任何字段——否则每接一个厂商就要改一次通用层。
func normalizeAdapterOptions(raw json.RawMessage, adapterID string) (model.JSON, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isClearingJSON(raw) {
		return nil, nil
	}
	if !relay.SupportsAdapterOptions(adapterID) {
		return nil, &relay.ConfigError{
			Scope: "渠道适配器配置错误",
			Field: "adapter_options",
			Msg:   "适配器 " + adapterID + " 不使用渠道私有配置",
		}
	}
	if err := relay.ValidateAdapterOptions(adapterID, raw); err != nil {
		return nil, err
	}
	// 重新序列化：去掉多余空白/键序差异，保证落库后的字节与运行期解析结果稳定可 diff。
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &relay.ConfigError{Scope: "渠道适配器配置错误", Field: "adapter_options", Msg: "不是合法的配置对象：" + err.Error()}
	}
	enc, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return model.JSON(enc), nil
}

// boundModelsByChannel 按渠道 ID 批量取**全部绑定**（含停用；只查当前页，避免全表聚合）。
//
// 为什么不再过滤 enabled：删除渠道的拦截判据是「任意绑定（含停用）」，
// 只给启用绑定会让前端算出「0 个模型」却删不掉（后端 409），
// 且左栏「N 个模型」与右栏列表（含停用）口径不一致。启用数是前端从 Enabled 数的。
func boundModelsByChannel(channelIDs []uint64, base string) map[uint64][]BoundModel {
	var rows []struct {
		ChannelID     uint64
		ModelID       string
		UpstreamModel string
		Weight        int
		Enabled       bool
	}
	out := map[uint64][]BoundModel{}
	if len(channelIDs) == 0 {
		return out
	}
	err := db.DB.Model(&model.ModelBinding{}).
		Select("model_bindings.channel_id, model_bindings.model_id, model_bindings.upstream_model, model_bindings.weight, model_bindings.enabled").
		Where("model_bindings.channel_id IN ?", channelIDs).
		Order("model_bindings.channel_id ASC, model_bindings.model_id ASC").
		Scan(&rows).Error
	if err != nil {
		return out
	}
	modelIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		out[r.ChannelID] = append(out[r.ChannelID], BoundModel{
			Model: r.ModelID, Alias: r.UpstreamModel, Weight: r.Weight, Enabled: r.Enabled,
		})
		modelIDs = append(modelIDs, r.ModelID)
	}
	// 图标走 iconindex 而不是在 SQL 里 JOIN models+vendors：回落规则只在那里一份。
	// 手写 JOIN 也能出图，但"模型没自设图标时显示供应商图标"这条一旦在第二处实现，
	// 两边的 COALESCE 条件迟早不一致，表现是"模型表格有图、渠道绑定行是首字"。
	// 这里没有 ctx 可传（本函数与三处调用点的既有签名一致），一次小 IN 查询不受影响。
	icons := iconindex.Resolve(context.Background(), modelIDs, nil)
	for ch, list := range out {
		for i := range list {
			list[i].IconURL = iconref.URL(base, icons.Model(list[i].Model))
		}
		out[ch] = list
	}
	return out
}

func keyCount(raw model.JSON) int {
	if len(raw) == 0 {
		return 0
	}
	var tokens []string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return 0
	}
	return len(tokens)
}

func decodeHealth(raw model.JSON) Health {
	h := Health{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &h)
	}
	return h
}

func decodeStrings(raw model.JSON) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	return out
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func dropEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

/* ------------------------------ 出厂值 / 增量更新 / 适配器解析 ------------------------------ */

// Defaults 是新建渠道的出厂值。
//
// 为什么必须只有一个来源：管理台与 AI 编排各写一份字面量时，
// "AI 建出来的渠道"与"人建出来的渠道"会悄悄变成两种东西
// （历史事故：AI 侧用 1/1/0/0，保存后把 rpm 从 60 重置成 0，等于取消了限流）。
func Defaults() Req {
	return Req{
		Weight:         5,
		Priority:       1,
		RPMLimit:       60,
		MaxConcurrency: 10,
		AutoBan:        true,
		MultiKeyMode:   ModeRandom,
	}
}

// Patch 是「只改提到的字段」的渠道更新入参。
//
// 指针的意义：nil = 本次不改（沿用库内现值），显式 0 = 真的要写 0。
// 用值类型做不到这件事：`rpm_limit: 0` 是合法的"不限流"，与"没提这个字段"
// 必须能区分，否则要么改不了限流、要么每次保存都被重置。
type Patch struct {
	Name         *string
	Icon         *string
	AdapterID    *string
	BaseURL      *string
	MultiKeyMode *string
	Groups       *[]string
	// Keys 只为"显式拒绝"而保留：`Merge` 见到它就报错，指向密钥池接口。
	// 删掉字段会让调用方以为改成功了（静默忽略），留一个错误更诚实。
	Keys           *[]string
	Weight         *int
	Priority       *int
	RPMLimit       *int
	MaxConcurrency *int
	AutoBan        *bool
	// SwitchKeyRetry：nil = 不改。
	SwitchKeyRetry *bool
	// AdapterConfig / AdapterOptions：nil = 不改；显式 null/{}= 清空。
	AdapterConfig  json.RawMessage
	AdapterOptions json.RawMessage
}

// Merge 用库内现值补齐未提交字段，产出 UpdateTx 需要的完整 Req。
func (p Patch) Merge(cur *model.Channel) (Req, error) {
	req := Req{
		Name: cur.Name, AdapterID: cur.AdapterID, BaseURL: cur.BaseURL,
		MultiKeyMode: cur.MultiKeyMode,
		Weight:       cur.Weight, Priority: cur.Priority,
		RPMLimit: cur.RPMLimit, MaxConcurrency: cur.MaxConcurrency,
		AutoBan:        cur.AutoBan,
		SwitchKeyRetry: cur.SwitchKeyRetry,
		AdapterConfig:  json.RawMessage(cur.AdapterConfig),
	}
	// Icon 必须先从库里复制：UpdateTx 只在非 nil 时写，看起来"不复制也行"，
	// 但一旦哪天它改成全列写，nil 就会把图标静默清空 —— 与 display_name 的
	// 历史事故同一形态。复制后 req 始终是完整快照，语义不依赖下游实现。
	curIcon := cur.Icon
	req.Icon = &curIcon
	req.AdapterOptions = json.RawMessage(cur.AdapterOptions)
	if len(cur.Groups) > 0 {
		var g []string
		if err := json.Unmarshal(cur.Groups, &g); err == nil {
			req.Groups = g
		}
	}
	if p.Name != nil {
		req.Name = *p.Name
	}
	if p.Icon != nil {
		req.Icon = p.Icon
	}
	if p.AdapterID != nil {
		req.AdapterID = *p.AdapterID
	}
	if p.BaseURL != nil {
		req.BaseURL = *p.BaseURL
	}
	if p.MultiKeyMode != nil {
		req.MultiKeyMode = *p.MultiKeyMode
	}
	if p.Groups != nil {
		req.Groups = *p.Groups
	}
	if p.Keys != nil {
		// 保留字段名而不删，是为了让"传了 keys"得到一句明确的错误，而不是被静默忽略：
		// 静默忽略会让 AI 提案显示"改成功"、密钥池却一行没变。
		return Req{}, &relay.ConfigError{
			Field: "keys", Scope: "渠道",
			Msg: "更新不支持修改密钥池；请用密钥池接口（PATCH /admin/channels/:id/keys）",
		}
	}
	if p.Weight != nil {
		req.Weight = *p.Weight
	}
	if p.Priority != nil {
		req.Priority = *p.Priority
	}
	if p.RPMLimit != nil {
		req.RPMLimit = *p.RPMLimit
	}
	if p.MaxConcurrency != nil {
		req.MaxConcurrency = *p.MaxConcurrency
	}
	if p.AutoBan != nil {
		req.AutoBan = *p.AutoBan
	}
	if p.SwitchKeyRetry != nil {
		req.SwitchKeyRetry = *p.SwitchKeyRetry
	}
	if p.AdapterConfig != nil {
		merged, err := mergeAdapterConfig(json.RawMessage(cur.AdapterConfig), p.AdapterConfig)
		if err != nil {
			return Req{}, err
		}
		req.AdapterConfig = merged
	}
	if p.AdapterOptions != nil {
		req.AdapterOptions = p.AdapterOptions
	}
	// 换适配器但没重新给声明式配置：旧配置可能不被新适配器支持。
	// 静默保留会让"配置在库里、运行期没人读"，比直接报错更难查。
	if p.AdapterID != nil && *p.AdapterID != cur.AdapterID && p.AdapterConfig == nil {
		if len(bytes.TrimSpace(cur.AdapterConfig)) > 0 && !relay.SupportsDeclarativeConfig(req.AdapterID) {
			return Req{}, &relay.ConfigError{
				Field: "adapter_config",
				Msg:   "新适配器不使用渠道级声明式配置，请显式清空 adapter_config（传 null）后再切换",
			}
		}
	}
	return req, nil
}

// mergeAdapterConfig 把增量补丁**按顶层键**合并进现值（path_overrides 按键合并）。
//
// 为什么不是整体替换：patch 语义的调用方（AI 提案）只提交要改的字段，
// 整体替换会顺手抹掉没提到的 auth_prefix / path_overrides ——
// 而工具描述明确写着"只提交要改的字段"。显式传 null 才是清空。
func mergeAdapterConfig(cur, patch json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(patch)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(trimmed), nil
	}
	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &patchMap); err != nil {
		return nil, &relay.ConfigError{Field: "adapter_config", Msg: "adapter_config 必须是 JSON 对象"}
	}
	merged := map[string]json.RawMessage{}
	if base := bytes.TrimSpace(cur); len(base) > 0 && !bytes.Equal(base, []byte("null")) {
		_ = json.Unmarshal(base, &merged)
	}
	for k, v := range patchMap {
		// path_overrides 是"标准路径 → 上游路径"的字典：按键合并才有意义，
		// 整体覆盖会让"只加一条路径"变成"删掉其它所有路径"。
		if k == "path_overrides" {
			merged[k] = mergeStringMap(merged[k], v)
			continue
		}
		merged[k] = v
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// mergeStringMap 按键合并两个 JSON 字符串字典（cur 可以是 nil/非对象）。
func mergeStringMap(cur, patch json.RawMessage) json.RawMessage {
	base := map[string]string{}
	if len(cur) > 0 {
		_ = json.Unmarshal(cur, &base)
	}
	if base == nil {
		base = map[string]string{}
	}
	add := map[string]string{}
	if err := json.Unmarshal(patch, &add); err != nil {
		return patch
	}
	for k, v := range add {
		base[k] = v
	}
	b, err := json.Marshal(base)
	if err != nil {
		return patch
	}
	return b
}

// PatchTx 是增量更新：读现值 → 合并 → 走与人工保存完全相同的 UpdateTx。
func PatchTx(gdb *gorm.DB, id uint64, p Patch, expectVersion int, base string) (*Item, error) {
	cur, err := getTx(gdb, id)
	if err != nil {
		return nil, err
	}
	req, err := p.Merge(cur)
	if err != nil {
		return nil, err
	}
	return UpdateTx(gdb, id, req, expectVersion, base)
}

// AdapterResolvable 判断 adapter_id 是否对应一个可用的适配器。
//
// 规格形态（spec:<slug>）必须查**传入的连接**：同一提案里"建规格 + 建渠道"
// 在提交前 relay 的解析器（走 db.DB）看不到新规格，用全局连接会把正确配置判成
// "适配器不存在"。内置适配器则查内存注册表。
//
// 刻意不用 relay.GetAdapter：它会把失败写进 5s 负缓存，下一次重试会继续误判。
//
// 对 spec:<slug> 额外取 FOR SHARE 行锁：与 specsvc.Delete 的 FOR UPDATE 互斥，
// 保证「保存渠道」与「删除规格」不会交错出一个指向已删规格的渠道。
// 未在事务里时这条锁随语句结束释放，无副作用。
func AdapterResolvable(gdb *gorm.DB, adapterID string) bool {
	adapterID = strings.TrimSpace(adapterID)
	if adapterID == "" {
		return false
	}
	if slug, ok := specsvc.SlugOf(adapterID); ok {
		if gdb == nil {
			gdb = db.DB
		}
		var row model.AdapterSpec
		err := gdb.Clauses(clause.Locking{Strength: "SHARE"}).
			Select("id").
			Where("slug = ? AND status = ?", slug, model.SpecStatusEnabled).
			Limit(1).Find(&row).Error
		return err == nil && row.ID != 0
	}
	return relay.CanResolve(adapterID)
}
