// Package apidoc 是「对外 API 文档」的唯一真相源。
//
// 三层分工（详见 docs 方案 §2.1）：
//   - 本包（代码）：端点契约 —— 路径、方法、传输形态、字段、枚举、计费角色；
//   - models 表：模型事实 —— 支持哪些端点、有哪些档位、秒数/张数上限；
//   - model_docs 表：模型文案 —— 人话说明、示例、注意事项（**不存字段结构**）。
//
// 对外投影（DocProjection）由这三层在内存里合成，**永不落库**：
// 一旦把合成结果存下来，改价/改端点后文档就会与事实脱节，而且没人会发现。
//
// 本包不 import relay：relay 依赖本包（用它算端点路径与模态），反向依赖会成环。
package apidoc

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/pkg/modality"
	"lingdong-api/internal/pkg/wireformat"
)

// Kind 是端点的稳定标识。
//
// 为什么不用「路径」当标识：`POST /v1/video/generations` 与 `GET /v1/video/generations`
// 路径相同、语义完全不同（提交 vs 列表）。用路径当 key 会把两件事挤进一个条目，
// 文档里就会出现「创建视频任务」下面挂着列表参数。
//
// 因此 Kind 是**不透明 id**，而进 `models.endpoints` 的写法由 Transport.EndpointKey 表达。
// Kind 是端点的**文档稳定 id**（进 URL、进 JSON、进前端路由），不是 models.endpoints 的写法。
//
// 两者刻意分开：文档 id 要稳定、语义化、可出现在 `/api/docs/endpoints?k=` 里；
// 而 models.endpoints 的写法是运营在库里手填的既成事实（如 `video/generations`），
// 改它会牵动保存期校验与历史数据。对应关系见 endpointTable 的 EndpointKey。
//
// 例：KindVideoSubmit="video/submit" ↔ EndpointKey="video/generations"。
// 混用这两个字符串的后果是「文档页 404」或「判定永远不命中」。
type Kind string

const (
	// ── 同步转发：走适配器专用方法或通用 Forward ──
	KindChatCompletions Kind = "chat/completions"
	KindCompletions     Kind = "completions"
	KindResponses       Kind = "responses"
	// KindResponsesCompact 是 Codex 的上下文压缩端点（POST /v1/responses/compact）。
	//
	// 它是 Responses 家族的**协议变体**，不是新能力：请求/响应形状与 usage 语义
	// 都与 /v1/responses 一致，只有上游路径不同。因此 EndpointKey 留空，
	// 与 KindMessages 同一条规矩（见 kind_path_test.go 的协议变体清单）。
	KindResponsesCompact Kind = "responses/compact"
	// ── Gemini（Google Generative Language）──
	//
	// 三个端点都是**协议变体**：生成复用 chat 的能力声明、向量化复用 embeddings，
	// 因此 EndpointKey 全部留空（与 KindMessages 同一条规矩）。
	KindGeminiModels       Kind = "gemini/models"
	KindGeminiGenerate     Kind = "gemini/generate-content"
	KindGeminiEmbedContent Kind = "gemini/embed-content"
	// KindMessages 是 Anthropic Messages 协议的入站端点（POST /v1/messages）。
	//
	// 它是 chat 能力的**另一种线格式**，不是新能力：EndpointKey 刻意留空，
	// 因此它不进 models.endpoints、不参与渠道能力校验、不改变「哪些模型可用」
	// —— 所有 chat 模型自动可用。能力与协议分开表达，才不会出现
	// 「运营要在两个地方勾选同一件事」的第二真相源。
	KindMessages    Kind = "messages"
	KindEmbeddings  Kind = "embeddings"
	KindModerations Kind = "moderations"
	KindRerank      Kind = "rerank"

	// KindImagesGenerations 同时承载同步出图与异步任务两种模式（由请求里的
	// `async:true` 选择，见 Transport.AcceptsAsync）。
	//
	// 不另开 `images/generations-async` 端点：同步与异步在上游是**同一套流程**
	// 只差「等不等」（duomi 的同步出图本身就是提交任务 + 请求内轮询）。拆成两条
	// 路径的直接代价是两篇同字段文档、两个预设 TAB、侧栏两行，以及「用户不知道
	// 该用哪个地址」。
	KindImagesGenerations Kind = "images/generations"
	KindImagesEdits       Kind = "images/edits"
	KindImagesVariations  Kind = "images/variations"

	KindAudioSpeech         Kind = "audio/speech"
	KindAudioTranscriptions Kind = "audio/transcriptions"
	KindAudioTranslations   Kind = "audio/translations"
	KindAudioMusic          Kind = "audio/music"
	KindAudioLyrics         Kind = "audio/lyrics"

	// ── SUNO 兼容面（new-api 的 `/suno/*` 形状，顶层路径、无 /v1 前缀）──
	//
	// 三个 Kind 都是**协议变体**：EndpointKey 留空，能力仍是
	// `audio/music/generations` 与 `audio/lyrics`（提交时按 action 映射）。
	// 为什么不在 `models.endpoints` 里新增 `suno/submit/...`：那会让管理台的
	// 端点勾选凭空多出两个与原生入口完全等价的选项，运营勾错一个就少一半流量。
	KindSunoSubmit     Kind = "suno/submit"
	KindSunoFetch      Kind = "suno/fetch"
	KindSunoFetchBatch Kind = "suno/fetch-batch"

	// ── 异步任务：提交 / 查询 / 取产物 / 取消 ──
	KindVideoSubmit     Kind = "video/submit"
	KindVideoRegenerate Kind = "video/regenerations"

	KindTasksList            Kind = "tasks/list"
	KindTasksGet             Kind = "tasks/get"
	KindTasksArtifacts       Kind = "tasks/artifacts"
	KindTasksArtifactContent Kind = "tasks/artifact-content"
	KindTasksCancel          Kind = "tasks/cancel"
	// KindVideoTaskGet 是 new-api 的视频查询壳（与 POST /v1/video/generations 配对）。
	//
	// 它与 KindTasksGet 返回**同一个任务对象**，差别只在信封与 result_url：
	// 协议形状不同不等于数据模型不同，字段表仍然只有一份。
	KindVideoTaskGet Kind = "video/task-get"
	// KindVideoGet / KindVideoContent 是 OpenAI Video 的读面
	// （`/v1/videos/{id}` 与 `/v1/videos/{id}/content`）。
	//
	// 与 KindVideoTaskGet 的区别是**形状**而不是数据：后者是兼容封套 + 扁平任务，
	// 这两个是视频对象（四态 + 整数进度）与字节流。三条读面都投影自同一行。
	KindVideoGet     Kind = "video/get"
	KindVideoContent Kind = "video/content"

	// ── 元信息与账户 ──
	KindModelsList     Kind = "models/list"
	KindModelsGet      Kind = "models/get"
	KindModelsDefaults Kind = "models/defaults"
	KindAccount        Kind = "account"
	KindUsageList      Kind = "usage/list"
	KindUsageSummary   Kind = "usage/summary"

	// ── 文件 ──
	KindFilesUpload  Kind = "files/upload"
	KindFilesList    Kind = "files/list"
	KindFilesGet     Kind = "files/get"
	KindFilesContent Kind = "files/content"
	KindFilesDelete  Kind = "files/delete"
	KindFileDirect   Kind = "files/direct"

	// ── 计费兼容（NextChat / LobeChat 需要的两个只读接口） ──
	KindBillingSubscription Kind = "billing/subscription"
	KindBillingUsage        Kind = "billing/usage"

	// ── 平台自有 ──
	KindMediaProxy Kind = "media/proxy"
)

