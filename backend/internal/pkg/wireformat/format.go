// Package wireformat 是「线格式」词表的唯一处。
//
// 线格式描述请求/响应**长什么样**（字段名、流式事件、错误体形状），
// 不描述能力：能力仍由 apidoc.EndpointKey + models.endpoints 表达。
//
// 为什么需要一个独立叶子包：apidoc（契约）、response（错误体）、middleware（注入）、
// relay（转换判定）四处都要判断"这次说的是哪种协议"。各自写字面量字符串的结果是
// 改一处漏一处 —— 例如错误体按 claude 分支了、转换却没生效，客户端拿到一个
// 形状对但内容错的响应，比直接报错更难查。
//
// 本包零依赖（不 import 任何 internal 包），因此可以被上述任何一层引用而不成环。
package wireformat

// Format 是线格式标识。
type Format string

const (
	// OpenAI 是默认线格式：请求 {model,messages,...}、响应 {choices}、错误 {error:{...}}。
	// 空串归一为它 —— 绝大多数端点从来不需要显式声明。
	OpenAI Format = "openai"
	// Claude 是 Anthropic Messages 协议：请求 {model,system,messages,max_tokens}、
	// 响应 {type:"message",content:[...]}、流式带 event: 行、错误 {type:"error",error:{...}}。
	Claude Format = "claude"
	// NewAPI 是 new-api 的任务封套：成功 `{code:"success",message:"",data:{...}}`、
	// 失败 `{code:"<err>",message,data:null}`。只用于任务端点（视频查询壳），
	// 不参与请求体转换 —— 它与 OpenAI 的差别只在**响应信封**。
	NewAPI Format = "newapi"
	// Gemini 是 Google Generative Language 协议：请求 `{contents:[{role,parts}]}`、
	// 响应 `{candidates:[{content:{parts}}]}`、错误 `{error:{code,message,status}}`。
	// 它的错误体里 code 是**整数 HTTP 状态**、status 是 Google 的字符串枚举，
	// 与 OpenAI 的 `{error:{message,type,code}}` 完全不同的形状。
	Gemini Format = "gemini"
)

// Normalize 把外部字符串归一为 Format（未知值按 OpenAI 处理）。
//
// 未知值不报错而是回落：这个值只影响响应形状与转换判定，
// 让它把一次正常请求打成 500 得不偿失；「未知」本身由契约表在编译期保证不会出现。
func Normalize(s string) Format {
	switch Format(s) {
	case Claude:
		return Claude
	case NewAPI:
		return NewAPI
	case Gemini:
		return Gemini
	default:
		return OpenAI
	}
}

// IsClaude 是 Normalize 的便捷判定。
func IsClaude(s string) bool { return Normalize(s) == Claude }
