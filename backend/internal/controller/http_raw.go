package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/docdocs"
)

// 本文件是「大块原文」类响应的唯一出口：文档站的 Markdown / OpenAPI /
// 内嵌的 Swagger UI 静态资源都要么是纯文本、要么是几百 KB 的 HTML。
//
// 为什么统一用 ETag 而不是纯 no-store：这几类响应体大（Markdown 全量导出 100KB+），
// 每次打开都重传一遍纯属浪费；而它们的更新频率是「改一篇文档」，
// 用弱 ETag 让浏览器自己判断更划算。

// weakTag 算弱 ETag。
//
// **必须是弱验证器（`W/"..."`）**：响应会经过全局 gzip（带 `Vary: Accept-Encoding`），
// 同一个 ETag 对应两种传输表示（压缩/未压缩），强验证器在语义上不成立 ——
// 代理按强 ETag 做字节级缓存，遇到压缩与未压缩混用会返回乱码。
func weakTag(body []byte) string {
	sum := sha256.Sum256(body)
	return `W/"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagHit 判断 If-None-Match 是否命中（允许 `*` 与逗号列表，忽略 W/ 前缀）。
func etagHit(header, tag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	bare := strings.TrimPrefix(tag, "W/")
	for _, part := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(part), "W/") == bare {
			return true
		}
	}
	return false
}

// writeETag 写入 ETag；命中 `If-None-Match` 时回 304 并返回 true。
//
// maxAge 单位是秒；0 表示 `no-cache`（允许存但每次要回来问）。
// noStore 用于含账户数据的响应（`no-store`，代理与磁盘都不许留）。
// **公开接口的缓存语义在这里定**；管理台的原文响应走 writeAdminDocsRaw（private）。
func writeETag(c *gin.Context, body []byte, maxAge int, noStore, weak bool) bool {
	switch {
	case noStore:
		c.Header("Cache-Control", "no-store")
	case maxAge > 0:
		c.Header("Cache-Control", "public, max-age="+strconv.Itoa(maxAge))
	default:
		c.Header("Cache-Control", "no-cache")
	}
	return etagWrite(c, body, weak)
}

// etagWrite 只处理 ETag 与 304，不动 Cache-Control（调用方已经写好）。
func etagWrite(c *gin.Context, body []byte, weak bool) bool {
	tag := weakTag(body)
	if !weak {
		tag = strings.TrimPrefix(tag, "W/")
	}
	if inm := strings.TrimSpace(c.GetHeader("If-None-Match")); inm != "" && etagHit(inm, tag) {
		c.Header("ETag", tag)
		c.Status(http.StatusNotModified)
		return true
	}
	c.Header("ETag", tag)
	return false
}

// writeAdminDocsRaw 写一段管理台原文（OpenAPI / Markdown / 静态资源）。
//
// 缓存一律 `private`：这些内容在管理台鉴权之后（接口手册）或是随二进制发布的
// 内嵌资源，让共享代理缓存它们等于把管理台的文档缓存给下一个人。
// contentType 显式传入而不是靠探测：`http.DetectContentType` 会把 Markdown
// 判成 text/plain，浏览器就不再渲染了。
func writeAdminDocsRaw(c *gin.Context, raw []byte, contentType string, maxAge int) {
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, max-age="+strconv.Itoa(maxAge))
	if etagWrite(c, raw, true) {
		return
	}
	c.Data(http.StatusOK, contentType, raw)
}

// writeAdminDocsJSON 写一份**不经平台信封**的 JSON（管理台接口手册目录）。
//
// 与 response.OK 的区别：那个会包成 `{code,data,message}`，而这里的读者是
// 前端的表格与 OpenAPI 导入工具，包一层就要在两边各写一套解包。
func writeAdminDocsJSON(c *gin.Context, payload any, maxAge int) {
	raw, err := response.Encode(payload)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	writeAdminDocsRaw(c, raw, "application/json; charset=utf-8", maxAge)
}

// isUserInputErr 判断错误是不是「内容不合法」（400）而不是服务端故障（500）。
func isUserInputErr(err error) bool {
	var in *docdocs.InputError
	return errors.As(err, &in)
}
