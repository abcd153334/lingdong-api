// Package agent 是管理台 AI 编排的服务层：提案（plan）的解析/校验/差异、
// 单事务 apply、快照回滚、LLM 调用与工具集。
//
// 边界（改之前先读）：
//   - **AI 不写生产表**。它只能产出 plan，plan 经人工确认后由本包的 apply 落库；
//   - **plan 的合法性由服务端判定**，不依赖模型自觉（Prompt 只是提示，不是约束）；
//   - 所有对生产表的写入都走各 service 的 Tx 变体，保证人工保存与 AI 应用同一条校验路径。
package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"lingdong-api/internal/model"

	"lingdong-api/internal/apidoc"
	specdsl "lingdong-api/internal/relay/spec"
	"lingdong-api/internal/service/modelcfg"
)

// 操作与实体枚举（AI 能表达的**全部**动作）。
//
// 刻意没有 delete：删除渠道/模型是不可逆的（绑定、历史调用记录都会失去归属），
// 属于"人来做更合适"的操作。缺位比提示词里写"请不要删除"可靠得多。
const (
	OpCreate  = "create"
	OpUpdate  = "update"
	OpDisable = "disable"
	OpReplace = "replace"

	EntityVendor        = "vendor"
	EntityAdapterSpec   = "adapter_spec"
	EntityChannel       = "channel"
	EntityModel         = "model"
	EntityModelBindings = "model_bindings"
)

// EntityDoc 只用于**入口作用域**，不是提案实体。
//
// 文档工具直接写草稿、不产生提案，所以它没有 create/update 操作，也不会出现在
// plan.Operations 里；但「从哪个入口进来」同样要限制它 —— 从「新建适配器」弹窗
// 进来的会话不该顺手改写对外文档。词表和 EnforceScope 放在一起，是为了让
// 「这次点击允许碰什么」只有一处定义。
const EntityDoc = "doc"

// entityOrder 是 apply 的**固定执行顺序**（依赖方向）。
//
// 为什么不信任 AI 给的顺序：模型必须在其绑定的渠道之后建、绑定必须在模型之后写。
// 让模型自己排顺序，10 次里有 1 次是错的，而错的那次会以"外键/引用不存在"的形式失败，
// 运营完全看不出原因。
var entityOrder = map[string]int{
	EntityVendor:        0,
	EntityAdapterSpec:   1,
	EntityChannel:       2,
	EntityModel:         3,
	EntityModelBindings: 4,
}

// Plan 是 AI 产出的唯一形状（写入 agent_proposals.plan）。
type Plan struct {
	// Summary 是一句话说明（展示在提案卡片上，也是运营判断的主要依据）。
	Summary string `json:"summary"`
	// DocIDs 是本次依据的上游文档（可追溯"照哪份文档写的"）。
	DocIDs []uint64 `json:"doc_ids,omitempty"`
	// Operations 是无序的：服务端按 entityOrder 重排后再执行。
	Operations []Operation `json:"operations"`
	// Unsupported 只出现在 kind=unsupported 的提案里（L3 结论）。
	//
	// 为什么必须进 Plan 结构而不是"随手塞几个顶层字段"：
	// DecodePlan 对未知键是严格拒绝的，历史上 submit_unsupported 自己拼了
	// vendor/reasons/required_features 三个顶层键 → 100% 被自己的解码器拒掉，
	// 于是"判到 L3"这条路从来没成功过（AI 只能反复重试或放弃）。
	Unsupported *UnsupportedPlan `json:"unsupported,omitempty"`
}

// 提案种类（agent_proposals.kind）。
const (
	ProposalKindChange      = "change"
	ProposalKindUnsupported = "unsupported"
)

// UnsupportedPlan 是「该上游必须写内置适配器」的结论正文。
type UnsupportedPlan struct {
	// Vendor 是厂商标识（vendors.id 或名字），用于运营归组与后续排期。
	Vendor string `json:"vendor"`
	// Reasons 是判定 L3 的具体原因（每条都要能让工程师复现）。
	Reasons []string `json:"reasons"`
	// RequiredFeatures 是要新增的平台能力（写入适配器规格书的要点）。
	RequiredFeatures []string `json:"required_features,omitempty"`
}

