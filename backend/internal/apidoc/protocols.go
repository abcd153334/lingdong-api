package apidoc

import (
	"strings"
)

// 本文件是「协议文档」的**唯一声明处**：语言模型（对话 / 补全 / 向量 / 审核）
// 不再按模型建族，而是固定几篇讲协议的文档。
//
// 为什么必须固定：这些端点的请求/响应完全由协议决定（OpenAI 官方 / Anthropic
// Messages），上一个模型不该多一篇、下一个模型也不该少一节。早先把它们做成
// 「模型族」的后果是：所有对话模型挤进一个叫 openai 的单例组，而 Claude 原生
// 那条 `/v1/messages` 因为 `EndpointKey` 为空，**连章节都进不去** —— 平台真实
// 支持的第二套协议在文档站里根本不存在。
//
// 与模型的关系只有一处：每一节末尾列「本节可用模型」（`EndpointSection.Models`），
// 清单由模型事实派生，篇数恒定。

// ProtocolDoc 是一篇协议文档的静态定义（不含任何落库内容）。
type ProtocolDoc struct {
	// Key 是 URL 段（`/docs/protocols/<key>`），也是切换时重置表单的身份。
	Key string
	// Title / Summary 用于侧栏与搜索结果；Summary 一句话说清"这协议能干什么"。
	Title   string
	Summary string
	Intro   string
	// Highlights / Limitations / Notes 是协议级说明（认证、流式、错误体形状）。
	Highlights  []string
	Limitations []string
	Notes       []string
	// Endpoints 是本文档的章节（顺序即页内顺序）。
	//
	// 只放**平台真实挂载**的端点：没有 `/v1beta`，也没有恒返回 501 的
	// `/v1/messages/count_tokens` —— 写进去等于承诺一个做不到的能力。
	Endpoints []DocumentKey
}

// ProtocolKey 是协议文档的标识（唯一取值集合见 ProtocolDocs）。
const (
	ProtocolOpenAI     = "openai"
	ProtocolClaude     = "claude"
	ProtocolEmbeddings = "embeddings"
	// ProtocolGemini 是 Google Generative Language 协议（/v1beta）。
	ProtocolGemini = "gemini"
)

