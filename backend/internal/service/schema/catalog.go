// Package schema 是「平台的形状」的单一真相源：模态、计费方式、端点、档位、
// 计价格字段、限额、内置适配器能力。
//
// 为什么需要它（不建这个包会怎样）：
// 同一组事实原先散落在三处——保存期校验（controller）、计费实现（billing）、
// 前端下拉与提示（web）——三处各写一份，改一处另两处不跟，症状是
// 「校验通过但运行期永不命中」「前端提示 1–3600、后端拒绝 3600」这类**不报错**的错。
//
// 本包只做**投影**，不持有业务规则：
//   - 模态 × 计费方式的合法组合：这里定义（原本在 controller.validQuotaType）；
//   - 计价格字段集：来自反射 billing.SpecPrices（billing 是唯一权威）；
//   - 端点：来自 apidoc.SpecEndpointTable（apidoc 是唯一权威）；
//   - 适配器能力：来自 relay.Builtins()（注册表混有运行期解析出的 spec:*，不能当清单）；
//   - 限额：来自 relay/spec 与 billing 的导出常量。
//
// 管理台 AI 编排（/admin/agent）与规则提示都读这个目录；**不允许**再各自内联常量。
package schema

import (
	"sort"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	specdsl "lingdong-api/internal/relay/spec"
	"lingdong-api/internal/service/billing"
)

// Version 是目录结构版本：前端与 AI 提示词模板可据此判断兼容性。
// 只在**删改字段语义**时递增（新增字段不递增，前端按可选处理）。
const Version = 1

// Catalog 是下发给管理台（以及 AI 上下文）的完整形状描述。
type Catalog struct {
	Version       int               `json:"version"`
	Modalities    []Modality        `json:"modalities"`
	BillingModes  []BillingMode     `json:"billing_modes"`
	Endpoints     []Endpoint        `json:"endpoints"`
	Capabilities  []Capability      `json:"capabilities"`
	SpecPrices    SpecPricesSchema  `json:"spec_prices"`
	Adapters      []Adapter         `json:"adapters"`
	AdapterConfig AdapterConfigView `json:"adapter_config"`
	FieldLabels   map[string]string `json:"field_labels"`
	Limits        Limits            `json:"limits"`
}

// Capability 是一条模型能力（`models.capabilities` 的取值，来源 apidoc 词表）。
//
// 为什么要在目录里下发：AI 编排建模型时如果不知道这 9 个词，
// 会写出 `text_to_video`、`image2video` 这类"看起来合理"的名字，
// 保存期被拒（或更糟：被静默丢弃后文档右栏少几个 TAB）。词表必须可见。
type Capability struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Modalities 是允许声明该能力的模态。
	Modalities []string `json:"modalities"`
	// Endpoint 是该能力要求的端点（空串 = 无要求）。
	Endpoint string `json:"endpoint,omitempty"`
}

// Modality 是一种模型模态及其可用的计费方式。
type Modality struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// BillingModes 是该模态允许的计费方式（取值见 model.BillingMode*）。
	BillingModes []string `json:"billing_modes"`
	// SpecKind 是该模态 spec_prices.kind 的取值（仅按规格计费时有值）。
	SpecKind string `json:"spec_kind,omitempty"`
	// SpecCarriable 表示 L2 规格 DSL 能否承载该模态的端点（false = 必须写内置适配器）。
	SpecCarriable bool   `json:"spec_carriable"`
	Desc          string `json:"desc"`
}

// BillingMode 是计费方式枚举（字符串，与 models.billing_mode 列同值）。
type BillingMode struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
}

// Endpoint 是「渠道可声明的端点」视图（含不能由规格承载的与原因）。
type Endpoint struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Modality string `json:"modality"`
	Async    bool   `json:"async"`
	// Bridge 表示该端点可声明 async.mode=bridge（上游其实是异步，由适配器在本次请求内等结果）。
	Bridge bool `json:"bridge_allowed"`
	// Binary 表示响应体是二进制（音频/图片）：桥接时必须声明 extract.download。
	Binary    bool `json:"binary"`
	Supported bool `json:"spec_supported"`
	// Reason 是不能由规格承载的原因（Supported=false 时给运营/AI 看，避免反复试错）。
	Reason string `json:"reason,omitempty"`
}