// Operation 是一条变更。
type Operation struct {
	Op     string `json:"op"`
	Entity string `json:"entity"`
	// Key 是提案内的**本地引用名**（create 必填）：绑定用 channel_key 指向它。
	Key string `json:"key,omitempty"`
	// ID 是已存在实体的主键（update / disable 必填）。
	ID uint64 `json:"id,omitempty"`
	// ExpectVersion 是 AI 看到的版本号：apply 时与库内比对，不一致 → stale。
	ExpectVersion int `json:"expect_version,omitempty"`
	// Model / ChannelID 是 disable 针对的绑定目标。
	Model     string `json:"model,omitempty"`
	ChannelID uint64 `json:"channel_id,omitempty"`
	// Data 是实体载荷（按 entity 用 strict 结构解析，未知键直接拒绝）。
	Data json.RawMessage `json:"data,omitempty"`
}

/* ------------------------------ 实体载荷 ------------------------------ */

// VendorData 对应 vendors 表可写字段。
type VendorData struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	// Icon 是图标 token（="" | builtin:<key>）。供应商是人工运营出来的品牌条目，
	// AI 新建供应商时通常知道对应厂商（火山、快手…），带上图标省一次人工补录。
	// 不收 upload:<id>：上传是人工在管理台做的动作，AI 手上不会有 id。
	Icon string `json:"icon,omitempty"`
}

// AdapterSpecData 对应 adapter_specs 可写字段。
type AdapterSpecData struct {
	Label  string          `json:"label"`
	Vendor string          `json:"vendor,omitempty"`
	Spec   json.RawMessage `json:"spec"`
}

// ChannelData 对应渠道可写字段。
//
// 刻意不含 adapter_config / adapter_options：AI 写声明式补丁的能力意味着
// 它能改变"请求长什么样"，而那是 L1 的手工配置区，出错极难排查。
// 需要这两项时应写 adapter_spec（L2），让规则可校验、可 diff。
type ChannelData struct {
	Name           string   `json:"name"`
	AdapterID      string   `json:"adapter_id"`
	BaseURL        string   `json:"base_url"`
	Keys           []string `json:"keys,omitempty"`
	Groups         []string `json:"groups,omitempty"`
	Weight         *int     `json:"weight,omitempty"`
	Priority       *int     `json:"priority,omitempty"`
	MultiKeyMode   string   `json:"multi_key_mode,omitempty"`
	RPMLimit       *int     `json:"rpm_limit,omitempty"`
	MaxConcurrency *int     `json:"max_concurrency,omitempty"`
	AutoBan        *bool    `json:"auto_ban,omitempty"`
	// AdapterConfig 只开放三个字段（L1 改路径/鉴权头）。
	//
	// request_patch / response_path 仍然只能由人工配置：它们能改变"请求长什么样"，
	// 出错极难排查，而收益（接一个非标准上游）远小于风险。
	AdapterConfig *AdapterConfigPatch `json:"adapter_config,omitempty"`
	// Icon 是图标 token（"" | builtin:<key>）；nil = 不改，"" = 清空回落到首字。
	// 与 name 的区别：name 用零值表达"没提交"（空名字本来就非法），
	// 而图标空串是**合法值**，所以必须用指针。
	Icon *string `json:"icon,omitempty"`
}

// AdapterConfigPatch 是渠道级声明式配置的可写子集。
type AdapterConfigPatch struct {
	AuthHeader    string            `json:"auth_header,omitempty"`
	AuthPrefix    string            `json:"auth_prefix,omitempty"`
	PathOverrides map[string]string `json:"path_overrides,omitempty"`
}

