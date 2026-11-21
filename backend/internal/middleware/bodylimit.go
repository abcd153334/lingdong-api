package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/uploadcfg"
)

// 请求体上限分两档，**按路径**判定（不看 Content-Type）。
//
// 为什么不能看 Content-Type：旧实现是「路径不以 /v1/ 开头 **且** Content-Type 含
// application/json → 1 MiB，否则 → 上传总上限（默认 201 MiB）」。于是把
// Content-Type 写成 text/plain 就能让一个**未认证**的 /api/portal/auth/login
// 收到 201 MiB —— 一个请求头就把整档防护关掉了。
//
// 现在的判据反向：默认小档，只有明确登记过的大体积入口才放开。
//   - 上传类入口（/v1、/v1beta、/suno 全前缀 + 三个 /api multipart 端点）：
//     UploadBodyCap（上传配置推导的 RequestCap，默认 201 MiB）。
//     /v1 是网关转发入口，Ark 图片端点接受 b64 参考图（10 MB 图内联后 ≈13 MB），
//     按 1 MiB 收会在业务校验前就拒掉合法请求。
//   - 其余全部（门户/管理台 JSON、登录注册、支付回调、未匹配路由）：1 MiB。
//     本项目门户 JSON 入参最大是「粘贴一段提示词」与结构化 spec 表单，1 MiB 远超
//     正常用量；不设上限时一个未认证 POST 只要报个巨大的 Content-Length
//     就能让进程按需撑开内存。
//
// 上传体积的唯一真相源仍是 `uploadcfg`：任何在这里重新写死数字的改动都是回归。
// **新增大体积上传路由必须登记到 uploadExactPaths**，否则会被 1 MiB 卡住。
const (
	portalBodyLimit = 1 << 20
	// MaxRelayBodyBytes 是 /v1/* JSON 形态请求体的内层上限（relay.readRequest 执行）。
	MaxRelayBodyBytes = 16 << 20
)

// uploadPrefixes 是大体积网关入口的路径前缀。
//
// /v1beta 是 Gemini 协议面，/suno 是音乐任务面 —— 两者都不以 /v1/ 开头，
// 漏登记就会被 1 MiB 卡住（而它们今天本来就是大档）。
var uploadPrefixes = []string{"/v1/", "/v1beta/", "/suno/"}

// uploadExactPaths 是挂在 /api 下的 multipart 上传入口（精确匹配）。
//
// 已枚举全部 FormFile/ParseMultipartForm：file.go(/v1/files)、portal_file.go、
// avatar.go、icon.go、video.go(/v1/video/*)、relay multipart(/v1/images/edits、/v1/audio/*)、
// suno.go(/suno/*) —— 除这三条外都在前缀档里。
var uploadExactPaths = []string{
	"/api/portal/files",
	"/api/portal/me/avatar",
	"/api/admin/icons",
}

// UploadBodyCap 是 multipart 端点（图片/音频/文件上传）的请求体总量上限。
//
// 值来自上传配置（带 3 秒缓存，见 uploadcfg.RequestCapCached）；单独导出是为了让
// relay 在丢弃超限 part 时复用同一个数，不再各写一个常量。
func UploadBodyCap() int64 { return uploadcfg.RequestCapCached() }

// bodyLimitFor 返回该路径的请求体上限。
func bodyLimitFor(path string) int64 {
	for _, p := range uploadPrefixes {
		if strings.HasPrefix(path, p) {
			return UploadBodyCap()
		}
	}
	for _, p := range uploadExactPaths {
		if path == p {
			return UploadBodyCap()
		}
	}
	return portalBodyLimit
}

// BodyLimit 限制请求体大小（见文件头说明）。
//
// 超限时读取返回 *http.MaxBytesError：relay.readRequest 与各 handler 把它翻译成
// **413**（不是 400）——「太大」和「写错了」是两件要用户做不同动作的事。
func BodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil {
			c.Next()
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyLimitFor(c.Request.URL.Path))
		c.Next()
	}
}
