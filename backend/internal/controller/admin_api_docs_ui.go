package controller

// 本文件是 Swagger UI 的「标准工具视图」入口。
//
// 与自研三栏页的关系：**同一个 OpenAPI，两种渲染器**。
// 自研页给中文文案、可深链、可调试；这一页给"熟悉的工具"，
// 接入方可以直接 Try it out，也可以把地址发给同事。
//
// 安全设计（三条锁，缺一条都有真实后果）：
//
//  1. **路由不读任何 query 参数。** Swagger UI 默认顶栏有一个「Explore」输入框，
//     接受 `?url=...`；保留它等于开放一个 SSRF 入口（让服务端去拉任意文档）。
//     我们的 index.html 去掉了顶栏，spec 地址是编译期常量。
//
//  2. **spec 地址用相对路径 `../openapi.json`**（见 specRelPath）。
//     相对路径以当前页面地址为基准解析，因此在 nginx 子路径部署下依然指对；
//     绝对路径 `/api/admin/...` 会丢掉前缀直接 404。同时它不含 host，
//     从形状上就不可能指向外部站点。
//
//  3. **validatorUrl: null + persistAuthorization: false**（写在 index.html 里）。
//     前者防 Swagger UI 把 spec 发到 validator.swagger.io；
//     后者防把粘贴的密钥写进 localStorage。
//
// 鉴权：挂在 `/api/admin` 组下，走 AdminAuth 读 Cookie。
// 浏览器新窗口打开会带 Cookie，所以可用；代价是会话过期时新窗口显示一段
// JSON 信封（不是漂亮登录页），且每个静态资源各过一次 authenticate()
// —— 有 86400 缓存，所以只发生在首次打开。

import (
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/controller/swaggerui"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
)

// specRelPath 是 Swagger UI 拉取 spec 的地址。
//
// 相对 `.../api-docs/ui/` 解析为 `.../api-docs/openapi.json`。
// 写成常量而不是从请求路径派生：派生要读 `c.Request.URL.Path`，
// 而那是客户端可控输入，越少碰越好。
const specRelPath = "../openapi.json"

// AdminAPIDocsUIIndex 处理 GET /api/admin/api-docs/ui/：返回入口页。
func AdminAPIDocsUIIndex(c *gin.Context) {
	html, ok := swaggerui.IndexHTML(specRelPath)
	if !ok {
		response.FailDef(c, errs.Internal)
		return
	}
	// index.html **不缓存**：它内部嵌着 spec 地址，将来改地址时若被缓存，
	// 浏览器会一直拉旧路径。体积只有 3KB，不缓存没有成本。
	writeAdminDocsRaw(c, html, "text/html; charset=utf-8", 0)
}

// AdminAPIDocsUIAsset 处理 GET /api/admin/api-docs/ui/:asset：返回静态资源。
//
// 只注册 catch-all（gin 不允许同层「叶子 + catch-all」），因此访问 `/ui`
// 会被 gin 301 到 `/ui/`。前端链接一律带尾斜杠。
func AdminAPIDocsUIAsset(c *gin.Context) {
	// gin 的 catch-all 参数带前导斜杠（`/swagger-ui.css`）。
	name := strings.TrimPrefix(c.Param("asset"), "/")
	if name == "" {
		// `/ui/` 本身落到 catch-all：直接给入口页，避免多一次 301。
		AdminAPIDocsUIIndex(c)
		return
	}
	raw, mime, ok := swaggerui.Asset(name)
	if !ok {
		// 不区分"不存在"与"不合法"：这个路径下只有 4 个合法文件名，
		// 区分只会给探测者提供信息。
		response.FailDef(c, errs.NotFound)
		return
	}
	// 静态资源长缓存（一天）：文件名带版本语义（随二进制发布），
	// 二进制更新时 URL 不变但内容变了 —— 一天的可接受窗口换来的是
	// 首次打开后不再重复过 AdminAuth（每个资源一次 DB 查询）。
	writeAdminDocsRaw(c, raw, mime, 86400)
}
