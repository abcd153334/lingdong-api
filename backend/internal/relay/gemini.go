package relay

import (
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/wireformat"
	"lingdong-api/internal/relay/geminiconv"
)

// 本文件是 Gemini 协议面的**唯一入口**。
//
// 一条路由承接四个动作：gin 的 param 唯一终止符是 `/`，所以
// `POST /v1beta/models/:model` 里的 `:model` 会捕获 `gemini-2.0-flash:generateContent`
// —— 动作在参数里，handler 切出来再分派。这样不必为每个动作注册一条路由，
// 也就不会出现「新动作忘了注册 ⇒ 404 且错误体是 OpenAI 形状」。
//
// 遗留路径 `/v1/engines/:model/embeddings` 走同一个 handler（动作固定 embedContent）。

// geminiActions 是动作分派表（**表而不是 if 链**：加动作只加一行）。
var geminiActions = map[string]func(c *gin.Context, model string){
	"generateContent": func(c *gin.Context, model string) {
		relayRequestIn(c, kindGeminiGenerate, geminiconv.Codec{Model: model})
	},
	"streamGenerateContent": func(c *gin.Context, model string) {
		// 真实 SDK 一律带 ?alt=sse。不带就明确 400 并指出怎么改：
		// 静默回成 JSON 数组会让客户端按事件流解析、报一个与原因无关的错。
		if alt := strings.ToLower(strings.TrimSpace(c.Query("alt"))); alt != "sse" {
			gateway.FailDefMsg(c, errs.InvalidParam,
				"streamGenerateContent 需要 ?alt=sse：本平台只提供 SSE 形态的流式响应")
			return
		}
		relayRequestIn(c, kindGeminiGenerate, geminiconv.Codec{Model: model, Stream: true})
	},
	"embedContent": func(c *gin.Context, model string) {
		relayRequestIn(c, kindGeminiEmbedContent, geminiconv.Codec{Model: model, Embed: true})
	},
	// 词块计数：给错数字比不给更难查（客户端会按它管理上下文长度）。
	// 与 /v1/messages/count_tokens 同一口径：501 + 指路。
	"countTokens": func(c *gin.Context, model string) {
		gateway.FailDefMsg(c, errs.NotImplemented,
			"本平台不提供词块计数，请按本地估算管理上下文长度")
	},
	"batchEmbedContents": func(c *gin.Context, model string) {
		gateway.FailDefMsg(c, errs.NotImplemented,
			"批量向量化请对 embedContent 逐条调用（单条路径已完全支持）")
	},
}

// GeminiCall 处理 /v1beta/models/:model 与 /v1/engines/:model/embeddings。
func GeminiCall(c *gin.Context) {
	// 线格式写死为 Gemini：本 handler 的**唯一语义**就是 Gemini 协议，
	// 而它挂了两条路径（/v1beta/models/:model 与 /v1/models/:model），
	// 后者的契约前缀判定会落到 OpenAI（`/v1/models/` 是模型查询端点的前缀）。
	// 不显式设的话，错误体形状随路径而变 —— 同一个 handler 两种形状，
	// 而客户端只会觉得"有时候报错看不懂"。
	gateway.SetFormat(c, wireformat.Gemini)
	model, action := geminiTarget(c)
	if model == "" {
		gateway.FailDef(c, errs.NotFound.WithMessage("缺少模型名"))
		return
	}
	if action == "" {
		// 路径没有动作后缀（例如把 GET 用的 `/v1/models/defaults` 用 POST 打过来）。
		// 不单独处理的话会落到下面那句"不支持的 Gemini 动作「」"，一个引号里空空的
		// 错误信息 —— 用户看不出是"少了动作"还是"动作不支持"。
		gateway.FailDef(c, errs.NotFound.WithMessage(
			"缺少动作后缀：Gemini 的生成/向量化请写成 /v1beta/models/<模型>:generateContent 或 :embedContent"))
		return
	}
	handler, ok := geminiActions[action]
	if !ok {
		// 未知动作必须 404 且**说清支持什么**：Gemini SDK 的报错只有状态码，
		// 用户看到 404 时第一反应是路径拼错，而真实原因是动作名不支持。
		gateway.FailDef(c, errs.NotFound.WithMessage(
			"不支持的 Gemini 动作「"+action+"」；支持 generateContent / streamGenerateContent / embedContent"))
		return
	}
	handler(c, model)
}

// geminiTarget 从请求里切出 (model, action)。
//
// 两种路由形态：
//   - `/v1beta/models/:model` —— `:model` 形如 `gemini-2.0-flash:generateContent`；
//   - `/v1/engines/:model/embeddings` —— 遗留路径，动作恒为 embedContent。
//
// 切在**最后一个** `:` 上：模型 ID 里可能带冒号（部分渠道的版本号写法），
// 切第一个会把 model 截断成一个不存在的名字，报错却指向"模型不存在"。
func geminiTarget(c *gin.Context) (model, action string) {
	param := strings.TrimSpace(c.Param("model"))
	if strings.HasSuffix(c.FullPath(), "/engines/:model/embeddings") {
		return strings.TrimPrefix(param, "models/"), "embedContent"
	}
	i := strings.LastIndexByte(param, ':')
	if i <= 0 {
		return strings.TrimPrefix(param, "models/"), ""
	}
	return strings.TrimPrefix(param[:i], "models/"), param[i+1:]
}
