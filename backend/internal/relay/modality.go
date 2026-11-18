package relay

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/pricing"
)

// endpointModality 返回端点所属的模态（对外目录与 KEY 白名单用的枚举值）。
//
// chat / completions / responses 同属 chat：它们共用同一个模型族（token 计费）。
// 模态值来自 apidoc 的端点表，不再在 relay 里重复一张 switch。
func endpointModality(kind endpointKind) string {
	t, ok := apidoc.TransportOf(docKindOf[kind])
	if !ok {
		return ""
	}
	return t.Modality
}

// endpointPathOf 返回端点的路径标识（归一为无前导斜杠、无 v1 前缀的形式）。
//
// 归一后与 `models.endpoints` 的存量写法兼容：库里既有 `video/generations`，
// 也可能有人写成 `/v1/video/generations`，两种都认。
func endpointPathOf(kind endpointKind) string {
	t, ok := apidoc.TransportOf(docKindOf[kind])
	if !ok {
		return ""
	}
	return t.EndpointKey
}

// endpointDisplayOf 返回端点给**人看**的标识（错误文案、日志用）。
//
// 与 endpointPathOf 分开的理由：后者是 `models.endpoints` 的写法，协议变体端点
// （messages）刻意没有它 —— 它不表达新能力。但文案里不能出现空串，
// 否则会拼出「模型 x 的模态是 chat，不能调用 ；」这种断句。
func endpointDisplayOf(kind endpointKind) string {
	if p := endpointPathOf(kind); p != "" {
		return p
	}
	if t, ok := apidoc.TransportOf(docKindOf[kind]); ok {
		return normalizeEndpointPath(t.Path)
	}
	return ""
}

// normalizeEndpointPath 归一端点标识（去空白、去前导 `/`、去 `v1/` 前缀）。
//
// 委托 apidoc：文档判定、网关目录、模态闸门必须用同一套归一规则，
// 三处各写一份的结果是「模型广场显示支持、实际路由 404」。
func normalizeEndpointPath(p string) string {
	return apidoc.NormalizeEndpointKey(p)
}

// modalityAllows 判断模型是否允许被该模态的端点调用。
//
// 规则（顺序即优先级）：
//  1. `models.endpoints` 显式声明了当前端点 → 放行（这是唯一的逃生口：
//     跨模态复用的模型——例如同一个模型既能 chat 又能 embedding——必须靠它）；
//  2. 模型 modality 为空 → 视为 chat（存量数据的实际语义）；
//  3. 否则必须与端点模态完全相等。
//
// 为什么必须校验：此前只校验「模型存在 + 启用 + KEY 白名单」。一个 quota_type=0 的
// chat 模型被调用 /v1/images/generations 时，token 估算返回 (0,0)，
// Estimate 直接返回 0 → 真实出图、账单 0 元。
func modalityAllows(item *pricing.ModelItem, kind endpointKind) bool {
	if item == nil {
		return false
	}
	want := endpointModality(kind)
	if want == "" {
		return true
	}
	if declaredEndpoint(item.Endpoints, endpointPathOf(kind)) {
		return true
	}
	m := strings.TrimSpace(item.Modality)
	if m == "" {
		m = "chat"
	}
	return m == want
}

// declaredEndpoint 判断 endpoints JSON 数组里是否声明了给定端点。
func declaredEndpoint(raw model.JSON, path string) bool {
	if len(raw) == 0 || path == "" {
		return false
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return false
	}
	for _, e := range list {
		if normalizeEndpointPath(e) == path {
			return true
		}
	}
	return false
}

// tokenBillable 判断端点是否按 token 计费（即 billing_mode=tokens 在该端点上有意义）。
//
// 图像/音频/视频端点的响应里没有 token 用量，配成按量就会恒为 0 元。
func tokenBillable(kind endpointKind) bool {
	// 同上：按上游语义判（协议变体不改变"能不能按 token 计费"）。
	kind = upstreamKind(kind)
	switch kind {
	// kindMessages 与 kindChat 同属对话：按 token 计费，参与同一套二道闸。
	case kindChat, kindMessages, kindCompletions, kindResponses, kindEmbeddings, kindRerank, kindModerations:
		return true
	default:
		return false
	}
}

// 图片尺寸的解析与档位归一已整体搬到 billing（唯一实现，见 billing/imagespec.go）。
//
// 搬走的原因有两条，都是「多真相源」造成的实际错账：
//   1. 旧实现按长边分档，与上游「档位 = 总像素」的定义冲突（1K 的 21:9 判成 2K）；
//   2. 归一必须知道**这个模型卖哪几档**才对得上（相邻档之间的像素面积差着一个量级，
//      任何单侧全局边界都会让一端出错），而模型价目在 billing 侧。
// relay 现在只做「把 size 与 aspect_ratio 交给 billing.ParseImageSize + ResolveImageTier」，
// 不再保留任何阈值或优先级规则。

