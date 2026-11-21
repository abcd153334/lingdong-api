package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/service/billing"
)

// DefaultAdapterTimeout 是适配器调用上游的默认超时（S5 可改为读 system_config）。
//
// 它同时约束**响应体读取**（http.Client.Timeout 覆盖到 body EOF），所以只适合
// 秒级返回的端点。长耗时端点必须按族抬升（见 raiseTimeoutFor），否则会出现
// "超时 → 归因为上游故障 → 换渠道重跑 → 同一份产物付两次"。
const DefaultAdapterTimeout = 60 * time.Second

// 长耗时端点的超时上限。
//
// 取值依据：反代 /v1/ 的 proxy_read_timeout 是 960s（入口层模板：deploy/edge/nginx/snippets/timeouts.conf），
// 应用层不能再往上顶，否则客户端先拿到 nginx 的 504、而我们在后台继续等。
const (
	// ImageEndpointTimeout 覆盖同步出图（gpt-image / dall-e / 各家同步生图）。
	// 实测最慢档（4K、多张）在 9 分钟内返回。
	ImageEndpointTimeout = 10 * time.Minute
	// AudioEndpointTimeout 覆盖 TTS 与长音频转写：整段音频要在一次请求里读完，
	// 60s 会把正常的长文本朗读判成上游故障。
	AudioEndpointTimeout = 5 * time.Minute
)

// raiseTimeoutFor 按端点族抬高 cfg.Timeout（只升不降）。
//
// cfg 是 channelConfig 每次请求新构造的，直接改字段是安全的；这里仍然只做
// "取大"，避免把调用方显式配的更长时间压回去。
func (c *ChannelConfig) raiseTimeoutFor(kind endpointKind) {
	if c == nil {
		return
	}
	var want time.Duration
	switch {
	case isImageKind(kind):
		want = ImageEndpointTimeout
	case kind == kindAudioSpeech || kind == kindAudioTranscriptions || kind == kindAudioTranslations:
		want = AudioEndpointTimeout
	}
	if want > c.Timeout {
		c.Timeout = want
	}
}

// ErrUpstreamBodyTooLarge 表示上游响应体超过允许上限。
//
// 必须是 relay 包级哨兵而不是各适配器私有变量：网关（dispatch）要据此把
// 「上游回了超大垃圾体」归因成上游故障（502 + 渠道健康度），而不是当成客户端断开（499）。
// 私有变量做不到 errors.Is，就只能靠字符串匹配，改动文案即失效。
var ErrUpstreamBodyTooLarge = errors.New("upstream_body_too_large")

// ChannelConfig 是适配器调用所需的渠道运行态配置（密钥已解密）。
type ChannelConfig struct {
	ID      uint64
	Name    string
	BaseURL string
	APIKey  string
	Timeout time.Duration

	// Decl 是渠道级声明式适配配置；零值 = 完全不启用（行为与改造前一致）。
	Decl AdapterConfig
	// DeclErr 非 nil 表示 channels.adapter_config 解析/校验失败（脏数据、手工改库）。
	//
	// 适配器必须在构造请求前返回它，**不允许静默降级为"无配置"**：
	// 静默降级会让"配置写错了"表现成"请求能发但结果不对"，排查成本极高。
	// 之所以用字段而不是让 ChannelConfigFor 返回 error：后者被 4 处任务链路调用，
	// 改签名是纯噪声，而 fail-closed 正是这里想要的语义。
	DeclErr error

	// Options 是渠道级「适配器私有配置」（channels.adapter_options 原文，jsonb）。
	//
	// relay 只负责搬运字节，**不认识其中任何字段名**：形状由实现 OptionsSupporter 的
	// 适配器自己定义与校验。空（nil/""）表示未配置，由适配器决定内置默认值。
	Options json.RawMessage
	// OptionsErr 非 nil 表示 channels.adapter_options 不是合法 JSON（脏数据、手工改库）。
	//
	// 语义与 DeclErr 完全一致：适配器必须在构造请求前返回它，不允许静默降级为
	// "按默认配置跑"——否则运营改了配置却看不出没生效，只能靠结果不对来猜。
	OptionsErr error
}

// RelayRequest 是一次转发请求：目标模型 + 原始请求体（JSON 或 multipart）+ 原始 Content-Type。
type RelayRequest struct {
	Model       string
	Body        json.RawMessage
	ContentType string // 空 = application/json；multipart 端点需带 boundary

	// ModelID 是**平台对外模型 ID**（models.id），与 Model 的区别不能混用：
	// Model 是 binding 上的 upstream_model（上游真名），ModelID 是运营/调用方看到的本地 ID。
	//
	// 为什么适配器需要它：按模型区分协议的适配器（多米API 的 nano/gptimage/kling/suno）
	// 用本地 ID 作为档案键，上游名可能被运营改写成别名（秘塔的 metaso-h3 → MiniMax-H3）。
	// 只给 Model 的后果是「拿别名去查档案查不到」，同步桥接会在选路成功之后才失败。
	ModelID string

	// OnAccepted 由网关注入，适配器在**上游已经接受请求**（拿到 2xx、开始推流）
	// 时调用一次；未注入时不得调用（用 NotifyAccepted 保证 nil 安全）。
	//
	// 为什么需要这个信号（P1-9）：图片流式从「上游接受」到「第一帧」可能隔 90 秒。
	// 网关若等首帧才写 SSE 头，客户端与中间代理在这段时间里既看不到状态码、
	// 也收不到任何字节，只能干等到自己的空闲超时；但「上游还没接受就失败」
	// 又必须能返回 400/503 并换渠道 —— 所以头不能无条件提前写。
	// 由适配器在确切时点回调，这两件事才能同时成立。
	OnAccepted func()

	// ImageSize 是网关已经解析并归一过的图片尺寸规格（非图片请求为 nil）。
	//
	// 适配器**必须优先用它**，不要再从 Body 里自己解析 size：
	// 二次解析就是第二份真相源 —— 同步链路认 `1K` / 异步认 `1k`、一边认 `×`
	// 一边不认，症状是「同一个请求同步 200、异步 400」。
	// 适配器拿它只做一件事：翻译成该上游认识的写法（Ark 要 WxH、OpenAI 要原生枚举）。
	ImageSize *billing.ImageSize
}

// NotifyAccepted 安全触发「上游已接受」回调（nil 安全：适配器不必判空）。
func NotifyAccepted(req *RelayRequest) {
	if req != nil && req.OnAccepted != nil {
		req.OnAccepted()
	}
}

// RelayResponse 是上游响应：状态码 + 原始响应体。
type RelayResponse struct {
	StatusCode int
	Body       json.RawMessage
}