// Phase 描述端点在生命周期里的位置（决定文档写「同步返回」还是「提交后轮询」）。
type Phase int

const (
	// PhaseSync 同步：一次请求拿到结果（可能是二进制流）。
	PhaseSync Phase = iota
	// PhaseSubmit 异步提交：返回 200 + 任务对象（202 是早期口径，已统一成 200）。
	PhaseSubmit
	// PhaseRead 异步读面：查询状态 / 取产物 / 取消。
	PhaseRead
)

// String 用于 Markdown 与前端展示。
func (p Phase) String() string {
	switch p {
	case PhaseSubmit:
		return "异步提交"
	case PhaseRead:
		return "任务查询"
	default:
		return "同步"
	}
}

// Transport 是端点的传输契约。
//
// 这是 relay 与文档的**唯一共同来源**：relay.specOf 从这里取 path/multipart/binary/modality，
// 文档从这里取 method/path/传输形态。以前这些值写死在 relay.specOf 的 switch 里，
// 文档只能另抄一份，抄错就是「文档说 multipart、实际拒收文件」。
type Transport struct {
	// Path 是上游相对路径（含 /v1 前缀）；空串 = 走适配器专用方法，没有可转发的路径。
	Path string
	// Method 是对外 HTTP 方法。
	Method string
	// EndpointKey 是 `models.endpoints` 里的归一化写法（无前导斜杠、无 v1 前缀）；
	// 空串 = 这个端点不允许出现在 models.endpoints（查询/取消/元信息类）。
	EndpointKey string
	// Multipart 表示请求体是 multipart/form-data（文件上传类）。
	Multipart bool
	// Binary 表示响应体是二进制（如 TTS 音频），不是 JSON。
	Binary bool
	// Dedicated 表示该端点走适配器的专用方法（Chat / Completions / Embeddings），
	// **不**经由通用 Forward 转发 —— 即使它有 Path，也不是可转发的上游路径。
	//
	// 这个字段是必需的：relay 用「path 是否为空」来决定走专用方法还是 Forward，
	// 若把 chat 的真实路径填进 Path 而不标记 Dedicated，对话请求会被当成图片那样的
	// 通用转发，适配器收到一个它不认识的路径 → 全量 500。
	Dedicated bool
	// Modality 是该端点的默认模态（模型必须属于此模态才能调用）。
	Modality string
	// Phase 是生命周期位置。
	Phase Phase
	// Group 是文档分组 id（见 group.go）。
	Group string
	// Aliases 是同方法同语义的额外路径（如 /v1/videos 是 /v1/video/generations 的 Sora 别名）。
	Aliases []string
	// CapabilityChecked 表示「该端点出现在 models.endpoints 时参与渠道能力校验」。
	//
	// 只对异步端点有意义：同步端点一律校验（ProbeEndpointPath 先按 sync 反查）。
	// **全部异步提交端点都必须为 true**：漏标的表现不是"不校验"，而是更糟——
	// ProbeEndpointPath 返回 Known=false，adapterCovers 对内置适配器直接 false，
	// 于是「这条端点没有任何绑定能承接」恒成立，运营**根本声明不了**它
	// （video/regenerations 曾经就是这种状态，metaso 明明支持却保存必失败）。
	CapabilityChecked bool
	// AcceptsAsync 表示该端点**同一路径**还受理 `async:true` 的任务提交
	// （返回 200 任务对象，之后走 /v1/tasks/*）。
	//
	// 与 Phase 的分工：Phase 描述「这个端点只有一种返回形态」，AcceptsAsync
	// 描述「同一个端点有两种返回形态」。图像是唯一一例——同步与异步在上游是
	// 同一套流程，只差平台等不等。视频/音乐本身就是任务端点（Phase=PhaseSubmit），
	// 不需要也不需要设这个标志。
	//
	// 判断「某模型能不能异步」的唯一入口是 HasTaskFace，不要在别处
	// 各写一遍 `HasPhase(PhaseSubmit)` —— 那会让图像模型被判成「不能异步」。
	AcceptsAsync bool
	// Protocol 是该端点的**线格式**（请求/响应/流式事件/错误体的形状）。
	//
	// 与 EndpointKey 的分工：EndpointKey 表达"能不能用"（能力，进 models.endpoints），
	// Protocol 表达"长什么样"（协议）。同一个能力可以有多种线格式
	// —— /v1/chat/completions 与 /v1/messages 都是 chat 能力，前者 openai、后者 claude。
	//
	// 空值等价 openai：绝大多数端点从来不需要显式声明。
	// 判定只能读这里，不能按 channels.adapter_id 猜 —— 渠道说的是上游协议，
	// 而这里说的是**入站**协议，两者本就可以不同（Claude 入站 → OpenAI 上游）。
	Protocol wireformat.Format
	// Stream 表示该端点支持流式（SSE）响应。
	//
	// 显式声明而不是让文档测试按路径白名单判断：文档侧要校验
	// 「标了流式运行模式的端点必须真的支持流式」，判据只能来自契约。
	// 按路径写白名单的话，新增流式端点时要么漏加（文档报错）要么乱加（文档骗人）。
	Stream bool
}

