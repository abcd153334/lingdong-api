package gateway

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
)

// Fail 按本次请求的线格式返回错误（协议面唯一错误出口）。
//
// 为什么保留「状态码 + 码 + 文案」这种底层签名：协议面有若干错误不属于
// errs 词表（例如上游报文异常、端点未实现），它们的码必须能逐字下发。
// 但**新代码应当优先用 FailDef**：Def 是登记过的码，文档与门禁都认它。
func Fail(c *gin.Context, status int, code, message string) {
	serialize(c, status, code, message)
}

// FailDef 按错误码定义返回错误（协议面的首选写法）。
//
// 状态码/业务码/type **一律走 WireOf**：调用方不许自己算「额度不足该给 402 还是
// 403」—— 那是这张投影表的事。
func FailDef(c *gin.Context, def errs.Def) {
	w := WireOf(def)
	// 先登记**内部码**（insufficient_balance），再写对外码（insufficient_user_quota）：
	// 访问日志与调用记录要的是稳定身份 —— 对外码会随协议靶子变，而分类、
	// 预检文案、文档码表都挂在内部码上。MarkErrorCode 只认首个，故顺序不能反。
	MarkErrorCode(c, def.Code)
	serializeWith(c, w.Status, w.Code, def.Message, w.Type, w.RequestID)
}

// FailDefMsg 与 FailDef 相同，但用 msg 覆盖文案（码与状态不变）。
//
// 用在"调用方能自己改对"的场景：码仍是可定位的那一个，文案里补上具体原因
// 与可选项。**不改码** — 自定义码会绕开 errs 登记，让文档与预检文案同时失效。
func FailDefMsg(c *gin.Context, def errs.Def, msg string) {
	w := WireOf(def)
	// 与 FailDef 同序：先登记内部码，再写对外码。
	// 漏这一步的后果是「日志里记的是投影后的码」：走 FailDefMsg 的失败
	// （上游超时 / 上游报错 / 服务不可用）在访问日志与预检记录里与
	// 「额度不足」这类被投影过的码混在一起，排障时无法按根因分类。
	MarkErrorCode(c, def.Code)
	serializeWith(c, w.Status, w.Code, msg, w.Type, w.RequestID)
}

// NotFoundRoute 是「这条路径本平台没有」的兜底（兼容协议语义）。
//
// 与「地址存在、能力没开」的分工必须清楚：
//   - NotFoundRoute（本函数）：地址不存在（写错了路径 / 别家的端点）。文案里带方法与路径，
//     客户端能立刻看出自己拼错了什么；
//   - 能力没开（灰度关闭 / 明确不提供）：走 FailDef(c, errs.NotImplemented)，
//     码是 not_implemented、状态 501，客户端据此知道「地址没错、能力没开」，
//     而不是改 base_url。
//
// 状态码用 404 而不是 501：501 会让 SDK 认为"服务端不支持该功能"而放弃，
// 而真实原因往往只是路径写错。**必须同时压 Cache-Control: no-store** ——
// 不压的话 CDN/浏览器会按启发式规则把 404 缓存下来，于是"端点刚上线"在一部分
// 用户那里仍然是 404，且清缓存才能恢复。
func NotFoundRoute(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	message := "Invalid URL"
	if c != nil && c.Request != nil {
		message = "Invalid URL (" + c.Request.Method + " " + c.Request.URL.Path + ")"
	}
	// type 用 invalid_request_error、code 留空：兼容协议在这条路径上就是这么写的，
	// 而客户端按 type 分流时不会把它当成服务端故障去重试。
	serializeWith(c, 404, "", message, "invalid_request_error", false)
}

// MethodNotAllowed 是「路径存在、方法不对」的 405。
//
// gin 默认不开 HandleMethodNotAllowed，方法错会落进 NoRoute —— 对一个存在的
// 端点发错方法会被答成「地址不存在」，把配置错误伪装成路径错误。
func MethodNotAllowed(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	message := "Method Not Allowed"
	if c != nil && c.Request != nil {
		message = "Method Not Allowed (" + c.Request.Method + " " + c.Request.URL.Path + ")"
	}
	serializeWith(c, 405, "", message, "invalid_request_error", false)
}