// RawResponse 是通用透传的原始响应（支持二进制体，如 TTS 音频），ContentType 回传上游声明。
type RawResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
	// Stream 非 nil 时 Body 恒为空，调用方必须 io.Copy 到客户端后 Close。
	//
	// 为什么大二进制体不能走 Body：TTS 音频按文本长度可达数十 MB，
	// 若整段 ReadAll 进内存，几十路并发就是 GB 级驻留，且要先完整收完才能开始回写
	// （首字节延迟 = 整个音频生成时长）。二进制端点因此改为边收边发。
	// 非 2xx 一律不流式：错误体很小，且网关要读出来做脱敏与归因。
	Stream io.ReadCloser
	// DurationSecs 是产物的**实际时长**（秒），0 = 上游未提供。
	//
	// 按秒计费的唯一可靠来源：桥接链路里请求参数（要多少秒）与产物时长（生成多少秒）
	// 经常不一致，按请求参数收就是系统性错账。有值时覆盖请求侧估算。
	DurationSecs int64
}

// UsageInfo 是上游返回的用量（对齐 usages 的 token 通道列）。
// Semantic/Raw/Source 用于口径归一与事后复核（S8 整改）。
type UsageInfo struct {
	// billing.Usage 是**唯一**的 token 通道定义：这里嵌入而不是抄一份字段。
	//
	// 曾经这里手写 13 个字段、结算前再由 toBillingUsage 逐字段搬运一次，
	// 结果是新增通道（CacheWrite5m/1h、ImageTokens、Semantic）只加在了 billing.Usage 上，
	// 搬运函数漏掉它们 → 缓存写入与图片 token **完全免费**，且没有任何 anomaly 提示。
	// 嵌入后编译期就不存在"两个字段集"这件事：加通道只需要改 billing.Usage 一处。
	billing.Usage
	Source string // upstream | mixed | estimated | none（结算口径来源）
	Raw    string // 上游 usage 原文（截断后落库）
}

// Billing 返回计费口径的用量（唯一映射出口）。
//
// 不返回裸的 u.Usage 而保留这个方法：调用点语义更清楚，且将来若需要在
// "上游用量"与"计费口径"之间插入归一（例如某个适配器的口径修正），只有这一处要改。
func (u UsageInfo) Billing() billing.Usage { return u.Usage }

// HasTokenUsage 判断上游是否给出了任一带 token 语义的用量。
func (u UsageInfo) HasTokenUsage() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0 || u.CacheTokens > 0 ||
		u.CacheWrite5mTokens > 0 || u.CacheWrite1hTokens > 0 || u.ImageTokens > 0 ||
		u.AudioInputTokens > 0 || u.AudioOutputTokens > 0
}

// SSEFrame 是一条流式增量帧（原始 data 载荷，不含 "data: " 前缀）。
type SSEFrame struct {
	Data []byte
	// Event 是 SSE 事件名（Anthropic 协议依赖它，如 message_start）。
	//
	// 为空表示"不写 event 行"——OpenAI 系上游全是这种，写回时与改造前逐字节一致。
	// 由入站编码器产出（把 OpenAI chunk 翻成 Claude 事件时填），
	// 上游适配器永远留空：它们说的是 OpenAI 协议，没有事件名这个概念。
	Event string
	Usage *UsageInfo
	Done  bool
}

// StreamResult 是流式转发的终态（状态码 + 汇总用量 + 上游请求 ID）。
type StreamResult struct {
	StatusCode        int
	Usage             UsageInfo
	UpstreamRequestID string
	ErrorMsg          string
	// CompletionTokensEst 是流中观测到的输出 token 估算（无 usage 时用于兜底）。
	CompletionTokensEst int64
	// Frames 是实际收到的数据帧数（不含 [DONE]）。
	//
	// 为什么必须显式计数：上游回 200 却一帧都不给（网关空转、上游写完响应头就断开）
	// 在今天的分支里会被判成「成功但无 usage」→ 按估算结算 → 用户为一无所获付钱。
	// 有了这个计数就能把它识别成 empty_response 并全额释放。
	Frames int
}

// Adapter 是渠道适配器统一契约（step-04 §⑥ 冻结）。
// 后续步骤只允许新增方法，不得修改已有签名；其余厂商适配器在 S6/S12 扩展。
type Adapter interface {
	// Name 返回适配器唯一标识（对齐 channels.adapter_id）。
	Name() string
	// Caps 返回能力矩阵子集：text/image/edit_image/video/tts/asr/embedding/passthrough。
	Caps() []string
	// Test 执行渠道连通性探测（鉴权 + 可达性）。
	Test(ctx context.Context, cfg *ChannelConfig) error
	// Chat 转发一次对话请求（透传上游响应）。
	Chat(ctx context.Context, cfg *ChannelConfig, req *RelayRequest) (*RelayResponse, error)
	// Embeddings 转发一次向量请求（透传上游响应）。
	Embeddings(ctx context.Context, cfg *ChannelConfig, req *RelayRequest) (*RelayResponse, error)
	// Completions 转发一次旧版补全请求（透传上游响应）。
	Completions(ctx context.Context, cfg *ChannelConfig, req *RelayRequest) (*RelayResponse, error)
	// StreamChat 转发流式对话：逐帧回调 onFrame；onFrame 返回错误（客户端断开）须立即中止并回传该错误。
	StreamChat(ctx context.Context, cfg *ChannelConfig, req *RelayRequest, onFrame func(SSEFrame) error) (*StreamResult, error)
	// Forward 通用透传：按 path 转发原始请求体（JSON 或 multipart），回传原始响应（可含二进制体）。
	// S6 多模态端点（images/audio/moderations/rerank）统一经此路径，避免为每个端点新增专用方法。
	Forward(ctx context.Context, cfg *ChannelConfig, req *RelayRequest, path string) (*RawResponse, error)
}