// ModelData 对应 models 可写字段。
//
// DisplayName / Description 用指针 + omitempty：nil = 这条提案没提这个字段
// （沿用库内）。2026-10-29 审计发现"只改价格"的提案会把展示名与描述写成空串，
// 与人工 PUT 是同一个根因 —— 两条路径都必须能表达"未提交"。
type ModelData struct {
	DisplayName *string `json:"display_name,omitempty"`
	Vendor      string  `json:"vendor"`
	// Modality / BillingMode 在 update 时可以省略（沿用库内）。
	// 用零值表达"没提交"是不够的：省略与"显式写 tokens"必须能区分，
	// 否则一条"只改价格"的提案会把按次模型静默改成按量 —— 永久 0 元。
	Modality    string   `json:"modality,omitempty"`
	BillingMode *string  `json:"billing_mode,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	Endpoints   []string `json:"endpoints,omitempty"`
	// Capabilities 是模型能力（`apidoc.Capabilities()` 的封闭词表，如 t2v/i2v/ref_audio）。
	//
	// 对外文档右栏的「预设 TAB」（文生视频 / 图生视频 / 多模态）全靠它；
	// 不填 = 空能力 = 只出该模态最基础的预设，**不阻断创建**（文档少一个 TAB
	// 不该让建模型这件事失败）。填了就必须在词表内且与模态匹配。
	Capabilities []string `json:"capabilities,omitempty"`
	// SpecPrices 是按规格计价的价目（结构见 model.SpecPrices，单位**微**）。
	// 严格解码：旧键名（*_quota 后缀）会直接报错，不会静默按"未配置"计费。
	SpecPrices model.SpecPrices `json:"spec_prices,omitempty"`
	// 以下九条 lane 与 models.prices 一一对应，单位**微**（1 单位货币 = 1e6 微）。
	CallPrice          *int64  `json:"call_price,omitempty"`
	InputPrice         *int64  `json:"input_price,omitempty"`
	OutputPrice        *int64  `json:"output_price,omitempty"`
	CachePrice         *int64  `json:"cache_price,omitempty"`
	CacheCreatePrice   *int64  `json:"cache_create_price,omitempty"`
	CacheCreatePrice1h *int64  `json:"cache_create_price_1h,omitempty"`
	ImagePrice         *int64  `json:"image_price,omitempty"`
	AudioPrice         *int64  `json:"audio_price,omitempty"`
	AudioOutputPrice   *int64  `json:"audio_output_price,omitempty"`
	Description        *string `json:"description,omitempty"`
	// Icon 是图标 token（"" | builtin:<key> | upload:<ulid>）。
	//
	// 指针语义与 DisplayName 同：省略 = 不改（沿用库内），"" = 清空（回落到
	// 供应商图标 / 首字）。不给它加这一项，AI 就永远无法设置图标 ——
	// 而 strictUnmarshal 会拒未知键，AI 也学不会"这个字段可以提"。
	Icon *string `json:"icon,omitempty"`
	// ContextWindow / MaxOutputTokens 是模型级覆盖（0 = 继承助手全局设置）。
	// 指针：省略 = 不改；显式 0 = 改回"继承"。
	ContextWindow   *int `json:"context_window,omitempty"`
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	// Tags 是营销标签（数组顺序即展示顺序，最多 3 个）。
	//
	// **必须是指针**：nil = 提案没提（沿用库内），`&[]` = 显式清空。
	// 用切片 + omitempty 时，回滚快照里"标签为空"会被省略，回滚后标签原样
	// 留在库里，而界面显示「已回滚」—— 这就是"看起来对、落地会错"。
	Tags *[]model.ModelTag `json:"tags,omitempty"`
}

// BindingData 对应 model_bindings 的整体替换。
type BindingData struct {
	Bindings []BindingItem `json:"bindings"`
}

// BindingItem 是一条绑定。
//
// ChannelKey 指向**同一提案内** create channel 的 Key；ChannelID 指向已存在的渠道。
// 两者必须给一个且只能给一个：都给会让"到底绑到哪个渠道"变成歧义。
type BindingItem struct {
	ChannelKey    string `json:"channel_key,omitempty"`
	ChannelID     uint64 `json:"channel_id,omitempty"`
	UpstreamModel string `json:"upstream_model"`
	Weight        *int   `json:"weight,omitempty"`
	Priority      *int   `json:"priority,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

/* ------------------------------ 解析与哈希 ------------------------------ */

// DecodePlan 严格解析 plan：未知键、类型不符都报错。
//
// 为什么必须 DisallowUnknownFields：AI 极擅长"发明"看起来合理的字段名
// （`price`、`per_image_price`、`channel`）。静默忽略它们的后果是提案显示"已配置价格"，
// 实际什么都没写 —— 运营按 diff 确认后才发现线上没价。
func DecodePlan(raw []byte) (*Plan, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("提案内容为空")
	}
	// 先做一次"未知键"翻译：模型把 slug 写在 plan 顶层时，
	// 原文 `json: unknown field "slug"` 既不说是哪一层、也不说允许什么。
	if fe := unknownFieldsIn(raw, reflect.TypeOf(Plan{}), "plan", ""); fe != nil {
		return nil, fmt.Errorf("%s：%s", fe.Path, fe.Message())
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("提案不是合法的 JSON 结构：%v", err)
	}
	return &p, nil
}