// UpstreamVideoPollPath 是**上游**轮询路径（`{id}` 由适配器替换）。
//
// 它不是对外端点：对外的查询/取产物/取消已经统一到 /v1/tasks/*，这条路径只用于
// 声明式适配器拼上游 URL（path_overrides 的合法键之一）。放在这里而不是 relay，
// 是因为「路径只有一处定义」这条规则对上游路径同样成立。
const UpstreamVideoPollPath = "/v1/video/generations/{id}"

type endpointEntry struct {
	kind Kind
	t    Transport
}

// endpointTable 是**全部端点**的声明，顺序即文档目录顺序。
//
// 改动本表的三条铁律：
//  1. 新增一行必须同时在 relay 侧确认有对应路由（TestEveryRelayKindMapsToContract 双向守）；
//  2. Path 只能出现在这里，relay 不得再写路径字面量（TestNoPathLiteralInRelay）；
//  3. EndpointKey 必须与 `models.endpoints` 的既有写法一致（拿不准就问数据库，别猜）。
var endpointTable = []endpointEntry{
	// ── 语言模型 ──
	{KindChatCompletions, Transport{
		Path: "/v1/chat/completions", Method: "POST", EndpointKey: "chat/completions",
		Dedicated: true, Modality: "chat", Phase: PhaseSync, Group: "lang",
		Stream: true,
	}},
	{KindCompletions, Transport{
		Path: "/v1/completions", Method: "POST", EndpointKey: "completions",
		Dedicated: true, Modality: "chat", Phase: PhaseSync, Group: "lang",
	}},
	{KindResponses, Transport{
		Path: "/v1/responses", Method: "POST", EndpointKey: "responses",
		Modality: "chat", Phase: PhaseSync, Group: "lang",
		// 真流式：适配器按上游路径逐帧转发 SSE（PathStreamer），
		// 终止语义是 response.completed 而不是 [DONE]。
		Stream: true,
	}},
	{KindGeminiModels, Transport{
		// 与 /v1/models 同一份数据、同一套 KEY 白名单（controller.allowedCatalog），
		// 只是形状换成 Gemini 的 {models:[{name:"models/<id>"}]}。
		Path: "/v1beta/models", Method: "GET", Phase: PhaseSync, Group: "model",
		Protocol: wireformat.Gemini,
	}},
	{KindGeminiGenerate, Transport{
		// gin 的 param 唯一终止符是 `/`，所以 `:generateContent` 是参数值的一部分：
		// 路由注册成 POST /v1beta/models/:model，handler 再按 `:` 切出动作。
		Path: "/v1beta/models/{model}:generateContent", Method: "POST",
		EndpointKey: "", Dedicated: true, Modality: "chat",
		Phase: PhaseSync, Group: "lang", Protocol: wireformat.Gemini, Stream: true,
		// `/v1` 下的通配形态：把 base_url 填成 `https://host/v1` 的 Gemini 客户端
		// 会在后面拼 `models/<name>:动作`，整条路径就落在 /v1 下。
		// 同路径在 /v1 侧还有「模型详情」(GET)，两者靠**方法**区分 ——
		// 这也是 kindByPath 必须"主路径优先"的原因（见那里的注释）。
		Aliases: []string{"/v1/models/{model}"},
	}},
	{KindGeminiEmbedContent, Transport{
		Path: "/v1beta/models/{model}:embedContent", Method: "POST",
		EndpointKey: "", Dedicated: true, Modality: "embedding",
		Phase: PhaseSync, Group: "lang", Protocol: wireformat.Gemini,
		// 遗留路径：new-api 把 /v1/engines/{model}/embeddings 也路由到 Gemini 处理器。
		// 同一个 codec、同一个 Kind，只多一条可命中的路径。
		Aliases: []string{"/v1/engines/{model}/embeddings"},
	}},
	{KindResponsesCompact, Transport{
		// Dedicated 不设：path 非空 → 通用 Forward 直发上游同名路径。
		// EndpointKey 留空 = 不表达新能力（复用 responses 的渠道能力声明）。
		Path: "/v1/responses/compact", Method: "POST", EndpointKey: "",
		Modality: "chat", Phase: PhaseSync, Group: "lang",
	}},
	{KindMessages, Transport{
		// Dedicated：走适配器专用方法（Chat），不经通用 Forward。
		// 请求/响应由 relay 的入站编解码器转换，上游线格式仍是 OpenAI。
		// EndpointKey 留空 = 不表达新能力（见 KindMessages 注释）。
		Path: "/v1/messages", Method: "POST", EndpointKey: "",
		Dedicated: true, Modality: "chat", Phase: PhaseSync, Group: "lang",
		Protocol: wireformat.Claude, Stream: true,
	}},

	// ── 向量与检索 ──
	{KindEmbeddings, Transport{
		Path: "/v1/embeddings", Method: "POST", EndpointKey: "embeddings",
		Dedicated: true, Modality: "embedding", Phase: PhaseSync, Group: "vec",
	}},
	{KindRerank, Transport{
		Path: "/v1/rerank", Method: "POST", EndpointKey: "rerank",
		Modality: "rerank", Phase: PhaseSync, Group: "vec",
	}},

	// ── 图片 ──
	{KindImagesGenerations, Transport{
		Path: "/v1/images/generations", Method: "POST", EndpointKey: "images/generations",
		Modality: "image", Phase: PhaseSync, Group: "image",
		// 图像是唯一「同一路径可同步可异步」的端点：`async:true` 时返回 200
		// 任务对象，否则在请求内等到出图。CapabilityChecked 必须为 true ——
		// 它是 `TaskEndpointKeys()` 的词表来源（见 task_endpoints.go）。
		AcceptsAsync: true, CapabilityChecked: true,
	}},
	{KindImagesEdits, Transport{
		Path: "/v1/images/edits", Method: "POST", EndpointKey: "images/edits",
		Multipart: true, Modality: "image", Phase: PhaseSync, Group: "image",
		// /v1/edits 是它的旧别名（同一能力、同一上游路径，历史上只发 JSON 体）。
		// 不另开 Kind：同路径再开一个端点会让 models.endpoints、探针与文档判定
		// 出现两套写法，而它们描述的是同一件事。
		Aliases: []string{"/v1/edits"},
	}},
	{KindImagesVariations, Transport{
		Path: "/v1/images/variations", Method: "POST", EndpointKey: "images/variations",
		Multipart: true, Modality: "image", Phase: PhaseSync, Group: "image",
	}},
	// ── 视频 ──
	{KindVideoSubmit, Transport{
		Path: "/v1/video/generations", Method: "POST", EndpointKey: "video/generations",
		Modality: "video", Phase: PhaseSubmit, Group: "video",
		Aliases: []string{"/v1/videos"}, CapabilityChecked: true,
		// 响应与错误体都是 new-api 的封套：这条路径本身就是它的协议面。
		Protocol: wireformat.NewAPI,
	}},
	{KindVideoTaskGet, Transport{
		Path: "/v1/video/generations/{id}", Method: "GET", Phase: PhaseRead, Group: "task",
		Protocol: wireformat.NewAPI,
	}},
	{KindVideoGet, Transport{
		Path: "/v1/videos/{id}", Method: "GET", Phase: PhaseRead, Group: "task",
	}},
	{KindVideoContent, Transport{
		Path: "/v1/videos/{id}/content", Method: "GET", Phase: PhaseRead, Group: "task",
		Binary: true,
	}},
	{KindVideoRegenerate, Transport{
		Path: "/v1/video/regenerations", Method: "POST", EndpointKey: "video/regenerations",
		Modality: "video", Phase: PhaseSubmit, Group: "video", CapabilityChecked: true,
	}},
	// ── 音频 ──
	{KindAudioSpeech, Transport{
		Path: "/v1/audio/speech", Method: "POST", EndpointKey: "audio/speech",
		Binary: true, Modality: "audio", Phase: PhaseSync, Group: "audio",
	}},
	{KindAudioTranscriptions, Transport{
		Path: "/v1/audio/transcriptions", Method: "POST", EndpointKey: "audio/transcriptions",
		Multipart: true, Modality: "audio", Phase: PhaseSync, Group: "audio",
	}},
	{KindAudioTranslations, Transport{
		Path: "/v1/audio/translations", Method: "POST", EndpointKey: "audio/translations",
		Multipart: true, Modality: "audio", Phase: PhaseSync, Group: "audio",
	}},

	// ── 音乐 ──
	{KindAudioMusic, Transport{
		Path: "/v1/audio/music/generations", Method: "POST", EndpointKey: "audio/music/generations",
		Modality: "music", Phase: PhaseSubmit, Group: "music", CapabilityChecked: true,
	}},
	{KindAudioLyrics, Transport{
		Path: "/v1/audio/lyrics", Method: "POST", EndpointKey: "audio/lyrics",
		Modality: "music", Phase: PhaseSubmit, Group: "music", CapabilityChecked: true,
	}},

	// ── SUNO 兼容面（完全对标 new-api 的 /suno/*）──
	//
	// 提交：`/suno/submit/{action}`（action=music|lyrics，大小写不敏感）。
	// 查询：单查与批量查分两条路由 —— new-api 就是两条，且批量是 POST + body，
	// 合并成一条会让「GET 带 body」这种客户端永远调不通。
	{KindSunoSubmit, Transport{
		Path: "/suno/submit/{action}", Method: "POST",
		Modality: "music", Phase: PhaseSubmit, Group: "music",
		Protocol: wireformat.NewAPI,
	}},
	{KindSunoFetch, Transport{
		Path: "/suno/fetch/{task_id}", Method: "GET",
		Modality: "music", Phase: PhaseRead, Group: "music",
		Protocol: wireformat.NewAPI,
	}},
	{KindSunoFetchBatch, Transport{
		Path: "/suno/fetch", Method: "POST",
		Modality: "music", Phase: PhaseRead, Group: "music",
		Protocol: wireformat.NewAPI,
	}},

	// ── 内容安全 ──
	{KindModerations, Transport{
		Path: "/v1/moderations", Method: "POST", EndpointKey: "moderations",
		Modality: "moderation", Phase: PhaseSync, Group: "safe",
	}},

	// ── 文件 ──
	{KindFilesUpload, Transport{
		Path: "/v1/files", Method: "POST", Multipart: true, Phase: PhaseSync, Group: "file",
	}},
	{KindFilesList, Transport{
		Path: "/v1/files", Method: "GET", Phase: PhaseSync, Group: "file",
	}},
	// files/get 与 files/content 是 OpenAI SDK 的 files.retrieve / files.content 对应物。
	//
	// 早先只有 DELETE /v1/files/{id}：SDK 能传、能列、能删，却**读不回元信息、
	// 也下不了内容**（只能拿上传时返回的 /f/ 直链）。直链 24 小时就失效且没有归属校验，
	// 需要长期引用的场景（把文件 id 存进业务库）根本走不通。
	{KindFilesGet, Transport{
		Path: "/v1/files/{id}", Method: "GET", Phase: PhaseSync, Group: "file",
	}},
	{KindFilesContent, Transport{
		Path: "/v1/files/{id}/content", Method: "GET", Binary: true, Phase: PhaseSync, Group: "file",
	}},
	{KindFilesDelete, Transport{
		Path: "/v1/files/{id}", Method: "DELETE", Phase: PhaseSync, Group: "file",
	}},
	{KindFileDirect, Transport{
		Path: "/f/{id}/{name}", Method: "GET", Binary: true, Phase: PhaseSync, Group: "file",
	}},

	// ── 异步任务（全模态统一读面） ──
	{KindTasksList, Transport{
		Path: "/v1/tasks", Method: "GET", Phase: PhaseRead, Group: "task",
	}},
	{KindTasksGet, Transport{
		Path: "/v1/tasks/{id}", Method: "GET", Phase: PhaseRead, Group: "task",
	}},
	{KindTasksArtifacts, Transport{
		Path: "/v1/tasks/{id}/artifacts", Method: "GET", Phase: PhaseRead, Group: "task",
	}},
	{KindTasksArtifactContent, Transport{
		Path: "/v1/tasks/{id}/artifacts/{key}/content", Method: "GET", Binary: true, Phase: PhaseRead, Group: "task",
	}},
	{KindTasksCancel, Transport{
		Path: "/v1/tasks/{id}", Method: "DELETE", Phase: PhaseRead, Group: "task",
	}},

	// ── 元信息与账户 ──
	{KindModelsList, Transport{
		Path: "/v1/models", Method: "GET", Phase: PhaseSync, Group: "model",
		// Gemini 的「OpenAI 兼容」入口：Google 的 OpenAI 兼容层就是
		// `<base>/v1beta/openai/`，SDK 只改 base_url 就能用。
		// 它必须保持 **OpenAI 错误体**（见 middleware.GeminiSurface 的例外）。
		Aliases: []string{"/v1beta/openai/models"},
	}},
	{KindModelsGet, Transport{
		Path: "/v1/models/{model}", Method: "GET", Phase: PhaseSync, Group: "model",
	}},
	{KindModelsDefaults, Transport{
		Path: "/v1/models/defaults", Method: "GET", Phase: PhaseSync, Group: "model",
	}},
	{KindAccount, Transport{
		Path: "/v1/account", Method: "GET", Phase: PhaseSync, Group: "acct",
	}},
	{KindUsageList, Transport{
		Path: "/v1/usage", Method: "GET", Phase: PhaseSync, Group: "acct",
	}},
	{KindUsageSummary, Transport{
		Path: "/v1/usage/summary", Method: "GET", Phase: PhaseSync, Group: "acct",
	}},
	// 同两份能力在 /dashboard/* 上还有一份 new-api 老习惯的别名路径：
	// 不登记的话，那条链上的 401/403 既判不出线格式、也不进调用记录（D6）。
	{KindBillingSubscription, Transport{
		Path: "/v1/dashboard/billing/subscription", Method: "GET", Phase: PhaseSync, Group: "acct",
		Aliases: []string{"/dashboard/billing/subscription"},
	}},
	{KindBillingUsage, Transport{
		Path: "/v1/dashboard/billing/usage", Method: "GET", Phase: PhaseSync, Group: "acct",
		Aliases: []string{"/dashboard/billing/usage"},
	}},

	// ── 平台自有：同步产物的能力 URL ──
	{KindMediaProxy, Transport{
		Path: "/v1/media/{token}", Method: "GET", Binary: true, Phase: PhaseSync, Group: "file",
	}},
}