// TaskSubmitRequest 是异步任务提交入参（S7 视频等长任务）：上游模型名 + 原始请求体（model 字段由适配器重写）。
type TaskSubmitRequest struct {
	UpstreamModel string
	Body          json.RawMessage
	// Kind 是任务子类型，空串等价于「生成」。目前只有 "regeneration"（再生成）有语义；
	// AutoDL 等只看生成路径的适配器可忽略它，因此新增该字段对既有适配器零影响。
	Kind string
	// EndpointKey 是**入站端点**（models.endpoints 的写法，如 "video/generations"），
	// 由平台按路由填，适配器不得猜。
	//
	// 为什么必须由平台给：同一个适配器可能承接多个异步端点（多米：视频/图像/音乐/歌词），
	// 而「这次是哪个」只有入站路由知道。让适配器按模态或按模型名推断，等于把路由决策
	// 复制一份到适配器里 —— 两边一旦不一致，请求就打到错的上游端点，报错还是 502。
	//
	// 空串 = 重试路径（没有入站端点）：此时只要求「该适配器能做异步」。
	EndpointKey string
	// ModelID 是对外模型 ID（与 Body.model 同源，但由平台填入，不信任请求体）。
	//
	// 多协议适配器据此查模型档案（协议 / mode / 固定档位），**不允许**改读绑定表的
	// upstream_model 来推断：运营把 upstream_model 改成别名时，路由决策不能跟着漂。
	// 空串 = 旧行为（适配器只按 UpstreamModel 工作）。
	ModelID string
	// SubKind 由适配器**回写**（req 是指针，值接收者也写得进去），平台落 tasks.subtask_kind，
	// 轮询时原样带回。语义：同一适配器内部的子协议标记（如 "kling.t2v"），空串 = 单路径。
	//
	// 为什么必须落库：提交与轮询是两次调用，中间没有任何共享状态；多路径协议（可灵 5 条
	// 提交路径对应不同查询路径）若不带回子协议，轮询必然查错端点，产物永远拿不到。
	SubKind string
	// AutoDuration 表示上游应使用「智能时长」（如 Ark 的 duration=-1）。
	//
	// 为什么必须由平台显式告知、不能让适配器从 Body 猜：
	//   - 计费基准（正数秒数）与上游参数（-1）是两个语义，平台已在 task 层拆开
	//     （params.Seconds / params.SecondsAuto）；适配器再猜一份就是第二真相源；
	//   - Body 里可能同时有 seconds 与 duration，猜哪一个是不可判定的。
	// 空 = 普通时长（沿用 Body 里的秒数）。
	AutoDuration bool
	// OmitDuration 表示**不要下发任何时长字段**，由上游用自己的默认值。
	//
	// 与 AutoDuration 互斥，两者都是「用户没写秒数」的分支：
	//   - 模型接受 -1 → AutoDuration（上游按 prompt 智能选长）；
	//   - 模型不接受 -1 → OmitDuration（缺省即上游默认，通常是 5 秒）。
	// 以前这里只有 AutoDuration，非智能时长的模型只能被 400 逼用户填秒数。
	OmitDuration bool
	// Seconds 是平台**归一后**的时长（秒）；0 = 用户没写。
	//
	// 为什么必须给具体值：AutoDuration/OmitDuration 只表达「要不要自动」，
	// 适配器仍得从 Body 里猜具体秒数 —— 而 Body 可能同时有 seconds 与 duration，
	// 猜错就是「上游出 10 秒、平台按 5 秒收」。有这条之后，适配器一律用平台归一值。
	Seconds int64
	// Notes 是适配器**回写**的「非致命事实」（只留痕，不改行为、不影响成败）。
	//
	// 存在的理由：有些差异既不该拦截（上游确实会执行）、又必须可查（它会让
	// 「平台计费档」与「上游交付档」不一致）。典型是方舟视频的提示词弱校验参数
	// （`--rs 4k`）：写进提示词就生效，而平台只按 body 计费。
	//
	// 平台侧处置：逐条 Warn（带 task/channel/model），并往 usages.anomaly 追加一个
	// 固定标记。适配器不引 model 包，所以这里只放**人读的说明**，标记由 task 层决定。
	Notes []string
}

// TaskKindRegeneration 是 TaskSubmitRequest.Kind 的再生成取值（与 task.params.Kind 同源）。
const TaskKindRegeneration = "regeneration"

// TaskStatus 是异步任务的上游状态视图（v2.0 §4.7.1）；未知状态由调用方归一为 running。
type TaskStatus struct {
	Status    string // queued/running/succeeded/failed/canceled
	Progress  int    // 上游自报进度（0-100，缺失为 0）
	ResultURL string // 兼容字段：单产物链接（适配器可不填，Succeed 内统一折进 ResultURLs）
	Error     string // 失败原因
	// StatusMatched 表示 Status 是**被识别**出来的，而不是「不认识所以当成 running」。
	//
	// 为什么必须能区分：桥接里两者都会继续轮询，但「不认识」是确定性错误
	// （status_map 少写了一个值），继续等到 600 秒只会白付上游成本；
	// 而「认识但还在跑」是正常等待，绝不能当成配置错。
	StatusMatched bool
	// RawStatus 是上游原始状态词（未归一），仅在 StatusMatched=false 时用于报错定位。
	RawStatus string
	// ResultURLs 是多产物链接（图像多图 / 音乐多首）。ResultURL 非空且本字段为空时自动折叠为单元素。
	//
	// 顺序即产物顺序，平台按此顺序稠密编号 0..n-1；空字符串元素被跳过（不占号）。
	ResultURLs []string
	// ResultText 是文本产物（歌词）。非空时平台直接落盘为 text/plain 文件，不走链接下载。
	// 与 ResultURLs 可并存（某些上游既给歌词又给成品）。
	ResultText string
	// Usage 是上游自报的实际用量（B3-3）。零值表示上游没给，平台回落到请求参数。
	//
	// 为什么要上游值：视频按时长计费，而「请求 5 秒、上游实际生成 10 秒」是常态
	// （上游自动续长、按档位取整）。只按请求参数收就是系统性少收一半。
	//
	// 但**回落方向取决于请求的意图**（见 mergeUpstreamUsage）：
	// 显式秒数时取 max(请求, 上游) 防少收；智能时长时以上游为准，
	// 因为请求侧的秒数只是冻结预算，不是用户要的时长。
	Usage TaskUsage
}

// TaskUsage 是异步任务的上游自报用量（各适配器按能力填充，未提供的字段留零值）。
//
// 全部字段都用「原始值」而不是已计费金额：计费公式必须留在平台侧，
// 上游一旦改价或改口径，平台不能跟着漂。
//
// 字段带 json tag 的理由：这个结构会被**原样落进** `tasks.upstream_usage`（jsonb），
// 上游没给 usage 时它就是唯一的存证。没有 tag 时落库键名是 Go 字段名
// （`{"OutputSeconds":5}`），与真正的上游原文（`{"total_seconds":5}`）长得不一样，
// 事后核对的人分不清哪份是上游说的、哪份是平台算的。
type TaskUsage struct {
	// OutputSeconds 是实际生成的音视频时长（秒）。视频按时长计费时用它。
	OutputSeconds float64 `json:"output_seconds,omitempty"`
	// InputSeconds 是参考视频的总时长（秒）。
	InputSeconds float64 `json:"input_seconds,omitempty"`
	// ImageCount 是上游实际产出的图片张数。
	//
	// 注意：结算**不用**它，而用「实际成功转存数」——用户拿不到的图不该收费。
	// 保留它只为在两者不一致时写 anomaly 供对账。
	ImageCount int64 `json:"image_count,omitempty"`
	// AudioSeconds 是生成音频时长（秒）。音乐按任务计费时不用它。
	AudioSeconds float64 `json:"audio_seconds,omitempty"`
	// Raw 是上游 usage 对象的**逐字节原文**（未重排、未裁剪）。
	//
	// 为什么必须有：上面的四个字段是「平台认识的形状」。上游多给一个
	// `video_duration` / `output_tokens`，平台没解析出来的部分就彻底消失了 ——
	// 等到发现少收，连「上游当初到底报了什么」都无从查证（唯一的替代是抓包）。
	//
	// **只作证据，不作计费输入**：金额一律由上面已解析的字段算出，
	// 原文只写进 `usages.raw_usage` 供人工取证（见 task.upstreamUsageRaw）。
	// 谁要是回头去解析它算钱，就会多出第二套口径 —— 两个口径必然分叉。
	//
	// 约定：只允许 JSON **对象**。`null`、`[]`、标量一律视为「没上报」置空 ——
	// 否则 `raw:null` 会被读的人当成「上游确实报了个 null」。
	Raw json.RawMessage `json:"raw,omitempty"`
}