// protocolDocs 是固定 4 篇（声明顺序 = 侧栏顺序）。
var protocolDocs = []ProtocolDoc{
	{
		Key:     ProtocolOpenAI,
		Title:   "OpenAI 兼容",
		Summary: "对话、文本补全、智能体接口与内容审核，按 OpenAI 官方协议收发。",
		Intro: "请求与响应遵循 OpenAI 官方协议，官方 SDK 只需把 base_url 改到本站、api_key 换成站内密钥。 " +
			"同一批对话模型也能用 Anthropic 协议调用，见「Claude 原生」—— 两种协议指向同一套模型与计费。",
		Highlights: []string{
			"官方 SDK 可直接接入：只改 base_url 与 api_key。",
			"流式响应用 SSE，按 `data: [DONE]` 结束。",
			"工具调用走 `tools` / `tool_choice`，回包里的 `tool_calls` 需要回传。",
		},
		Limitations: []string{
			"厂商私有字段（如即梦的图层拆分）不在本页，见对应模型文档。",
			"不同模型支持的取值不同（档位、时长、工具能力），以每节的模型清单为准。",
		},
		Endpoints: []DocumentKey{
			{Kind: KindChatCompletions},
			{Kind: KindCompletions},
			{Kind: KindResponses},
			{Kind: KindModerations},
		},
		Notes: []string{
			"鉴权：请求头 `Authorization: Bearer <API KEY>`。",
			"错误体与 OpenAI 一致：`{\"error\":{\"message\",\"type\",\"code\"}}`，`code` 用于程序判断。",
		},
	},
	{
		Key:     ProtocolClaude,
		Title:   "Claude 原生",
		Summary: "Anthropic Messages 协议，Claude 官方 SDK 与客户端可直接接入。",
		Intro: "按 Anthropic Messages 协议收发：请求体用 `system` / `messages` / `max_tokens`， " +
			"响应是 `{type:\"message\", content:[...]}`，流式按事件名（`message_start` / `content_block_delta` / `message_stop`）。 " +
			"它与「OpenAI 兼容」是同一个能力的两种线格式，模型、计费、限额完全一致。",
		Highlights: []string{
			"Claude 官方 SDK 可直接接入：只改 base_url 与 api_key。",
			"流式以 `message_stop` 事件结束（没有 `[DONE]`）。",
			"`max_tokens` 在本协议下**必填**，缺失或为 0 返回 400。",
		},
		Limitations: []string{
			"本平台不实现 `top_k`：传了既不改变结果也不报错。",
			"`/v1/messages/count_tokens` 不提供（返回 501），请按实际请求后的 usage 计。",
		},
		Endpoints: []DocumentKey{
			{Kind: KindMessages},
		},
		Notes: []string{
			"鉴权：`x-api-key: <API KEY>`（也接受 `Authorization: Bearer <API KEY>`）。",
			"错误体按 Anthropic 形状返回：`{\"type\":\"error\",\"error\":{\"type\",\"message\"}}`。",
		},
	},
	{
		Key:     ProtocolGemini,
		Title:   "Gemini 兼容",
		Summary: "Google Generative Language 协议：模型列表、内容生成与向量化，google-genai 可直接接入。",
		Intro: "按 Gemini 协议收发：请求是 `{contents:[{role,parts}]}`、响应是 `{candidates:[{content:{parts}}]}`， " +
			"路径前缀为 `/v1beta`。它与「OpenAI 兼容」是同一个能力的两种线格式，模型、计费、限额完全一致 —— " +
			"同一个对话模型既可以用 `/v1/chat/completions`，也可以用 `/v1beta/models/<model>:generateContent`。",
		Highlights: []string{
			"google-genai / LangChain-ChatGoogle 可直接接入：只改 base_url 与 api_key。",
			"模型名既可以写 `gemini-x`，也可以写 `models/gemini-x`。",
			"流式要求 `?alt=sse`，以末帧的 `finishReason` 结束（没有 `[DONE]`）。",
		},
		Limitations: []string{
			"只支持文本生成：`responseModalities` 写图片会被 400（图片请用 `/v1/images/generations`）。",
			"`cachedContent` 返回 400：平台不支持上下文缓存引用，静默忽略会按全价收费。",
			"`topK` / `safetySettings` 不生效：传了既不改变结果也不报错。",
			"`thinkingConfig.thinkingBudget` 会被离散成四档，精度会丢失。",
			"`countTokens` / `batchEmbedContents` 不提供（返回 501）。",
			"`/v1beta/openai/*` 是 Gemini 侧的 OpenAI 兼容入口，它保持 **OpenAI 错误体**。",
		},
		Endpoints: []DocumentKey{
			{Kind: KindGeminiModels},
			{Kind: KindGeminiGenerate},
			{Kind: KindGeminiEmbedContent},
		},
		Notes: []string{
			"鉴权：`x-goog-api-key: <API KEY>`（也接受 `Authorization: Bearer <API KEY>` 与 `?key=`）。",
			"错误体按 Gemini 形状返回：`{\"error\":{\"code\":400,\"message\":\"…\",\"status\":\"INVALID_ARGUMENT\"}}`。",
		},
	},
	{
		Key:     ProtocolEmbeddings,
		Title:   "Embeddings",
		Summary: "文本向量化与相关性重排。",
		Intro: "把文本转成向量，或对候选集做相关性重排。两节分别对应向量化与重排， " +
			"模型清单各自独立 —— 能向量化的模型不一定能做重排。",
		Highlights: []string{
			"向量接口与 OpenAI 协议一致，返回 `data[].embedding`。",
			"`dimensions` 只在模型支持时生效，超出范围返回 400。",
		},
		Limitations: []string{
			"不同模型的向量维度不同，换模型等于换向量空间，不能混用历史向量。",
		},
		Endpoints: []DocumentKey{
			{Kind: KindEmbeddings},
			{Kind: KindRerank},
		},
		Notes: []string{
			"鉴权：请求头 `Authorization: Bearer <API KEY>`。",
		},
	},
}

// ProtocolDocs 返回全部协议文档（顺序即侧栏顺序）。
func ProtocolDocs() []ProtocolDoc {
	out := make([]ProtocolDoc, len(protocolDocs))
	copy(out, protocolDocs)
	return out
}

// ProtocolDocOf 按 key 取协议文档。
func ProtocolDocOf(key string) (ProtocolDoc, bool) {
	for _, p := range protocolDocs {
		if p.Key == key {
			return p, true
		}
	}
	return ProtocolDoc{}, false
}

// AppliesDoc 判断一个模型是否出现在某端点的文档里（**唯一判据**）。
//
// 三条规则对应三种端点形态，缺一条就会造出"文档说能用、实际跑不通"：
//  1. `messages`：`EndpointKey` 为空（它是 chat 的另一种线格式），模型声明不了它
//     —— 只能按模态判定，否则 Claude 篇的模型清单永远是空的；
//  2. 读面（查任务 / 取产物 / 取消）：没有 `models.endpoints` 声明，判据是能否异步；
//  3. 其余端点：声明即能力（`models.endpoints`）。
func AppliesDoc(f ModelFacts, k Kind) bool {
	if k == KindMessages {
		return NormalizeModality(f.Modality) == "chat"
	}
	if kindIsRead(k) {
		return HasTaskFace(f)
	}
	return hasKind(f, k)
}

// 协议文档只保留**契约本体**（ProtocolDoc 与「哪些模型适用」）。
//
// v6 的 ProtocolProjection / ProjectProtocol / ProtocolChapters 已删除：
// 那套投影把「页内章节 + 每节模型清单」算在代码里，而 v7 的同一件事
// 由 `docdocs.ProjectNode` 从库里读节点与接口拼出来 —— 页面结构只有一个出口。

// ProtocolSlugOf 是协议文档的 URL 段（与 Key 相同，独立函数是为了让调用方
// 不必知道 Key 就是 slug —— 将来若要加前缀，只改这里）。
func ProtocolSlugOf(key string) string { return strings.TrimSpace(key) }