// transportIndex 是 Kind → Transport 的索引（由 endpointTable 单源构建）。
var transportIndex = func() map[Kind]Transport {
	m := make(map[Kind]Transport, len(endpointTable))
	for _, e := range endpointTable {
		m[e.kind] = e.t
	}
	return m
}()

// TransportOf 返回端点的传输契约；未登记返回 false（调用方必须显式处理，不能拿到零值就继续）。
func TransportOf(k Kind) (Transport, bool) {
	t, ok := transportIndex[k]
	return t, ok
}

// MustPath 取端点的上游路径；缺契约直接 panic。
//
// 用于包级变量初始化（relay 的 Path* 别名）与测试断言：这两种场景都没有
// 合理的降级行为。返回空串比 panic 更危险——空路径会被 BuildURL 拼成上游根地址，
// 请求打到错的地方，日志里却只看到"上游 404"。
func MustPath(k Kind) string {
	t, ok := transportIndex[k]
	if !ok || t.Path == "" {
		panic("apidoc: 端点 " + string(k) + " 未登记或缺少上游路径")
	}
	return t.Path
}

// Kinds 按声明顺序返回全部 Kind（文档目录顺序）。
func Kinds() []Kind {
	out := make([]Kind, 0, len(endpointTable))
	for _, e := range endpointTable {
		out = append(out, e.kind)
	}
	return out
}

