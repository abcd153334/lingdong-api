package adapters

// 本文件是「渠道私有配置结构」的**唯一真相源**：键集、枚举、默认放行清单。
//
// 前端 web/src/lib/adapterOptions.gen.ts 由 cmd/dump-adapter-options 从这里生成，
// 前端模块只做 re-export，禁止再手写一份。
//
// 为什么必须这样：2026-11-04 的事故是前端把后端已删除的 `fixed_tier` 当合法键，
// 运营一保存就写进库，运行期 DisallowUnknownFields 让整个渠道 12 个模型全部 503。
// 靠注释里写「与后端一致」维持两份常量，迟早还会漂移。

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------- 多米

// duomiProtocolOrder 是协议枚举的唯一有序来源。
// 校验用的 map 与错误文案都从它派生，避免三处各写一份。
var duomiProtocolOrder = []string{
	duomiProtoSeedance,
	duomiProtoKling,
	duomiProtoKlingMulti,
	duomiProtoKlingOmni,
	duomiProtoMuse,
	duomiProtoNano,
	duomiProtoGPTImage,
	duomiProtoSunoMusic,
	duomiProtoSunoLyrics,
}

// duomiPathOrder 是 paths 允许的键（固定枚举，避免配置错别字静默不生效）。
//
// 「以提交路径为默认」的键不在这里重复一份默认值：默认值的真相源仍是
// duomiapi_images.go / duomiapi_suno.go 里的路径常量。
//
// nano 只有两个提交端点 + 一个查询端点：官方把图生图单独开在
// `/api/gemini/nano-banana-edit`，而任务状态只挂在 `/api/gemini/nano-banana/{id}`。
var duomiPathOrder = []string{
	"seedance.submit",
	"kling.prefix",
	// muse 的提交与查询是**两段无前缀关系的路径**，所以必须是两个键：
	// 合并成一个会让"提交路径 + /{id}"的写法重新变得可能，那是本协议最贵的错。
	"muse.submit",
	"muse.poll",
	"nano.submit",
	"nano.edit",
	"nano.poll",
	"gptimage.submit",
	"gptimage.poll",
	"suno.submit",
	"suno.feed",
	"suno.lyrics",
}

// duomiMetadataOrder 是 metadata_allow 可以放行的**全量**已知键。
var duomiMetadataOrder = []string{"aigc_watermark", "generate_audio", "watermark"}

// duomiDefaultMetadataAllow 是未配置时的放行清单。
//
// 只放「是否出声 / 平台水印」：它们直接改变上游产出与成本。
// aigc_watermark 是历史键（与 watermark 语义重叠），刻意不默认放行。
var duomiDefaultMetadataAllow = []string{"generate_audio", "watermark"}

// duomiOverrideKeyOrder 是 model_overrides.<id> 允许的子键，与 duomiOverride 的 json tag 一一对应。
var duomiOverrideKeyOrder = []string{"protocol", "mode", "upstream_model", "duration_as_string"}

// ---------------------------------------------------------------- 秘塔

// metasoUpstreamOrder 是 resolution_map 允许的上游字面量；"" = 该档未开通。
var metasoUpstreamOrder = []string{"", "480P", "768P", "1080P", "2K"}

// ---------------------------------------------------------------- AutoDL

// autodlUpstreamOrder 是 resolution_map 允许的上游字面量；"" = 不支持该档。
var autodlUpstreamOrder = []string{"", "480p", "768p", "1080p"}

// autodlUpstreamSet 是 autodlUpstreamOrder 的查表形式。
var autodlUpstreamSet = stringSet(autodlUpstreamOrder)

// ---------------------------------------------------------------- 工具

func stringSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// KnownModelSchema 是内置档案的可序列化视图（只含前端需要的三个字段）。
type KnownModelSchema struct {
	Protocol string `json:"protocol"`
	Upstream string `json:"upstream"`
	Mode     string `json:"mode"`
}

// DuomiOptionsSchema 是多米渠道私有配置的结构快照。
type DuomiOptionsSchema struct {
	Protocols            []string                    `json:"protocols"`
	PathKeys             []string                    `json:"path_keys"`
	OverrideKeys         []string                    `json:"override_keys"`
	MetadataKeys         []string                    `json:"metadata_keys"`
	DefaultMetadataAllow []string                    `json:"default_metadata_allow"`
	KnownModels          map[string]KnownModelSchema `json:"known_models"`
}

