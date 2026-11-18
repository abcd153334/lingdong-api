package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/relay"
	specdsl "lingdong-api/internal/relay/spec"
	"lingdong-api/internal/service/channel"
	"lingdong-api/internal/service/pricing"
	"lingdong-api/internal/service/schema"
	specsvc "lingdong-api/internal/service/spec"
)

// 本文件是 AI 能调用的全部工具（20 个）。
//
// 三条设计原则（改之前先读）：
//  1. **工具不写生产表**。唯一的写工具是 propose_change / submit_unsupported，
//     它们只写 agent_proposals。缺位比 prompt 里的"请不要"可靠一万倍：
//     模型被注入后能做的事，就是这些工具能做的事。
//  2. 工具失败**以数据形式返回**（`{"ok":false,...}`），不抛传输错误。
//     模型看到结构化的失败原因才能自我修正；抛错误只会让它换个说法再试一次。
//  3. 结果 ≤ 8KB。一轮里塞三份全量表就会把上下文顶爆，模型随后开始"忘记"前面的约束。

const (
	// toolResultMaxBytes 是单个工具结果的字节上限（超出截断并置 truncated）。
	toolResultMaxBytes = 8 * 1024
	// toolDefaultLimit / toolMaxLimit 是所有列表工具的默认与上限条数。
	toolDefaultLimit = 20
	toolMaxLimit     = 50
)

// toolEnv 携带一次会话的上下文与副作用收集器。
type toolEnv struct {
	Ctx       context.Context
	SessionID uint64
	MessageID uint64
	AdminID   uint64
	// ProposedIDs 收集本次会话内产生的提案（供编排层发 SSE proposal 事件）。
	ProposedIDs []uint64
	// ToolsUsed 是本次会话调过的工具名（落进消息轨迹，事后可查"它查了什么"）。
	ToolsUsed []string
	// AllProposed 是本轮产生过的全部提案（含已发过事件的），只用于落消息轨迹。
	AllProposed []uint64
	// Calls 是"工具 + 参数"的调用次数（防同一调用反复烧 token）。
	Calls map[string]int
	// FailStreak 是同一工具连续**业务**失败次数（目标不存在/上游拒绝这类，
	// 换措辞也不会变）。成功的工具调用会把它清零。
	FailStreak map[string]int
	// ArgStreak 是同一工具连续**参数/语法**失败次数。
	//
	// 与 FailStreak 分开的理由：参数写坏只说明这一段没写对，工具与目标是好的。
	// 混在一起就会出现"写坏三次 JSON 就整轮中止"，而那三次其实是同一个括号。
	ArgStreak map[string]int
	// Repeat 是同一工具"重复相同调用且被防重复拦下"的次数。
	//
	// 它不是失败（工具与目标都没问题），但它说明**模型卡住了**：拿到同样的结果
	// 还在原样再要一次。不单独计数会空转到步数耗尽（实测 12 步全 repeated_call），
	// 计入 FailStreak 又会在第 3 次直接中止而救不回来 —— 所以它是一个独立阈值。
	Repeat map[string]int
	// Trace 是本轮的有序执行轨迹（文本 / 思维链 / 工具 / 提案），落进 parts.items。
	// 为什么不用两个平铺数组（tools + proposals）代替：那样恢复不出先后顺序，
	// 前端就只能把工具卡统一堆在消息末尾 —— 那正是重构前的样子。
	Trace []traceItem

	// Scope 是本次入口允许触碰的实体（空 = 不限制）。
	//
	// 与 EnforceScope 同一套词表：apply 时校验提案，工具层校验**直接写库**的动作
	// （文档工具就是这一类 —— 它不产生提案，scope 必须在调用点拦住）。
	Scope []string

	// RunID / Step / MaxSteps 供 ask_user 判断"还来不来得及问"。
	RunID    uint64
	Step     int
	MaxSteps int
	// AskReady 表示本轮允许提问（运行期注入；单测里不带它 → 直接返回 unavailable，
	// 绝不能让工具在测试里等 60 秒）。
	AskReady bool
	// AskCount 是本轮已问次数（上限见 askMaxPerRun）。
	AskCount int
	// WaitTotal 是本轮花在"等人回答"上的时长。
	// finalize 会把它从 latency 里扣掉：否则 agent_runs.latency_ms 会把
	// "人在打字"算成"模型很慢"，stats 的均值跟着失真。
	WaitTotal time.Duration
	// Emit 让工具能发事件（目前只有 ask 与心跳用它）。
	Emit Emitter
}

// emitEvent 是工具层发事件的唯一出口（Emit 为空时静默成功）。
func (e *toolEnv) emitEvent(ev Event) error {
	if e == nil || e.Emit == nil {
		return nil
	}
	return e.Emit(ev)
}