// NormalizeEndpointKey 归一端点标识：去空白、去前导 `/`、去 `v1/` 前缀。
//
// 库里 `models.endpoints` 既有 `video/generations`，也可能有人写成 `/v1/video/generations`，
// 两种都必须认——否则判定「这个模型需要独立文档吗」时永远不命中，全部模型都被判成需要。
func NormalizeEndpointKey(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimPrefix(p, "v1/")
	return p
}

// NormalizeModality 归一模型模态：空串按 chat 处理（存量数据的实际语义）。
//
// 唯一实现：relay 的模态闸门、文档判定、目录派生三处必须一致，
// 否则会出现「网关说这个模型是 chat、文档说它没有模态」。
func NormalizeModality(m string) string { return modality.Normalize(m) }

// endpointKeyByPath 是「平台路径 → models.endpoints 写法」的索引（含别名的归一形态）。
//
// 存在的理由（真实缺陷）：relay 把**平台路径**（`/v1/audio/speech`）交给适配器的
// Forward，而规格 DSL 里声明的 `endpoints.<key>.path` 是**上游路径**
// （如 `/api/v1/tts/submit`）。没有这张反查表，适配器只能拿平台路径去比上游路径，
// 于是任何「上游路径 ≠ 平台路径」的规格（也就是绝大多数非标厂商）永远路由不到端点，
// 报的却是「规格没有声明路径 /v1/audio/speech 对应的端点」——把配置正确说成配置错误。
//
// 同一路径可能出现在多个 Kind 上（如 `/v1/videos` 是 video/generations 的别名），
// 先登记者优先：endpointTable 的声明顺序就是语义优先级。
var endpointKeyByPath = func() map[string]string {
	m := make(map[string]string, len(endpointTable))
	for _, e := range endpointTable {
		key := e.t.EndpointKey
		if key == "" {
			continue
		}
		add := func(p string) {
			n := NormalizeEndpointKey(p)
			if n == "" {
				return
			}
			if _, exists := m[n]; !exists {
				m[n] = key
			}
		}
		add(e.t.Path)
		for _, alias := range e.t.Aliases {
			add(alias)
		}
	}
	return m
}()