// ────────────────────────────────────────────────────────────────────────────
// 绑定校验用的能力探针（P1-35 / B2-6）
//
// 为什么放在 relay 而不是 controller：端点 → 能力键的唯一真相是
// `apidoc` 的端点表 + 各适配器声明的 Caps，而 relay 是唯一能同时看到「适配器实例」与
// 「端点枚举」的包。管理台若自己维护一份「模态 → 适配器能力」表，新增能力时
// 必然漏改，表现为「保存时放过、上线后 100% 失败」——最难排查的一类配置错误。
// ────────────────────────────────────────────────────────────────────────────

// SyncEndpointKinds 是全部**同步**端点（含 chat/completions 这类同模态多路径）。
//
// 异步任务端点（video/music）不在其中：它们没有 EndpointKind，能力判定走
// TaskEndpointSupported（按端点，见 task_endpoints.go）。
var SyncEndpointKinds = []EndpointKind{
	kindChat, kindCompletions, kindEmbeddings,
	kindImagesGenerations, kindImagesEdits, kindImagesVariations,
	kindAudioSpeech, kindAudioTranscriptions, kindAudioTranslations,
	kindModerations, kindRerank, kindResponses,
}

// EndpointPathOf 是 endpointPathOf 的导出版本（供绑定校验拼错误信息，避免两处各拼一份）。
func EndpointPathOf(kind EndpointKind) string { return endpointPathOf(kind) }

// EndpointKindByPath 把端点路径标识反查为 EndpointKind（无法识别返回 false）。
func EndpointKindByPath(p string) (EndpointKind, bool) {
	want := normalizeEndpointPath(p)
	if want == "" {
		return 0, false
	}
	for _, k := range SyncEndpointKinds {
		if endpointPathOf(k) == want {
			return k, true
		}
	}
	return 0, false
}

// SupportsEndpointOn 判断「指定适配器能否承接该同步端点」（按 adapter_id 查实例）。
//
// 未注册的适配器返回 false：绑到一个不存在的适配器上本就是死绑定，必须在保存期拦住。
// 内置 openai 适配器未实现 EndpointSupporter，按契约「未实现 = 全支持」返回 true。
func SupportsEndpointOn(adapterID string, kind EndpointKind) bool {
	a, ok := GetAdapter(adapterID)
	if !ok {
		return false
	}
	return adapterSupports(a, kind)
}

// TaskEndpointPaths 是异步任务端点路径的允许集合（models.endpoints 里的合法写法）。
//
// 由 apidoc 的端点表派生（Phase=submit 或 AcceptsAsync，且 CapabilityChecked），不再手写列表：
// 手写列表与真实路由/其它表之间没有任何约束，改一处漏一处就会让「声明了某个异步端点
// 的模型」被跳过能力校验，绑到不支持异步任务的渠道也能保存成功，直到第一次调用才失败。
//
//	/v1/video/generations          视频（seedance / kling / metaso / autodl）
//	/v1/images/generations         图像（nano / gpt-image：默认同步，async:true 才转任务）
//	/v1/audio/music/generations    音乐（SUNO）—— 注意归一化后带 audio/ 前缀，
//	                               早期这里写成 music/generations，与真实路由永远对不上
var TaskEndpointPaths = apidoc.TaskEndpointKeys()

// EndpointProbe 描述一个端点声明的能力判定方式（同步端点用 kind，异步用 task）。
type EndpointProbe struct {
	Path   string
	Kind   EndpointKind
	IsTask bool
	Known  bool
}

// ProbeEndpointPath 解析 models.endpoints 里的单个路径标识。
//
// Known=false 表示既不是同步端点也不是已知异步端点。这类写法**不做能力校验**：
// 存量库里可能存着历史别名或运营自定义备注，硬拒会让整页保存 400（改个名字都保存不了）。
// 无能力校验不等于无风险——它仍会在运行期被 relay 的模态闸门或 task 提交入口拦住。
func ProbeEndpointPath(p string) EndpointProbe {
	path := normalizeEndpointPath(p)
	pr := EndpointProbe{Path: path}
	if path == "" {
		return pr
	}
	if k, ok := EndpointKindByPath(path); ok {
		pr.Kind, pr.Known = k, true
		return pr
	}
	for _, tp := range TaskEndpointPaths {
		if tp == path {
			pr.IsTask, pr.Known = true, true
			return pr
		}
	}
	return pr
}