// ToolSpecs 返回全部工具声明（顺序稳定，便于测试与 prompt 缓存）。
func ToolSpecs() []ToolSpec {
	defs := []struct {
		name   string
		desc   string
		params string
	}{
		{"schema_get", "读取平台字段手册（唯一权威，不要凭记忆编字段名）。何时用：写 data 前必须读 section=\"plan\" + entity=<实体>；写 spec 前必须读 section=\"spec_dsl\"；算价前读 section=\"spec_prices\"；写对外文档前读 section=\"docs_text\"（流程/判据/体检码；字段形状见 doc_* 工具参数）。一次只取要写的那一层，别把 all 全拉下来。",
			""},
		{"spec_support_check", "根据上游文档归纳出的特征，判断该供应商能否用声明式规格（L2）接入，还是必须写内置适配器（L3）。",
			`{"type":"object","properties":{"features":{"type":"array","items":{"type":"string"},"description":"从文档里归纳出的能力特征，如 \"custom auth header\"、\"async task polling\""}},"required":["features"]}`},
		{"vendor_list", "列出供应商分组（模型的 vendor 字段必须先存在）。",
			`{"type":"object","properties":{"limit":{"type":"integer"}}}`},
		{"adapter_list", "列出已注册的适配器（内置与规格）及其能力。",
			`{"type":"object","properties":{}}`},
		{"channel_list", "列出渠道（不含密钥）。",
			`{"type":"object","properties":{"query":{"type":"string"},"adapter_id":{"type":"string"},"limit":{"type":"integer"}}}`},
		{"channel_get", "读取单个渠道详情与它绑定的模型。",
			`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`},
		{"model_list", "列出模型。",
			`{"type":"object","properties":{"modality":{"type":"string"},"query":{"type":"string"},"limit":{"type":"integer"}}}`},
		{"model_get", "读取单个模型的完整配置（含计价格与绑定）。",
			`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`},
		{"spec_list", "列出声明式规格。", `{"type":"object","properties":{}}`},
		{"spec_get", "读取单个规格的完整 DSL。",
			`{"type":"object","properties":{"slug":{"type":"string"}},"required":["slug"]}`},
		{"pricing_basis", "读取币种与金额刻度（微）、汇率、分组折扣（微比例）与默认价。算价前必读。",
			`{"type":"object","properties":{}}`},
		{"skill_read", "读取平台内置知识包（SKILL）：判层、提案形状、规格 DSL、定价、绑定、文档写作。写配置前先读对应那一篇。",
			""},
		// ── 对外文档（v7）──
		//
		// 这 10 个工具**没有发布能力**：发布是人的动作（缺位比提示词可靠）。
		// 写工具只落草稿，且必须带上"能不能发布"的体检结论（problems），
		// 让模型自己发现"没绑端点/缺 202 变体"这类错误，而不是等人工点发布才被打回。
		{"doc_tree", "列出对外文档的节点与接口（含草稿）。写文档前先看这里找到 node/key。",
			`{"type":"object","properties":{"query":{"type":"string","description":"按标题/key 过滤"},"section":{"type":"string","description":"只取某个板块（如 images/videos/general）"},"limit":{"type":"integer","description":"最多返回多少个节点，默认 20"}}}`},
		{"doc_node_get", "读一个文档节点（标题/成员/接口清单/体检结论）。改节点前必须先读。",
			`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`},
		{"doc_node_create", "新建文档节点（草稿）。kind=family 时必须给 members（模型 ID）。",
			`{"type":"object","properties":{"key":{"type":"string","description":"小写字母/数字/-_.，创建后不可改"},"kind":{"type":"string","enum":["family","general","guide","protocol"]},"title":{"type":"string"},"section":{"type":"string"},"summary":{"type":"string"},"members":{"type":"array","items":{"type":"string"}},"representative":{"type":"string","description":"示例里 model 填哪个；留空取第一个成员"}},"required":["key","kind","title","section"]}`},
		{"doc_node_update", "改节点元信息（增量：只提交要改的键；数组是整体替换）。保存会转为草稿。",
			`{"type":"object","properties":{"key":{"type":"string"},"title":{"type":"string"},"summary":{"type":"string"},"section":{"type":"string"},"members":{"type":"array","items":{"type":"string"}},"representative":{"type":"string"},"hidden":{"type":"boolean"},"sort":{"type":"integer"},"blocks":{"type":"array","items":{"type":"object","properties":{"type":{"type":"string","enum":["para","note","steps","bullets","code"]},"text":{"type":"string"},"title":{"type":"string"},"items":{"type":"array","items":{"type":"string"}},"lang":{"type":"string"},"code":{"type":"string"}},"required":["type"]}},"version":{"type":"integer","description":"读到的 version；与库内不一致会返回 version_conflict"}},"required":["key"]}`},
		{"doc_endpoint_get", "读一篇接口文档全文（字段/响应/示例/错误码/体检），并给出本板块可绑定的平台端点。改之前必须先读。",
			`{"type":"object","properties":{"node":{"type":"string"},"key":{"type":"string"}},"required":["node","key"]}`},
		{"doc_endpoint_create", "在节点下新建接口文档（草稿）。能绑定就先绑（看 doc_endpoint_get 的 bindable_transports）。",
			""},
		{"doc_endpoint_update", "改一篇接口文档（增量：只提交要改的键；数组整体替换，不是合并）。保存会转为草稿。",
			""},
		{"doc_fields_write", "只替换请求字段（字段多时用它，避免整篇回传）。数组整体替换。",
			""},
		{"doc_check", "文档体检（与发布按钮同一个判据）。不给参数=全站。",
			`{"type":"object","properties":{"node":{"type":"string"},"key":{"type":"string","description":"给了 node+key 就查这一篇；只给 node 查整节点"}}}`},
		{"doc_seed_diff", "只读差异：出厂种子里有哪些节点/接口还没落库（不写库，启动时会自动补齐）。",
			`{"type":"object","properties":{}}`},
		{"docs_read", "读取已上传的上游文档副本（分段）。目录在系统提示里（含 kind 与 read_hint）；一次读一块，next_offset 推进到 eof 才算读完。",
			`{"type":"object","properties":{"doc_id":{"type":"integer"},"offset":{"type":"integer"},"limit":{"type":"integer"},"section":{"type":"integer","description":"按目录小节下标读（与 offset 二选一）"}},"required":["doc_id"]}`},
		{"validate_draft", "预演草稿（与 propose_change 同一条管线：同事务内真跑一遍再回滚）。何时用：提交提案前必做。一次多大：1–2 个 operations 最稳，上限 12。写不下就分块：先 mode=\"shape\" 只验 JSON 与规格语法，凑齐后必须再跑 mode=\"full\"。失败怎么改：按返回的 index/field/line/column 只改那一处，别整份重写。",
			""},
		{"ask_user", "向运营提一个必须由人决定的问题，并**原地等回答**（最多 60 秒），答完继续本轮。仅在缺信息会导致做错方向时用；能自己查到的（端点/价格/规格字段）不要问。",
			""},
		{"dry_run_draft", "零成本连通性/鉴权探测（不建渠道、不扣费）。何时用：新建渠道、或改 base_url/adapter_id 时必须先探，否则提案无法应用。规格还没落库时把 DSL 内联在 spec 里。同一会话内同地址同密钥 24 小时内探过就不必重探（回执会说明）。失败怎么改：401/403 是密钥问题，404 是路径问题，超时是地址或网络问题。",
			`{"type":"object","properties":{"adapter_id":{"type":"string","description":"如 spec:<slug> 或内置适配器名"},"base_url":{"type":"string","description":"上游根地址，不要带路径"},"api_key":{"type":"string","description":"只用于本次探测，不落库、不回显"},"spec":{"type":"object","description":"规格尚未落库时内联 DSL（与 adapter_spec 的 spec 同构）"},"probe":{"type":"string","enum":["models","chat_min","none"],"description":"探到哪一步：models=列模型（最强，推荐）、chat_min=发一次最小对话、none=只校验地址可达与鉴权头"}},"required":["adapter_id","base_url"]}`},
		// operations 的 schema 由结构体反射生成（见 plan_schema.go）：
		// 手写在这里会与 planspec 的结构体分叉，而分叉的症状是"模型照着写却 plan_invalid"。
		{"propose_change", "提交变更提案（只写提案表，不生效；由运营在管理台确认）。何时用：validate_draft 通过之后。一次多大：operations 上限 12，超了就分多份提案。失败怎么改：按返回的 index/field 只改那一处再提交，不要整份重写。",
			""},
		{"submit_unsupported", "当该上游必须写内置适配器（L3）时，把结论沉淀成一条 unsupported 提案。",
			`{"type":"object","properties":{"vendor":{"type":"string"},"reasons":{"type":"array","items":{"type":"string"}},"required_features":{"type":"array","items":{"type":"string"}}},"required":["vendor","reasons"]}`},
	}
	out := make([]ToolSpec, 0, len(defs))
	for _, d := range defs {
		params := json.RawMessage(d.params)
		if d.name == "propose_change" {
			params = proposeChangeParams()
		}
		if d.name == "skill_read" {
			params = skillReadParams()
		}
		if d.name == "schema_get" {
			params = schemaGetParams()
		}
		if d.name == "validate_draft" {
			params = draftParams()
		}
		if d.name == "ask_user" {
			params = askParams()
		}
		if d.name == "doc_endpoint_create" {
			params = docEndpointCreateParams()
		}
		if d.name == "doc_endpoint_update" {
			params = docEndpointUpdateParams()
		}
		if d.name == "doc_fields_write" {
			params = docFieldsWriteParams()
		}
		out = append(out, ToolSpec{
			Type: "function",
			Function: ToolFunction{
				Name: d.name, Description: d.desc, Parameters: params,
			},
		})
	}
	return out
}

