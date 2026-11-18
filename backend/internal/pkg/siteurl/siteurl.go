// Package siteurl 是「平台对外访问地址」的唯一推导点。
//
// 为什么要有这个包：这段逻辑原本在 controller.siteBase 与 relay.publicBaseURL
// 各写了一遍，两边对 X-Forwarded-Proto 的处理还不完全相同。对外地址是会进
// 用户剪贴板、进文档示例、进产物直链的东西——两处各算一次，迟早出现
// 「文件页给的是 https、文档示例给的是 http」，而两个页面看起来都正常。
//
// 两种语义**不可互换**（混用即死链）：
//   - Base：**入口面**（用户这一次从哪个域名进来）。产物直链 /f/、图标 /api/icons/、
//     文档示例、图片改写都用它 —— 多线路部署下必须指回用户实际访问的那条线路。
//     候选只来自运营配置的 API 地址，**不回落站点地址**：门户域名未必把
//     /f/ 与 /api/icons/ 转发到后端，拿它当 base 会让产物 404。
//   - DisplayBases：浏览器面（门户「OpenAI 兼容地址」列表）。配置为空时回落
//     站点地址，再不济回落当前请求 origin，并**标注来源**供 UI 提示。
//
// 给**第三方机器**用的地址（支付回调、交给上游回取的素材直链）固定取
// ConfiguredBase（第一条），不跟随入口 —— 那条域名必须长期稳定，不能因为
// 某条线路下线就让回调收不到、上游取不到图。
package siteurl

import (
	"net"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/sitecfg"
)

// DisplayBases 的取值来源（前端据此决定是否标注「默认」）。
const (
	SourceConfig  = "config"   // 运营在站点设置里填的地址
	SourceSiteURL = "site_url" // 未填 API 地址，用了站点地址
	SourceRequest = "request"  // 都没填，用了当前请求的 origin
)

// Base 返回**本次请求入口**的对外基址（不带尾斜杠；无法推导时返回空串）。
//
// 判定顺序（命中即返回）：
//  1. X-Forwarded-Host（已过白名单）命中所配置的某条 API 地址；
//  2. 无 XFH 时，用 Request.Host 命中所配置的地址（直连/反代覆盖 Host 的场景）；
//  3. 都不命中回落第一条（与旧行为一致）。
//
// 两个关键性质：
//   - **只有一条地址时，返回值与旧实现完全一致**（恒为第一条），单基址部署零回归；
//   - 回显的是配置里的原串，不是请求头 —— Host/XFH 伪造只能让它"匹配不到"，
//     不可能把产物直链指到攻击者的域名。
//
// 顺序仍是「先配置后推导」：运营填了正式域名时，不会被本地 localhost 覆盖。
func Base(c *gin.Context) string {
	s := sitecfg.Load()
	if c == nil || c.Request == nil {
		return s.PrimaryBase()
	}
	if len(s.APIBaseURLs) == 0 {
		return FromRequest(c)
	}
	raw := forwardedHost(c)
	if raw == "" {
		raw = strings.TrimSpace(c.Request.Host)
	}
	if b := s.MatchBase(raw, requestScheme(c)); b != "" {
		return b
	}
	return s.PrimaryBase()
}

// requestScheme 取本次请求的协议，只用于端口/协议匹配（返回值只影响匹配，不影响输出）。
//
// 默认 https：生产入口一定在 TLS 之后；真正的 http 直连（本地开发）会走到
// "host+port 精确匹配"那一档，端口对得上就不依赖 scheme。
func requestScheme(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return "https"
	}
	if c.Request.TLS != nil {
		return "https"
	}
	proto := c.GetHeader("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	if strings.EqualFold(strings.TrimSpace(proto), "http") {
		return "http"
	}
	return "https"
}