// EndpointKeyByPath 把**平台路径**（`/v1/audio/speech`，可带别名、可带 v1 前缀）
// 反查成 models.endpoints 与规格共用的写法（`audio/speech`）。
//
// 走端点表而不是各适配器自己写一张表：路径表只有一份（endpointTable），
// 适配器各自映射必然漏项，漏的那一项在运行期表现为「端点不支持」。
func EndpointKeyByPath(path string) (string, bool) {
	key, ok := endpointKeyByPath[NormalizeEndpointKey(path)]
	return key, ok
}

// KindOfEndpointKey 把 `models.endpoints` 里的写法反查为 Kind。
//
// 只匹配声明了 EndpointKey 的端点：查询/取消/元信息类不在 models.endpoints 里，
// 反查不到返回 false 是正确语义。
func KindOfEndpointKey(key string) (Kind, bool) {
	want := NormalizeEndpointKey(key)
	if want == "" {
		return "", false
	}
	for _, e := range endpointTable {
		if e.t.EndpointKey != "" && e.t.EndpointKey == want {
			return e.kind, true
		}
	}
	return "", false
}

// kindByPath 是「平台路径 → Kind」的索引（含别名）。
//
// 与 endpointKeyByPath 的区别：那张表按 EndpointKey 建索引，而协议变体端点
// （/v1/messages）的 EndpointKey 是空的（它不表达新能力），因此在那里查不到。
// 中间件需要拿「这次请求命中的是哪个端点」来决定线格式，必须按路径直接查 Kind。
var kindByPath = func() map[string]Kind {
	m := make(map[string]Kind, len(endpointTable))
	put := func(n string, k Kind) {
		if n == "" {
			return
		}
		if _, exists := m[n]; !exists {
			m[n] = k
		}
	}
	// **两遍扫描，主 Path 先于一切 Aliases**。
	//
	// 一遍扫描（Path 与 Alias 混在一起按声明顺序裁决）曾经把一个**同路径不同协议**
	// 的真实冲突交给"谁先声明"决定：`/v1/models/{model}` 既是「模型详情」(GET) 的
	// 主路径，也是「Gemini 通配生成」(POST) 的别名形态。按声明顺序，Gemini 段在
	// 前，于是 `GET /v1/models/<id>` 被判定成 Gemini 协议 —— 它的错误体会变成
	// `{error:{code:404,status:"NOT_FOUND"}}`，而 OpenAI SDK 只认 `error.type`，
	// 客户端显示"未知错误"。这种错只在该端点出错时暴露。
	//
	// 语义：一个路径若是某端点的**主路径**，它就是那个端点的地址；别名只是
	// "同一件事的另一种拼法"，不能反过来抢主路径的身份。
	for _, e := range endpointTable {
		put(NormalizeEndpointKey(e.t.Path), e.kind)
	}
	for _, e := range endpointTable {
		for _, alias := range e.t.Aliases {
			put(NormalizeEndpointKey(alias), e.kind)
		}
	}
	return m
}()