// proposeChangeParams 是 propose_change 的参数契约（operations 由结构体反射生成）。
func proposeChangeParams() json.RawMessage {
	m := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"summary": map[string]any{
				"type": "string", "description": "一句话说明这次变更（展示在提案卡片上）",
			},
			"operations": map[string]any{
				"type":        "array",
				"minItems":    1,
				"maxItems":    maxDraftOps,
				"description": "变更操作（1–12 条，与 validate_draft 完全同构）。服务端按依赖顺序（供应商→规格→渠道→模型→绑定）重排，不必自己排序；update 时 data 只提交要改的字段，省略的保持库内现值。",
				"items":       json.RawMessage(planOperationsSchema()),
			},
			"doc_ids": map[string]any{
				"type": "array", "items": map[string]any{"type": "integer"},
				"description": "本次设计依据的上游文档 id（可追溯）",
			},
		},
		"required": []string{"summary", "operations"},
	}
	b, _ := json.Marshal(m)
	return b
}

// draftParams 是 validate_draft 的参数 schema。
//
// 与 propose_change 同构，但**只留 operations**：
//  1. 预演不写提案，`summary` / `doc_ids` 没有落点；
//  2. `toolValidateDraft` 用 strictUnmarshal 解析，多传一个字段会被当成参数错误，
//     模型照着 propose_change 的 schema 传 summary 就会莫名其妙失败。
func draftParams() json.RawMessage {
	m := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"operations": map[string]any{
				"type":        "array",
				"minItems":    1,
				"maxItems":    maxDraftOps,
				"description": "要预演的操作（1–12 条，与 propose_change 的 operations 完全同构）。mode=full 时服务端在同一事务里真跑一遍再回滚：通过即代表提案能提交。",
				"items":       json.RawMessage(planOperationsSchema()),
			},
			"mode": map[string]any{
				"type": "string",
				"enum": []string{"full", "shape"},
				"description": "full（默认）=完整预演；shape=只校验 JSON 结构与 spec DSL 语法（跳过跨操作规则、不查库、不试跑），" +
					"用于分块确认「这一段写对了」。shape 通过后，全部操作凑齐还必须再跑一次 full。",
			},
		},
		"required": []string{"operations"},
	}
	b, _ := json.Marshal(m)
	return b
}

// ExecTool 执行一个工具调用。
//
// 参数一律走严格解析（DisallowUnknownFields）：模型极擅长发明看起来合理的字段名，
// 静默忽略它们的后果是"提案显示已配置、实际什么都没写"。
func ExecTool(env *toolEnv, call ToolCall) any {
	fn, ok := toolImpls[call.Name]
	if !ok {
		return toolError("unknown_tool", "没有名为 "+call.Name+" 的工具", "")
	}
	// 参数解析只有这一个入口（jsonargs.go）：它负责无歧义修复、截断识别，
	// 以及**可定位的**失败回执（offset/行列/片段/未闭合栈/hint）。
	args, argErr := ParseToolArgs(call.Arguments, call.Truncated)
	if argErr != nil {
		return argErr.Result()
	}
	raw, err := fn(env, args)
	if err != nil {
		// 工具实现不该把 Go error 直接漏到这一层：它会被 json.Marshal 成 `{}`，
		// 且 okFlag 默认 true —— 模型看到"成功但结果为空"，FailStreak 也不增长。
		// 统一转成结构化工具错误，让模型能读懂并自我修正。
		return toolError("tool_internal", err.Error(), "")
	}
	return clipResult(raw)
}

type toolFunc func(env *toolEnv, args json.RawMessage) (any, error)

var toolImpls = map[string]toolFunc{
	"schema_get":          toolSchemaGet,
	"spec_support_check":  toolSpecSupportCheck,
	"vendor_list":         toolVendorList,
	"adapter_list":        toolAdapterList,
	"channel_list":        toolChannelList,
	"channel_get":         toolChannelGet,
	"model_list":          toolModelList,
	"model_get":           toolModelGet,
	"spec_list":           toolSpecList,
	"spec_get":            toolSpecGet,
	"pricing_basis":       toolPricingBasis,
	"skill_read":          toolSkillRead,
	"doc_tree":            toolDocTree,
	"doc_node_get":        toolDocNodeGet,
	"doc_node_create":     toolDocNodeCreate,
	"doc_node_update":     toolDocNodeUpdate,
	"doc_endpoint_get":    toolDocEndpointGet,
	"doc_endpoint_create": toolDocEndpointCreate,
	"doc_endpoint_update": toolDocEndpointUpdate,
	"doc_fields_write":    toolDocFieldsWrite,
	"doc_check":           toolDocCheck,
	"doc_seed_diff":       toolDocSeedDiff,
	"docs_read":           toolDocsRead,
	"validate_draft":      toolValidateDraft,
	"ask_user":            toolAskUser,
	"dry_run_draft":       toolDryRunDraft,
	"propose_change":      toolProposeChange,
	"submit_unsupported":  toolSubmitUnsupported,
}

/* ------------------------------ 读工具 ------------------------------ */

// schemaSectionNames 是 schema_get 的合法 section（工具声明、分派、报错三处共用）。
//
// 三处共用是刻意的：这份清单曾经只在 enum 里出现，而 switch 漏了 `plan`，
// 于是提示词让模型调一个不存在的分支，模型只能猜字段 —— 本次故障就是这么来的。
var schemaSectionNames = []string{
	"all", "endpoints", "capabilities", "spec_prices", "labels", "limits", "adapters", "plan", "spec_dsl", "docs_text",
}

// schemaReadSections 是"平台目录"类 section 的投影（表驱动，避免 switch 漂移）。
var schemaReadSections = map[string]func(schema.Catalog) map[string]any{
	"endpoints": func(c schema.Catalog) map[string]any {
		return map[string]any{"endpoints": c.Endpoints}
	},
	"capabilities": func(c schema.Catalog) map[string]any {
		return map[string]any{"capabilities": c.Capabilities, "modalities": c.Modalities}
	},
	"spec_prices": func(c schema.Catalog) map[string]any {
		return map[string]any{"spec_prices": c.SpecPrices, "modalities": c.Modalities}
	},
	"labels":   func(c schema.Catalog) map[string]any { return map[string]any{"field_labels": c.FieldLabels} },
	"limits":   func(c schema.Catalog) map[string]any { return map[string]any{"limits": c.Limits} },
	"adapters": func(c schema.Catalog) map[string]any { return map[string]any{"adapters": c.Adapters} },
}

// schemaGetParams 与 PlanDictionary/SpecDictionary 的实际能力保持一致。
func schemaGetParams() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"section": map[string]any{
				"type": "string", "enum": schemaSectionNames,
				"description": "plan=提案字段表；spec_dsl=规格 DSL 字段表；all=平台目录",
			},
			"entity": map[string]any{
				"type": "string", "enum": planEntityNames(),
				"description": "仅 section=plan：只回该实体（省上下文）",
			},
			"part": map[string]any{
				"type": "string", "enum": specParts,
				"description": "仅 section=spec_dsl：只回该层",
			},
		},
	})
	return b
}

func planEntityNames() []string {
	out := make([]string, 0, len(planEntityList))
	for _, e := range planEntityList {
		out = append(out, e.name)
	}
	return out
}