// SpecPricesSchema 是按规格计费的字段与档位白名单。
type SpecPricesSchema struct {
	// Fields 的键集来自反射（billing.SpecPrices 的 json tag），顺序稳定。
	Fields []SpecPriceField `json:"fields"`
	// TierKeys 是档位白名单：image 必须大写 K，video 必须小写 p/k。
	TierKeys map[string][]string `json:"tier_keys"`
	// Kinds 是各模态允许的 kind 取值。
	Kinds map[string]string `json:"kinds"`
}

// SpecPriceField 是 spec_prices 的一个字段。
type SpecPriceField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Kind 是取值类型：number / map / string。
	Kind string `json:"kind"`
	Desc string `json:"desc,omitempty"`
}

// Adapter 是内置适配器的能力视图（规格不在此列：规格是数据，来自 adapter_specs）。
type Adapter struct {
	Name                string   `json:"name"`
	Caps                []string `json:"caps"`
	SupportsDeclarative bool     `json:"supports_declarative"`
	SupportsOptions     bool     `json:"supports_options"`
}

// AdapterConfigView 是渠道级声明式配置的字段清单（L1）。
type AdapterConfigView struct {
	Fields []AdapterConfigField `json:"fields"`
	// ReservedKeys 是不允许由 request_patch 写死的计费维度字段。
	ReservedKeys []string `json:"reserved_keys"`
}

// AdapterConfigField 是 L1 声明式配置的一个字段。
type AdapterConfigField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
}

// Limits 是各类硬上限（编辑器的即时提示与后端校验必须同源）。
type Limits struct {
	Spec    SpecLimits  `json:"spec"`
	Pricing PriceLimits `json:"pricing"`
}

// SpecLimits 是规格 DSL 的限额。
type SpecLimits struct {
	MaxEndpoints   int    `json:"max_endpoints"`
	MaxConstKeys   int    `json:"max_const_keys"`
	MaxCopyKeys    int    `json:"max_copy_keys"`
	MaxArrays      int    `json:"max_arrays"`
	MaxArrayItems  int    `json:"max_array_items"`
	MaxStaticQuery int    `json:"max_static_query"`
	MaxPathLen     int    `json:"max_path_len"`
	MaxSpecBytes   int    `json:"max_spec_bytes"`
	MaxSlugLen     int    `json:"max_slug_len"`
	MaxLabelRunes  int    `json:"max_label_runes"`
	PollKeySuffix  string `json:"poll_key_suffix"`
	// AuthHeaders 是允许承载渠道密钥的头名（超出即为配置错误，不是"可能不支持"）。
	AuthHeaders []string `json:"auth_headers"`
}

// PriceLimits 是计价维度的限额。
type PriceLimits struct {
	MaxSeconds int `json:"max_seconds"`
}

// modalityRules 是「模态 × 计费方式」的唯一权威表。
//
// 关键约束（改之前先读）：
//   - audio 可以按次或按规格（per_second）：TTS/ASR 的响应是二进制或纯文本，
//     token 解析永远为空，配成按量就是**永久 0 元**；按秒计价要求时长有真实来源
//     （桥接 extract.duration 或音频头探测），否则只能按次；
//   - image/video/music 只能按规格：它们没有 token 也没有请求数语义；
//   - chat/embedding/rerank/moderation 可以按量或按次（按次用于固定价模型）。
var modalityRules = []Modality{
	{ID: "chat", Label: "对话", BillingModes: []string{model.BillingModeTokens, model.BillingModeCall}, SpecCarriable: true,
		Desc: "文本对话与补全；按量计 token，按次用于固定价模型"},
	{ID: "embedding", Label: "向量", BillingModes: []string{model.BillingModeTokens, model.BillingModeCall}, SpecCarriable: true,
		Desc: "文本向量化"},
	{ID: "rerank", Label: "重排", BillingModes: []string{model.BillingModeTokens, model.BillingModeCall}, SpecCarriable: true,
		Desc: "检索结果重排"},
	{ID: "moderation", Label: "审核", BillingModes: []string{model.BillingModeTokens, model.BillingModeCall}, SpecCarriable: true,
		Desc: "内容审核"},
	{ID: "image", Label: "图像", BillingModes: []string{model.BillingModeSpec}, SpecKind: billing.SpecKindPerImage,
		SpecCarriable: true, Desc: "图像生成；按分辨率档位计价"},
	{ID: "video", Label: "视频", BillingModes: []string{model.BillingModeSpec}, SpecKind: billing.SpecKindPerVideo,
		SpecCarriable: true, Desc: "视频生成；按秒数 × 档位计价，可叠加参考图/音频/视频"},
	{ID: "music", Label: "音乐", BillingModes: []string{model.BillingModeSpec}, SpecKind: billing.SpecKindPerClip,
		SpecCarriable: true, Desc: "音乐与歌词生成；按实际交付条数计价（0 条免单）"},
	{ID: "audio", Label: "语音", BillingModes: []string{model.BillingModeCall, model.BillingModeSpec},
		SpecKind: billing.SpecKindPerSecond,
		// 语音端点（speech/transcriptions/translations）：speech 是二进制出参、JSON 入参，
		// 可以由规格承载（含 bridge 异步桥接）；transcriptions/translations 是 multipart，
		// 规格只重建 JSON 请求体，必须写内置适配器 —— 那一层由端点级
		// spec_supported 精确表达，模态这一级必须为 true，否则助手会把「TTS 接不进来」
		// 当成平台限制，直接判 L3。
		SpecCarriable: true,
		Desc:          "语音合成与识别；按次或按秒计价（按秒需要真实时长来源）"},
}