// ModalityProbes 返回「某模态的模型至少要被一个绑定渠道承接」时所用的端点探针集合。
//
// 语义是**任一满足**：image 模型的渠道只要能出图（generations）或能改图（edits）
// 就足以承接该模型，不要求三种图像端点全支持。
//
// 为什么不直接按「模态 → 能力键」映射：Caps 枚举里没有 "audio"/"music" 这类模态键
// （见 relay 的 Caps 常量），按模态硬映射必然写出查不到的键。
// 走端点探针等价于复用 capabilityOf 这份唯一真相，且能自然表达「任一」。
func ModalityProbes(modality string) []EndpointProbe {
	paths := apidoc.DefaultEndpoints(modality)
	if len(paths) == 0 {
		return nil
	}
	out := make([]EndpointProbe, 0, len(paths))
	for _, p := range paths {
		out = append(out, ProbeEndpointPath(p))
	}
	return out
}

// AdapterSatisfiesProbe 判断某适配器能否承接单个端点探针。
//
// task 端点必须**按端点**判定（不是「能不能做异步」的布尔）：一个适配器可能只承接
// 视频、却完全不认识音乐。按布尔判会让音乐模型绑到只做视频的渠道，保存体检通过、
// 首次调用把音乐 body 打到视频端点 —— 报错是 502，与真实原因无关。
func AdapterSatisfiesProbe(adapterID string, pr EndpointProbe) bool {
	if !pr.Known {
		return true // 未知端点不参与校验
	}
	if pr.IsTask {
		return TaskEndpointSupported(adapterID, pr.Path)
	}
	return SupportsEndpointOn(adapterID, pr.Kind)
}

// modalityCaps 是「模态 → 适配器 Caps 里能代表它的能力键」。
//
// 为什么必须先看 Caps、再退回端点探针：
// Caps 是适配器**显式声明**的能力集合，而端点探针是从「承不承接某个同步端点」
// 推断出来的。两者冲突时以声明为准 —— 声明是运营/开发者写下来的意图，探针只是推断。
//
// 真实踩到的场景：多米API 的图像 / 音乐是**异步任务**（images/generations 的 async 面、
// audio/music/generations），它的 SupportsEndpoint 全 false（不承接任何同步端点，
// 否则同步网关会把 chat 请求派给它）。于是探针把 image 模态判成「不支持」——
// 表现是「模型能建出来，但保存绑定时 400 说适配器不支持图像模态」，
// 而同一个模型通过迁移直接写库却能正常出图。声明（Caps 含 image）才是对的。
//
// 只登记能一一对应的模态；rerank / moderation 在 Caps 枚举里没有对应键，
// 仍走端点探针（embeddings / moderations），不要在这里硬塞。
var modalityCaps = map[string][]string{
	"chat":      {"text"},
	"image":     {"image", "edit_image"},
	"audio":     {"tts", "asr"},
	"video":     {"video"},
	"music":     {"music"},
	"embedding": {"embedding"},
}

// AdapterCaps 返回适配器声明的能力集合（未注册的适配器返回 nil）。
func AdapterCaps(adapterID string) []string {
	a, ok := GetAdapter(adapterID)
	if !ok {
		return nil
	}
	return a.Caps()
}

// AdapterSatisfiesModalityCaps 用**显式能力集合 + 端点探针**判定模态。
//
// 用途：规格适配器在同一事务里刚建、尚未提交时，注册表里查不到它，
// AdapterCaps 返回 nil → 正常的规格渠道会被误判成"不支持该模态"。
// 把判定规则留在这里（modalityCaps 与 ModalityProbes 的唯一来源），
// 避免调用方自己再抄一份"模态→能力"的映射。
func AdapterSatisfiesModalityCaps(caps []string, modality string, supportsEndpoint func(path string) bool) bool {
	modality = strings.TrimSpace(modality)
	if want, ok := modalityCaps[modality]; ok {
		for _, c := range caps {
			for _, w := range want {
				if c == w {
					return true
				}
			}
		}
	}
	probes := ModalityProbes(modality)
	if len(probes) == 0 {
		return true
	}
	if supportsEndpoint == nil {
		return false
	}
	for _, p := range probes {
		if supportsEndpoint(p.Path) {
			return true
		}
	}
	return false
}

// AdapterSatisfiesModality 判断渠道适配器是否具备服务该模态模型的**基础**能力。
//
// 注意这是粗粒度闸门：它只回答「这个渠道有没有该类能力」，不回答「这个具体模型能不能用」。
// 后者要靠真实联调，平台无法在保存期证明。
//
// 判定顺序：适配器声明的 Caps 命中 → 通过；否则退回端点探针（任一命中即通过）。
func AdapterSatisfiesModality(adapterID, modality string) bool {
	modality = strings.TrimSpace(modality)
	if want, ok := modalityCaps[modality]; ok {
		have := AdapterCaps(adapterID)
		for _, c := range have {
			for _, w := range want {
				if c == w {
					return true
				}
			}
		}
	}
	probes := ModalityProbes(modality)
	if len(probes) == 0 {
		return true // 未知模态不拦（存量脏数据不该锁死保存）
	}
	for _, pr := range probes {
		if AdapterSatisfiesProbe(adapterID, pr) {
			return true
		}
	}
	return false
}