func toolSchemaGet(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Section string `json:"section"`
		Entity  string `json:"entity"`
		Part    string `json:"part"`
	}
	// 参数解析失败不致命：section 走默认全量，模型仍能拿到字段表。
	_ = strictUnmarshal(args, &in)
	section := strings.ToLower(strings.TrimSpace(in.Section))
	if section == "" {
		section = "all"
	}
	// plan / spec_dsl 是纯反射产物：不读库、不受目录缓存影响，
	// 先于 schema.Build() 返回（字段手册不该依赖"库里有没有模型"）。
	switch section {
	case "plan":
		dict, ok := PlanDictionary(in.Entity)
		if !ok {
			return map[string]any{
				"ok": false, "code": "invalid_arguments", "field": "entity",
				"message": "未知的实体：" + strings.TrimSpace(in.Entity),
				"allowed": planEntityNames(),
			}, nil
		}
		return map[string]any{"ok": true, "section": section, "source": "reflection", "plan": dict}, nil
	case "docs_text":
		// 对外文档文案的字段与上限：数字来自 apidoc.DocCaps，
		// 知识包不再抄一份（抄的那份已经漂过一次）。
		return map[string]any{"ok": true, "section": section, "source": "docs", "docs_text": docsTextDictionary()}, nil
	case "spec_dsl":
		dict, ok := SpecDictionary(in.Part)
		if !ok {
			return map[string]any{
				"ok": false, "code": "invalid_arguments", "field": "part",
				"message": "未知的 part：" + strings.TrimSpace(in.Part),
				"allowed": specParts,
			}, nil
		}
		return map[string]any{"ok": true, "section": section, "source": "reflection", "spec_dsl": dict}, nil
	}

	cat := schema.Build()
	out := map[string]any{"ok": true, "version": cat.Version, "section": section}

	if section == "all" {
		out["modalities"] = cat.Modalities
		out["billing_modes"] = cat.BillingModes
		out["endpoints"] = cat.Endpoints
		out["capabilities"] = cat.Capabilities
		out["spec_prices"] = cat.SpecPrices
		out["adapters"] = cat.Adapters
		out["field_labels"] = cat.FieldLabels
		out["limits"] = cat.Limits
		return out, nil
	}

	fn, ok := schemaReadSections[section]
	if !ok {
		return map[string]any{
			"ok": false, "code": "invalid_arguments", "field": "section",
			"message": "未知的 section：" + section,
			"allowed": schemaSectionNames,
		}, nil
	}
	for k, v := range fn(cat) {
		out[k] = v
	}
	return out, nil
}

