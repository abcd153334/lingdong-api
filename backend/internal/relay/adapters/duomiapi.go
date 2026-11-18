package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"lingdong-api/internal/relay"
)

// DuoMi 适配器：多米API（duomiapi.com）聚合上游，一个密钥可调视频 / 图像 / 音乐多类模型。
//
// 设计要点（改动前先读）：
//  1. **一个适配器 + 多子协议**。多米把多家上游（火山 Seedance、可灵、Gemini 图像、SUNO）
//     统一在同一域名下，但请求/响应信封各不相同。子协议用 profile.Protocol 表示，
//     提交时回写 SubKind（如 "kling.image2video"）落 tasks.subtask_kind，轮询据此选查询路径。
//     **提交与轮询是两次独立调用，SubKind 是唯一的上下文载体**，漏落库 = 查错端点、产物永远拿不到。
//  2. **出站体一律白名单重建**。上游把平台的请求体做过 normalizeSubmitBody 改写
//     （duration→seconds、width/height→resolution、image→input_reference），
//     原样透传会把平台内部字段（resolution/size）塞给不认它的上游，直接 400。
//  3. **影响计费的维度只认模型档案**。可灵的画质由 model_name + mode 决定、请求无法指定，
//     所以 mode 只来自档案/渠道配置，请求体里的同名键**一律不读**（否则能用 std 价买 pro）。
//     请求若显式传了与档案不符的 resolution，直接 CallerParamError（那一定是计费倒挂）。
//  4. **metadata 是允许清单**。generate_audio / watermark 会改变上游产出与成本，
//     开放给调用方等于让人自助改计费维度；默认只放行 aigc_watermark。
//  5. **参数错用 relay.CallerParamError**：任务服务据此短路候选循环、直接给调用方 400。
//     用错会把「用户的锅」变成「上游提交失败 502」，还会污染渠道健康度。
type DuoMi struct{}

// 子协议标识（profile.Protocol）。
const (
	duomiProtoSeedance   = "seedance"
	duomiProtoKling      = "kling"       // 按素材自动选 text2video / image2video
	duomiProtoKlingMulti = "kling.multi" // 固定多图参考
	duomiProtoKlingOmni  = "kling.omni"  // 固定 omni-video（3–15 秒）
	// 图像（B6-1）：nano 系是 Gemini 图像，gptimage 系是 OpenAI 兼容图像。
	//
	// nano **只有一个协议**：官方把文生图与图生图拆成两个 POST 端点
	// （/api/gemini/nano-banana 与 /nano-banana-edit），但用的是同一套 model 枚举、
	// 同一个查询端点。端点由「有没有参考图」决定，不是模型属性 —— 把端点写进协议
	// 会造成「同一个模型只能干一件事」，并逼出一堆假模型（nano-banana-edit）。
	duomiProtoNano     = "nano"
	duomiProtoGPTImage = "gptimage"
	// 音乐（B6-1）：SUNO 生成（一次 2 首）与 SUNO 歌词。
	duomiProtoSunoMusic  = "suno.music"
	duomiProtoSunoLyrics = "suno.lyrics"
	// 视频（muse）：多米**自有**视频协议，不是任何一家的转发。
	//
	// 提交 POST /v1/videos/generations，查询 GET /v1/videos/tasks/{id}。
	// 这两段路径**没有任何前缀关系**：查询不是"提交路径 + /{id}"。写成后者不会报错，
	// 只会让每次轮询都 404，把已生成好的视频拖到超时退费。
	duomiProtoMuse = "muse"
)

// SubKind（落 tasks.subtask_kind，轮询据此选路径）。
const (
	duomiSubSeedance  = "seedance"
	duomiSubKlingText = "kling.text2video"
	duomiSubKlingImg  = "kling.image2video"
	duomiSubKlingMult = "kling.multi-image2video"
	duomiSubKlingOmni = "kling.omni-video"
	// duomiSubMuse 落 tasks.subtask_kind。轮询据它选 /v1/videos/tasks 前缀 ——
	// 空值会落 duomiPollTarget 的 default 分支（seedance 路径），那是本协议最贵的错。
	duomiSubMuse = "muse"
)

// 上游路径。
const (
	duomiSeedanceTaskPath = "/api/v3/contents/generations/tasks"
	duomiKlingVideoPrefix = "/api/video/kling/v1/videos/"

	duomiKlingText2Video  = "text2video"
	duomiKlingImage2Video = "image2video"
	duomiKlingMulti2Video = "multi-image2video"
	duomiKlingOmniVideo   = "omni-video"

	// muse 的提交路径与**查询路径前缀**是两个独立常量：上游没有"提交路径 + /{id}"这种结构。
	duomiMuseSubmitPath = "/v1/videos/generations"
	duomiMusePollPrefix = "/v1/videos/tasks"
)

// 配额与长度上限。
const (
	duomiMaxOptionsBytes = 4096
	duomiMaxPromptRunes  = 20000
	duomiMaxTaskIDLen    = 128
	duomiMaxPathLen      = 200
	duomiMaxKlingImages  = 4 // multi-image2video 的 image_list 上限（omni 是 7，见 duomiMaxOmniImages）
)

// duomiDefaultAuthPrefix 是默认鉴权前缀。多米文档的示例头是 `Authorization: Bearer <token>`，
// 但也有渠道只认裸 token；渠道配置 auth_prefix 可切（填 "-" 表示空前缀）。
const duomiDefaultAuthPrefix = "Bearer "

// duomiTaskIDPattern 限定上游任务号字符集：既挡注入，也让 URL 拼接无需担心路径穿越。
// 可灵与 seedance 的任务号都是 UUID 形态；放宽到字母数字与 -_ 覆盖两者。
var duomiTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var errDuoMiNoEndpoint = errors.New("适配器 duomiapi：不支持该端点（仅提供异步任务）")

