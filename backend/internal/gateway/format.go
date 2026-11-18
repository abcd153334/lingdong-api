// Package gateway 是**对外协议面（/v1、/v1beta、/dashboard）的唯一响应出口**。
//
// 为什么必须有这一层：改造前 `/v1` 的响应由 7 处各自拼装 —— 每加一个端点就
// 再抄一遍信封与错误体。同一份协议形状散在 controller/relay/middleware 三处，
// 于是「同一个错误在两个端点给出不同的状态码与 code」这类漂移无法被发现：
// 单测直接调 handler，看不到 HTTP 层；e2e 打桩比真后端"更正确"。
//
// 本包只做三件事，且每件事都只有一处实现：
//  1. 按线格式写出**错误体**（OpenAI / Claude / 兼容封套）—— error.go + format.go；
//  2. 写出**成功体**：裸对象、兼容封套（`{code:"success"}`）、OpenAI Video 对象；
//  3. SSE 帧字节的构造（`data: ...\n\n`、`[DONE]`）。
//
// 依赖方向是叶子：errs + wireformat → gateway → controller/relay/middleware/router。
// 除本包外，任何文件都不许再调 `c.JSON` / `c.AbortWithStatusJSON` / `c.Data`
// 写协议面响应（由 controller 的 AST 门禁封死）。
package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/wireformat"
)

// ClientErrorBodyKey 是失败响应体的 gin context 键。
//
// 由报文留痕（relay 的 persistIO）在响应写完之后读取，落进 call_io.delivered_raw。
// 为什么走 context 而不是给每个错误出口都加参数：错误体在**这里**才最终成型，
// 在这里挂一次，所有出口自动覆盖。
const ClientErrorBodyKey = "client_error_body"

// clientErrorBodyMax 是留痕上限。错误体通常 < 1 KB；超过这个量级说明 message 异常长，
// 截断只影响留痕，**不影响**发给客户端的字节（发出去的是完整 raw）。
const clientErrorBodyMax = 16 << 10

// 线格式在 gin.Context 里的存放位置（请求级，不共享）。
const formatKey = "response_format"

// errorCodeKey 是本次响应的业务错误码（访问日志读取）。
const errorCodeKey = "error_code"

// SetFormat 标记本次请求的线格式（由 ingress 中间件在路由匹配后写入）。
//
// 必须在鉴权之前调用：401/403/429 也由本包写出，
// 中间件挂晚了这些响应就会退回 OpenAI 形状，Claude 客户端只能显示"未知错误"。
func SetFormat(c *gin.Context, f wireformat.Format) {
	if c == nil {
		return
	}
	c.Set(formatKey, wireformat.Normalize(string(f)))
}

// FormatOf 读取本次请求的线格式（未设置时按 OpenAI）。
func FormatOf(c *gin.Context) wireformat.Format {
	if c == nil {
		return wireformat.OpenAI
	}
	if v, ok := c.Get(formatKey); ok {
		if f, ok := v.(wireformat.Format); ok {
			return wireformat.Normalize(string(f))
		}
	}
	return wireformat.OpenAI
}

// FormatExplicit 读取本次请求的线格式，并报告它**是否已被中间件确定**。
//
// 与 FormatOf 的分工（两者不可互换）：
//   - FormatOf：用于「写协议面响应」，未设置时按 OpenAI —— 调用方已经确定这是协议面；
//   - FormatExplicit：用于「选择出口」——未设置说明这不是协议面（或中间件还没跑到），
//     调用方应退回控制面信封，而不是把 OpenAI 形状糊在一个 /api 请求上。
//
// 典型使用方是全局 Recover：它拿不到路由归属，只能靠这个判定写哪种错误体。
func FormatExplicit(c *gin.Context) (wireformat.Format, bool) {
	if c == nil {
		return wireformat.OpenAI, false
	}
	v, ok := c.Get(formatKey)
	if !ok {
		return wireformat.OpenAI, false
	}
	f, ok := v.(wireformat.Format)
	if !ok {
		return wireformat.OpenAI, false
	}
	return wireformat.Normalize(string(f)), true
}

// MarkErrorCode 把本次响应的错误码写入 gin.Context，供访问日志记录。
//
// 只写首个错误码：一个请求可能经多层包装（adapter → relay → controller），
// 后写的往往是「最终对外」的那个，但对外码才是用户看到的；用 c.Get 已存在则保留首个
// 更符合「根因优先」的排障习惯。
//
// 控制面信封（pkg/response）也调它 —— 两面共用一个键，日志才只有一份 error_code。
func MarkErrorCode(c *gin.Context, code string) {
	if c == nil || code == "" {
		return
	}
	if _, exists := c.Get(errorCodeKey); exists {
		return
	}
	c.Set(errorCodeKey, code)
}