// staticPrefixOf 返回路径里第一个 `{` 之前的部分（无占位符时返回空串）。
//
// 为什么要它：契约路径里的 `{model}` / `{id}` 是**占位符**，真实请求路径永远不会
// 含花括号。于是「按 p+"/" 判前缀」对这类路径**恒不成立** —— 表现为
// `/v1beta/models/gemini-x:generateContent` 掉进 NoRoute 时拿不到 Gemini 形状，
// `/v1/tasks/abc` 也同理。截到第一个占位符才是真正可比的静态前缀。
func staticPrefixOf(p string) string {
	i := strings.IndexByte(p, '{')
	if i < 0 {
		return ""
	}
	return p[:i]
}

// kindByStaticPrefix 是「静态前缀 → Kind」的索引（只收含占位符的契约路径）。
//
// 同一前缀可能命中多个 Kind（`/v1beta/models/{model}:generateContent` 与
// `:embedContent` 前缀相同），先登记者优先 —— 与 endpointTable 的语义优先级一致。
// 允许这种重叠的理由：它们本来就是同一协议、同一路由（action 在参数里），
// 这里只用于**错误体形状**的兜底判定，具体走哪个 handler 由路由表决定。
var kindByStaticPrefix = func() map[string]Kind {
	m := make(map[string]Kind, 8)
	put := func(p string, k Kind) {
		n := NormalizeEndpointKey(staticPrefixOf(p))
		if n == "" {
			return
		}
		if _, exists := m[n]; !exists {
			m[n] = k
		}
	}
	// 与 kindByPath 同一条规矩：主 Path 的前缀先占位，别名只补空缺。
	// 否则 Gemini 的 `/v1/models/{model}` 别名会拿下 `models/` 前缀，
	// 把所有 `/v1/models/*` 的兜底错误体都染成 Gemini 形状。
	for _, e := range endpointTable {
		put(e.t.Path, e.kind)
	}
	for _, e := range endpointTable {
		for _, alias := range e.t.Aliases {
			put(alias, e.kind)
		}
	}
	return m
}()

// KindOfPath 把平台路径（可带 v1 前缀、可用别名）反查为 Kind。
//
// 先精确命中，再按最长**静态前缀**命中（占位符之前的路径片段）。
// 供 ingress 中间件按「已匹配的路由」决定线格式用。未登记路径返回 false，
// 调用方必须按默认（openai）处理 —— 未知路径的 404/501 也应当是默认形状。
func KindOfPath(path string) (Kind, bool) {
	n := NormalizeEndpointKey(path)
	if k, ok := kindByPath[n]; ok {
		return k, true
	}
	best := ""
	var bestKind Kind
	for p, k := range kindByStaticPrefix {
		if strings.HasPrefix(n, p) && len(p) > len(best) {
			best, bestKind = p, k
		}
	}
	if best == "" {
		return "", false
	}
	return bestKind, true
}

// ProtocolOfKind 返回端点的线格式（未登记或未声明时为 openai）。
func ProtocolOfKind(k Kind) wireformat.Format {
	t, ok := TransportOf(k)
	if !ok {
		return wireformat.OpenAI
	}
	return wireformat.Normalize(string(t.Protocol))
}

// ProtocolOfRequestPath 用于**没匹配到任何路由**的请求（NoRoute 兜底）。
//
// 先精确查路径，再按最长前缀查：`/v1/messages/xxx` 不是已注册端点，
// 但它显然是 Claude 协议下的错误请求，返回 OpenAI 形状会让客户端只显示"未知错误"。
// 含占位符的契约路径（`/v1beta/models/{model}:generateContent`）按其静态前缀比，
// 否则花括号永远比不中，整棵 `/v1beta` 树的 404 都会退回 OpenAI 形状。
//
// 只读契约表，不写字面量：新增协议端点时不需要再改这里，
// 否则「加了端点忘了改兜底」的表现就是"错误体形状不对"，而这只有在真出错时才暴露。
func ProtocolOfRequestPath(path string) wireformat.Format {
	if k, ok := KindOfPath(path); ok {
		return ProtocolOfKind(k)
	}
	best := ""
	var bestKind Kind
	for _, e := range endpointTable {
		try := func(p string) {
			if p == "" {
				return
			}
			hit := false
			if strings.Contains(p, "{") {
				// 静态前缀本身以 `/` 结尾，直接比前缀。
				if pre := staticPrefixOf(p); pre != "" {
					hit = strings.HasPrefix(path, pre)
					p = pre
				}
			} else {
				hit = strings.HasPrefix(path, p+"/")
			}
			if !hit {
				return
			}
			if len(p) > len(best) {
				best, bestKind = p, e.kind
			}
		}
		try(e.t.Path)
		for _, alias := range e.t.Aliases {
			try(alias)
		}
	}
	if best != "" {
		return ProtocolOfKind(bestKind)
	}
	// 整棵 /v1beta 树都是 Gemini 协议面 —— 包括**没登记过的路径**：
	// SDK 拼错路径时拿到 OpenAI 形状的 404 只会显示"未知错误"，而真实原因
	// 往往是少写了一层 /v1beta 或动作名不受支持。
	//
	// 唯一例外是 /v1beta/openai/*：那一层存在的意义就是「OpenAI SDK 只改 base_url」，
	// 换掉错误体形状等于改了它的前提。
	if strings.HasPrefix(path, "/v1beta/") && !strings.HasPrefix(path, "/v1beta/openai/") {
		return wireformat.Gemini
	}
	// 整棵 /suno 树同理：它是 new-api 的 SUNO 协议面，成功与失败都是
	// `{code,message,data}`。未登记的 /suno 路径若回落 OpenAI，客户端会把
	// `{error:{…}}` 解析成"成功但 data 为空"，于是它以为任务提交失败但没有原因。
	if strings.HasPrefix(path, "/suno/") {
		return wireformat.NewAPI
	}
	return wireformat.OpenAI
}