// 编译期断言：改任何一个接口方法签名都会在这里报错，而不是等到线上类型断言失败。
var (
	_ relay.Adapter              = DuoMi{}
	_ relay.TaskAdapter          = DuoMi{} // 旧签名，PollTaskOf 的回落路径用
	_ relay.DetailedTaskPoller   = DuoMi{} // 新接口，按 SubKind 选查询路径
	_ relay.OptionsSupporter     = DuoMi{} // adapter_options 由本适配器校验
	_ relay.EndpointSupporter    = DuoMi{}
	_ relay.DeclarativeSupporter = DuoMi{}
	_ relay.ModelPreflighter     = DuoMi{} // 保存绑定时告知「模型能否被承接」
)

func init() {
	// 注册失败只可能是命名冲突（编码错误），与其它适配器保持一致。
	_ = relay.RegisterAdapter(DuoMi{})
}

// Name 返回适配器标识（对齐 channels.adapter_id）。
func (DuoMi) Name() string { return "duomiapi" }

// Caps 返回能力子集：视频 + 图像 + 图生图 + 音乐（B6-1 起名副其实）。
//
// **不放 tts**：SUNO 是音乐不是 TTS，放 tts 会让同步 /v1/audio/speech 选到该渠道
// （adapterSupports 只看 caps，SupportsEndpoint 已全 false，此处为双保险）。
//
// music 是平台新增的第 8 类模态（决策 H）：它只出现在**异步任务**侧，
// 不参与同步端点选路，所以这里列出它不会给 /v1/audio/* 的同步转发带来误选。
func (DuoMi) Caps() []string { return []string{"video", "image", "edit_image", "music"} }

// SupportsDeclarativeConfig 显式声明不支持声明式配置：路径与鉴权由本适配器按子协议决定，
// auth_header 之类的渠道级改写会让不同子协议互相污染。
func (DuoMi) SupportsDeclarativeConfig() bool { return false }

// SupportsEndpoint 声明本适配器承接哪些**同步**端点。
//
// 只放行图像两个端点（B6-1 之后的同步桥接，见 duomiapi_sync_image.go）：
//   - /v1/images/generations：文生图 / gpt-image 带参考图
//   - /v1/images/edits：图生图与编辑（multipart 由 relay 转成公网直链后进来）
//
// **其余端点必须保持 false**：本适配器没有任何同步聊天/音频实现，
// 放行会让同步网关把 chat 请求派过来，然后在 Forward 里抛协议错——
// 那种错会被归因成渠道故障，把一条健康渠道熔断。
func (DuoMi) SupportsEndpoint(kind relay.EndpointKind) bool {
	switch kind {
	case relay.KindImagesGenerations, relay.KindImagesEdits:
		return true
	}
	return false
}

// AsyncEndpoints：多米是唯一承接四类异步端点的内置适配器
// （自有多路径视频 / nano+gpt-image 图像异步 / SUNO 音乐与歌词）。
func (DuoMi) AsyncEndpoints() []string {
	return []string{
		"video/generations",
		"images/generations",
		"audio/music/generations",
		"audio/lyrics",
	}
}

// ---------------------------------------------------------------- 模型档案

// duomiProfile 是一个对外模型 ID 的上游档案。
//
// 为什么要有这张表（而不是只用绑定表的 upstream_model）：可灵的协议路径取决于
// 「有没有参考图 / 几张 / 是不是 omni」，这些信息不在绑定表里；mode（std/pro）
// 又是决定画质档位的**计费维度**，只能固化在代码或渠道配置里。
type duomiProfile struct {
	// Protocol 是子协议：seedance / kling / kling.multi / kling.omni。
	Protocol string
	// Upstream 是上游模型名（绑定表 upstream_model 仍优先，这里作为兜底与文档锚点）。
	Upstream string
	// Mode 只对可灵有效（std / pro）：决定上游画质档位。
	//
	// **空值表示上游不认 mode 字段**（例如 kling-v1.6-multi）。空值必须做到
	// 「不下发该字段」而不是「下发空串」：可灵把空串当成非法枚举，直接 400，
	// 而这条错误会被归因成「渠道故障」，把一条健康渠道熔断。
	Mode string
	// DurationAsString 为 true 时 duration 以字符串下发（上游按 string 校验的实现）。
	DurationAsString bool
	// Family 是命中的协议族名（"kling.pro" / "gpt-image"），只用于日志与错误文案。
	// 精确表命中时为空 —— 它代表「这条档案是内置的」，不需要族名来解释。
	Family string
	// 这里曾经有六个「模型能力」字段：FixedTier / Durations / MinDuration /
	// MaxDuration / AllowImageSize / MaxTier。它们让适配器在提交期替上游判定
	// 「这个型号能出 720p 还是 4k、时长只能 5/10、图像能不能带 image_size」，
	// 与实际后果相反：上游一扩容，平台就先把自己的用户挡在门外；而型号升级
	// （同一 ID 换了上游、能力变大）时，代码里的旧上限会静默把请求截断或拒绝。
	//
	// 现在这些约束全部交还给厂商官方文档：用户写什么就下发什么，上游不认会明确报错。
	// 平台只保留**计费必需**的东西（Procotol / Upstream / Mode / DurationAsString）。
}