// DuomiSchema 返回多米的结构快照（含内置档案，按模型 ID 排序）。
func DuomiSchema() DuomiOptionsSchema {
	models := make(map[string]KnownModelSchema, len(duomiModels))
	for id, p := range duomiModels {
		models[id] = KnownModelSchema{Protocol: p.Protocol, Upstream: p.Upstream, Mode: p.Mode}
	}
	return DuomiOptionsSchema{
		Protocols:            append([]string(nil), duomiProtocolOrder...),
		PathKeys:             append([]string(nil), duomiPathOrder...),
		OverrideKeys:         append([]string(nil), duomiOverrideKeyOrder...),
		MetadataKeys:         append([]string(nil), duomiMetadataOrder...),
		DefaultMetadataAllow: append([]string(nil), duomiDefaultMetadataAllow...),
		KnownModels:          models,
	}
}

// MetasoOptionsSchema 是秘塔渠道私有配置的结构快照。
type MetasoOptionsSchema struct {
	Tiers          []string          `json:"tiers"`
	TierDefaults   map[string]string `json:"tier_defaults"`
	UpstreamValues []string          `json:"upstream_values"`
}

// MetasoSchema 返回秘塔的结构快照。
func MetasoSchema() MetasoOptionsSchema {
	return MetasoOptionsSchema{
		Tiers:          append([]string(nil), metasoTiers...),
		TierDefaults:   metasoDefaultResolution,
		UpstreamValues: append([]string(nil), metasoUpstreamOrder...),
	}
}

// AutoDLOptionsSchema 是 AutoDL 渠道私有配置的结构快照。
type AutoDLOptionsSchema struct {
	Modes            []string          `json:"modes"`
	ModeLabels       map[string]string `json:"mode_labels"`
	WorkflowDefaults map[string]string `json:"workflow_defaults"`
	TierDefaults     map[string]string `json:"tier_defaults"`
	UpstreamValues   []string          `json:"upstream_values"`
}

// AutoDLSchema 返回 AutoDL 的结构快照。
func AutoDLSchema() AutoDLOptionsSchema {
	modes := make([]string, 0, len(autodlModes))
	labels := make(map[string]string, len(autodlModeLabel))
	workflows := make(map[string]string, len(autodlDefaultWorkflows))
	for _, m := range autodlModes {
		modes = append(modes, string(m))
		labels[string(m)] = autodlModeLabel[m]
		workflows[string(m)] = autodlDefaultWorkflows[m]
	}
	return AutoDLOptionsSchema{
		Modes:            modes,
		ModeLabels:       labels,
		WorkflowDefaults: workflows,
		TierDefaults:     autodlDefaultResolution,
		UpstreamValues:   append([]string(nil), autodlUpstreamOrder...),
	}
}

// RenderOptionsSchemaTS 生成前端常量文件的内容。
//
// 生成逻辑与结构定义放在同一个包：放到 cmd 里的话，测试要反过来 import main，
// 而「生成物是否最新」的测试正是防止漂移的最后一道闸门，不能缺席。
func RenderOptionsSchemaTS() ([]byte, error) {
	d := DuomiSchema()
	m := MetasoSchema()
	a := AutoDLSchema()

	var b bytes.Buffer
	b.WriteString(`// 由 backend/cmd/dump-adapter-options 生成，请勿手改。
// 来源：backend/internal/relay/adapters/options_schema.go
// 重新生成（backend 目录）：go run ./cmd/dump-adapter-options
//
// 这些常量决定「前端能写哪些键」。与后端不一致的代价不是配置不生效，
// 而是运行期整段解析失败 —— 该渠道绑定的所有模型都会 503。

`)
	for _, item := range []struct {
		name string
		v    any
	}{
		{"DUOMI_PROTOCOLS", d.Protocols},
		{"DUOMI_PATH_KEYS", d.PathKeys},
		{"DUOMI_OVERRIDE_KEYS", d.OverrideKeys},
		{"DUOMI_METADATA_KEYS", d.MetadataKeys},
		{"DUOMI_DEFAULT_METADATA_ALLOW", d.DefaultMetadataAllow},
		{"DUOMI_KNOWN_MODELS", d.KnownModels},
		{"METASO_TIERS", m.Tiers},
		{"METASO_RESOLUTION_DEFAULTS", m.TierDefaults},
		{"METASO_UPSTREAM_RESOLUTIONS", m.UpstreamValues},
		{"AUTODL_MODES", a.Modes},
		{"AUTODL_MODE_LABELS", a.ModeLabels},
		{"AUTODL_WORKFLOW_DEFAULTS", a.WorkflowDefaults},
		{"AUTODL_RESOLUTION_DEFAULTS", a.TierDefaults},
		{"AUTODL_UPSTREAM_RESOLUTIONS", a.UpstreamValues},
	} {
		raw, err := json.Marshal(item.v)
		if err != nil {
			return nil, fmt.Errorf("adapters: 序列化 %s 失败: %w", item.name, err)
		}
		fmt.Fprintf(&b, "export const %s = %s as const;\n", item.name, raw)
	}
	return b.Bytes(), nil
}