// IsProtocolSurface 报告一条路径是否属于**协议面**（/v1、/v1beta、/suno、/dashboard）。
//
// 与 ProtocolOfRequestPath 的分工：本函数只回答「这是不是协议面」，不回答「是哪一种」
// —— 它用于**选择错误出口**（协议面信封 vs 控制面信封）。
//
// 为什么不能直接用 ProtocolOfRequestPath 的返回值判面：那个函数在判不出时会回落
// OpenAI，于是「/api/xxx」与「/v1/xxx」看起来一样。而全局 Recover 必须能分辨：
// panic 发生在 IngressFormat 之前时 context 里还没有线格式，只能按路径判面。
func IsProtocolSurface(path string) bool {
	switch {
	case path == "/v1", strings.HasPrefix(path, "/v1/"):
		return true
	case strings.HasPrefix(path, "/v1beta/"):
		return true
	case strings.HasPrefix(path, "/suno/"):
		return true
	case path == "/dashboard", strings.HasPrefix(path, "/dashboard/"):
		return true
	}
	return false
}

// TaskEndpointKeys 返回「参与渠道能力校验的异步端点」写法集合。
//
// 逐字对应 relay.TaskEndpointPaths 的语义：这里多写一个不存在的路径，运营按它声明端点
// 会被判成「已知」，运行期却 404；少写一个真实路径，声明它的模型会被判成「未知」而
// **跳过能力校验**，绑到不支持异步任务的渠道也能保存成功，直到第一次调用才失败。
//
// 判据必须同时覆盖两种形态：`Phase==PhaseSubmit`（视频/音乐这类天生异步的端点）与
// `AcceptsAsync`（图像这种同路径可同步可异步的端点）。只看 Phase 的后果是
// 图像异步提交会被 SubmitTaskOf 的闸门拒成「端点不支持任务」。
func TaskEndpointKeys() []string {
	var out []string
	for _, e := range endpointTable {
		if !e.t.CapabilityChecked || e.t.EndpointKey == "" {
			continue
		}
		if e.t.Phase == PhaseSubmit || e.t.AcceptsAsync {
			out = append(out, e.t.EndpointKey)
		}
	}
	return out
}

// modalityDefaults 是「模态 → 至少一个端点被承接」的探针路径。
//
// 语义是**任一满足**：image 模型的渠道只要能出图或能改图就足以承接该模型。
// 为什么用端点而不是「模态 → 适配器能力键」：Caps 枚举里没有 audio/music 这类模态键，
// 按模态硬映射必然写出查不到的键。
var modalityDefaults = map[string][]string{
	"chat":       {"chat/completions", "responses"},
	"embedding":  {"embeddings"},
	"rerank":     {"rerank"},
	"moderation": {"moderations"},
	"image":      {"images/generations", "images/edits"},
	"audio":      {"audio/speech", "audio/transcriptions"},
	"video":      {"video/generations"},
	"music":      {"audio/music/generations"},
}

// DefaultEndpoints 返回某模态的默认端点写法（用于 `models.endpoints` 为空时派生）。
//
// **不做空串 → chat 的归一**：调用方若需要「空模态视为 chat」的语义，必须先调
// NormalizeModality。原因很实际——网关目录用空模态表示「模型没配模态」，
// 派生出一组 chat 端点会让 /v1/models 凭空多出两个端点；而模态闸门那边空模态
// 早就按 chat 处理了。两件事不能混在一个函数里，否则改一处就会静默改变另一处。
//
// 未知模态返回 nil：调用方必须能区分「没有默认」与「默认是空数组」。
func DefaultEndpoints(modality string) []string {
	src, ok := modalityDefaults[strings.TrimSpace(modality)]
	if !ok {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// DeclaredEndpoints 解析 models.endpoints 里**显式书写**的端点（归一、去重、保序）。
//
// 这是端点写法的**唯一**解析入口（文档、能力判定、/v1/models 都走它）。
// 不做任何兜底：图像模态的默认端点里含 `images/edits`，一旦拿兜底值去推能力，
// 所有图像模型都会被推荐成「支持图生图」，用户照文档改图却 400。
// 没填端点的模型就是"没声明任何接口"——保存期会拒掉（见 modelcfg）。
func DeclaredEndpoints(raw json.RawMessage) []string {
	var declared []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &declared)
	}
	out := make([]string, 0, len(declared))
	seen := make(map[string]struct{}, len(declared))
	for _, e := range declared {
		key := NormalizeEndpointKey(e)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}