// billingModeLabels 是计费方式的展示文案。
var billingModeLabels = []BillingMode{
	{Value: model.BillingModeTokens, Label: "按量", Desc: "按 token 数计费（chat/embedding/rerank/moderation）"},
	{Value: model.BillingModeCall, Label: "按次", Desc: "按调用次数计费（固定价模型、语音）"},
	{Value: model.BillingModeSpec, Label: "按规格", Desc: "按分辨率/时长/任务计费（图像/视频/音乐）"},
}

// priceFieldLabels 是 spec_prices 字段的中文名与取值类型。
//
// 键集必须与 model.SpecPrices 的 json tag 完全一致（由守漂移测试保证）：
// 少写一条只会让编辑器少一个提示，多写一条则是**幻觉字段**（运营填了它、系统不认）。
// 单位一律是**微**（1e-6 结算货币单位），与 price_* 列同单位。
// 档位可选项**从 billing 派生**，不手写：这里曾写死「键必须是 480p/720p/1080p/2k」，
// 而 billing 早已加入 4k —— 运营照提示配置就会以为 4k 不合法。
var priceFieldLabels = map[string]specPriceFieldMeta{
	"kind":                   {"类型", "string", "per_image / per_video / per_task / per_second / per_clip"},
	"tier_price":             {"分辨率档单价", "map", "键是运营自由配置的档位（建议 " + strings.Join(billing.ImageTierKeys, "/") + "）；平台不校验上游能否出该档"},
	"max_n":                  {"单次产出上限", "number", "图片是单次张数上限；按条计费（per_clip）时是单次任务的计费条数上限"},
	"second_price":           {"每秒单价", "map", "键是运营自由配置的档位（建议 " + strings.Join(billing.VideoTierKeys, "/") + "）；配了哪些档就开放哪些档"},
	"max_seconds":            {"最大秒数", "number", "留空表示只受全局上限约束"},
	"allow_auto_duration":    {"允许智能时长", "bool", "开启后才接受 seconds=-1，由上游决定时长并按冻结额结算"},
	"resolution_fixed":       {"默认档", "string", "请求没写分辨率时按它计费；图片必须命中 tier_price，视频必须命中 second_price；图片留空且有多档时，不传尺寸的请求会被拒绝"},
	"long_after_seconds":     {"超时阈值（秒）", "number", "与超时倍率必须成对填写"},
	"long_video_multiplier":  {"超时倍率", "number", "微比例，1.5 填 1500000；必须大于 1"},
	"ref_image_price":        {"参考图单价", "number", "按张加收"},
	"ref_image_free":         {"参考图免费张数", "number", "0 表示不免费（要全免费请把单价填 0）"},
	"ref_audio_price":        {"参考音频单价", "number", "按个加收"},
	"ref_video_price":        {"参考视频每条单价", "number", "按条计费（与按秒互斥）"},
	"ref_video_second_price": {"参考视频每秒单价", "map", "按秒计费（与按条互斥），键与 second_price 同一套档位"},
	"per_task_price":         {"每任务单价", "number", "一次任务固定价（音乐的存量口径，与交付条数无关）"},
	"per_clip_price":         {"每条单价", "number", "音乐/歌词按实际交付条数计费；0 条交付免单"},
	"per_second_price":       {"每秒单价", "number", "音频按产物秒数计费"},
	"min_price":              {"最低消费", "number", "单次不足此额按此额收（模拟上游最低计费时长）"},
}