// specSupportCheck 是"该写规格还是写内置适配器"的判定。
//
// 判定依据是**平台表达力的边界**，不是模型的主观感受：
// 规格 DSL 只能做"换路径/换鉴权头/字段改名/静态常量"，做不了
// 自定义签名（HMAC）、多步流程、响应结构转换、轮询状态机映射。
func toolSpecSupportCheck(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Features []string `json:"features"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "features"), nil
	}
	// L3 只留**规格表达力真的做不到**的六类。历史表里还有 transform / envelope /
	// state-machine 三条，它们全是错的：字段改名是 body.copy、信封拆解是 extract 路径、
	// 状态映射是 status_map —— 把这三条当 L3 会让助手提前放弃，
	// 于是"任何非标接口都接不进来"（用户看到的"被判 L3 然后 submit_unsupported 失败"）。
	l3 := map[string]string{
		"signature":      "需要自研签名（HMAC/RSA/时间戳+nonce）",
		"sign":           "需要自研签名",
		"hmac":           "需要自研签名",
		"multi-step":     "需要多步调用（先取令牌再调用）",
		"token-exchange": "需要先换取临时令牌",
		"ws":             "需要 WebSocket 长连接",
		"websocket":      "需要 WebSocket 长连接",
		"callback":       "依赖异步回调（平台未提供回调接收）",
		"upload":         "需要先上传素材再引用（规格只重建 JSON 请求体）",
	}
	// l2 是"看起来复杂、其实规格能表达"的正向映射：命中只作为理由补充，
	// 不改判级。它的价值在于把助手的判断从"像 L3"拉回"这一步用哪个字段表达"。
	l2 := map[string]string{
		"rename":    "字段改名/改值由 body.copy、body.const 表达",
		"field-map": "字段映射由 body.copy 表达（支持候选链）",
		"envelope":  "外层信封由 extract 的取值路径拆解（$.data.result.url）",
		"transform": "结构转换由 body.arrays 重建数组、body.copy 重命名字段",
		"status":    "上游状态值由 status_map 映射到平台五态（queued/running/succeeded/failed/canceled）",
		"state":     "上游状态值由 status_map 映射到平台五态（queued/running/succeeded/failed/canceled）",
		"async":     "异步（提交+查询）由 async 表达：分钟级 mode=task，秒级 mode=bridge",
		"poll":      "轮询由 <端点>@poll + path 里的 {task_id} 表达",
		"binary":    "二进制/音频产物：@poll 端点用 extract.result_urls（或 extract.result）给出链接，bridge 形态会下载成同步响应体",
		"base64":    "base64 内联产物：先用 extract.result_urls 取出字段，平台按端点契约折成响应",
	}
	l1 := map[string]string{
		"openai-compatible": "与 OpenAI 协议兼容，可用内置 openai 适配器 + adapter_config 调整",
		"openai":            "与 OpenAI 协议兼容",
		"bearer":            "Bearer 鉴权，内置适配器即可",
	}
	var reasons, missing []string
	level := "L2"
	low := make([]string, 0, len(in.Features))
	for _, f := range in.Features {
		low = append(low, strings.ToLower(f))
	}
	for _, f := range low {
		for k, why := range l3 {
			if strings.Contains(f, k) {
				level = "L3"
				reasons = append(reasons, why)
				missing = append(missing, k)
			}
		}
	}
	if level == "L2" {
		for _, f := range low {
			for k, why := range l1 {
				if strings.Contains(f, k) {
					level = "L1"
					reasons = append(reasons, why)
					break
				}
			}
			if level == "L1" {
				break
			}
		}
	}
	if level == "L2" {
		for _, f := range low {
			for k, why := range l2 {
				if strings.Contains(f, k) {
					reasons = append(reasons, why)
				}
			}
		}
		reasons = append(reasons, "差异仅在于路径/鉴权头/字段名/静态常量/异步查询，声明式规格（L2）可承载")
	}
	sort.Strings(missing)
	missing = dedupeStrings(missing)
	return map[string]any{
		"ok": true, "level": level, "reasons": reasons,
		"missing": missing,
		"hint":    supportHint(level),
	}, nil
}

func supportHint(level string) string {
	switch level {
	case "L1":
		return "直接用内置适配器建渠道（adapter_config 可改路径与鉴权头），不需要写规格。"
	case "L2":
		return "写一份声明式规格（adapter_id = spec:<slug>），再用 propose_change 提案。"
	default:
		return "该上游必须新增内置适配器：请用 submit_unsupported 记录结论，并给出规格书要点。"
	}
}

func toolVendorList(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Limit int `json:"limit"`
	}
	_ = strictUnmarshal(args, &in)
	var rows []model.Vendor
	if err := db.DB.Order("builtin DESC, id ASC").Limit(clampLimit(in.Limit)).Find(&rows).Error; err != nil {
		return toolError("db_error", err.Error(), ""), nil
	}
	items := make([]map[string]any, 0, len(rows))
	for _, v := range rows {
		items = append(items, map[string]any{
			"id": v.ID, "name": v.Name, "builtin": v.Builtin,
		})
	}
	return map[string]any{"ok": true, "items": items}, nil
}

func toolAdapterList(_ *toolEnv, _ json.RawMessage) (any, error) {
	// 内置与规格**分成两份**：注册表里混着运行期解析出来的 spec:*，
	// 直接遍历会让模型把「自己刚生成的规格」当成内置能力，进而写出引用自身的渠道。
	builtins := relay.Builtins()
	items := make([]map[string]any, 0, len(builtins)+4)
	for _, a := range builtins {
		name := a.Name()
		items = append(items, map[string]any{
			"id":          name,
			"caps":        a.Caps(),
			"options":     relay.SupportsAdapterOptions(name),
			"declarative": relay.SupportsDeclarativeConfig(name),
			"is_spec":     false,
		})
	}
	if specs, err := specsvc.List(context.Background()); err == nil {
		for _, sp := range specs {
			items = append(items, map[string]any{
				"id":          sp.AdapterID,
				"caps":        sp.Caps,
				"options":     false,
				"declarative": true,
				"is_spec":     true,
				"channels":    sp.ChannelCount,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i]["id"].(string) < items[j]["id"].(string)
	})
	return map[string]any{"ok": true, "items": items}, nil
}

func toolChannelList(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Query     string `json:"query"`
		AdapterID string `json:"adapter_id"`
		Limit     int    `json:"limit"`
	}
	_ = strictUnmarshal(args, &in)
	q := db.DB.Model(&model.Channel{})
	if s := strings.TrimSpace(in.Query); s != "" {
		q = q.Where("name ILIKE ? OR adapter_id ILIKE ?", "%"+s+"%", "%"+s+"%")
	}
	if s := strings.TrimSpace(in.AdapterID); s != "" {
		q = q.Where("adapter_id = ?", s)
	}
	var rows []model.Channel
	if err := q.Order("id ASC").Limit(clampLimit(in.Limit)).Find(&rows).Error; err != nil {
		return toolError("db_error", err.Error(), ""), nil
	}
	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		c := &rows[i]
		items = append(items, map[string]any{
			"id": c.ID, "name": c.Name, "adapter_id": c.AdapterID,
			"base_url": c.BaseURL, "groups": groupsOf(c.Groups),
			"status": c.Status, "version": c.Version,
			// 只给数量：密钥本身既不该进上下文，也不该被模型再写回去。
			"key_count": len(channel.DecryptKeys(c.Keys)),
		})
	}
	return map[string]any{"ok": true, "items": items}, nil
}

func toolChannelGet(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		ID uint64 `json:"id"`
	}
	if err := strictUnmarshal(args, &in); err != nil || in.ID == 0 {
		return toolError("invalid_arguments", "id 必填", "id"), nil
	}
	var c model.Channel
	if err := db.DB.First(&c, in.ID).Error; err != nil {
		return toolError("not_found", "渠道不存在", "id"), nil
	}
	var bindings []model.ModelBinding
	_ = db.DB.Where("channel_id = ?", c.ID).Find(&bindings).Error
	bound := make([]string, 0, len(bindings))
	for _, b := range bindings {
		bound = append(bound, b.ModelID)
	}
	return map[string]any{
		"ok": true,
		"item": map[string]any{
			"id": c.ID, "name": c.Name, "adapter_id": c.AdapterID, "base_url": c.BaseURL,
			"groups": groupsOf(c.Groups), "status": c.Status, "version": c.Version,
			"weight": c.Weight, "priority": c.Priority, "auto_ban": c.AutoBan,
			"key_count":       len(channel.DecryptKeys(c.Keys)),
			"adapter_config":  json.RawMessage(c.AdapterConfig),
			"adapter_options": json.RawMessage(c.AdapterOptions),
		},
		"bound_models": bound,
	}, nil
}

func toolModelList(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Modality string `json:"modality"`
		Query    string `json:"query"`
		Limit    int    `json:"limit"`
	}
	_ = strictUnmarshal(args, &in)
	q := db.DB.Model(&model.Model{})
	if s := strings.TrimSpace(in.Modality); s != "" {
		q = q.Where("modality = ?", s)
	}
	if s := strings.TrimSpace(in.Query); s != "" {
		q = q.Where("id ILIKE ? OR display_name ILIKE ?", "%"+s+"%", "%"+s+"%")
	}
	var rows []model.Model
	if err := q.Order("id ASC").Limit(clampLimit(in.Limit)).Find(&rows).Error; err != nil {
		return toolError("db_error", err.Error(), ""), nil
	}
	items := make([]map[string]any, 0, len(rows))
	for i := range rows {
		m := &rows[i]
		items = append(items, map[string]any{
			"id": m.ID, "display_name": m.DisplayName, "vendor": m.Vendor,
			"modality": m.Modality, "billing_mode": m.BillingMode,
			"enabled": m.Enabled, "is_free": m.IsFree, "version": m.Version,
		})
	}
	return map[string]any{"ok": true, "items": items}, nil
}

func toolModelGet(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := strictUnmarshal(args, &in); err != nil || strings.TrimSpace(in.ID) == "" {
		return toolError("invalid_arguments", "id 必填", "id"), nil
	}
	var m model.Model
	if err := db.DB.Where("id = ?", strings.TrimSpace(in.ID)).First(&m).Error; err != nil {
		return toolError("not_found", "模型不存在", "id"), nil
	}
	data := modelDataOf(&m)
	var bindings []model.ModelBinding
	_ = db.DB.Where("model_id = ?", m.ID).Find(&bindings).Error
	bound := make([]map[string]any, 0, len(bindings))
	for _, b := range bindings {
		bound = append(bound, map[string]any{
			"channel_id": b.ChannelID, "upstream_model": b.UpstreamModel,
			"enabled": b.Enabled, "weight": b.Weight, "priority": b.Priority,
		})
	}
	return map[string]any{
		"ok": true,
		"item": map[string]any{
			"id": m.ID, "version": m.Version, "enabled": m.Enabled,
			"data": data, "bindings": bound,
		},
	}, nil
}

func toolSpecList(_ *toolEnv, _ json.RawMessage) (any, error) {
	items, err := specsvc.List(context.Background())
	if err != nil {
		return toolError("db_error", err.Error(), ""), nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, map[string]any{
			"id": it.ID, "slug": it.Slug, "label": it.Label, "vendor": it.Vendor,
			"status": it.Status, "version": it.Version,
			"endpoints": it.Endpoints, "channel_count": it.ChannelCount,
		})
	}
	return map[string]any{"ok": true, "items": out}, nil
}

func toolSpecGet(_ *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Slug string `json:"slug"`
	}
	if err := strictUnmarshal(args, &in); err != nil || strings.TrimSpace(in.Slug) == "" {
		return toolError("invalid_arguments", "slug 必填", "slug"), nil
	}
	slug := strings.TrimPrefix(strings.TrimSpace(in.Slug), specsvc.AdapterPrefix)
	sp, err := specsvc.ResolveBySlug(slug)
	if err != nil || sp == nil {
		// 把"为什么读不到"原样告诉模型：停用与不存在要改的东西完全不同。
		reason := "规格不存在或未启用"
		if err != nil {
			reason = err.Error()
		}
		return toolError("not_found", reason, "slug"), nil
	}
	return map[string]any{"ok": true, "slug": slug, "spec": sp}, nil
}

func toolPricingBasis(_ *toolEnv, _ json.RawMessage) (any, error) {
	snap, err := pricing.Get()
	if err != nil || snap == nil {
		return toolError("pricing_unavailable", "定价快照不可用", ""), nil
	}
	return map[string]any{
		"ok":       true,
		"currency": snap.Currency, "currency_symbol": snap.CurrencySymbol,
		"scale": snap.Scale, "cny_per_usd": snap.CnyPerUSD,
		"group_discounts": snap.GroupDiscounts, "defaults": snap.Defaults,
	}, nil
}

func toolDocsRead(env *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		DocID   uint64 `json:"doc_id"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
		Section *int   `json:"section"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "doc_id"), nil
	}
	if in.DocID == 0 {
		return toolFieldError("doc_id", "必须指定文档 id（见系统提示里的文档目录）"), nil
	}
	ctx := context.Background()
	if env != nil && env.Ctx != nil {
		ctx = env.Ctx
	}
	input := DocReadInput{Offset: in.Offset, Limit: in.Limit}
	if in.Section != nil {
		input.Section, input.HasSection = *in.Section, true
	}

	// "量后收缩"：先按上限取，装配后再量 JSON 体积。超了就缩 limit **重取**，
	// 这样 next_offset 永远等于真正返回的正文末尾 —— 先取再砍会让模型跳过一段。
	limit := in.Limit
	if limit <= 0 || limit > DocChunkMax {
		limit = DocChunkMax
	}
	for attempt := 0; ; attempt++ {
		input.Limit = limit
		res, err := ReadDoc(ctx, in.DocID, input)
		if err != nil {
			return docReadError(err), nil
		}
		out := docReadPayload(in.DocID, res)
		buf, _ := json.Marshal(out)
		if len(buf) <= toolResultMaxBytes-256 || limit <= 1024 || attempt >= 4 {
			return out, nil
		}
		limit = limit * 3 / 4
	}
}

// docReadPayload 把一次读取装配成工具结果（正文包在不可信分隔符里）。
func docReadPayload(id uint64, res *DocReadResult) map[string]any {
	text, markers := wrapUntrustedChunk(id, res.Start, res.Kind, res.Text)
	out := map[string]any{
		"ok": true, "doc_id": id, "kind": res.Kind,
		"text": text, "offset": res.Start, "next_offset": res.Next, "eof": res.EOF,
		// 文档是**不可信输入**：这里是程序级标记，正文里的分隔符是模型级标记，两者都要。
		"untrusted": true,
	}
	if res.LimitClamped {
		out["limit_effective"] = res.LimitEffective
		out["limit_clamped"] = true
	}
	if res.HasSection {
		out["section"] = res.SectionUsed
		out["section_title"] = res.SectionTitle
	}
	if markers > 0 {
		out["injection_markers"] = markers
	}
	return out
}

// docReadError 把读取错误转成模型能读懂的结果。
func docReadError(err error) map[string]any {
	switch {
	case errors.Is(err, ErrDocNotFound):
		return toolError("not_found", "文档不存在或已删除（见系统提示里的文档目录）", "doc_id")
	case errors.Is(err, ErrDocSectionConflict):
		return toolFieldError("section", "offset 与 section 只能二选一")
	}
	var re *DocSectionRangeError
	if errors.As(err, &re) {
		return map[string]any{
			"ok": false, "code": "invalid_arguments", "field": "section",
			"message": re.Error(), "sections": re.Count,
		}
	}
	return toolError("read_failed", err.Error(), "")
}

/* ------------------------------ 校验与探针 ------------------------------ */

func toolValidateDraft(env *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Operations json.RawMessage `json:"operations"`
		// Mode 决定校验深度：shape 只查结构与 spec DSL 语法（分块用），full 是完整预演。
		Mode string `json:"mode"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolFieldError("operations", "operations 必须是数组："+err.Error()), nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(in.Operations, &raw); err != nil {
		return toolFieldError("operations", "operations 必须是数组"), nil
	}
	if len(raw) == 0 {
		return toolFieldError("operations", "至少给一个操作"), nil
	}
	if len(raw) > maxDraftOps {
		return toolFieldError("operations", fmt.Sprintf("一次最多预演 %d 个操作，请拆开校验", maxDraftOps)), nil
	}

	// 与服务端提案完全同一条路径：DecodePlan → ValidateStructure → trialApply。
	// 不另写校验器：两处规则一旦分叉，就回到"预演通过、提交被拒"。
	planJSON, err := json.Marshal(map[string]any{"summary": "草稿预演", "operations": in.Operations})
	if err != nil {
		return toolError("invalid_arguments", err.Error(), "operations"), nil
	}
	plan, err := DecodePlan(planJSON)
	if err != nil {
		return map[string]any{"ok": false, "code": "plan_invalid", "field": "operations", "message": err.Error()}, nil
	}
	// mode=shape：只校验结构与 spec DSL 语法（跳过跨操作规则、不试跑）。
	// 它存在的理由很具体：模型一次性写 3.6KB、深 11 层的 JSON 时最容易错在括号，
	// 而分块时第 1 块只有 spec、必然触发"新建模型必须带绑定"这类跨操作规则。
	shape := strings.EqualFold(strings.TrimSpace(in.Mode), "shape")
	if in.Mode != "" && !shape && !strings.EqualFold(strings.TrimSpace(in.Mode), "full") {
		return toolFieldError("mode", `mode 只能是 "full"（默认）或 "shape"`), nil
	}
	if err := plan.ValidateStructureWith(ProposalKindChange, StructureOptions{SkipCrossOp: shape}); err != nil {
		return draftResultOf(err), nil
	}
	if shape {
		return map[string]any{
			"ok": true, "mode": "shape", "operations": len(plan.Operations),
			"skipped":  []string{"cross_op_bindings", "cross_op_channel_key", "db_facts", "trial_apply"},
			"warnings": []string{},
			// 明确写出「没查库」：否则模型会把「空的 warnings」误解成「一定可用」。
			"warnings_note": "shape 模式不查库，不做模型承接预检；需要确认时请用 mode=\"full\"",
			"note":          "结构与规格语法通过。这只是分块自检：跨操作规则（新建模型必须带绑定、channel_key 只能指向本提案新建的渠道）和库内事实还没校验，全部操作凑齐后必须再用 mode=\"full\" 跑一次。",
		}, nil
	}
	ctx := context.Background()
	if env != nil && env.Ctx != nil {
		ctx = env.Ctx
	}
	if db.DB == nil {
		return map[string]any{"ok": false, "code": "db_error", "message": "预演需要数据库，当前不可用"}, nil
	}
	warnings, err := trialApply(ctx, plan)
	if err != nil {
		return draftResultOf(err), nil
	}

	entities := make([]string, 0, len(plan.Operations))
	seen := map[string]bool{}
	for _, op := range plan.Operations {
		if !seen[op.Entity] {
			seen[op.Entity] = true
			entities = append(entities, op.Entity)
		}
	}
	// warnings 恒为数组：AI 会直接取 len()，null 会让它以为没有这个字段。
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]any{
		"ok": true, "mode": "full", "operations": len(plan.Operations), "entities": entities,
		"warnings": warnings,
		"note": "预演通过：与提交提案走同一套校验（事务内真跑一遍再回滚），可以直接 propose_change。" +
			"warnings 非空表示配置合法但首次调用可能失败（例如渠道承接不了该模型），请原样转述给用户，不要自己改写渠道私有配置。",
	}, nil
}