// UsageRaw 返回上游 usage 原文（保证是合法 JSON 对象，否则返回 nil）。
//
// 适配器里手写 `st.Usage.Raw = body` 会让 `null` / `[]` / 截断体直接进库，
// 所以统一走这个入口做形状校验。
func (u *TaskUsage) SetRaw(raw []byte) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		u.Raw = nil
		return
	}
	u.Raw = append(json.RawMessage(nil), trimmed...)
}

// EndpointKind 是 endpointKind 的**导出别名**。
//
// 为什么要有它：端点的判定逻辑（SupportsEndpoint）由 relay 之外的包实现——
// apidoc 契约层要做双向覆盖、DuoMi/AutoDL/规格适配器都要按端点声明能力，
// 而 endpointKind 是 relay 包内未导出类型，跨包无法命名它、方法签名写不出来。
// 用别名 + 导出常量暴露「可引用的值」，既让外部包能实现 EndpointSupporter，
// 又不必把 relay 内部 30 多处引用全部改名。
type EndpointKind = endpointKind

// 导出端点常量（值与原内部常量完全相同，仅为跨包可见）。
const (
	KindChat                = kindChat
	KindCompletions         = kindCompletions
	KindEmbeddings          = kindEmbeddings
	KindImagesGenerations   = kindImagesGenerations
	KindImagesEdits         = kindImagesEdits
	KindImagesVariations    = kindImagesVariations
	KindAudioSpeech         = kindAudioSpeech
	KindAudioTranscriptions = kindAudioTranscriptions
	KindAudioTranslations   = kindAudioTranslations
	KindModerations         = kindModerations
	KindRerank              = kindRerank
	KindResponses           = kindResponses
)

// EndpointKeyOf 把端点枚举转成 models.endpoints 写法；未知枚举（或无对应能力键）返回空串。
//
// 值**只从 apidoc 取**（`Transport.EndpointKey`），不在这里再抄一张表：
// 曾经这里是第二份手写清单，新增端点时漏改的表现是「规格说支持、网关判不支持」，
// 而这既不报错也不打日志 —— 只表现为某个端点偶尔 501。
func EndpointKeyOf(kind EndpointKind) string {
	k, ok := docKindOf[kind]
	if !ok {
		return ""
	}
	t, ok := apidoc.TransportOf(k)
	if !ok {
		return ""
	}
	return t.EndpointKey
}

// EndpointSupporter 是**可选**能力接口：适配器声明自己承接哪些端点。
//
// 不实现 = 全支持（内置 openai 适配器零改动）。实现者是插件适配器：
// 它的 Forward 只路由 /images/ 与 /audio/speech，其余端点返回协议错——那种错会被
// 归因为渠道故障，所以必须在**选路阶段**就把候选排除掉，而不是等它报错。
type EndpointSupporter interface {
	// SupportsEndpoint 表示当前适配器能否承接该端点（能力声明 + 真实实现取交集）。
	SupportsEndpoint(kind EndpointKind) bool
}

// OptionsSupporter 是**可选**能力接口：适配器接受「渠道级私有配置」（channels.adapter_options）。
//
// 不实现 = 不接受（fail-closed）：运营在管理台给它配了私有配置，保存期就会被拒绝，
// 而不是"存进去了但没人读"。形状完全由实现方定义（relay 不引入任何厂商字段名）。
type OptionsSupporter interface {
	// ValidateAdapterOptions 校验配置原文；返回 *ConfigError 便于管理台定位到具体字段。
	ValidateAdapterOptions(raw []byte) error
}

// SupportsAdapterOptions 是「是否接受渠道私有配置」的唯一判定入口。
//
// 管理台的保存校验与 /admin/adapters 的 supports_options 都调它，避免两处各写一份判定。
func SupportsAdapterOptions(adapterID string) bool {
	a, ok := GetAdapter(adapterID)
	if !ok {
		return false
	}
	_, ok = a.(OptionsSupporter)
	return ok
}

// ValidateAdapterOptions 把校验转交给适配器实现（未实现时给出明确错误）。
func ValidateAdapterOptions(adapterID string, raw []byte) error {
	a, ok := GetAdapter(adapterID)
	if !ok {
		return &ConfigError{Scope: "渠道适配器配置错误", Field: "adapter_options", Msg: "适配器 " + adapterID + " 未注册"}
	}
	s, ok := a.(OptionsSupporter)
	if !ok {
		return &ConfigError{Scope: "渠道适配器配置错误", Field: "adapter_options", Msg: "适配器 " + adapterID + " 不使用渠道私有配置"}
	}
	return s.ValidateAdapterOptions(raw)
}

// ModelPreflight 是「该适配器能否承接这个平台模型 ID」的一次**无副作用**检查结果。
//
// 为什么需要它：多协议适配器（多米）按模型 ID 决定用哪套信封/端点，而"能否承接"
// 在保存绑定那一刻就可以判定。没有这一步，运营只能等第一次真实调用才发现配置不完整
// —— 而那时用户已经付了钱、额度已经冻结。
type ModelPreflight struct {
	// OK 为 true 表示按当前渠道配置可以承接。
	OK bool `json:"ok"`
	// Reason 是 OK=false 时给运营看的可读原因（可含可粘贴的配置片段）。
	Reason string `json:"reason,omitempty"`
	// Family 是命中的协议族名（如 "kling.pro"），便于运营对照文档。
	Family string `json:"family,omitempty"`
	// Note 是 OK=true 时的补充说明（例如"该协议不使用上游模型名"），可为空。
	Note string `json:"note,omitempty"`
}

// ModelPreflighter 是**可选**能力接口：适配器愿意在保存期对外暴露「模型可否承接」。
//
// 不实现 = 不参与预检（零影响）。只有按模型 ID 决定路由的多协议适配器需要实现它；
// openai / volcengine-ark / metaso / autodl 都是无条件透传，没有可判定的东西。
type ModelPreflighter interface {
	// PreflightModel 用渠道当前的 adapter_options 判定；rawOptions 允许为 nil（默认配置）。
	PreflightModel(modelID string, rawOptions []byte) ModelPreflight
}

// PreflightModel 是预检的唯一入口；bool=false 表示该适配器不参与预检。
//
// 调用方（绑定保存）据此决定是否产出非阻断告警，**不得**把它当成拦截依据：
// 预检只覆盖适配器层的协议识别，渠道之外的闸门（价目、模态、目录收录）由各自入口负责。
func PreflightModel(adapterID, modelID string, rawOptions []byte) (ModelPreflight, bool) {
	a, ok := GetAdapter(adapterID)
	if !ok {
		return ModelPreflight{}, false
	}
	p, ok := a.(ModelPreflighter)
	if !ok {
		return ModelPreflight{}, false
	}
	return p.PreflightModel(modelID, rawOptions), true
}