// duomiModels 是对外模型 ID → 上游档案。
//
// 这张表只放**内置且需要固定 mode / 上游别名**的型号（开箱可用）。
// 厂商新增同族型号**不需要改这里**：duomiapi_family.go 会按平台模型 ID 的前缀推断协议，
// 上游真名由绑定的 upstream_model 提供。只有「跨族但走同一信封」或「命名看不懂」时
// 才需要渠道的 model_overrides。
var duomiModels = map[string]duomiProfile{
	"seedance-2.0":      {Protocol: duomiProtoSeedance, Upstream: "doubao-seedance-2-0-260128"},
	"seedance-2.0-fast": {Protocol: duomiProtoSeedance, Upstream: "doubao-seedance-2-0-fast-260128"},
	// 可灵 v3：mode 决定画质（std→720p / pro→1080p），请求无法指定分辨率。
	"kling-v3-std": {Protocol: duomiProtoKling, Upstream: "kling-v3", Mode: "std"},
	"kling-v3-pro": {Protocol: duomiProtoKling, Upstream: "kling-v3", Mode: "pro"},
	// omni 的 duration 只认字符串写法（与 kling-v1.6-multi 同款：上游按 string 校验）。
	"kling-v3-omni": {Protocol: duomiProtoKlingOmni, Upstream: "kling-v3-omni", Mode: "std",
		DurationAsString: true},
	// 多图参考：上游**不接受 mode**（画质由 multi-image2video 端点本身决定），
	// duration 只认字符串写法。两条都是该型号的硬约束，写错不会报错、只会 400。
	"kling-v1.6-multi": {
		Protocol: duomiProtoKlingMulti, Upstream: "kling-v1-6", DurationAsString: true,
	},

	// ---------------------------------------------------------------- 视频（muse）
	// 多米自有视频协议：1–8 张参考图 + 画幅，**没有 resolution 参数**。
	// Upstream 与对外 ID 同名仍显式写出：绑定表未填 upstream_model 时靠它兜底，
	// 同时它也是文档与测试的锚点。
	"muse-video": {Protocol: duomiProtoMuse, Upstream: "muse-video"},

	// ---------------------------------------------------------------- 图像（B6-1）
	// nano 系（Gemini 图像）：**上游 model 必须是官方 enum 的 gemini-* 名**，
	// `nano-banana` 只是文档里的展示名（x-apifox-enum.name），发出去会被当未知模型拒绝。
	// 这三条是**平台对外别名**：同一个上游模型对外只暴露一个平台 ID 时无需登记它们，
	// 登记只是给运营一个更好念的名字（协议与上游名都在这里固定，不会二次分叉）。
	"nano-banana":     {Protocol: duomiProtoNano, Upstream: "gemini-2.5-flash-image"},
	"nano-banana-2":   {Protocol: duomiProtoNano, Upstream: "gemini-3.1-flash-image-preview"},
	"nano-banana-pro": {Protocol: duomiProtoNano, Upstream: "gemini-3-pro-image-preview"},
	// gpt-image 系（OpenAI 兼容，异步任务）：size / quality / background / mask / image 原样透传。
	"gpt-image-2":            {Protocol: duomiProtoGPTImage, Upstream: "gpt-image-2"},
	"gpt-image-2-vip":        {Protocol: duomiProtoGPTImage, Upstream: "gpt-image-2-vip"},
	"gpt-image-2.5":          {Protocol: duomiProtoGPTImage, Upstream: "gpt-image-2.5"},
	"gpt-image-2.5-flare":    {Protocol: duomiProtoGPTImage, Upstream: "gpt-image-2.5-flare"},
	"gpt-image-2.5-sunburst": {Protocol: duomiProtoGPTImage, Upstream: "gpt-image-2.5-sunburst"},

	// ---------------------------------------------------------------- 音乐（B6-1）
	// SUNO 一次生成 2 首（上游行为，不是参数）：计费按 per_task 一次任务，
	// 产物按实际 2 条返回，用户拿不到的不收（B3 已按成功转存数结算）。
	"chirp-v4": {Protocol: duomiProtoSunoMusic, Upstream: "chirp-v4"},
	// 歌词：上游不吃 model 字段（编号由任务号决定），Upstream 留空，
	// 提交时不带 model（带了会被当成未知模型拒绝）。
	"suno-lyrics": {Protocol: duomiProtoSunoLyrics},
}

// duomiProtocols 是 model_overrides 允许的协议枚举（由 options_schema.go 的有序表派生）。
var duomiProtocols = stringSet(duomiProtocolOrder)

// duomiProtocolList 是给运维看的协议枚举（同一份来源，不会与校验集合漂移）。
var duomiProtocolList = strings.Join(duomiProtocolOrder, " / ")

// duomiPathKeys 是 channels.adapter_options.paths 允许的键（由 options_schema.go 的有序表派生）。
var duomiPathKeys = stringSet(duomiPathOrder)

// duomiPathKeyList 是给运维看的路径键枚举（同一份来源）。
var duomiPathKeyList = strings.Join(duomiPathOrder, " / ")

// ---------------------------------------------------------------- 渠道私有配置

// duomiOptions 是 channels.adapter_options 归一后的结果。
type duomiOptions struct {
	authPrefix     string
	modelOverrides map[string]duomiOverride
	paths          map[string]string
	metadataAllow  map[string]bool
	// suno 是音乐子协议的渠道级默认值（B6-1）。
	//
	// 为什么放在渠道而不是请求里：SUNO 的 custom_mode/make_instrumental 决定
	// 「这次出的是歌还是纯音乐」，属于**产品形态**而不是用户每次要传的参数；
	// 让调用方随便改会让同一模型产出完全不同的东西，计费口径（per_task）却一样。
	// 调用方仍可显式覆盖（见 duomiBody.CustomMode），但默认值由运营定。
	suno duomiSunoDefaults
}

// duomiSunoDefaults 是 SUNO 的渠道级默认参数。
type duomiSunoDefaults struct {
	customMode   bool
	instrumental bool
	// mv 是 SUNO 的模型版本（chirp-v4 等）。空 = 用绑定表的上游模型名。
	mv string
}

// duomiOverride 是单个模型的协议覆盖（键 = 对外模型 ID）。
type duomiOverride struct {
	Protocol string `json:"protocol"`
	Mode     string `json:"mode"`
	Upstream string `json:"upstream_model"`
	// DurationAsString 覆盖「duration 是否按字符串下发」。
	// 指针语义：不写 = 沿用内置档案，写 false 才是「显式要求数字」。
	DurationAsString *bool `json:"duration_as_string"`
}

func duomiCfgErr(field, msg string) error {
	return &relay.ConfigError{Scope: "渠道适配器配置错误", Field: field, Msg: msg}
}

