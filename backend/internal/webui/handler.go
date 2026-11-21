// Package webui 把前端构建产物内嵌进二进制，让**一个进程**同时提供页面与 API。
//
// 为什么要有这一层（v2.2.0）：此前前端由独立的 nginx 容器提供，部署面因此长成
// 「4 个容器 + 一份 169 行的 nginx 站点配置 + 三层 X-Forwarded-* 链路」。
// 那一层的每一条规则都对应过一次真机故障：
//   - `X-Forwarded-Host $host` 丢掉端口 → CSRF 完整 origin 比对必然不等 → 登录 403；
//   - `expires` 与 `add_header Cache-Control` 并存 → 双份头，且 location 内的
//     `add_header` 会中断 server 级继承，`nosniff` 悄悄消失；
//   - gzip 从未打开，1.79MB 的主 chunk 一直裸传。
//
// 这些都是「静态资源与 API 被拆成两个进程」的副产物。合并之后它们从结构上不存在：
// 同源、同一个端口、没有可配错的转发头。
//
// 与 new-api 的差别：它把 `web/dist` 提交进仓库，我们用入库的空文件 `dist/.keep`
// 保证纯净 clone 下 `go build ./...` / `go test ./...` 永远能通过（embeded 目录为空
// 时编译依然成功），构建镜像时才把真正的产物拷进来。运行时若没有 index.html，
// 返回 503 并说明原因，而不是让用户看到一片白屏。
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/response"
)

// dist 是构建期由 deploy/backend.Dockerfile 从 web/dist 拷进来的目录。
// `all:` 前缀让下划线/点开头以外的文件一并包含（Vite 产物里有 .vite 元数据）。
//
//go:embed all:dist
var dist embed.FS

// distRoot 必须与 go:embed 的路径一致，改一处就必须改另一处。
const distRoot = "dist"

// indexName 是 SPA 的入口文件，也是「这个二进制到底有没有内嵌前端」的唯一判据。
const indexName = "index.html"

// noFallbackPrefix 是不允许回落到 index.html 的路径前缀。
//
// 少了这道闸的表现比 404 更难查：`/api/...` 打错一个字母会返回 200 + HTML，
// 客户端按 JSON 解析失败，报的是「响应格式错误」而不是「地址不存在」；
// `/assets/xxx.js` 缺失若回落成 index.html，浏览器会因为 MIME 是 text/html 拒绝执行，
// 控制台只有一句 "Expected a JavaScript module"。
var noFallbackPrefix = []string{"/api/", "/v1/", "/v1beta/", "/suno/", "/f/", "/metrics"}

// entry 是一个已加载的静态资源。
type entry struct {
	body        []byte
	contentType string
}

// server 是静态资源的内存索引：启动时一次性读完，请求期不再碰 embed.FS。
//
// 前端产物总大小在几 MB 量级，常驻内存远比每请求拷一份 1.79MB 的 chunk 划算。
type server struct {
	files map[string]entry
	index *entry
}

// Handler 返回可直接挂在路由末端的处理器。
func Handler() gin.HandlerFunc {
	return newServer(dist).handle
}

// newServer 从 FS 建索引。FS 为空（只剩 .keep）是合法状态：index 为 nil，
// 运行时返回 503 并给出成因，编译与单测都不受影响。
func newServer(fsys fs.FS) *server {
	s := &server{files: map[string]entry{}}
	root, err := fs.Sub(fsys, distRoot)
	if err != nil {
		return s
	}
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		body, rerr := fs.ReadFile(root, p)
		if rerr != nil {
			return nil
		}
		// 名字统一成以 / 开头的 URL 形式，查表时不必再做两套归一。
		name := "/" + strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
		e := entry{body: body, contentType: contentTypeOf(p, body)}
		s.files[name] = e
		if name == "/"+indexName {
			copied := e
			s.index = &copied
		}
		return nil
	})
	return s
}

// contentTypeOf 先按扩展名，再按字节嗅探。
//
// 扩展名优先是刻意的：`http.DetectContentType` 对 .js 会给出 text/plain，
// 浏览器在 `nosniff` 下会拒绝执行 —— 那是最容易被误判成「前端构建坏了」的一种故障。
func contentTypeOf(name string, body []byte) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return http.DetectContentType(body)
}

// handle 处理的顺序：先试真实文件，再决定是 404 还是回落 index.html。
func (s *server) handle(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		// 非 GET/HEAD 落到这里的都是未注册的写请求，交给统一信封。
		response.NotFound(c)
		return
	}
	name := cleanPath(c.Request.URL.Path)

	if e, ok := s.files[name]; ok {
		writeAsset(c, name, e)
		return
	}
	// 目录形式的 URL（Vite 不会产出，但用户可能手敲 /assets/）落到 index 之前先判前缀，
	// 否则 /assets/ 下任何拼错都会被答成整页 HTML。
	if isNoFallback(name) || strings.HasPrefix(name, "/assets/") {
		response.NotFound(c)
		return
	}
	if s.index == nil {
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusServiceUnavailable, "text/plain; charset=utf-8",
			[]byte("前端资源未内嵌：镜像构建时应由 deploy/backend.Dockerfile 拷入 web/dist。\n"+
				"本地开发请用 `pnpm dev`（Vite 自带代理），无需内嵌。\n"))
		return
	}
	writeAsset(c, "/"+indexName, *s.index)
}

// isNoFallback 判定是否禁止 SPA 回落。
//
// 用 `前缀 + "/"` 匹配而不是裸前缀：`/apiary` 这种真实存在的页面不该被误判成 API。
// 另外 `/api` 与 `/v1` 本身（无尾斜杠）也要拦，它们同样不是页面。
func isNoFallback(name string) bool {
	for _, p := range noFallbackPrefix {
		if name+"/" == p || strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// cleanPath 归一化请求路径，顺带吃掉 `..`：
// 静态资源来自内存表，路径穿越本身读不到文件，但归一后仍然查不到才是正确语义
// （返回 404，而不是被回落成首页）。
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

// writeAsset 写响应。缓存策略按「内容是否会变」分三档：
//   - /assets/*：文件名带内容哈希，内容变了名字就变了 → 一年 + immutable，省掉全部条件请求；
//   - index.html：**必须**每次回源（no-cache），否则发版后用户仍加载旧的 chunk 引用；
//   - 其余（favicon、robots.txt 等）：一小时，够短。
//
// 压缩交给全局 middleware.Gzip()（GzipSmart 按 Content-Type/长度惰性决定），
// 这里只管把正确的 Content-Type 交出去 —— 重复实现一套压缩只会多一处会漂的配置。
func writeAsset(c *gin.Context, name string, e entry) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Frame-Options", "SAMEORIGIN")
	c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
	c.Header("Cache-Control", cacheControlOf(name))
	c.Data(http.StatusOK, e.contentType, e.body)
}

func cacheControlOf(name string) string {
	switch {
	case strings.HasPrefix(name, "/assets/"):
		return "public, max-age=31536000, immutable"
	case name == "/"+indexName:
		return "no-cache"
	default:
		return "public, max-age=3600"
	}
}
