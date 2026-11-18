// Package swaggerui 内嵌一份 Swagger UI 静态资源，作为管理台接口文档的
// 「标准工具视图」。
//
// 为什么走 go:embed 而不是 npm 依赖：
//
//	`swagger-ui-react` 与 React 19 的 peer 依赖冲突，且会把 1.2MB 打进
//	已有的 1.55MB 主包（管理台是低频入口，为它牺牲首屏不值得）。
//	内嵌静态资源零 npm 依赖、不进 React 包、可以独立缓存。
//
// 代价要认：**1.9MB 二进制进仓库**（仓库体积、diff 噪音、审计）。
// 用 `Version` 常量 + 随包的 LICENSE/NOTICE + 一条断言 bundle 含版本号的测试
// 来回答"这份 bundle 哪来的"。退路（build tag + stub）更糟：离线构建会
// **静默**没有 Swagger UI，而没人会发现。
//
// 只提交 5 个文件，不是整包 32 个：
//
//	swagger-ui.css          187KB  样式
//	swagger-ui-bundle.js   1.68MB  渲染器（含 React 运行时）
//	LICENSE / NOTICE               Apache-2.0 合规
//	index.html                    自写（原版依赖 standalone-preset 且带 Explore 输入框）
//
// 砍掉 swagger-ui-standalone-preset.js（264KB）：它只给「模型定义」独立面板用，
// 而 BaseLayout 不需要。少一个文件就少一次 AdminAuth 鉴权（每个静态资源各过一次）。
package swaggerui

import (
	"embed"
	"io/fs"
	"strings"
)

// Version 内嵌资源的版本。
//
// 改 assets 时必须同步改它：`TestBundleMatchesVersion` 会断言 bundle 里
// 含有这个版本号 —— 否则"提交了一份来源不明的 bundle"永远无法被自动发现。
const Version = "swagger-ui-dist@5.34.0"

// specPlaceholder 是 index.html 里等待替换的占位符。
//
// 用占位符而不是让前端从 location 拼地址：拼地址就要读 query，而读 query
// 就是把 `?url=` 变成一个可控输入。
const specPlaceholder = "%%SPEC_URL%%"

// assets 是内嵌的静态资源目录。
//
//go:embed assets
var assets embed.FS

// FS 返回 assets 子目录（去掉 "assets/" 前缀，文件名即 URL 段）。
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// 只可能在 embed 指令写错时发生，属于编译期错误的运行期表现。
		panic("swaggerui: embed 目录缺失: " + err.Error())
	}
	return sub
}

// IndexHTML 渲染入口页，把 spec 地址注入进去。
//
// specPath 必须是**服务端派生**的地址（见 controller 的 specPathFrom），
// 不能来自任何请求参数。这里额外做一次白名单校验：即使调用方传错，
// 也不会把一段 javascript: 或外部 URL 塞进 HTML。
func IndexHTML(specPath string) ([]byte, bool) {
	if !ValidSpecPath(specPath) {
		return nil, false
	}
	raw, err := assets.ReadFile("assets/index.html")
	if err != nil {
		return nil, false
	}
	return []byte(strings.ReplaceAll(string(raw), specPlaceholder, specPath)), true
}

// ValidSpecPath 校验 spec 地址形态：站内绝对路径，或以 `../` 开头的相对路径。
//
// 相对路径是**首选**（见 controller 里的 specRelPath）：它以当前页面地址为基准
// 解析，因此在 nginx 子路径部署（`/lingdong/api/...`）下依然指对，
// 而绝对路径 `/api/admin/...` 会丢掉前缀直接 404。
//
// 拒绝 `//evil.com/x`（协议相对 URL，浏览器会当成外站）、`http://`、
// `javascript:`、引号与尖括号。这是"不读 query"之外的**第二道**锁：
// 即便将来有人从别处把值传进来，这里也会拦下。
func ValidSpecPath(p string) bool {
	if p == "" || len(p) > 200 {
		return false
	}
	// 连续斜杠是协议相对 URL 的特征（`//host/path`），一律拒绝。
	if strings.Contains(p, "//") {
		return false
	}
	if !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "../") {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '-', r == '_', r == '.', r == '~':
		default:
			return false
		}
	}
	return true
}

// Asset 返回一个静态资源的字节与 MIME。
//
// MIME 由扩展名决定，不读文件内容嗅探：资源集合是封闭的 4 个文件，
// 嗅探只会引入"某个文件被识别成 text/plain 后浏览器不执行"的偶发问题。
func Asset(name string) ([]byte, string, bool) {
	// 先挡目录穿越：`../` 之类在 fs.Sub 之后仍可能命中，显式拒绝更省心。
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, `\/`) {
		return nil, "", false
	}
	raw, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return nil, "", false
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		return raw, "text/css; charset=utf-8", true
	case strings.HasSuffix(name, ".js"):
		return raw, "application/javascript; charset=utf-8", true
	case strings.HasSuffix(name, ".html"):
		return raw, "text/html; charset=utf-8", true
	default:
		return raw, "application/octet-stream", true
	}
}