// claudeErrorType 把 HTTP 状态映射为 Anthropic 的 error.type。
//
// 单表一处：形状错（字段名不对）客户端直接进不了自己的错误分支，
// 用户看到的是"未知错误"，连重试建议都给不出。
//
// code 参与判定只有一条理由：额度不足在协议面已统一成 403，而 Anthropic 把它
// 归在 api_error（403 的 permission_error 语义是"你没权限"，Claude Code 会去
// 检查工作区/权限配置，而不是提示充值）。
func claudeErrorType(status int, code string) string {
	switch code {
	case errs.CodeInsufficientBalance, errs.CodeRequestQuotaExceeded:
		return "api_error"
	}
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 504, 529:
		return "overloaded_error"
	default:
		// 402/422/500/502 等：Anthropic 把"余额不足"也归 api_error。
		return "api_error"
	}
}

// openAIType 把 HTTP 状态映射为 OpenAI error.type。
func openAIType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// geminiStatus 把 HTTP 状态 + 业务码映射为 Google 的 error.status 枚举。
//
// **顺序必须是「先按业务码、再按状态码」**：额度不足在协议面是 403，若先按状态码
// 判就会落到 PERMISSION_DENIED，Gemini SDK 会把它理解成"当前凭据无权访问该项目"
// 并引导用户去查 IAM —— 与 Claude 面把额度不足归 api_error 的教训同源。
//
// 未知 4xx 归 INVALID_ARGUMENT、未知 5xx 归 INTERNAL：Google 客户端按这两个值
// 决定"改请求"还是"重试"，给出一个不在枚举里的字符串会让它直接走 default 分支。
func geminiStatus(status int, code string) string {
	switch code {
	// 三个码都必须在：本函数收到的是**对外投影之后**的码（serializeWith 的入参），
	// 而额度不足在协议面已经被投影成 insufficient_user_quota。
	// 只写内部码的话这里永远不命中，403 会落成 PERMISSION_DENIED。
	case errs.CodeInsufficientBalance, errs.CodeRequestQuotaExceeded, "insufficient_user_quota":
		return "RESOURCE_EXHAUSTED"
	}
	switch status {
	case 400, 413, 422:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 503:
		return "UNAVAILABLE"
	case 504:
		return "DEADLINE_EXCEEDED"
	}
	if status >= 500 {
		return "INTERNAL"
	}
	return "INVALID_ARGUMENT"
}

// geminiBody 是 Google Generative Language 的错误体。
//
// 与 OpenAI 的两处硬差别：code 是**整数**（就是 HTTP 状态，不是业务码字符串）、
// 分类在 status（Google 的枚举）。把业务码塞进 code 会让 google-genai 的
// APIError.code 变成 "insufficient_user_quota"，SDK 里所有 `if code == 429` 的判断全部失效。
type geminiBody struct {
	Error geminiError `json:"error"`
}

type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// claudeBody 是 Anthropic 的错误体。
//
// 没有 code 字段（Anthropic 协议里 type 就是分类），所以内部的业务码只写访问日志。
type claudeBody struct {
	Type  string      `json:"type"`
	Error claudeError `json:"error"`
}

type claudeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ErrorBody 是 OpenAI 兼容错误体（对外字段名固定，改一个键就会打掉一片客户端）。
type ErrorBody struct {
	Error ErrorObject `json:"error"`
}

// ErrorObject 是错误体里的 error 对象。
type ErrorObject struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	// Param 指出「哪个参数错了」。我们目前不细分到字段（那要在每个校验点
	// 一路传上来），因此恒为空串而不是省略 —— OpenAI 与兼容协议的客户端
	// 都会读这个键，缺字段与空串在部分 SDK 里是两种分支。
	Param string `json:"param"`
	Code  string `json:"code"`
}

// NewAPIEnvelope 是兼容协议（new-api 风格）的信封：**成功与失败同一个形状**。
//
// 形状差异要注意：code 是**字符串**，成功时为 "success"，失败时是业务码；
// data 恒存在（失败为 null）。这与 OpenAI 的 `{error:{...}}` 不能混用 ——
// 那类客户端判断的是 `code != "success"`，给它 OpenAI 形状等于让它永远认为成功。
type NewAPIEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// sunoWire 把 (状态码) 投影成 SUNO 协议面的 (状态码, 码)。
//
// 这是 `/suno/*` **唯一**的码表（此前 controller 与 gateway 各有一份，两处的
// 边界不同：controller 那份把 413/405/422 全压成 400，gateway 那份又漏了它们，
// 于是同一个 413 在「谁先发现」不同时得到不同状态码）。
//
// 判据是状态码段而不是具体路由：整棵 /suno 树的码表由对端协议锁定（只有七个码），
// 而 /v1 的视频兼容面用的是 new-api 自己的词表（同一状态码下码不同），
// 所以不能按 `FormatOf == NewAPI` 一刀切。
//
// 为什么状态码也要一起投影：402（额度不足）与 403（没权限）在 SUNO 里共用一个码，
// HTTP 状态必须收敛到 403，否则同一语义会随「中间件判出 402 / controller 判出 403」
// 而漂移。5xx 例外：503 与 500 对客户端是两种动作（退避重试 vs 别重试），
// 必须保留原状态码 —— 之前统一回 500，等于把「模型没在售」说成"服务端 bug"。
func sunoWire(status int) (int, string) {
	switch {
	case status == 401:
		return 401, errs.CodeSunoAuth
	case status == 402, status == 403:
		return 403, errs.CodeSunoPermission
	case status == 404:
		return 404, errs.CodeSunoTaskNotFound
	case status == 409:
		return 409, errs.CodeSunoConflict
	case status == 429:
		return 429, errs.CodeSunoRateLimited
	case status >= 500:
		return status, errs.CodeSunoServerError
	case status >= 400:
		// 400 / 405 / 406 / 408 / 413 / 415 / 422 都归「请求不合法」，
		// 但**保留原状态码**：413 与 400 对调用方是两种不同的改法。
		return status, errs.CodeSunoInvalidRequest
	}
	return status, ""
}