// maxDraftOps 与默认 MaxSteps 对齐：一次校验吃掉整轮预算就没法再改。
const maxDraftOps = 12

// draftResultOf 把预演错误转成工具结果：校验错误带 field，其它视为环境问题。
//
// 区分这两类是刻意的：把 DB 故障报成 plan_invalid，模型会反复重写草稿，
// 而真正要做的只是稍后重试。
func draftResultOf(err error) map[string]any {
	var ve *ValidationError
	if errors.As(err, &ve) {
		out := map[string]any{
			"ok": false, "code": "plan_invalid",
			"index": ve.Index, "message": ve.Error(),
		}
		if ve.Field != "" {
			out["field"] = ve.Field
		}
		return out
	}
	return map[string]any{
		"ok": false, "code": "db_error",
		"message": "预演需要数据库，当前不可用：" + err.Error(),
	}
}

// dryRunDraft 是对候选渠道的零成本探测。
//
// 零副作用是硬要求：不建渠道、不冻结额度、不写 usages/call_io/tasks、不扣上游钱。
// 实现上只调适配器的 Test（它就是"鉴权 + 可达性"的探针，且各适配器都已按
// `__probe__` 路径实现成不产生业务数据）。
func toolDryRunDraft(env *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		AdapterID string          `json:"adapter_id"`
		BaseURL   string          `json:"base_url"`
		APIKey    string          `json:"api_key"`
		Spec      json.RawMessage `json:"spec"`
		Probe     string          `json:"probe"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), ""), nil
	}
	res, err := DryRun(env.Ctx, DryRunInput{
		AdapterID: in.AdapterID, BaseURL: in.BaseURL, APIKey: in.APIKey,
		Spec: in.Spec, Probe: in.Probe,
	})
	if err != nil {
		if errors.Is(err, ErrDryRunBlocked) {
			return toolError("dry_run_blocked",
				"目标地址不被允许：只允许公网 http/https（自建内网需由运维开启 LD_UPSTREAM_ALLOW_PRIVATE）", "base_url"), nil
		}
		if fe := asToolError(err); fe != nil {
			return fe, nil
		}
		return map[string]any{"ok": false, "code": "dry_run_failed", "message": err.Error()}, nil
	}
	cached := recordProbe(env, &in, res)
	out := map[string]any{
		"ok": res.OK, "status": res.Status, "latency_ms": res.LatencyMS,
		"checks": res.Checks, "ref": res.Ref,
	}
	if cached {
		out["cached"] = true
		out["note"] = "同一会话内该地址与密钥已探测通过（24 小时内有效），本次结果沿用上次凭据。"
	}
	return out, nil
}

// recordProbe 把探测结果写进 agent_dry_runs（**凭据的唯一来源**）。
//
// 为什么不再存 run 内存：那份记录只在"同一轮里先探测再 propose_change"时才会
// 被快照进提案，运营分两步做（这轮探、下轮提）就一条都不剩 —— 表现就是
// "明明探过了，点应用还是要求探测"。
//
// 写失败只记日志、不改结论：探测本身是只读操作，凭据写不进去应由闸门在
// apply 时拒绝，而不是把"上游其实通了"报成失败。
func recordProbe(env *toolEnv, in *struct {
	AdapterID string          `json:"adapter_id"`
	BaseURL   string          `json:"base_url"`
	APIKey    string          `json:"api_key"`
	Spec      json.RawMessage `json:"spec"`
	Probe     string          `json:"probe"`
}, res *DryRunResult) bool {
	if env == nil || db.DB == nil {
		return false
	}
	// 键必须由 probeKeyOf 算（与读取侧同一条路径）：早前这里自己算一遍、
	// 读取侧又算一遍且漏了 sessionID，指纹恒不相等 —— 闸门永远报"密钥已更换"。
	adapter := strings.TrimSpace(in.AdapterID)
	base, fp := probeKeyOf(env.SessionID, adapter, in.BaseURL, in.APIKey)
	rec := &model.AgentDryRun{
		SessionID: env.SessionID, RunID: env.RunID, MessageID: env.MessageID,
		AdapterID: adapter, BaseURL: base,
		KeyFP: fp,
		OK:    res.OK, Status: res.Status,
		Message:   clipText(res.UpstreamMessage, 400),
		LatencyMS: res.LatencyMS, Ref: res.Ref,
		CreatedAt: time.Now(),
	}
	cached, err := RecordDryRun(env.Ctx, rec)
	if err != nil {
		logger.L().Warn("探测凭据落库失败（apply 会要求重探）",
			zap.Uint64("session_id", env.SessionID), zap.String("base_url", base), zap.Error(err))
		return false
	}
	return cached
}

/* ------------------------------ 写工具（只写提案） ------------------------------ */

func toolProposeChange(env *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Summary    string          `json:"summary"`
		Operations json.RawMessage `json:"operations"`
		DocIDs     []uint64        `json:"doc_ids"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "operations"), nil
	}
	plan := map[string]any{"summary": in.Summary, "operations": json.RawMessage(in.Operations), "doc_ids": in.DocIDs}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return toolError("invalid_arguments", err.Error(), ""), nil
	}
	// 提案不再携带探测凭据：凭据由 dry_run_draft 直接落 agent_dry_runs，
	// apply 时按"会话 + 适配器 + 地址 + 密钥指纹"实时判定（跨轮有效）。
	prop, err := CreateProposal(env.Ctx, ProposalInput{
		SessionID: env.SessionID, MessageID: env.MessageID,
		Kind: "change", Plan: planJSON,
	})
	if err != nil {
		if ve, ok := err.(*ValidationError); ok {
			return map[string]any{
				"ok": false, "code": "plan_invalid", "message": ve.Error(),
				"field": ve.Field,
			}, nil
		}
		return map[string]any{"ok": false, "code": "propose_failed", "message": err.Error()}, nil
	}
	if env != nil {
		env.ProposedIDs = append(env.ProposedIDs, prop.ID)
	}
	added, changed, removed := DiffStats(json.RawMessage(prop.Diff))
	return map[string]any{
		"ok": true, "proposal_id": prop.ID, "status": prop.Status, "plan_hash": prop.PlanHash,
		"diff_stats": map[string]int{"added": added, "changed": changed, "removed": removed},
		"note":       "提案已生成，但**尚未生效**。需要运营在管理台确认后才会写入。",
	}, nil
}