// CanonicalJSON 返回 key 有序、数字保持原样文本的规范 JSON。
//
// 用途是 plan_hash：前端回传的 hash 与服务端算出的必须逐字节一致，
// 而 map 遍历顺序在 Go 里是随机的 → 必须先规范化。
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var anyV any
	if err := dec.Decode(&anyV); err != nil {
		return nil, err
	}
	// encoding/json 对 map[string]any 输出时按 key 排序，因此再编一次即得规范形式。
	return json.Marshal(anyV)
}

// PlanHash 是 plan 的稳定指纹。
func PlanHash(p *Plan) (string, error) {
	b, err := CanonicalJSON(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

/* ------------------------------ 校验 ------------------------------ */

// Checker 是校验时需要问的库内事实（接口化便于单测不连库）。
type Checker interface {
	VendorExists(id string) (bool, error)
	ChannelNameExists(name string) (bool, error)
	ChannelExists(id uint64) (bool, error)
	ModelExists(id string) (bool, error)
	ModelVersion(id string) (int, error)
	ChannelVersion(id uint64) (int, error)
	AdapterSpecExists(slug string) (bool, error)
	// AdapterSpecRefs 返回引用该 slug 的渠道数。
	AdapterSpecRefs(slug string) (int64, error)
	// BoundEndpoints 返回某模型当前绑定的渠道所支持的端点键集合（规则 10 用）。
	BoundEndpoints(modelID string) (map[string]bool, error)
	// ModelEndpoints 返回模型当前声明的端点（update 时未给 endpoints 的兜底）。
	ModelEndpoints(modelID string) ([]string, error)
}

// ValidationError 是带定位的提案校验错误（前端可直接高亮某个 op）。
type ValidationError struct {
	// Index 是 operations 下标（-1 表示整体错误）。
	Index int
	// Field 是字段路径（如 `operations[0].data.keys`）。
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + "：" + e.Message
}

func vErr(idx int, field, format string, args ...any) *ValidationError {
	return &ValidationError{Index: idx, Field: field, Message: fmt.Sprintf(format, args...)}
}

// entityErr 把严格解析失败翻成带定位的校验错误。
//
// FieldSpecError 自带字段路径（data.slug / data.bindings[0]）与允许字段，
// 直接拿它当 Field：模型据此只改一个字段，而不是整份重写。
func entityErr(i int, path string, err error) error {
	if fe := asFieldSpecError(err); fe != nil {
		p := fe.Path
		if p == "" {
			p = path
		}
		return vErr(i, p, "%s", fe.Message())
	}
	return vErr(i, path, "%v", err)
}

// ValidateStructure 执行**不查库**的结构规则。
//
// 语义规则（适配器可解析、vendor 存在、绑定完整性、负价、端点覆盖、墓碑、
// 折扣与版本的库内事实）全部交给 CreateProposal 里的 trialApply：
// 在同一事务里真跑一遍写库路径再回滚。为什么不再抄一份规则到校验器里——
// 两处规则一旦分叉，就回到"提案说合法、应用后报错"，而且没人知道哪份是对的。
//
// kind 决定正文形状：change 必须是操作清单，unsupported 必须是结论块。
// 两种形态共用同一个结构体与同一条解码路径，是为了只保留一个真相源 ——
// 历史上 L3 结论走的是"另一个拼装函数"，它拼出的 JSON 从来没通过过解码。
// StructureOptions 控制结构校验的严格度（只有一处需要放宽，见 SkipCrossOp）。
type StructureOptions struct {
	// SkipCrossOp 跳过「跨操作」规则：
	//   ① 新建模型必须同时给出 model_bindings；
	//   ② channel_key 必须指向本提案内 create 的渠道。
	//
	// 这两条天生要求"全部操作同时在场"，所以分块校验（mode=shape）必须先跳过它们，
	// 否则第一块只写 spec 时就永远过不去。**跳过不等于放宽**：完整预演与
	// propose_change 仍然跑 Strict（默认零值），跨操作规则一条不少。
	SkipCrossOp bool
}

func (p *Plan) ValidateStructure(kind string) error {
	return p.ValidateStructureWith(kind, StructureOptions{})
}

// ValidateStructureWith 是带选项的版本（shape 分块校验用 SkipCrossOp）。
//
// 三个调用点各自显式选边，不给默认值留漂移空间：
//   - propose_change / apply：Strict（零值）
//   - validate_draft(mode=full)：Strict
//   - validate_draft(mode=shape)：SkipCrossOp
func (p *Plan) ValidateStructureWith(kind string, opt StructureOptions) error {
	if len(strings.TrimSpace(p.Summary)) == 0 {
		return vErr(-1, "summary", "必须用一句话说明这次变更")
	}
	if kind == ProposalKindUnsupported {
		if p.Unsupported == nil {
			return vErr(-1, "unsupported", "unsupported 提案必须给出 unsupported 块（vendor + reasons）")
		}
		if strings.TrimSpace(p.Unsupported.Vendor) == "" {
			return vErr(-1, "unsupported.vendor", "请填写厂商标识（vendors.id 或名称）")
		}
		if len(p.Unsupported.Reasons) == 0 {
			return vErr(-1, "unsupported.reasons", "至少给一条判定 L3 的具体原因")
		}
		if len(p.Operations) > 0 {
			return vErr(-1, "operations", "unsupported 提案不能同时带变更操作")
		}
		return nil
	}
	if p.Unsupported != nil {
		return vErr(-1, "unsupported", "只有 kind=unsupported 的提案才能带 unsupported 块")
	}
	if len(p.Operations) == 0 {
		return vErr(-1, "operations", "提案至少包含一个操作")
	}

	seen := map[string]bool{}
	createdModels := map[string]bool{}   // 本提案内新建的模型
	boundReplaced := map[string]bool{}   // 本提案内替换过绑定的模型
	createdChannels := map[string]bool{} // 本提案内新建的渠道 Key
	specs := map[string]*specdsl.Spec{}  // 本提案内新建的规格（slug → 规格，供 apply 复用）

	for i := range p.Operations {
		op := &p.Operations[i]
		// 规则 1：op / entity 枚举
		switch op.Op {
		case OpCreate, OpUpdate, OpDisable, OpReplace:
		default:
			return vErr(i, fmt.Sprintf("operations[%d].op", i), "不支持的操作 %q（只能是 create/update/disable/replace）", op.Op)
		}
		if _, ok := entityOrder[op.Entity]; !ok {
			return vErr(i, fmt.Sprintf("operations[%d].entity", i), "不支持的实体 %q", op.Entity)
		}

		// 规则 2：各 op 的必填项
		//
		// 注意 ID 的形态随实体而变：channels / adapter_specs 是自增主键（数字），
		// 而 models / vendors 的主键**就是名字**（string）。因此这两类实体的定位键是 Key，
		// 不是 ID —— 让 AI 去猜数字主键是必错的。
		numericID := op.Entity == EntityChannel || op.Entity == EntityAdapterSpec
		identity := ""
		switch op.Op {
		case OpCreate:
			if strings.TrimSpace(op.Key) == "" {
				return vErr(i, "key", "create 必须提供 key（提案内的引用名）")
			}
			identity = op.Entity + ":" + op.Key
		case OpUpdate:
			if strings.TrimSpace(op.Key) == "" && (numericID && op.ID == 0) {
				return vErr(i, "id", "update %s 必须提供 %s", op.Entity, idFieldName(numericID))
			}
			if op.ExpectVersion <= 0 {
				// 强制要求版本号：没有它就无法判断"AI 看到的是不是最新状态"，
				// 会静默覆盖运营在生成提案期间做的修改。
				return vErr(i, "expect_version", "update 必须提供 expect_version")
			}
			identity = op.Entity + ":" + idValue(op, numericID)
		case OpDisable:
			if op.Entity == EntityModelBindings {
				if strings.TrimSpace(op.Model) == "" || op.ChannelID == 0 {
					return vErr(i, "model", "disable model_bindings 必须提供 model 与 channel_id")
				}
				identity = fmt.Sprintf("%s:%s:%d", op.Entity, op.Model, op.ChannelID)
			} else {
				if strings.TrimSpace(op.Key) == "" && (numericID && op.ID == 0) {
					return vErr(i, "id", "disable %s 必须提供 %s", op.Entity, idFieldName(numericID))
				}
				identity = op.Entity + ":" + idValue(op, numericID)
			}
		case OpReplace:
			if op.Entity != EntityModelBindings {
				return vErr(i, "entity", "replace 只能用于 model_bindings")
			}
			if strings.TrimSpace(op.Key) == "" {
				return vErr(i, "key", "replace model_bindings 必须提供 key（模型 ID）")
			}
			identity = fmt.Sprintf("%s:%s", op.Entity, op.Key)
		}
		// 规则 3：同一实体不得出现两次
		if seen[identity] {
			return vErr(i, "key", "同一实体在一条提案里出现了两次：%s", identity)
		}
		seen[identity] = true

		if err := p.validateData(i, op, createdModels, boundReplaced, createdChannels, specs, opt); err != nil {
			return err
		}
	}

	// 规则 9：新建模型必须同时给出非空绑定（守住 P0-16：只建模型不建绑定 = 门户永远看不到）
	if !opt.SkipCrossOp {
		for id := range createdModels {
			if !boundReplaced[id] {
				return vErr(-1, "operations", "新建模型 %q 必须同时给出 model_bindings（否则门户永远看不到它）", id)
			}
		}
	}
	return nil
}

// validateData 校验单条操作的载荷（规则 4/5/6/11/12/13）。
func (p *Plan) validateData(i int, op *Operation, createdModels, boundReplaced, createdChannels map[string]bool,
	specs map[string]*specdsl.Spec, opt StructureOptions) error {

	switch op.Entity {
	case EntityVendor:
		var d VendorData
		if err := decodeEntity(op.Data, &d, "data", op.Entity); err != nil {
			return entityErr(i, "data", err)
		}
		if strings.TrimSpace(d.Name) == "" {
			return vErr(i, "data.name", "供应商名称不能为空")
		}
		if op.Op != OpCreate {
			// 供应商没有版本列，无法参与乐观锁；改其名称又会牵连展示与分组，
			// 属于"人来做更合适"的操作。明确拒绝，而不是假装支持。
			return vErr(i, "op", "供应商只支持新建；修改请在管理台操作")
		}

	case EntityAdapterSpec:
		var d AdapterSpecData
		if err := decodeEntity(op.Data, &d, "data", op.Entity); err != nil {
			return entityErr(i, "data", err)
		}
		if op.Op == OpDisable {
			break
		}
		if op.Op == OpCreate {
			// 二级结构（spec DSL）同样要把未知键翻成中文：它是本次故障的现场。
			if fe := unknownFieldsIn(d.Spec, reflect.TypeOf(specdsl.Spec{}), "data.spec", EntityAdapterSpec); fe != nil {
				return vErr(i, fe.Path, "%s", fe.Message())
			}
			s, err := specdsl.Decode(d.Spec)
			if err != nil {
				return vErr(i, "data.spec", "规格内容不合法：%v（字段表见 schema_get(spec_dsl)）", err)
			}
			if s.Slug != "" && s.Slug != op.Key {
				return vErr(i, "data.spec.slug", "规格内容里的 slug 与 key 不一致")
			}
			if d.Label != "" {
				s.Label = d.Label
			}
			if d.Vendor != "" {
				s.Vendor = d.Vendor
			}
			s.Slug = op.Key
			if err := s.Validate(); err != nil {
				_, msg := specdsl.AsFieldError(err)
				return vErr(i, "data.spec", "规格校验失败：%s", msg)
			}
			specs[op.Key] = s
		}

	case EntityChannel:
		// adapter_options 仍然禁止：渠道私有配置的形状由适配器自己定义，
		// 模型写不出"可校验的值"，写错只会在第一次真实请求时炸。
		if rawHasKey(op.Data, "adapter_options") {
			return vErr(i, "data", "渠道不支持 adapter_options（适配器私有配置请人工填写）")
		}
		// adapter_config 只开放三个字段。先于严格解析判断，给出中文原因：
		// 否则会被 DisallowUnknownFields 的英文报错抢走，模型看不懂。
		if rawHasKey(op.Data, "adapter_config") {
			var probe struct {
				Cfg map[string]json.RawMessage `json:"adapter_config"`
			}
			if err := json.Unmarshal(op.Data, &probe); err == nil {
				for k := range probe.Cfg {
					switch k {
					case "auth_header", "auth_prefix", "path_overrides":
					default:
						return vErr(i, "data.adapter_config",
							"adapter_config 只允许 auth_header / auth_prefix / path_overrides（%s 请人工配置）", k)
					}
				}
			}
		}
		var d ChannelData
		if err := decodeEntity(op.Data, &d, "data", op.Entity); err != nil {
			return entityErr(i, "data", err)
		}
		if op.Op == OpDisable {
			break
		}
		// update 是"只改提到的字段"（省略 = 保持库内现值），所以必填项只对 create 生效。
		// 强制 update 也提交全量字段，会让"只改个名字"变成"必须重述限流/并发/分组"，
		// 而漏一个就被重置 —— 这正是过去静默破坏配置的成因。
		if op.Op == OpCreate {
			if strings.TrimSpace(d.Name) == "" {
				return vErr(i, "data.name", "渠道名称不能为空")
			}
			if strings.TrimSpace(d.AdapterID) == "" {
				return vErr(i, "data.adapter_id", "必须指定适配器")
			}
			createdChannels[op.Key] = true
		}

	case EntityModel:
		// 规则 13：派生字段不许写。必须**先于**严格解析判断，否则会被
		// DisallowUnknownFields 的英文报错抢走，运营看不懂「unknown field is_free」。
		if rawHasKey(op.Data, "is_free") || rawHasKey(op.Data, "is_default") {
			return vErr(i, "data", "is_free / is_default 是派生字段，不能由提案写入")
		}
		var d ModelData
		if err := decodeEntity(op.Data, &d, "data", op.Entity); err != nil {
			return entityErr(i, "data", err)
		}
		if op.Op == OpDisable {
			break
		}
		if op.Op == OpCreate {
			// 新建必须给全：省掉 billing_mode 会默认成"按量"，而对音频这类模态
			// 按量等于永久免费（解析不出 token）。
			if d.BillingMode == nil || strings.TrimSpace(*d.BillingMode) == "" {
				return vErr(i, "data.billing_mode", "新建模型必须指定计费方式（tokens 按量 / call 按次 / spec 按规格）")
			}
			if err := modelcfg.ValidateModality(d.Modality); err != nil {
				return vErr(i, "data.modality", "%v", err)
			}
		} else if d.Modality != "" {
			if err := modelcfg.ValidateModality(d.Modality); err != nil {
				return vErr(i, "data.modality", "%v", err)
			}
		}
		// 模态与计费方式都提交了才判互斥；只提交一个时交给运行期（FillMissing 会补齐）。
		// 规则本身只有一份（modelcfg），这里只是提前到提案期以给出精确的 operations[i]。
		if d.BillingMode != nil && d.Modality != "" {
			if err := modelcfg.ValidateBillingMode(d.Modality, *d.BillingMode); err != nil {
				return vErr(i, "data.billing_mode", "%v", err)
			}
		}
		if d.BillingMode != nil && *d.BillingMode == model.BillingModeSpec && d.Modality != "" {
			// 规则 6：按规格计费必须过统一校验（归一化后再校验，与人工保存完全一致）
			if _, _, err := modelcfg.ValidateSpecPrices(d.Modality, d.SpecPrices); err != nil {
				return vErr(i, "data.spec_prices", "%v", err)
			}
		}
		// 规则 5：endpoints 必须是已知端点键（apidoc 是闭集，反查不到就是编的）
		for _, e := range d.Endpoints {
			if e = strings.TrimSpace(e); e == "" {
				return vErr(i, "data.endpoints", "端点键不能为空")
			}
			if _, ok := apidoc.KindOfEndpointKey(e); !ok {
				return vErr(i, "data.endpoints", "未知端点 %q（可用端点见 schema 目录）", e)
			}
		}
		// 规则 5b：能力必须落在封闭词表内且与模态匹配。
		//
		// 不校验「能力要求的端点已声明」：endpoints 留空时会按模态派生默认端点，
		// 在这里判非法会让「不填端点」与「填能力」变成互斥操作。
		// 空能力也不报错 —— 只出最基础的预设 TAB，不该阻断建模型。
		if err := apidoc.ValidateCapabilityKeys(d.Modality, d.Capabilities); err != nil {
			return vErr(i, "data.capabilities", "%v", err)
		}
		if op.Op == OpCreate {
			createdModels[op.Key] = true
		}

	case EntityModelBindings:
		if op.Op == OpDisable {
			break
		}
		var d BindingData
		if err := decodeEntity(op.Data, &d, "data", op.Entity); err != nil {
			return entityErr(i, "data", err)
		}
		if len(d.Bindings) == 0 {
			return vErr(i, "data.bindings", "绑定不能为空（没有绑定的模型在门户里看不到）")
		}
		modelID := op.Key
		if op.ID != 0 {
			modelID = "" // 已有模型：apply 时用 id 查真实模型名
		}
		for j := range d.Bindings {
			b := &d.Bindings[j]
			if strings.TrimSpace(b.UpstreamModel) == "" {
				return vErr(i, fmt.Sprintf("data.bindings[%d].upstream_model", j), "必须填写上游模型名")
			}
			if (b.ChannelKey == "") == (b.ChannelID == 0) {
				return vErr(i, fmt.Sprintf("data.bindings[%d]", j), "channel_key 与 channel_id 必须且只能给一个")
			}
			if !opt.SkipCrossOp && b.ChannelKey != "" && !createdChannels[b.ChannelKey] {
				return vErr(i, fmt.Sprintf("data.bindings[%d].channel_key", j),
					"channel_key %q 不在本提案的渠道创建里（应改用 channel_id 指向已有渠道）", b.ChannelKey)
			}
		}
		if modelID != "" {
			// 只登记"该模型有绑定"；端点覆盖由 modelcfg.CheckCoverage 用库内事实判定（试跑期），
			// 不在这里做，避免把"可能要解析规格/看适配器能力"的逻辑塞进纯结构校验。
			boundReplaced[modelID] = true
		}
	}
	return nil
}

/* ------------------------------ 排序与差异 ------------------------------ */

// Ordered 返回按依赖顺序重排后的操作（同实体内保持原相对顺序）。
//
// 排序必须**稳定**：同一份 plan 两次 apply 的执行顺序一致，diff 才可复现。
type OrderedOp struct {
	// Index 是这条操作在 Plan.Operations 里的原始位置。
	Index int
	Op    Operation
}

// Ordered 按依赖顺序（供应商→规格→渠道→模型→绑定）返回操作，并带上原始下标。
//
// 为什么必须带下标：试跑失败要把 `operations[i].data.field` 回给模型，
// 而模型按这个下标去改的是**原始数组**。用重排后的位置，它会改到另一条操作上
// —— 提示看起来只是"不够精确"，实际是把模型引向错误修改。
func (p *Plan) Ordered() []OrderedOp {
	idx := make([]int, len(p.Operations))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return entityOrder[p.Operations[idx[a]].Entity] < entityOrder[p.Operations[idx[b]].Entity]
	})
	out := make([]OrderedOp, 0, len(idx))
	for _, i := range idx {
		out = append(out, OrderedOp{Index: i, Op: p.Operations[i]})
	}
	return out
}

// strictUnmarshal 严格解析：未知键报错（防止 AI 编造字段）。
func strictUnmarshal(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return errors.New("缺少 data")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// rawHasKey 判断 JSON 对象里是否存在某个键（用于"禁止字段"的显式报错）。
func rawHasKey(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// idFieldName 返回定位键在该实体上的字段名。
//
// channels / adapter_specs 用自增主键；models / vendors 的主键**就是名字**，
// 让模型去猜数字主键是必错的。
func idFieldName(numericID bool) string {
	if numericID {
		return "id"
	}
	return "key"
}

// idValue 取定位键的字符串形式（用于"同一实体出现两次"的去重键）。
func idValue(op *Operation, numericID bool) string {
	if numericID {
		return fmt.Sprintf("%d", op.ID)
	}
	return strings.TrimSpace(op.Key)
}