// FromRequest 由当前请求推导站点基址（不看配置）。
//
// 单独导出是因为有调用方需要「这一跳到底是从哪个域名进来的」——
// 例如诊断类接口与 CSRF 的同源比对（后者只做比对、不对外展示）。
// 需要给出对外地址的绝大多数场景仍应使用 Base。
func FromRequest(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	host := forwardedHost(c)
	if host == "" {
		host = strings.TrimSpace(c.Request.Host)
	}
	if host == "" {
		return ""
	}
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

// DisplayBases 返回门户要展示的对外地址（每条都是 origin，不含 /v1）。
//
// `/v1` 由展示层追加：这些地址同时是 /f/ 与 /api/pay/ 的基址，写进 /v1 会
// 让产物直链与支付回调一起带歪。
func DisplayBases(c *gin.Context) ([]string, string) {
	s := sitecfg.Load()
	if len(s.APIBaseURLs) > 0 {
		return s.APIBaseURLs, SourceConfig
	}
	if s.SiteURL != "" {
		return []string{s.SiteURL}, SourceSiteURL
	}
	if o := FromRequest(c); o != "" {
		return []string{o}, SourceRequest
	}
	return []string{}, SourceRequest
}

// ConfiguredHosts 返回「被认可的本站域名」集合（全部 API 地址 ∪ 站点地址）。
func ConfiguredHosts() []string {
	return sitecfg.Load().Hosts()
}

// forwardedHost 取反代写进来的原始域名（`X-Forwarded-Host`）。
//
// 为什么必须优先它：本项目的 nginx 模板统一写 `proxy_set_header Host 127.0.0.1`
// （见 deploy/vps/*.proxy.conf），于是 `Request.Host` 在任何部署下都恒为内网地址。
// 这对「用户可见的绝对地址」是致命的：图标 `icon_url`、任务产物直链、文档示例
// 会一律变成 `https://127.0.0.1/...`——页面看着正常（图片全裂），而接口返回的是
// 一个语法合法的 URL，任何"URL 非空"的断言都发现不了。
//
// 取第一段：多级代理会拼成 `a.com, b.com`，最左是原始请求的域名。
func forwardedHost(c *gin.Context) string {
	raw := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if raw == "" {
		return ""
	}
	first := raw
	if i := strings.IndexByte(raw, ','); i >= 0 {
		first = raw[:i]
	}
	first = strings.TrimSpace(first)
	// 只在「反代确实转发到本站配置的域名」时采信这个头。
	//
	// 不校验的话，任何请求都能用 `X-Forwarded-Host: evil.example` 把产物直链、
	// 图标 URL、文档示例里的主机名指到自己的域（配合 public 缓存头就是缓存投毒）。
	// 判据是**全部**已配置域名：只认站点地址时，入口域名与之不同就会恒被拒，
	// 对外地址静默回落 127.0.0.1。集合为空（什么都没配）时保持采信——
	// 那是本地开发/首次部署唯一能用的推导方式。
	if want := ConfiguredHosts(); len(want) > 0 && !hasHost(want, hostOnly(first)) {
		return ""
	}
	return first
}

// hasHost 判断 host 是否在集合内（大小写不敏感）。
func hasHost(set []string, host string) bool {
	for _, h := range set {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// hostOnly 取 host:port 的主机名（小写、去端口、去 IPv6 方括号）。
func hostOnly(hostport string) string {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return ""
	}
	if parsed, _, err := net.SplitHostPort(h); err == nil {
		h = parsed
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

// ConfiguredBase 返回**配置里**的对外基址（不做请求推导；取不到返回空串）。
// 供启动期检查使用：那时还没有任何请求上下文。
func ConfiguredBase() string {
	return sitecfg.Load().PrimaryBase()
}

// PublicBaseMisfitWarning 在「本地/测试环境 + 配置基址是公网域名」时返回提示文案。
//
// 这是部署配置问题的**可见性**措施（P1，无法代码根治）：本地 dev 的 /f/ 直链
// origin 会指向生产域名，上游回取必然 404/410——素材上传模式A 全链路在本地
// 物理不可闭环，而日志里此前没有任何线索。返回空串表示无需提示。
// 判定只认配置（ConfiguredBase）：未配置时 Base 回落请求 Host，本地反而正常。
func PublicBaseMisfitWarning(appEnv string) string {
	switch strings.ToLower(strings.TrimSpace(appEnv)) {
	case "dev", "test":
	default:
		return ""
	}
	base := ConfiguredBase()
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	if isLocalishHost(u.Hostname()) {
		return ""
	}
	return "站点设置里的第一条 API 调用地址指向公网域名（" + base + "），而当前是 " + appEnv +
		" 环境：本地/自建部署下 /f/ 直链上游回取将失败（素材会打到该域名而非本机）。" +
		"本地开发请把 API 调用地址清空（回落请求 Host）或改为本机地址"
}

// isLocalishHost 判断主机名是否本机/内网（无需提示的形态）。
func isLocalishHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "host.docker.internal" ||
		strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") ||
		strings.HasSuffix(h, ".internal") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false // 普通域名 → 视为公网
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}