type specPriceFieldMeta struct {
	Label string
	Kind  string
	Desc  string
}

// adapterConfigFields 是 L1 声明式配置的字段清单（与 relay.AdapterConfig 同构）。
var adapterConfigFields = []AdapterConfigField{
	{Key: "auth_header", Label: "鉴权头名", Desc: "缺省 Authorization"},
	{Key: "auth_prefix", Label: "鉴权前缀", Desc: "例如 Bearer（含空格请自行保留）"},
	{Key: "path_overrides", Label: "路径改写", Desc: "平台路径 → 上游路径"},
	{Key: "request_patch", Label: "请求补丁", Desc: "按字段覆盖/追加常量，保留键不可写"},
	{Key: "response_path", Label: "响应取字段", Desc: "从非标准响应里取内容与用量（仅非流式对话）"},
}

// fieldLabels 是常见请求字段的中文名（给 AI 与运营看，不参与任何判定）。
var fieldLabels = map[string]string{
	"prompt":           "提示词",
	"negative_prompt":  "反向提示词",
	"input":            "输入内容",
	"messages":         "对话消息",
	"image":            "首帧/参考图",
	"image_url":        "图片地址",
	"image_tail":       "尾帧图",
	"image_list":       "多图列表",
	"size":             "尺寸",
	"resolution":       "分辨率档位",
	"aspect_ratio":     "画幅比例",
	"duration":         "时长（秒）",
	"seconds":          "秒数",
	"n":                "产出数量",
	"seed":             "随机种子",
	"mode":             "模式",
	"sound":            "是否带声音",
	"watermark":        "是否带水印",
	"guidance_scale":   "提示词遵循度",
	"steps":            "采样步数",
	"callback_url":     "回调地址",
	"reference_image":  "参考图",
	"reference_audio":  "参考音频",
	"reference_videos": "参考视频",
}

// Build 组装目录（每次调用都重新投影，价格字段集反射结果有缓存，代价可忽略）。
// capabilities 投影 apidoc 的能力词表（顺序即展示顺序，与文档预设一致）。
//
// 不排序、不重排：词表顺序就是右栏 TAB 的顺序，两处不一致会让
// 「管理台勾选顺序」与「文档 TAB 顺序」看起来像两个不同的东西。
func capabilities() []Capability {
	defs := apidoc.CapabilityDefs()
	out := make([]Capability, 0, len(defs))
	for _, d := range defs {
		out = append(out, Capability{
			Key:        string(d.Key),
			Label:      d.Label,
			Modalities: append([]string(nil), d.Modalities...),
			Endpoint:   d.Endpoint,
		})
	}
	return out
}

func Build() Catalog {
	return Catalog{
		Version:      Version,
		Modalities:   append([]Modality(nil), modalityRules...),
		BillingModes: append([]BillingMode(nil), billingModeLabels...),
		Endpoints:    endpoints(),
		Capabilities: capabilities(),
		SpecPrices:   specPrices(),
		Adapters:     adapters(),
		AdapterConfig: AdapterConfigView{
			Fields:       append([]AdapterConfigField(nil), adapterConfigFields...),
			ReservedKeys: ReservedPatchKeys(),
		},
		FieldLabels: fieldLabels,
		Limits: Limits{
			Spec: SpecLimits{
				MaxEndpoints:   specdsl.MaxEndpoints,
				MaxConstKeys:   specdsl.MaxConstKeys,
				MaxCopyKeys:    specdsl.MaxCopyKeys,
				MaxArrays:      specdsl.MaxArrays,
				MaxArrayItems:  specdsl.MaxArrayItems,
				MaxStaticQuery: specdsl.MaxStaticQuery,
				MaxPathLen:     specdsl.MaxPathLen,
				MaxSpecBytes:   specdsl.MaxSpecBytes,
				MaxSlugLen:     specdsl.MaxSlugLen,
				MaxLabelRunes:  specdsl.MaxLabelRunes,
				PollKeySuffix:  specdsl.PollKeySuffix,
				AuthHeaders:    specdsl.AuthHeaders(),
			},
			Pricing: PriceLimits{MaxSeconds: billing.MaxPriceSeconds},
		},
	}
}