func toolSubmitUnsupported(env *toolEnv, args json.RawMessage) (any, error) {
	var in struct {
		Vendor           string   `json:"vendor"`
		Reasons          []string `json:"reasons"`
		RequiredFeatures []string `json:"required_features"`
	}
	if err := strictUnmarshal(args, &in); err != nil {
		return toolError("invalid_arguments", err.Error(), "reasons"), nil
	}
	// 用 Plan 结构体拼装，而不是 map：map 里多写一个键，DecodePlan 的严格解码
	// 就会把这条提案整条拒掉（这正是历史上 L3 结论 100% 失败的原因）。
	plan := Plan{
		Summary: "该上游需要新增内置适配器：" + in.Vendor,
		Unsupported: &UnsupportedPlan{
			Vendor:           in.Vendor,
			Reasons:          in.Reasons,
			RequiredFeatures: in.RequiredFeatures,
		},
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return toolError("invalid_arguments", err.Error(), ""), nil
	}
	prop, err := CreateProposal(env.Ctx, ProposalInput{
		SessionID: env.SessionID, MessageID: env.MessageID,
		Kind: ProposalKindUnsupported, Plan: planJSON,
	})
	if err != nil {
		return map[string]any{"ok": false, "code": "propose_failed", "message": err.Error()}, nil
	}
	if env != nil {
		env.ProposedIDs = append(env.ProposedIDs, prop.ID)
	}
	return map[string]any{"ok": true, "proposal_id": prop.ID, "kind": ProposalKindUnsupported}, nil
}

/* ------------------------------ 辅助 ------------------------------ */

// DryRunInput 是一次候选渠道探测的入参。
type DryRunInput struct {
	AdapterID string
	BaseURL   string
	APIKey    string
	// Spec 是内联的规格 DSL（规格尚未落库时用它建临时适配器）。
	Spec  json.RawMessage
	Probe string
}

