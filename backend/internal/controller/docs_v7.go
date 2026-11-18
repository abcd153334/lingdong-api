package controller

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/docdocs"
)

// 对外文档站（v7）：目录 / 节点 / 接口。
//
// 三个 handler 都只做「加载 → 投影 → 下发」，不含任何渲染逻辑：
// 页面与 Markdown 必须共用 docdocs.Project*，否则同一字段表会出现两种措辞。

// DocsTree 返回侧栏目录（shape=7，板块 → 节点 → 接口）。
func DocsTree(c *gin.Context) {
	t, err := docdocs.LoadTree()
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	cat, err := docdocs.ProjectCatalog(t, siteurl.Base(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, cat)
}

// DocsNodeV7 返回节点头（成员、接口清单；说明页带正文块）。
func DocsNodeV7(c *gin.Context) {
	// LoadPublicNode：未发布、隐藏、以及**板块被隐藏**的节点一律 404。
	// 草稿只在管理台预览里出现；板块隐藏是「整块下线」，直接 URL 也不能绕过。
	b, err := docdocs.LoadPublicNode(c.Param("key"))
	if err != nil {
		response.NotFound(c)
		return
	}
	if b.Node.Status != "published" || b.Node.Hidden {
		response.NotFound(c)
		return
	}
	response.OK(c, docdocs.ProjectNode(b, siteurl.Base(c)))
}

// portalBase 是「站内链接」的基址：优先本次请求的 origin，其次回落门户地址。
//
// 为什么不用 siteurl.Base：它只认 API 地址（产物直链必须用它），
// 拿它拼 /docs 链接会把读者送到 API 域名下的 404。
func portalBase(c *gin.Context) string {
	if b := siteurl.FromRequest(c); b != "" {
		return b
	}
	if bases, _ := siteurl.DisplayBases(c); len(bases) > 0 {
		return bases[0]
	}
	return ""
}

// DocsEndpointV7 返回一篇接口文档的全文（唯一的正文入口）。
func DocsEndpointV7(c *gin.Context) {
	b, err := docdocs.LoadPublicEndpoint(c.Param("key"), c.Param("ep"))
	if err != nil {
		response.NotFound(c)
		return
	}
	// 可见性判据只有一处（docdocs 内部同一个函数）：页面、Markdown、侧栏同源。
	if b.Node.Status != "published" || b.Node.Hidden ||
		b.EP.Status != "published" || b.EP.Hidden {
		response.NotFound(c)
		return
	}
	response.OK(c, docdocs.ProjectEndpoint(b, siteurl.Base(c)))
}

// DocsEndpointMarkdown 导出**单篇**接口文档（页面上的「复制 Markdown」「下载 .md」）。
//
// 与页面同源：内容来自 `docdocs.RenderEndpointMarkdown`，它内部走的还是
// `ProjectEndpoint` —— 复制出来的字段顺序、示例、错误码与屏幕上逐字一致。
//
// 草稿/隐藏一律 404：导出能力不能成为绕过发布状态的读口。
func DocsEndpointMarkdown(c *gin.Context) {
	nodeKey, epKey := c.Param("key"), c.Param("ep")
	raw, err := docdocs.RenderEndpointMarkdown(nodeKey, epKey, siteurl.Base(c), portalBase(c))
	if err != nil {
		response.NotFound(c)
		return
	}
	body := []byte(raw)
	const ct = "text/markdown; charset=utf-8"
	c.Header("Content-Type", ct)
	// 文件名只由两个已校验的 key 拼成（`validKey` 只允许小写字母、数字与 -），
	// 仍然显式去掉可能的分隔符与引号，避免任何头注入的余地。
	name := safeFilename(nodeKey+"__"+epKey) + ".md"
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	if writeETag(c, body, 0, false, true) {
		return
	}
	c.Data(http.StatusOK, ct, body)
}

// safeFilename 把任意字符串收成文件名安全字符（只留字母数字与 - _ .）。
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// DocsMarkdown 导出全站 Markdown（游客可读）。
//
// 与页面的关系：**同一份投影**（docdocs.Project*），不是第二套渲染。
// 因此它天然只含已发布内容 —— 草稿在 `LoadTree` 那一步就被过滤掉了。
//
// 缓存用弱 ETag：文件几十到几百 KB，改一篇才变一次；
// 响应经过全局 gzip，强弱验证器不能混用（见 weakTag 的说明）。
func DocsMarkdown(c *gin.Context) {
	raw, err := docdocs.RenderMarkdown(siteurl.Base(c), portalBase(c))
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	body := []byte(raw)
	const ct = "text/markdown; charset=utf-8"
	c.Header("Content-Type", ct)
	// 空站不缓存：把「还没有任何已发布接口」缓存 5 分钟，运营刚补完也刷不出来。
	noStore := len(body) == 0 || !strings.Contains(raw, "](/docs/")
	if writeETag(c, body, 0, noStore, true) {
		return
	}
	c.Data(http.StatusOK, ct, body)
}