func endpoints() []Endpoint {
	all := apidoc.SpecEndpointTable()
	out := make([]Endpoint, 0, len(all))
	for _, e := range all {
		out = append(out, Endpoint{
			Kind: string(e.Kind), Key: e.Key, Method: e.Method, Path: e.Path,
			Modality: e.Modality, Async: e.Async, Bridge: e.BridgeAllowed, Binary: e.Binary,
			Supported: e.SpecSupported, Reason: e.UnsupportedReason,
		})
	}
	return out
}

func specPrices() SpecPricesSchema {
	fields := make([]SpecPriceField, 0, 16)
	for _, key := range billing.SpecPriceFields() {
		meta := priceFieldLabels[key]
		fields = append(fields, SpecPriceField{Key: key, Label: meta.Label, Kind: meta.Kind, Desc: meta.Desc})
	}
	return SpecPricesSchema{
		Fields: fields,
		TierKeys: map[string][]string{
			"image": billing.ImageTierKeys,
			"video": billing.VideoTierKeys,
		},
		Kinds: map[string]string{
			"image": billing.SpecKindPerImage,
			"video": billing.SpecKindPerVideo,
			"music": billing.SpecKindPerClip,
			// 音频按秒：与 modalities[audio].spec_kind 必须是同一个值。
			// 两处不一致时，助手会按"音频没有计价格"的结论放弃 spec 路线。
			"audio": billing.SpecKindPerSecond,
		},
	}
}

func adapters() []Adapter {
	// 只列内置：schema 目录给 AI 用，规格是它的产出物，不能出现在"可选项"里
	// （否则模型会以为 `spec:xxx` 是内置适配器，写出引用自身产物的渠道）。
	list := relay.Builtins()
	out := make([]Adapter, 0, len(list))
	for _, a := range list {
		out = append(out, Adapter{
			Name:                a.Name(),
			Caps:                a.Caps(),
			SupportsDeclarative: relay.SupportsDeclarativeConfig(a.Name()),
			SupportsOptions:     relay.SupportsAdapterOptions(a.Name()),
		})
	}
	return out
}

// ReservedPatchKeys 是 request_patch 不能写死的字段（计费维度）。
//
// 单一来源是 relay/spec 的保留键表；这里只做排序后的复制，
// 两处各写一份的结果是「规格禁止写死 seconds，L1 补丁却能写死」。
func ReservedPatchKeys() []string {
	out := specdsl.ReservedConstKeys()
	sort.Strings(out)
	return out
}

/* ------------------------------ 查询辅助（校验用） ------------------------------ */

// ModalitySet 返回「模态 → 是否允许」的集合形态（供 controller 的白名单直接用）。
func ModalitySet() map[string]bool {
	out := make(map[string]bool, len(modalityRules))
	for _, m := range modalityRules {
		out[m.ID] = true
	}
	return out
}

// IsModality 判断模态是否在枚举内。
func IsModality(m string) bool {
	for _, r := range modalityRules {
		if r.ID == m {
			return true
		}
	}
	return false
}

// AllowsBillingMode 判断「模态 × 计费方式」是否合法。
//
// 未知模态一律不放行：否则运营手滑写个 "vidoe" 就能绕过所有组合校验，
// 落到运行期变成"计价格解析不出来 → 拒绝调用"（比保存期报错难排查得多）。
func AllowsBillingMode(modality string, mode string) bool {
	for _, r := range modalityRules {
		if r.ID != modality {
			continue
		}
		for _, allowed := range r.BillingModes {
			if allowed == mode {
				return true
			}
		}
		return false
	}
	return false
}

// AllowedBillingModes 返回某模态允许的计费方式（错误提示直接展示给运营）。
func AllowedBillingModes(modality string) []string {
	for _, r := range modalityRules {
		if r.ID == modality {
			return append([]string(nil), r.BillingModes...)
		}
	}
	return nil
}

// SpecKindOf 返回模态的计价格 kind（非按规格计费的模态返回空串）。
func SpecKindOf(modality string) string {
	for _, r := range modalityRules {
		if r.ID == modality {
			return r.SpecKind
		}
	}
	return ""
}
