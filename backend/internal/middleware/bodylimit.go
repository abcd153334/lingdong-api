package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/uploadcfg"
)

// 请求体上限分两档。
//
// 为什么必须分档：Ark 图片端点接受 **b64 参考图**，一张 10 MB 的图内联后
// 是一段 ≈13 MB 的 base64 字符串。原先全局 1 MiB 会在业务校验之前就把它拒掉，
// 用户看到的是 400「参数不合法」—— 而参数完全合法，只是我们自己的门太小。
//
//   - /v1/*：网关转发入口。**不看 Content-Type** 就限到上传配置推导出的
//     RequestCap（最大类别上限 + 表单开销）——否则把 Content-Type 写成 text/plain
//     就能探出门的真实位置。更严的 JSON 16 MiB 由 relay 按端点契约执行
//     （relay.readRequest）：门 ≥ 内层才不会把合法请求拦死在门外。
//   - portalBodyLimit（其余 JSON）：1 MiB。本项目的门户/管理台 JSON 入参
//     最大是「粘贴一段提示词」与结构化 spec 表单，1 MiB 已远超正常用量；
//     不设上限时一个未认证的 POST 就能让进程按 Content-Length 直接分配内存。
//
// 上传体积的唯一真相源是 `uploadcfg`：改造前这里写死 25 MiB，导致
// 「配置 50MB 只能收 25MB、门户却能收 50MB」——两道门互相打架，
// 且超限表现为读错误而不是 413。任何在这里重新写死数字的改动都是回归。
const (
	portalBodyLimit = 1 << 20
	// MaxRelayBodyBytes 是 /v1/* JSON 形态请求体的内层上限（relay.readRequest 执行）。
	MaxRelayBodyBytes = 16 << 20
)

// UploadBodyCap 是 multipart 端点（图片/音频/文件上传）的请求体总量上限。
//
// 值来自上传配置（带 3 秒缓存，见 uploadcfg.RequestCapCached）；单独导出是为了让
// relay 在丢弃超限 part 时复用同一个数，不再各写一个常量。
func UploadBodyCap() int64 { return uploadcfg.RequestCapCached() }

// BodyLimit 限制请求体大小。
//
// 三档，只有一处在写数字：
//  1. /v1/* 是网关转发入口，**不看 Content-Type** 就限到 UploadBodyCap
//     （见上方注释）；超过自身档位的请求仍会被 relay 按端点契约用 **413** 拒绝
//     （readRequest 翻译 MaxBytesError）。
//  2. 其余 application/json 限到 1 MiB（门户/管理台入参）。
//  3. 其余全部（multipart 上传、无 Content-Type、表单回调）限到 UploadBodyCap。
//     改造前这一档**完全不包** —— 把 Content-Type 写成 text/plain 就能绕开第 2 档，
//     一个未认证 POST 只要报个巨大的 Content-Length 就能让进程按需撑开内存。
//     取 UploadBodyCap 而不是另立一个小数：它是上传类端点的**外**边界
//     （各端点自己的 MaxBytesReader 更小），套在这一层不会把任何合法上传压小。
//
// 超限时读取会返回 *http.MaxBytesError，由 relay.readRequest 与各 handler 翻译成 **413**
// （不是 400）——「太大」和「写错了」是两件要用户做不同动作的事。
func BodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil {
			c.Next()
			return
		}
		limit := UploadBodyCap()
		if !strings.HasPrefix(c.Request.URL.Path, "/v1/") &&
			strings.Contains(c.GetHeader("Content-Type"), "application/json") {
			limit = portalBodyLimit
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		c.Next()
	}
}