// DryRunResult 是探测结果（形状对齐方案 §2.3.4）。
//
// 注意：凭据不在这里 —— 它由 recordProbe 直接写 agent_dry_runs（唯一来源）。
// 曾经这里是 DryRunRecord，随提案一起落 dry_run 快照，跨轮即丢。
type DryRunResult struct {
	OK              bool          `json:"ok"`
	Status          int           `json:"status"`
	LatencyMS       int           `json:"latency_ms"`
	UpstreamMessage string        `json:"upstream_message"`
	Checks          []DryRunCheck `json:"checks"`
	// Ref 是一次成功探测的追溯码（展示用，**不参与**闸门判定）。
	Ref string `json:"ref,omitempty"`
}

// DryRunCheck 是一条探测项。
type DryRunCheck struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

var (
	ErrDryRunBlocked = errors.New("agent: 目标地址不被允许（内网/回环/非 http(s)）")
	ErrDryRunAdapter = errors.New("agent: 适配器不可用")
)

// DryRun 对候选渠道做一次零成本探测（不出业务请求、不落任何表）。
func DryRun(ctx context.Context, in DryRunInput) (*DryRunResult, error) {
	adapterID := strings.TrimSpace(in.AdapterID)
	base := strings.TrimSpace(in.BaseURL)
	if adapterID == "" || base == "" {
		return nil, errors.New("adapter_id 与 base_url 必填")
	}
	// 出站前必须过 netguard：模型（或被文档注入的模型）可以随手填一个地址，
	// 不校验就等于给它一个探测内网/回环/云元数据的内网穿透工具。
	// 自建上游由运维用 LD_UPSTREAM_ALLOW_PRIVATE=1 显式放行，与渠道保存同一口径。
	if err := netguard.ValidateURL(base, netguard.PolicyFromEnv()); err != nil {
		return nil, fmt.Errorf("%w：%s", ErrDryRunBlocked, base)
	}
	adapter, ok := relay.GetAdapter(adapterID)
	if !ok {
		// 内联 DSL：规格还没落库时也能探（否则"先探再提案"这个顺序根本走不通）。
		if slug, isSpec := specsvc.SlugOf(adapterID); isSpec && len(in.Spec) > 0 {
			sp, err := specdsl.Decode(in.Spec)
			if err != nil {
				return nil, err
			}
			if sp.Slug == "" {
				sp.Slug = slug
			}
			adapter = specdsl.New(sp)
		} else {
			return nil, fmt.Errorf("%w: %s", ErrDryRunAdapter, adapterID)
		}
	}
	ch := &model.Channel{AdapterID: adapterID, BaseURL: base}
	cfg := relay.ChannelConfigFor(ch, strings.TrimSpace(in.APIKey))

	started := time.Now()
	res := &DryRunResult{
		Checks: []DryRunCheck{{Name: "config", OK: true}},
	}
	err := adapter.Test(ctx, cfg)
	res.LatencyMS = int(time.Since(started).Milliseconds())
	if err != nil {
		msg := err.Error()
		res.OK = false
		res.UpstreamMessage = clipText(msg, 200)
		code := classifyProbeError(msg)
		res.Status = code
		res.Checks = append(res.Checks,
			DryRunCheck{Name: "reachability", OK: code != 0 && code != 401 && code != 403 && code != 404},
			DryRunCheck{Name: "auth", OK: code != 401 && code != 403})
		return res, nil
	}
	res.OK = true
	res.Status = 200
	res.Checks = append(res.Checks,
		DryRunCheck{Name: "reachability", OK: true}, DryRunCheck{Name: "auth", OK: true})
	// 凭据只对"这次探测本身"有意义：不存密钥，只存足够证明"探过"的信息。
	sum := fmt.Sprintf("%s|%s|%d", adapterID, base, time.Now().UnixMilli())
	res.Ref = "dry:" + shortHash(sum)
	return res, nil
}

// classifyProbeError 从适配器的错误文案里识别状态码语义。
//
// 只能靠文案：各适配器的 Test 返回的是 error，没有携带 Code。
// 这里刻意保守——认不出就返回 0（未知），不要假装知道。
func classifyProbeError(msg string) int {
	low := strings.ToLower(msg)
	for _, pair := range []struct {
		token string
		code  int
	}{
		{"401", 401}, {"403", 403}, {"404", 404}, {"429", 429}, {"500", 500}, {"502", 502}, {"503", 503},
		{"unauthorized", 401}, {"forbidden", 403}, {"鉴权失败", 401}, {"not found", 404},
		{"timeout", 0}, {"超时", 0}, {"connect", 0}, {"连通失败", 0},
	} {
		if strings.Contains(low, pair.token) {
			return pair.code
		}
	}
	return 0
}

// Validator 形态的工具错误（供模型自我修正）。
func toolError(code, msg, field string) map[string]any {
	out := map[string]any{"ok": false, "code": code, "message": msg}
	if field != "" {
		out["field"] = field
	}
	return out
}

func toolFieldError(field, msg string) map[string]any {
	return toolError("invalid_arguments", msg, field)
}

// asToolError 把"地址被拦"之类的错误转成工具错误（不进重试循环）。
func asToolError(err error) map[string]any {
	if errors.Is(err, ErrDryRunBlocked) {
		return toolError("blocked_url", err.Error(), "base_url")
	}
	if errors.Is(err, ErrDryRunAdapter) {
		return toolError("adapter_unavailable", err.Error(), "adapter_id")
	}
	return nil
}

// clipResult 把工具结果压到上限内（保留结构，只截断最长的文本字段）。
func clipResult(v any) any {
	buf, err := json.Marshal(v)
	if err != nil || len(buf) <= toolResultMaxBytes {
		return v
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{
			"ok": true, "truncated": true,
			"preview": clipText(string(buf), toolResultMaxBytes),
		}
	}
	m["truncated"] = true
	// 先砍最占地方的 items/text，再整体兜底截断。
	if items, ok := m["items"].([]map[string]any); ok {
		for len(items) > 0 {
			items = items[:len(items)/2]
			m["items"] = items
			out, err := json.Marshal(m)
			if err == nil && len(out) <= toolResultMaxBytes {
				return m
			}
			if len(items) == 0 {
				break
			}
		}
	}
	if s, ok := m["text"].(string); ok {
		m["text"] = clipText(s, toolResultMaxBytes/2)
		// 正文被二次截断时 next_offset 已不可信（它对应截断**前**的字节数）。
		// 让它回退到本次 offset 并标记：模型会用更小的 limit 重读同一块。
		// 宁可重复读一次，也不能静默跳过一段 —— 那正是过去丢掉 12KB 的成因。
		if _, has := m["next_offset"]; has {
			if off, ok := m["offset"].(int); ok {
				m["next_offset"] = off
			}
			m["eof"] = false
			m["retry_smaller"] = true
		}
	}
	out, err := json.Marshal(m)
	if err == nil && len(out) <= toolResultMaxBytes {
		return m
	}
	return map[string]any{"ok": true, "truncated": true, "preview": clipText(string(out), toolResultMaxBytes)}
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return safeCut(s, n) + "…"
}

func clampLimit(n int) int {
	if n <= 0 {
		return toolDefaultLimit
	}
	if n > toolMaxLimit {
		return toolMaxLimit
	}
	return n
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// billing.ValidateSpecPrices 的引用保持：dry-run/校验工具与保存期用同一套规则。