// DeclarativeSupporter 是**可选**能力接口：适配器声明自己是否支持渠道级声明式配置
// （auth_header / path_overrides / request_patch / response_path）。
//
// 不实现 = 不支持（fail-closed）。插件适配器就走这条：它的鉴权头与出网路径由 manifest
// 与插件代码决定，渠道级配置对它无效——静默忽略会让运营以为配置生效了。
type DeclarativeSupporter interface {
	SupportsDeclarativeConfig() bool
}

// SupportsDeclarativeConfig 是唯一的能力判定入口。
//
// service/channel 的保存校验与 controller 的参数校验都调它，避免两处各写一份判定
// （一处改了另一处没改，就会出现"能存进去但请求报错"或反之）。
func SupportsDeclarativeConfig(adapterID string) bool {
	a, ok := GetAdapter(adapterID)
	if !ok {
		return false
	}
	s, ok := a.(DeclarativeSupporter)
	return ok && s.SupportsDeclarativeConfig()
}

// capRoutingEnforced 是能力选路的灰度开关（LD_CAP_ROUTING）。
//
// 默认开启：插件的 Forward 本来就不支持这些端点，不拦的话会变成「协议错 → 记渠道失败 →
// 熔断」，比 503 更难排查。置 0 可退回「不做能力过滤」的旧行为（应急用，不建议长期）。
func capRoutingEnforced() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_CAP_ROUTING"))) {
	case "0", "false", "no":
		return false
	}
	return true
}

// adapterSupports 是端点能力的唯一判定入口（不要在别处再写一份）。
func adapterSupports(a Adapter, kind endpointKind) bool {
	if !capRoutingEnforced() {
		return true
	}
	if s, ok := a.(EndpointSupporter); ok {
		return s.SupportsEndpoint(kind)
	}
	return true
}

// TaskAdapter 是异步任务能力扩展接口（S7，对 Adapter 只增不改）：支持长任务的适配器按需实现。
//
// 「承接哪些端点」不看这个接口，而看可选的 AsyncEndpointSupplier（见 task_endpoints.go）：
// 只回答「能不能做异步」的布尔无法表达「能做视频但不能做音乐」，而那正是误配的来源。
type TaskAdapter interface {
	// SubmitTask 提交异步任务，返回上游任务 ID（严格解析；缺失 ID 视为提交失败）。
	SubmitTask(ctx context.Context, cfg *ChannelConfig, req *TaskSubmitRequest) (string, error)
	// PollTask 查询异步任务状态。
	PollTask(ctx context.Context, cfg *ChannelConfig, upstreamTaskID string) (*TaskStatus, error)
}

// GetTaskAdapter 取支持异步任务的适配器（未注册或未实现该能力返回 false）。
func GetTaskAdapter(name string) (TaskAdapter, bool) {
	a, ok := GetAdapter(name)
	if !ok {
		return nil, false
	}
	ta, ok := a.(TaskAdapter)
	return ta, ok
}

// TaskPollRequest 是轮询入参（新增类型，不改任何既有方法签名）。
type TaskPollRequest struct {
	UpstreamTaskID string
	// SubKind 是提交时适配器回写的子协议标记（tasks.subtask_kind）；空串 = 走默认查询路径。
	SubKind string
}

// DetailedTaskPoller 是**可选**能力接口：需要子协议上下文才能查上游的适配器实现它。
//
// 为什么不直接给 TaskAdapter.PollTask 加参数：TaskAdapter 可能被外部实现方
// **隐式实现**（方法集匹配），改签名时 Go 不会报编译错，只会让它静默地「不再实现
// TaskAdapter」，于是类型断言在运行期失败，表现为「这些渠道的异步任务全部 502、CI 全绿」。
// 加一个可选接口没有这个风险：没实现就自动回落到旧签名，行为一字不变。规格（`spec:`）
// 适配器需要它，因为同一 vendor 的不同子协议（如文生视频 / 图生视频）查询路径与状态值域不同。
type DetailedTaskPoller interface {
	PollTaskDetailed(ctx context.Context, cfg *ChannelConfig, req *TaskPollRequest) (*TaskStatus, error)
}

// PollTaskOf 是轮询的**唯一分派点**：实现了 DetailedTaskPoller 走新路径，否则回落旧签名。
//
// 调用方（worker / task.ResultURLAt）必须调它，不得自行类型断言——两处各写一份判定，
// 迟早出现「一处认识了新接口、另一处不认识」的静默降级。
func PollTaskOf(ctx context.Context, ta TaskAdapter, cfg *ChannelConfig, req *TaskPollRequest) (*TaskStatus, error) {
	if ta == nil {
		return nil, errors.New("relay: 适配器不支持异步任务")
	}
	if req == nil {
		req = &TaskPollRequest{}
	}
	if p, ok := ta.(DetailedTaskPoller); ok {
		return p.PollTaskDetailed(ctx, cfg, req)
	}
	return ta.PollTask(ctx, cfg, req.UpstreamTaskID)
}

// UnreplayableSupplier 是**可选**能力接口：适配器声明「哪些子协议的请求体不可重放」。
//
// 语义：任务重试要把请求体重发一次。绝大多数子协议可以（tasks.submit_body 存着原文），
// 但有的通道请求体完全由用户拼装、语义依赖上游概念，重发一份走样的报文
// 等于「按素材计费、只发一段文本」——这类子协议由适配器自己声明，平台不猜。
type UnreplayableSupplier interface {
	// UnreplayableSubKinds 返回不可重放的子协议标记（如 Ark 的 ark.native）。
	UnreplayableSubKinds() []string
}

// TaskReplayable 报告某个任务的请求体能否原样重发。
//
// 判据必须读 tasks.subtask_kind 这一列（由 SubmitTask 回写、随提交落库），
// 不能读 request_params 里的副本 —— 那份在任务行插入时就已序列化完毕，永远是空串。
// 是否存在不可重放的子协议由适配器声明，任务层硬比 "ark.native" 就是把这句话复制了一份。
func TaskReplayable(adapterID, subKind string) bool {
	subKind = strings.TrimSpace(subKind)
	if subKind == "" {
		return true
	}
	ta, ok := GetTaskAdapter(adapterID)
	if !ok {
		return true
	}
	s, ok := ta.(UnreplayableSupplier)
	if !ok {
		return true
	}
	for _, k := range s.UnreplayableSubKinds() {
		if strings.TrimSpace(k) == subKind {
			return false
		}
	}
	return true
}