// isSunoPath 报告本次请求是否落在 /suno 协议面。
func isSunoPath(c *gin.Context) bool {
	return c != nil && c.Request != nil && strings.HasPrefix(c.Request.URL.Path, "/suno/")
}

// serialize 按线格式写出错误体（**唯一**的错误写出口）。
//
// OpenAI 分支与改造前**逐字节一致**（字段顺序、omitempty 行为都不变）：
// 现有客户端与测试都依赖这个形状，顺手"优化"字段顺序会打掉一片断言，
// 而收益是零。
func serialize(c *gin.Context, status int, code, message string) {
	serializeWith(c, status, code, message, openAIType(status), false)
}

// serializeWith 是带 error.type 与 request-id 后缀的写出口。
//
// type 由 WireOf 决定（而非状态码单表）：鉴权家族要写 `new_api_error`，
// 其余按状态码派生。requestID 为真时 message 末尾追加 ` (request id: xxx)` ——
// 这是兼容协议里用户唯一能贴给客服的定位信息。
func serializeWith(c *gin.Context, status int, code, message, typ string, requestID bool) {
	// 码表也要按协议面投影，不只是信封形状。
	//
	// 中间件层的失败（鉴权、限速、跨域）不走 controller，它们带着内部码
	// （`invalid_api_key` / `rate_limited`）直接到这里；而 `/suno/*` 的客户端
	// 只有七个码可分流，其余一律落进 default 分支当成"未知错误"。
	// 真机实测：无效 Key 拿到 `invalid_api_key`，按 SUNO 协议写的客户端不会去换 Key。
	if isSunoPath(c) {
		if st, sc := sunoWire(status); sc != "" {
			status, code = st, sc
		}
	}
	if requestID {
		if id := RequestIDOf(c); id != "" {
			message += " (request id: " + id + ")"
		}
	}
	MarkErrorCode(c, code)
	// 「可能已计费、绝不能自动重试」的失败必须显式告诉客户端别重试：
	// 状态码（409/502）只能挡住一部分 SDK，`X-Should-Retry: false` 是
	// OpenAI/Anthropic 官方 SDK 都认的约定。
	if noRetryCodes[code] {
		c.Header("X-Should-Retry", "false")
	}
	var body any = ErrorBody{
		Error: ErrorObject{Message: message, Type: typ, Code: code},
	}
	switch FormatOf(c) {
	case wireformat.Claude:
		body = claudeBody{
			Type:  "error",
			Error: claudeError{Type: claudeErrorType(status, code), Message: message},
		}
	case wireformat.NewAPI:
		body = NewAPIEnvelope{Code: code, Message: message, Data: nil}
	case wireformat.Gemini:
		body = geminiBody{
			Error: geminiError{Code: status, Message: message, Status: geminiStatus(status, code)},
		}
	}
	// 自己 marshal 一次，而不是 c.AbortWithStatusJSON：后者在 gin 内部序列化，
	// 外面的报文留痕就拿不到「客户端到底收到了什么」。json.Marshal 与 gin 的 JSON
	// 渲染用的是同一个编码器，输出逐字节一致（无尾随换行）。
	raw, err := json.Marshal(body)
	if err != nil {
		// 理论不可达（body 全是字符串字段）；退回 gin 的写法，只是没有留痕。
		c.AbortWithStatusJSON(status, body)
		return
	}
	traced := raw
	if len(traced) > clientErrorBodyMax {
		traced = traced[:clientErrorBodyMax]
	}
	c.Set(ClientErrorBodyKey, traced)
	c.Abort()
	// HEAD 不写响应体：net/http 在真实服务器上会丢掉 body，但 Content-Length
	// 仍会按我们写出的长度下发 —— 于是探活方拿到"声明 N 字节、实际 0 字节"。
	// 在网关这一层直接判方法，比让每个 handler 各自记得"HEAD 时别写 body"可靠。
	if c.Request != nil && c.Request.Method == http.MethodHead {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Header("Content-Length", "0")
		c.Status(status)
		return
	}
	c.Data(status, "application/json; charset=utf-8", raw)
}