// parseDuomiOptions 是**唯一**的配置解析入口（保存期校验与运行期共用）。
// 空（nil/""/null/{}）= 全部使用内置默认。
func parseDuomiOptions(raw []byte) (*duomiOptions, error) {
	opts := &duomiOptions{
		authPrefix:     duomiDefaultAuthPrefix,
		modelOverrides: map[string]duomiOverride{},
		paths:          map[string]string{},
		// 默认允许清单（P1-25）：seedance 真正会改成本的两个键。
		// 故意**不**默认放行 aigc_watermark —— 它是历史键，且与 watermark 语义重叠，
		// 默认开着等于让调用方默认就能改上游成本。
		metadataAllow: stringSet(duomiDefaultMetadataAllow),
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return opts, nil
	}
	if len(raw) > duomiMaxOptionsBytes {
		return nil, duomiCfgErr("adapter_options", fmt.Sprintf("配置过大（上限 %d 字节）", duomiMaxOptionsBytes))
	}

	var doc struct {
		AuthPrefix     *string                  `json:"auth_prefix"`
		ModelOverrides map[string]duomiOverride `json:"model_overrides"`
		Paths          map[string]string        `json:"paths"`
		MetadataAllow  []string                 `json:"metadata_allow"`
		// SunoDefaults 是音乐子协议的渠道级默认（B6-1）。
		SunoDefaults *struct {
			CustomMode   *bool  `json:"custom_mode"`
			Instrumental *bool  `json:"make_instrumental"`
			MV           string `json:"mv"`
		} `json:"suno_defaults"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 未知字段必须报错：拼错一个键（如 path 少个 s）会让整段配置静默不生效，
	// 运营只能靠"结果不对"来发现。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, duomiCfgErr("adapter_options", "配置不是合法的 JSON 或含未知字段："+err.Error())
	}
	if dec.More() {
		return nil, duomiCfgErr("adapter_options", "配置尾部有多余内容")
	}

	if doc.AuthPrefix != nil {
		p, perr := normalizeDuomiAuthPrefix(*doc.AuthPrefix)
		if perr != nil {
			return nil, duomiCfgErr("auth_prefix", perr.Error())
		}
		if len(p) > 32 {
			return nil, duomiCfgErr("auth_prefix", "鉴权前缀过长（上限 32 字符）")
		}
		opts.authPrefix = p
	}

	for id, ov := range doc.ModelOverrides {
		key := strings.TrimSpace(id)
		if key == "" {
			return nil, duomiCfgErr("model_overrides", "模型 ID 不能为空")
		}
		if ov.Protocol != "" && !duomiProtocols[ov.Protocol] {
			return nil, duomiCfgErr("model_overrides."+key+".protocol",
				fmt.Sprintf("协议 %q 不支持（可选：%s）", ov.Protocol, duomiProtocolList))
		}
		if ov.Mode != "" && ov.Mode != "std" && ov.Mode != "pro" {
			return nil, duomiCfgErr("model_overrides."+key+".mode",
				fmt.Sprintf("mode %q 不支持（只支持 std / pro）", ov.Mode))
		}
		// kling.multi（multi-image2video）**不接受 mode**：上游按端点本身决定画质。
		// 下发 mode 会直接 400，而那条错误会被当成渠道故障 → 熔断一条健康渠道。
		// 在保存期拦住，比在提交期拦成本低得多（提交期还要先冻结再退款）。
		if ov.Protocol == duomiProtoKlingMulti && ov.Mode != "" {
			return nil, duomiCfgErr("model_overrides."+key+".mode",
				"kling.multi 协议不支持 mode（画质由 multi-image2video 端点本身决定），请删除该字段")
		}
		opts.modelOverrides[key] = ov
	}

	for k, v := range doc.Paths {
		if !duomiPathKeys[k] {
			return nil, duomiCfgErr("paths."+k, "未知的路径键（可选："+duomiPathKeyList+"）")
		}
		p := strings.TrimSpace(v)
		if !strings.HasPrefix(p, "/") {
			return nil, duomiCfgErr("paths."+k, "路径必须以 / 开头")
		}
		if strings.ContainsAny(p, "?#") {
			return nil, duomiCfgErr("paths."+k, "路径不能包含查询串或锚点")
		}
		if len(p) > duomiMaxPathLen {
			return nil, duomiCfgErr("paths."+k, fmt.Sprintf("路径过长（上限 %d 字符）", duomiMaxPathLen))
		}
		// kling.prefix 归一为「以 / 结尾」（P0-13）：提交与轮询都靠它拼路径，
		// 少了尾斜杠会拼出 `/v1/videostext2video`，提交直接 404。
		// 与 metaso 的 query_path 不同，这里**做**归一：它只是前缀（不是完整路径），
		// 归一不会改变语义，只消除一个纯格式陷阱。
		if k == "kling.prefix" && !strings.HasSuffix(p, "/") {
			p += "/"
		}
		opts.paths[k] = p
	}

	// SUNO 渠道默认值（B6-1）：只在显式配置时生效，缺省保持「非自定义模式、带人声」。
	if doc.SunoDefaults != nil {
		if doc.SunoDefaults.CustomMode != nil {
			opts.suno.customMode = *doc.SunoDefaults.CustomMode
		}
		if doc.SunoDefaults.Instrumental != nil {
			opts.suno.instrumental = *doc.SunoDefaults.Instrumental
		}
		if mv := strings.TrimSpace(doc.SunoDefaults.MV); mv != "" {
			if len(mv) > 64 {
				return nil, duomiCfgErr("suno_defaults.mv", "mv 过长（上限 64 字符）")
			}
			opts.suno.mv = mv
		}
		// 纯音乐 + 自定义歌词是互斥的产品形态：同时开等于「给的歌词永远不会被唱」，
		// 属于必然的运营误配，保存期就拒（运行期调用方还能覆盖，见 submitDuomiSuno）。
		if opts.suno.customMode && opts.suno.instrumental {
			return nil, duomiCfgErr("suno_defaults",
				"custom_mode 与 make_instrumental 不能同时为 true（纯音乐不唱歌词）")
		}
	}

	if doc.MetadataAllow != nil {
		if len(doc.MetadataAllow) > 8 {
			return nil, duomiCfgErr("metadata_allow", "允许键过多（上限 8 个）")
		}
		allow := map[string]bool{}
		for _, k := range doc.MetadataAllow {
			key := strings.TrimSpace(k)
			if key == "" {
				return nil, duomiCfgErr("metadata_allow", "允许键不能为空")
			}
			if !duomiMetadataKnown(key) {
				return nil, duomiCfgErr("metadata_allow",
					fmt.Sprintf("键 %q 不是已知的透传参数（可选：aigc_watermark / generate_audio / watermark）", key))
			}
			allow[key] = true
		}
		opts.metadataAllow = allow
	}

	// 覆盖表不得把已知模型改成不兼容协议：运营把 seedance-2.0 指向 nano 这类错误，
	// 提交时才发现会把"配置错"表现成"上游拒绝"，排查成本极高。
	for id, ov := range opts.modelOverrides {
		base, ok := duomiModels[id]
		if !ok || ov.Protocol == "" {
			continue
		}
		if base.Protocol != ov.Protocol {
			return nil, duomiCfgErr("model_overrides."+id+".protocol",
				fmt.Sprintf("模型 %s 属于 %s 协议，不能改成 %s（新增型号请换一个对外模型 ID）", id, base.Protocol, ov.Protocol))
		}
	}
	return opts, nil
}

// normalizeDuomiAuthPrefix 归一鉴权前缀（P2-07）。
//
// 三条必须归一，否则表现为「鉴权失败」而运营完全看不出是自己填错了：
//  1. `-` 是「空前缀」的显式写法（表单里空字符串无法区分"未配置"）；
//  2. 重复的 `Bearer `（表单拼接、复制粘贴时常见）只保留一个；
//  3. 自定义前缀**必须带尾随空格**，否则会拼成 `Tokenabc123`（少了分隔）。
func normalizeDuomiAuthPrefix(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if strings.ContainsAny(p, "\r\n") {
		return "", errors.New("鉴权前缀不能包含换行")
	}
	if p == "-" {
		return "", nil
	}
	bearer := false
	for {
		t := strings.TrimSpace(p)
		// 只把「独立的 Bearer」当方案名：`BearerToken` 这种是自定义前缀，不能剥。
		if len(t) >= 6 && strings.EqualFold(t[:6], "bearer") {
			rest := t[6:]
			if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
				bearer = true
				p = strings.TrimSpace(rest)
				continue
			}
		}
		break
	}
	if p == "" {
		if bearer {
			return "Bearer ", nil
		}
		return "", nil
	}
	if bearer {
		return "Bearer " + p, nil
	}
	if !strings.HasSuffix(p, " ") {
		p += " "
	}
	return p, nil
}

// duomiMetadataKnown 报告某个键是否在「可放行」的全量清单内（比默认放行清单宽）。
//
// seedance 的 generate_audio / watermark 直接改变上游产出与成本；
// aigc_watermark 是历史键（保留兼容，语义与 watermark 重叠）。
// kling 系没有「metadata 透传」概念（见 submitDuomiKling）。
func duomiMetadataKnown(k string) bool { return duomiMetadataSet[k] }

// duomiMetadataSet 是 duomiMetadataKnown 的查表形式（由 options_schema.go 的有序清单派生）。
var duomiMetadataSet = stringSet(duomiMetadataOrder)

// ValidateAdapterOptions 校验渠道私有配置（保存期）。
func (DuoMi) ValidateAdapterOptions(raw []byte) error {
	_, err := parseDuomiOptions(raw)
	return err
}

// ---------------------------------------------------------------- 档案解析

// duomiResolveProfile 按「渠道覆盖 > 内置档案」解析模型档案。
//
// 参数 modelID 是平台下发的对外模型 ID（不信任请求体），bodyModel 仅在 modelID 为空时兜底
// （重试路径的老任务可能没有 ModelID）。
func duomiResolveProfile(opts *duomiOptions, modelID, bodyModel string) (duomiProfile, string, error) {
	id := strings.TrimSpace(modelID)
	if id == "" {
		id = strings.TrimSpace(bodyModel)
	}
	if id == "" {
		return duomiProfile{}, "", &relay.CallerParamError{Msg: "请求缺少 model"}
	}

	// ① 骨架：内置精确表 → 族前缀推断 → 零值。
	//
	// 精确表优先：它承载内置型号固定的 mode 与上游别名，不能被族表反过来覆盖。
	// 族表只补「表里没有、但命名明显同族」的缺口（厂商升级型号的常态）。
	p, hasBase := duomiModels[id]
	if !hasBase {
		if fam, ok := duomiMatchFamily(id); ok {
			if fam.Unsupported != "" {
				return duomiProfile{}, id, duomiCfgErr("model_overrides",
					duomiUnsupportedFamilyMsg(id, fam.Unsupported))
			}
			p = duomiProfile{
				Protocol:         fam.Protocol,
				Mode:             fam.Mode,
				DurationAsString: fam.DurationAsString,
				Family:           fam.Name,
			}
			hasBase = true
		}
	}

	// ② 渠道覆盖是**字段级**的，永远是最后一步：运营显式声明优先于任何推断。
	ov, hasOv := opts.modelOverrides[id]
	if !hasBase && !hasOv {
		return duomiProfile{}, id, duomiCfgErr("model_overrides", duomiUnknownModelMsg(id))
	}
	if hasOv {
		if ov.Protocol != "" {
			p.Protocol = ov.Protocol
			p.Family = "override"
		}
		if ov.Mode != "" {
			p.Mode = ov.Mode
		}
		if ov.Upstream != "" {
			p.Upstream = ov.Upstream
		}
		if ov.DurationAsString != nil {
			p.DurationAsString = *ov.DurationAsString
		}
	}

	if p.Protocol == "" {
		return duomiProfile{}, id, duomiCfgErr("model_overrides", fmt.Sprintf("模型 %s 未配置协议", id))
	}
	if !duomiProtocols[p.Protocol] {
		return duomiProfile{}, id, duomiCfgErr("model_overrides",
			fmt.Sprintf("模型 %s 的协议 %q 不支持", id, p.Protocol))
	}
	// 新模型总开关放在这里（唯一解析入口）：同步图像桥与异步提交都会经过它，
	// 否则「置 0 止血」只对异步生效，/v1/images/generations 照样出网扣费。
	if err := duomiCheckNewModelGate(p.Protocol); err != nil {
		return duomiProfile{}, id, err
	}
	return p, id, nil
}

// ---------------------------------------------------------------- 提交

// SubmitTask 提交异步任务，返回上游任务号，并把子协议写回 req.SubKind。
func (DuoMi) SubmitTask(ctx context.Context, cfg *relay.ChannelConfig, req *relay.TaskSubmitRequest) (string, error) {
	if cfg == nil {
		return "", duomiCfgErr("channel", "渠道配置为空")
	}
	// 配置脏了必须在组装请求之前失败：按默认配置出网等于用错误的路径/鉴权跑生产。
	if cfg.OptionsErr != nil {
		return "", cfg.OptionsErr
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return "", duomiCfgErr("keys", "渠道未配置密钥")
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return "", duomiCfgErr("keys", "渠道密钥包含非法字符")
	}
	opts, err := parseDuomiOptions(cfg.Options)
	if err != nil {
		return "", err
	}
	if req == nil {
		return "", &relay.CallerParamError{Msg: "请求体为空"}
	}
	var body duomiBody
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return "", &relay.CallerParamError{Msg: "请求体不是合法的 JSON 对象"}
	}

	// 原始报文只在这里挂一次：所有出站组装点（nano/gpt-image/kling/seedance/suno）
	// 都从 body.raw 取「未声明的顶层键」，不再层层传参（漏传不报错，只会静默丢字段）。
	body.raw = req.Body

	// content[] 只属于火山方舟原生报文。这里拒收而不是透传：
	// 多米系上游没有一条认这个字段，转发过去会被当未知键丢掉 ——
	// 用户以为传了参考图、成片里却没有，而账单照收。
	if !isJSONAbsent(body.Content) {
		return "", &relay.CallerParamError{Msg: `"content" 只在使用火山方舟原生报文的模型上可用；` +
			`当前模型请改用 input_reference 传素材，并把画面描述写在 prompt 里`}
	}

	profile, modelID, err := duomiResolveProfile(opts, req.ModelID, body.Model)
	if err != nil {
		return "", err
	}
	upstream := strings.TrimSpace(req.UpstreamModel)
	if upstream == "" {
		upstream = profile.Upstream
	}
	// 歌词协议上游不吃 model 字段（编号由任务号决定），因此允许为空。
	// 其余协议的上游真名**只能来自绑定**：内置档案里的 Upstream 只是历史兜底与文档锚点，
	// 厂商换型号时它必然过期。文案必须指向「模型绑定」而不是 model_overrides ——
	// 后者是配协议的，跟上游名无关，写错方向会白试一轮。
	if upstream == "" && profile.Protocol != duomiProtoSunoLyrics {
		return "", duomiCfgErr("upstream_model", fmt.Sprintf(
			"模型 %s 未配置上游模型名：请在「模型 → 绑定」里填写 upstream_model（绑定优先于内置档案）", modelID))
	}

	var (
		taskID  string
		subKind string
	)
	switch profile.Protocol {
	case duomiProtoSeedance:
		taskID, subKind, err = submitDuomiSeedance(ctx, cfg, opts, profile, upstream, &body, req)
	case duomiProtoKling, duomiProtoKlingMulti, duomiProtoKlingOmni:
		taskID, subKind, err = submitDuomiKling(ctx, cfg, opts, profile, upstream, &body, req)
	case duomiProtoNano, duomiProtoGPTImage:
		taskID, subKind, err = submitDuomiImage(ctx, cfg, opts, profile, upstream, &body, duomiImageSizeOf(&body))
	case duomiProtoSunoMusic, duomiProtoSunoLyrics:
		taskID, subKind, err = submitDuomiSuno(ctx, cfg, opts, profile, upstream, &body)
	case duomiProtoMuse:
		taskID, subKind, err = submitDuomiMuse(ctx, cfg, opts, profile, upstream, &body, req)
	default:
		err = duomiCfgErr("model_overrides", fmt.Sprintf("协议 %q 暂未实现", profile.Protocol))
	}
	if err != nil {
		return "", err
	}
	req.SubKind = subKind
	return taskID, nil
}

// duomiNewModelProtocols 是需要受 LD_DUOMI_NEW_MODELS 开关约束的协议（B6-1 新增）。
var duomiNewModelProtocols = map[string]bool{
	duomiProtoNano:       true,
	duomiProtoGPTImage:   true,
	duomiProtoSunoMusic:  true,
	duomiProtoSunoLyrics: true,
}

// duomiNewModelsEnabled 读取新模型总开关（默认开；显式置 0/false/off 才关）。
//
// 为什么需要这个开关：图像/音乐是新增计费面（per_image / per_task），
// 上线后若发现某个上游的产物或计费口径不对，需要能在**不发版**的情况下立刻止血。
func duomiNewModelsEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LD_DUOMI_NEW_MODELS")))
	switch raw {
	case "", "1", "true", "on", "yes":
		return true
	}
	return false
}

// duomiCheckNewModelGate 拒绝已关闭的新模型。
//
// 用 ConfigError 而不是 CallerParamError：这是平台侧的运营开关，
// 报成「参数错」会让接入方以为自己传错了模型名，白白排查一轮。
func duomiCheckNewModelGate(protocol string) error {
	if !duomiNewModelProtocols[protocol] || duomiNewModelsEnabled() {
		return nil
	}
	return duomiCfgErr("model_overrides", fmt.Sprintf(
		"协议 %s 已被 LD_DUOMI_NEW_MODELS 关闭，暂不可用", protocol))
}

// ---------------------------------------------------------------- 轮询

// PollTask 实现旧签名（relay.PollTaskOf 在未实现 DetailedTaskPoller 时回落，这里保留是为了
// 让适配器在"只有旧接口"的调用路径上仍然可用）。
func (d DuoMi) PollTask(ctx context.Context, cfg *relay.ChannelConfig, upstreamTaskID string) (*relay.TaskStatus, error) {
	return d.PollTaskDetailed(ctx, cfg, &relay.TaskPollRequest{UpstreamTaskID: upstreamTaskID})
}

// PollTaskDetailed 按 SubKind 选查询路径。
//
// **不校验 OptionsErr**（与 metaso 一致）：配置在提交之后被改坏，不该让在途任务全部轮询失败——
// 连续 N 次失败会判超时退费，等于把已经生成好的产物丢掉。配置坏时用内置默认路径继续查。
func (DuoMi) PollTaskDetailed(ctx context.Context, cfg *relay.ChannelConfig, req *relay.TaskPollRequest) (*relay.TaskStatus, error) {
	if cfg == nil {
		return nil, duomiCfgErr("channel", "渠道配置为空")
	}
	if req == nil {
		return nil, errors.New("适配器 duomiapi：轮询请求为空")
	}
	id := strings.TrimSpace(req.UpstreamTaskID)
	if id == "" {
		return nil, errors.New("适配器 duomiapi：上游任务 ID 为空")
	}
	if len(id) > duomiMaxTaskIDLen || !duomiTaskIDPattern.MatchString(id) {
		return nil, errors.New("适配器 duomiapi：上游任务 ID 格式不合法")
	}
	opts, err := parseDuomiOptions(cfg.Options)
	if err != nil {
		opts = &duomiOptions{authPrefix: duomiDefaultAuthPrefix}
	}

	path, parse := duomiPollTarget(opts, req.SubKind, id)
	buf, err := duomiDo(ctx, cfg, opts.authPrefix, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return parse(buf)
}

// duomiKlingPath 是**唯一**的可灵路径拼接点（P0-13）：提交、轮询、探活三处必须同调。
//
// 改造前三处各写一遍 `prefix + seg + "/" + id`，其中只有提交做了 `TrimRight`：
// 运营把 prefix 写成不带尾斜杠（`/api/video/kling/v1/videos`）时，提交能成功，
// 轮询却拼成 `/api/video/kling/v1/videosomni-video/{id}` —— 任务生成好了也永远查不到，
// 一直 running 到超时退费。三处同调之后，这种"只在轮询阶段才炸"的缺陷不可能再出现。
func duomiKlingPath(prefix, seg, id string) string {
	p := strings.TrimSpace(prefix)
	if p == "" {
		p = duomiKlingVideoPrefix
	}
	base := strings.TrimRight(p, "/")
	if seg != "" {
		base += "/" + strings.Trim(seg, "/")
	}
	if id = strings.TrimSpace(id); id != "" {
		base += "/" + id
	}
	return base
}

// duomiPollTarget 返回查询路径与对应的响应解析器。
//
// SubKind 为空（老任务/单路径适配器）时按 seedance 处理：本适配器 P1 阶段只接入视频，
// 未知时选 seedance 是唯一不会把可灵任务当 seedance 解析的可行兜底——
// 由任务表落库保证不会出现空 SubKind 的可灵任务。
func duomiPollTarget(opts *duomiOptions, subKind, id string) (string, func([]byte) (*relay.TaskStatus, error)) {
	sk := strings.ToLower(strings.TrimSpace(subKind))
	switch {
	case strings.HasPrefix(sk, "kling."):
		seg := strings.TrimPrefix(sk, "kling.")
		if seg == "" {
			seg = duomiKlingText2Video
		}
		return duomiKlingPath(opts.paths["kling.prefix"], seg, id), parseDuomiKlingTask
	case sk == duomiProtoNano:
		// nano 系把任务号拼在查询路径后面（与可灵同形态）；两个提交端点共用它。
		return strings.TrimRight(duomiImagePollPath(opts, duomiProtoNano), "/") + "/" + id, parseDuomiImageTask
	case sk == duomiProtoGPTImage:
		return strings.TrimRight(duomiImagePollPath(opts, duomiProtoGPTImage), "/") + "/" + id, parseDuomiImageTask
	case sk == duomiProtoSunoMusic:
		return duomiSunoFeedPathFor(opts, id), parseDuomiMusicTask
	case sk == duomiProtoSunoLyrics:
		return duomiSunoFeedPathFor(opts, id), parseDuomiLyricsTask
	case sk == duomiProtoMuse:
		// **绝不能落 default**：default 读的是 "seedance.submit"
		// （/api/v3/contents/generations/tasks），拼出来是 /api/v3/contents/generations/tasks/{id}
		// → 上游 404 → duomiHTTPError 归成 HTTPStatusError → 每轮轮询都失败 →
		// 连续 20 次判超时退费。用户已经生成好的视频被丢掉，日志里只有一句"接口路径不存在"。
		return duomiMusePollPathOf(opts) + "/" + id, parseDuomiVideoTask
	default:
		submit := opts.paths["seedance.submit"]
		if submit == "" {
			submit = duomiSeedanceTaskPath
		}
		return strings.TrimRight(submit, "/") + "/" + id, parseDuomiSeedanceTask
	}
}

// duomiMusePollPathOf 返回 muse 的查询**前缀**（不含任务号）。
//
// 与 duomiKlingPath 同款归一：渠道把 muse.poll 写成不带尾斜杠时，少了 TrimRight
// 会拼出 /v1/videos/tasks{id}（缺分隔符）—— 提交成功、查询永远 404，
// 表现是"任务一直 running 到超时退费"。
func duomiMusePollPathOf(opts *duomiOptions) string {
	p := strings.TrimSpace(opts.paths["muse.poll"])
	if p == "" {
		p = duomiMusePollPrefix
	}
	return strings.TrimRight(p, "/")
}

// ---------------------------------------------------------------- 探活

// Test 探活：查一个必然不存在的任务号，用「路径是否命中 + 鉴权是否通过」判断渠道可用。
//
// 404 只有在响应体是多米信封时才判健康：否则运营少写一段路径前缀（比如漏了 /api）
// 也会被当成健康渠道，熔断中的渠道被误恢复。
func (DuoMi) Test(ctx context.Context, cfg *relay.ChannelConfig) error {
	if cfg == nil {
		return duomiCfgErr("channel", "渠道配置为空")
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return duomiCfgErr("keys", "渠道未配置密钥")
	}
	opts, err := parseDuomiOptions(cfg.Options)
	if err != nil {
		return err
	}
	prefix := opts.paths["kling.prefix"]
	path := duomiKlingPath(prefix, duomiKlingText2Video, "__probe__")

	resp, buf, doErr := duomiDoRaw(ctx, cfg, opts.authPrefix, http.MethodGet, path, nil)
	// 探活必须自己看状态码：duomiDoRaw 对非 2xx 一律返回错误，但「404 + 信封」
	// 恰恰说明路径与鉴权都是对的。只有拿不到响应（网络/构造失败）才算探活失败。
	if resp == nil {
		return doErr
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("适配器 duomiapi：鉴权失败（HTTP %d），请检查渠道密钥与 auth_prefix：%s", resp.StatusCode, snippet(buf))
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		if duomiEnvelopeOK(buf) {
			return nil // 上游用信封表达了"任务不存在/参数不对" = 路径与鉴权都对
		}
		return fmt.Errorf("适配器 duomiapi：查询接口路径不存在（HTTP %d），请检查 Base URL 与 paths.kling.prefix：%s", resp.StatusCode, snippet(buf))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// 400/422/429 只有在响应体是多米信封时才说明「路径与鉴权都通了」（P2-06）。
	//
	// 少了这一层判定，运营把 Base URL 写成一个「所有路径都回 400」的反向代理/错误域名，
	// 探活会判健康 → 熔断中的渠道被自动恢复 → 所有请求先撞它一次再 failover。
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity ||
		resp.StatusCode == http.StatusTooManyRequests {
		if duomiEnvelopeOK(buf) {
			return nil
		}
		return fmt.Errorf("适配器 duomiapi：探活返回 HTTP %d，但响应不是多米格式（可能 Base URL 写错或路径前缀不符）：%s",
			resp.StatusCode, snippet(buf))
	}
	return fmt.Errorf("适配器 duomiapi：探活失败（HTTP %d）：%s", resp.StatusCode, snippet(buf))
}

// ---------------------------------------------------------------- 同步端点（不支持）

// Chat 不承接同步对话。
func (DuoMi) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errDuoMiNoEndpoint
}

// Embeddings 不承接向量请求。
func (DuoMi) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errDuoMiNoEndpoint
}

// Completions 不承接旧版补全。
func (DuoMi) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errDuoMiNoEndpoint
}

// StreamChat 不承接流式对话。
func (DuoMi) StreamChat(context.Context, *relay.ChannelConfig, *relay.RelayRequest, func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return nil, errDuoMiNoEndpoint
}

// Forward 承接同步图像端点的桥接（generations / edits）。
//
// 其余路径必须继续报「不支持端点」：这两个以外的同步端点本适配器没有任何实现，
// 空实现成功会让调用方拿到一份不是图片的响应。
func (DuoMi) Forward(ctx context.Context, cfg *relay.ChannelConfig, req *relay.RelayRequest, path string) (*relay.RawResponse, error) {
	switch strings.TrimRight(strings.TrimSpace(path), "/") {
	case "/v1/images/generations":
		return duomiForwardImage(ctx, cfg, req, false)
	case "/v1/images/edits":
		return duomiForwardImage(ctx, cfg, req, true)
	}
	return nil, errDuoMiNoEndpoint
}

// ---------------------------------------------------------------- 文档锚点（避免误删）

// 多米视频协议速查（改动前先看这里，比翻 OpenAPI 快）：
//
//	seedance  提交 POST /api/v3/contents/generations/tasks
//	          体  {model, content:[{type:text,text},{type:image_url,image_url:{url},role}], duration, ratio, resolution, generate_audio, watermark}
//	          查 GET  /api/v3/contents/generations/tasks/{id} → {state|status, data.videos[].url | content.video_url}
//	kling     提交 POST /api/video/kling/v1/videos/{text2video|image2video|multi-image2video|omni-video}
//	          体  {model_name, prompt, image, image_tail, image_list, mode, duration, aspect_ratio, sound, multi_shot}
//	              **omni-video 只认 image_list，且元素键是 image_url（不是 image）、type 只能 first_frame/end_frame**
//	          回  {code:0, message:"SUCCEED", data:{task_id, task_status:"submitted"}}
//	          查 GET  .../{seg}/{id} → {code:0, data:{task_status, task_result:{videos:[{url,duration}]}}}
//	muse      提交 POST /v1/videos/generations
//	          体  {model, prompt, aspect_ratio, duration?, image_urls:["url"]}（image_urls 1–8 张）
//	          查 GET  /v1/videos/tasks/{id} → {id, state, data.videos[].url, progress, message, action}
//	          **查询不是提交路径 + /{id}**；上游没有 resolution，计费档是模型属性（720p）
//
// 上游计费相关的隐含约定：
//   - 可灵没有 resolution 参数（画质由 model_name + mode 决定）→ 计费档必须是模型属性；
//   - kling 的 duration 只认 5/10（omni 3–15），不能静默 clamp（等于"要 10 秒给 5 秒还按 10 秒收费"）；
//   - seedance 的 generate_audio/watermark 会改上游成本 → 只从 metadata 允许清单读。