// PathStreamer 是**可选**能力接口：按指定上游路径逐帧转发 SSE。
//
// 为什么需要它：/v1/responses 的上游就是一条 SSE（事件名 response.output_text.delta 等），
// 但它与 chat 的流式语义不同（没有 [DONE]、usage 嵌在终止事件的 response 里）。
// Chat 方法只能打 /v1/chat/completions，Forward 又整段读（受 60s 与 16MiB 限制），
// 两者都承接不了"逐帧转发另一条路径上的 SSE"。
//
// 与 ImageStreamer 同一范式（可选接口而不是给 Adapter 加方法）：给 Adapter 加方法会让
// 外部实现者静默地"不再实现 Adapter"，而运行期类型断言失败表现为"这些渠道全部 502、CI 全绿"。
type PathStreamer interface {
	// StreamPath 向 path 发流式请求并逐帧回调；onFrame 返回错误表示客户端断开，必须立即中止。
	// 返回的 StreamResult 只描述"上游实际发生了什么"，不含任何金额。
	StreamPath(ctx context.Context, cfg *ChannelConfig, req *RelayRequest, path string,
		onFrame func(SSEFrame) error) (*StreamResult, error)
}

// ImageStreamer 是**可选**能力接口：图片端点逐帧 SSE 转发。
//
// 为什么必须是独立接口而不是给 Adapter 加方法：Adapter 可能被外部（插件）隐式实现，
// 加方法会让它们静默地「不再实现 Adapter」——而它自己的 SupportsEndpoint 仍返回 true，
// 于是类型断言在运行期失败，表现为「这些渠道的图片全部 502、CI 全绿」。
// 与 DetailedTaskPoller 同一策略（见上方注释）。
//
// 适配器**不判断**「这个模型是否支持流式」：那是 models.capabilities 的事实，
// 由网关在预检期判一次（relayRequest）。适配器里再按模型名写一份白名单就是第二真相源，
// 运营改能力声明后两边会打架。上游若真的拒绝，适配器只负责把 4xx 翻译成
// CallerParamError（400），不做「我知道你不支持」的猜测。
type ImageStreamer interface {
	// StreamImages 逐帧回调 onFrame；onFrame 返回错误（客户端断开）必须立即中止并回传该错误。
	// 返回的 ImageStreamResult 只描述「上游实际发生了什么」，不含任何金额。
	StreamImages(ctx context.Context, cfg *ChannelConfig, req *RelayRequest,
		onFrame func(ImageFrame) error) (*ImageStreamResult, error)
}

// ImageFrame 是一帧图片流式事件（原始 SSE 的一帧）。
type ImageFrame struct {
	// Event 是 SSE 的 event 行（上游与服务端同值；未知事件原样透传）。
	Event string
	// Data 是 data 行的原始 JSON（不含 "data: " 前缀）。
	// 网关只改其中一个键（url），其余字节不动。
	Data []byte
	// URL 是旁路解析出的**上游原链**（改写前）；空表示本帧没有可改写的链接。
	URL   string
	Index int
	Size  string
	// HasImage 表示本帧携带一张**完整可用的图片**（url 或 b64_json 二选一）。
	//
	// 为什么不直接用 URL 判：`response_format=b64_json` 的流里，图片帧只有
	// b64_json 没有 url —— 只看 url 会把一整条成功的 b64 流判成"零交付"，
	// 结果是用户拿到图、平台按零帧 502 全额退款。
	HasImage bool
	// Terminal 表示该帧是终态事件（completed / 整单失败）。
	Terminal bool
}

// ImageStreamResult 是图片流式转发的终态（只描述上游事实，不含金额）。
type ImageStreamResult struct {
	StatusCode int
	Model      string
	// Frames 是收到的数据帧数（不含 [DONE]），用于「200 但零帧」判定。
	Frames int
	// Delivered 是实际成功下发的图帧数（网关写成功才计数）。
	Delivered int
	// GeneratedImages 是 completed.usage.generated_images（0 = 上游未给）。
	GeneratedImages int64
	// Images 是上游原链（顺序 = image_index），供对账。
	Images []string
	// UsageRaw 是 completed 帧的原始 data（落 call_io 的 resp_raw，替代文本拼接）。
	UsageRaw []byte
	// RawJSON 非空表示「上游回了 200 但报文是整段 JSON，不是 SSE」（P3-2）。
	//
	// 触发场景：客户端要 stream、模型或渠道实际不支持、上游忽略 stream=true 直接回整段结果。
	// 这时**上游已经出图并计费**，若网关仍按「零帧空流」处理，用户付了钱、图被丢掉，
	// 还落一个 502。网关必须把它当成普通 JSON 响应交付。
	RawJSON  []byte
	ErrorMsg string
}

// ErrAdapterRegistered 表示同名适配器重复注册。
var ErrAdapterRegistered = errors.New("relay: 适配器已注册")

// SyncWaitTimeoutError 表示适配器在同步桥接的等待上限内没等到上游终态。
//
// 为什么必须是独立类型而不是普通 error：dispatch 要据此返回 **504**（网关超时）
// 而不是 503（全渠道失败）。两者的运维含义完全不同——504 是「上游太慢，我们不等了」，
// 503 是「我们没有可用渠道」，照着 503 去查渠道配置会完全走错方向。
//
// 放在 relay 而不是 adapters 包：dispatch 需要 errors.As 判断它，
// 而 relay 不能导入 adapters（adapters 已经 import relay，会成环）。
type SyncWaitTimeoutError struct {
	// Waited 是实际等待时长。
	Waited time.Duration
	// Detail 是给调用方看的补充说明（如上游任务号）。
	Detail string
	// TaskID 是上游任务号（落 usages.upstream_request_id）。
	//
	// 单独留字段而不是从 Detail 里正则抠：这条记录是**唯一**能对上"平台已经付过钱、
	// 但结果没交付给用户"的那笔上游任务的凭据，解析失败就等于丢。
	TaskID string
}

// Error 实现 error。文案面向调用方，必须包含「已释放额度」与「可改用异步端点」两条出路。
func (e *SyncWaitTimeoutError) Error() string {
	// 文案会同时进入门户与 CSV 导出（usages.error_msg），因此**不许出现"上游"**：
	// 那等于告诉用户我们在转卖别家的服务。日志里也不缺这一点信息。
	msg := fmt.Sprintf("请求在 %d 秒内未完成生成，平台已停止等待并释放额度", int(e.Waited.Seconds()))
	if strings.TrimSpace(e.Detail) != "" {
		msg += "（" + e.Detail + "）"
	}
	return msg + "；长时间任务请改用异步端点"
}

// Class 让归因走 internal 而不是 upstream（见 outcome.go 的 classifyError）。
//
// 同步等待超时是**我们的策略**——上游没错，它只是还没跑完。若按 upstream 记，
// 后果是「模型越慢，越容易熔断自己的渠道」：gpt-image 单张 540s，阈值附近反复超时，
// 一条完全健康的渠道会被自己的慢任务打挂，且熔断后连快的模型也用不了。
func (e *SyncWaitTimeoutError) Class() string { return "slow" }

// ImageGenTimeoutError 表示同步图片生成在**适配器自己的上限**内没拿到结果。
//
// 与 SyncWaitTimeoutError 同族（`Class()=="slow"`，不记渠道健康），但对外语义不同，
// 所以必须是独立类型：
//   - SyncWait 是「异步任务已提交，我们不等了」——上游一定在跑，结局确定是超时；
//   - 本类型是「请求已经发出去，连响应头都没收到」——上游**可能已经出图并计费**，
//     只因链路超时我们看不到结果。
//
// 两个后果必须落到代码里，否则就是真金白银的损失：
//  1. **不换渠道重投**：换一条渠道等于让上游再生成一次，平台付两次钱，
//     用户还可能先后拿到两张不同的图；
//  2. **打 submit_ambiguous 标记**：这笔上游成本没有对应交付，对账时要能筛出来。
type ImageGenTimeoutError struct {
	// Waited 是本端等待上限。
	Waited time.Duration
	// Err 是底层超时原因（SDK/传输层的超时错误）。
	Err error
}

// Error 面向调用方：说清「已停止等待、本次未扣费/已释放」与「可能已生成」。
func (e *ImageGenTimeoutError) Error() string {
	return fmt.Sprintf("请求在 %d 秒内未返回结果，平台已停止等待并释放额度；"+
		"结果可能已生成，请勿直接重试（会产生第二次费用）",
		int(e.Waited.Seconds()))
}

// Unwrap 保留底层超时原因（便于日志里看到真实的传输层错误）。
func (e *ImageGenTimeoutError) Unwrap() error { return e.Err }

// Class 让归因走 internal（不记渠道健康）：慢是上游特性，不是渠道故障。
// 若按 upstream 记，「模型越慢越容易熔断自己的渠道」，正好打在最该保留的那条上。
func (e *ImageGenTimeoutError) Class() string { return "slow" }

// SyncAbandonedError 表示同步桥接在等待期间**客户端先走了**（关页面 / SDK 超时 / 反向代理
// 读超时），而上游任务仍在生成。
//
// 为什么要专门一个类型，而不是直接返回 ctx.Err()：
//   - 上游任务号是事后补图的**唯一线索**。直接返回 context.Canceled 时，调用记录里
//     只剩一句「客户端断开」，运维无法知道那笔已经付给上游的生成是哪一个任务；
//   - Unwrap 回 ctx.Err()，所以 dispatch 的 `ctx.Err() != nil` / errors.Is 判定完全不变
//     （仍走 499 + 全额释放，绝不扣用户的钱）。
type SyncAbandonedError struct {
	// TaskID 是上游任务号（可能为空：提交阶段就断了）。
	TaskID string
	// Waited 是已经等过的时长。
	Waited time.Duration
	// WaitLimit 是本端**允许**等待的上限（同步桥接的等待预算）。
	//
	// 有了它才能把「客户端自己取消」与「链路上的代理先断」在记录里区分开：
	// 等了 60 秒就没了、而本端本来能等 780 秒，那不是平台放弃，是中间某一层断了。
	WaitLimit time.Duration
	// Err 是原始取消原因（context.Canceled / DeadlineExceeded）。
	Err error
}

// Error 实现 error。
func (e *SyncAbandonedError) Error() string {
	msg := "客户端已断开"
	if d := int(e.Waited.Seconds()); d > 0 {
		msg += fmt.Sprintf("（等待 %d 秒后）", d)
	}
	msg += "，平台已停止等待并全额释放额度"
	if strings.TrimSpace(e.TaskID) != "" {
		msg += "；上游任务 " + e.TaskID + " 可能仍在生成"
	}
	// 只在「明显不是用户自己取消」时补诊断：等了很久却远没到本端上限。
	//
	// 这个判定的价值在于把最常见的误判一次性纠正过来——
	// 同步图像动辄要 500 秒，而链路上任何一层用默认读超时（nginx / 云负载均衡 /
	// 内网穿透隧道，默认普遍是 60 秒）都会在这个位置断掉。原来记录里只有一句
	// 「客户端已断开」，运维会去查模型、查上游、查用户，唯独不会去查前置代理。
	if e.WaitLimit > 0 && e.Waited >= 20*time.Second && e.Waited*4 < e.WaitLimit*3 {
		msg += fmt.Sprintf("。【诊断】本端可等 %d 秒却在这个时长断开，通常是请求链路上的"+
			"代理/客户端超时（nginx proxy_read_timeout、云负载均衡空闲超时、内网穿透隧道，"+
			"默认常见 60 秒），不是平台主动放弃",
			int(e.WaitLimit.Seconds()))
	}
	return msg
}

// Unwrap 让 errors.Is(err, context.Canceled) 仍然成立。
func (e *SyncAbandonedError) Unwrap() error { return e.Err }

var (
	regMu    sync.RWMutex
	adapters = map[string]Adapter{}
	// builtinNames 记录**启动期**注册的适配器名（规格适配器是运行期惰性注册的）。
	//
	// 为什么必需：注册表同时装着"内置身份"和"运行期缓存"两件事，
	// 直接遍历它当内置清单会导致 —— 一份刚被解析过的规格出现在「内置适配器」卡片里，
	// 而一份**没被解析过**的规格又不在清单里（管理台看不到，渠道下拉也选不到）。
	builtinNames = map[string]bool{}
)

// RegisterAdapter 注册一个适配器（内置 init 与规格惰性解析共用）；同名重复注册返回错误，避免静默覆盖。
func RegisterAdapter(a Adapter) error {
	if a == nil {
		return errors.New("relay: 适配器不能为空")
	}
	name := a.Name()
	if name == "" {
		return errors.New("relay: 适配器名称不能为空")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, ok := adapters[name]; ok {
		return fmt.Errorf("%w: %s", ErrAdapterRegistered, name)
	}
	adapters[name] = a
	builtinNames[name] = true
	return nil
}

// Builtins 返回**随进程启动注册**的适配器（不含运行期惰性解析出来的 spec:*），按名称升序。
//
// 这是「内置适配器清单」的唯一来源：管理台、schema 目录、渠道下拉、AI 的 adapter_list
// 全部用它，不再各自遍历注册表。
func Builtins() []Adapter {
	regMu.RLock()
	out := make([]Adapter, 0, len(builtinNames))
	for name := range builtinNames {
		if a, ok := adapters[name]; ok {
			out = append(out, a)
		}
	}
	regMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// UnregisterAdapter 注销一个适配器（规格停用/删除时调用）；未注册时返回 false。
// 已注册的适配器实例可能仍被在途请求持有，调用方需保证先阻断新请求再注销。
func UnregisterAdapter(name string) bool {
	regMu.Lock()
	defer regMu.Unlock()
	if _, ok := adapters[name]; !ok {
		return false
	}
	delete(adapters, name)
	delete(builtinNames, name)
	return true
}

// GetAdapter 按名称取适配器；未命中时尝试惰性解析（多实例下规格在其它实例新建后免重启生效）。
func GetAdapter(name string) (Adapter, bool) {
	regMu.RLock()
	a, ok := adapters[name]
	regMu.RUnlock()
	if ok {
		return a, true
	}
	return resolveAdapterLazily(name)
}

// adapterResolver 由 main 注入（`service/spec` 提供）：避免 relay ↔ service 循环导入。
var (
	resolverMu sync.RWMutex
	resolver   func(string) (Adapter, error)

	resolveMu     sync.Mutex
	resolveFailMu sync.Mutex
	resolveFail   = map[string]resolveFailure{}
)

// resolveFailure 是一次解析失败（带原因）。
//
// 原因必须一起缓存，而不是只存时间戳：运行期唯一能看到「为什么这个渠道的适配器
// 不可用」的地方就是它 —— 只存时间戳的话，停用、拼错、规格内容非法三种情况
// 在台账里全部显示成「适配器未注册」，运营只能靠猜。
type resolveFailure struct {
	at  time.Time
	err error
}

// resolveFailTTL 是「解析失败」的负缓存时长：避免拼错的名字或已停用的规格在每次请求上都打一次库。
const resolveFailTTL = 5 * time.Second

// SetAdapterResolver 注入惰性解析函数（传 nil 关闭）。
//
// 返回 error 而不是 bool：适配器不可用分三种（不存在 / 已停用 / 内容非法），
// 三者的处置完全不同，而 bool 把它们压成了一句「未注册」。
func SetAdapterResolver(fn func(string) (Adapter, error)) {
	resolverMu.Lock()
	resolver = fn
	resolverMu.Unlock()
}

// ForgetAdapter 清掉某适配器的注册与负缓存，让下一次 GetAdapter 立即重新解析。
//
// 为什么必须有它：新建/更新/停用规格后，若只写库不通知本进程，`GetAdapter` 命中
// resolveFail 的 5s 负缓存或返回旧实例，运营在管理台保存渠道时会看到
// 「适配器不存在或未注册」并反复重试——明明刚建好。规格的 create/update/status/delete
// **必须在事务提交后**调用本函数（事务内调用会让回滚后的失败状态也生效）。
func ForgetAdapter(name string) {
	if name == "" {
		return
	}
	regMu.Lock()
	delete(adapters, name)
	regMu.Unlock()

	resolveFailMu.Lock()
	delete(resolveFail, name)
	resolveFailMu.Unlock()
}

// resolveAdapterLazily 在注册表未命中时问一次解析器，并把结果返回给调用方。
//
// 代价控制：命中负缓存直接失败（5s 内不重复打库）；resolveMu 保证同一时刻只有一个协程在解析，
// 避免「N 个并发请求同时发现插件没注册 → N 次 DB 查询」。
func resolveAdapterLazily(name string) (Adapter, bool) {
	if name == "" {
		return nil, false
	}
	resolverMu.RLock()
	fn := resolver
	resolverMu.RUnlock()
	if fn == nil {
		return nil, false
	}

	resolveFailMu.Lock()
	fail, failed := resolveFail[name]
	resolveFailMu.Unlock()
	if failed && time.Since(fail.at) < resolveFailTTL {
		return nil, false
	}

	resolveMu.Lock()
	defer resolveMu.Unlock()
	// 双检：等锁期间可能已被别的协程注册
	regMu.RLock()
	a, ok := adapters[name]
	regMu.RUnlock()
	if ok {
		return a, true
	}

	a, rerr := fn(name)
	if rerr != nil || a == nil {
		if rerr == nil {
			rerr = &AdapterResolveError{Kind: ResolveNotFound, Name: name}
		}
		resolveFailMu.Lock()
		resolveFail[name] = resolveFailure{at: time.Now(), err: rerr}
		resolveFailMu.Unlock()
		return nil, false
	}
	// 解析成功即入表：否则每个请求都要重新走一次解析器（规格场景是查库 + 反序列化 64KB JSON）。
	// 已在表里说明别的协程刚注册完，用现成的那个，保证「同名只有一个实例」。
	regMu.Lock()
	if existing, exists := adapters[name]; exists {
		a = existing
	} else {
		adapters[name] = a
	}
	regMu.Unlock()
	resolveFailMu.Lock()
	delete(resolveFail, name)
	resolveFailMu.Unlock()
	return a, true
}

// CanResolve 判断适配器名当前是否**真的**可用（保存渠道时的校验入口）。
//
// 与 GetAdapter 的差别只在失败侧：GetAdapter 会把失败写进 5s 负缓存（选路期要省查询），
// 而保存校验面对的是「运营刚建完规格、立刻保存渠道」的时序——负缓存会让它误判为不可用。
// 因此这里**不写负缓存**，成功时仍入表复用（避免后续每个请求都查库）。
func CanResolve(name string) bool {
	if name == "" {
		return false
	}
	regMu.RLock()
	_, ok := adapters[name]
	regMu.RUnlock()
	if ok {
		return true
	}
	resolverMu.RLock()
	fn := resolver
	resolverMu.RUnlock()
	if fn == nil {
		return false
	}
	a, rerr := fn(name)
	if rerr != nil || a == nil {
		return false
	}
	regMu.Lock()
	if _, exists := adapters[name]; !exists {
		adapters[name] = a
	}
	regMu.Unlock()
	return true
}

// 解析失败的三种原因。
const (
	ResolveNotFound = "not_found"
	ResolveDisabled = "disabled"
	ResolveInvalid  = "invalid"
)

// AdapterResolveError 说明某个适配器名为什么不可用。
type AdapterResolveError struct {
	Kind string
	Name string
	// Detail 是字段级原因（invalid 时带 endpoints.<k>.path 这类定位）。
	Detail string
}

func (e *AdapterResolveError) Error() string {
	switch e.Kind {
	case ResolveDisabled:
		return "规格 " + specSlugOf(e.Name) + " 已停用"
	case ResolveInvalid:
		if e.Detail != "" {
			return "规格 " + specSlugOf(e.Name) + " 内容不合法：" + e.Detail
		}
		return "规格 " + specSlugOf(e.Name) + " 内容不合法"
	default:
		return "适配器 " + e.Name + " 不存在"
	}
}

// specSlugOf 去掉 spec: 前缀（只用于文案）。
func specSlugOf(name string) string {
	return strings.TrimPrefix(name, "spec:")
}

// AdapterResolveReason 返回某适配器名当前的不可用原因（可用或从未尝试过时为 nil）。
//
// 与 GetAdapter 共用同一份负缓存：写失败原因和读原因必须是同一条记录，
// 否则「缓存里说失败、原因表里说没有」这种不一致会让排障结论反复横跳。
func AdapterResolveReason(name string) error {
	resolveFailMu.Lock()
	fail, ok := resolveFail[name]
	resolveFailMu.Unlock()
	if !ok || time.Since(fail.at) >= resolveFailTTL {
		return nil
	}
	return fail.err
}

// Adapters 返回已注册适配器快照（按名称升序）。
func Adapters() []Adapter {
	regMu.RLock()
	out := make([]Adapter, 0, len(adapters))
	for _, a := range adapters {
		out = append(out, a)
	}
	regMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
