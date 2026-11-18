package middleware

import (
	"crypto/subtle"
	"net"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/authcookie"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/session"
)

// safeMethods 是只读方法（不做 CSRF 校验）。
//
// 前提是「现有 GET 全部只读」——新增任何有副作用的 GET 都必须先改这里，
// 否则它就是一个天然的 CSRF 入口。
var safeMethods = map[string]bool{
	"GET": true, "HEAD": true, "OPTIONS": true,
}

// CSRF 是已认证路由组的 CSRF 防护（双提交令牌 + Origin 校验）。
//
// 为什么两道都要：
//   - Origin 校验挡住「跨站表单直接提交」（浏览器一定会带 Origin/Referer）；
//   - 双提交令牌挡住「同站子域」这类 Origin 看起来合法的场景，
//     以及 Origin 头被剥离的老客户端。
//
// 为什么按路由组挂载而不是全局：`/v1` 与 `/api/pay/*` 走的是 API KEY / 渠道签名，
// 它们没有 Cookie 也就没有 CSRF 面。全局挂载会让第三方调用方全部 403。
func CSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		if safeMethods[c.Request.Method] {
			c.Next()
			return
		}
		if !originAllowed(c) {
			response.FailDef(c, errs.CSRFRejected.WithMessage("请求来源校验失败，请刷新页面后重试"))
			return
		}
		want := authcookie.CSRF(c)
		got := strings.TrimSpace(c.GetHeader(session.CSRFHeaderName))
		// 常量时间比较：令牌比对没有可被计时利用的实际风险，但代价为零。
		if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			response.FailDef(c, errs.CSRFRejected.WithMessage("页面已过期，请刷新后重试"))
			return
		}
		c.Next()
	}
}

// CSRFPublic 是公开写端点（登录 / 注册 / 找回密码）的来源校验。
//
// 这些端点没有会话，无法用双提交令牌 —— 但**仍然必须校验来源**：
// 针对登录接口的 CSRF（攻击者用自动提交的表单把受害者登进攻击者的账号，
// 之后受害者的充值、消费都进攻击者账号）不需要读取任何响应就能完成。
func CSRFPublic() gin.HandlerFunc {
	return func(c *gin.Context) {
		if safeMethods[c.Request.Method] {
			c.Next()
			return
		}
		if !originAllowed(c) {
			response.FailDef(c, errs.CSRFRejected.WithMessage("请求来源校验失败，请刷新页面后重试"))
			return
		}
		c.Next()
	}
}

// originAllowed 判断请求来源是否可信。
//
// 判定顺序（命中即返回）：Sec-Fetch-Site → 与本次请求的**完整 origin** 比对 →
// 显式白名单 → 开发环境放宽。全部不命中一律拒绝（fail-closed）。
//
// **为什么比完整 origin（scheme + host + port）而不是只比主机名**：
// Cookie 不区分端口，`http://app.example:8080` 上的任何一个服务（自助建站、
// 内网 Jenkins、被攻陷的兄弟应用）都能让浏览器带上 `app.example` 的会话去
// POST `http://app.example` —— 只比主机名等于把「同主机另一个端口沦陷」
// 直接升级成一次必成功的 CSRF。SameSite 的"站"定义忽略端口，Lax 也拦不住
// 这种跨端口请求，所以这里不能靠它兜底。
//
// 代价与配套：页面与接口不同端口（本地 :5178→:8080、反代不回传端口）时
// 完整比对会判跨站，此时靠 `LD_ALLOWED_ORIGINS` 显式登记，开发态再放宽到
// 回环/私有地址。现代浏览器都会带 `Sec-Fetch-Site: same-origin`，
// 在到达这一段比对之前就已经放行，因此这段收紧几乎只影响老客户端。
func originAllowed(c *gin.Context) bool {
	// ① Sec-Fetch-Site 由浏览器自己填写且脚本无法伪造（它是 forbidden header），
	//    判据比 Origin 更准，也能覆盖「反向代理剥掉了 Origin」的部署。
	if site := strings.ToLower(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site"))); site != "" {
		switch site {
		case "same-origin", "none":
			// none = 地址栏直接输入/书签打开，不含跨站发起者。
			return true
		case "cross-site":
			return false
			// same-site 继续往下比对：兄弟子域属于 same-site 但仍是跨站，
			// 而公开写端点没有双提交令牌兜底，不能只凭它放行。
		}
	}

	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin == "" {
		origin = originFromReferer(c.GetHeader("Referer"))
	}
	if origin == "" {
		// 非浏览器客户端（curl / 服务端 SDK）不带这两个头。
		// 一律拒绝：浏览器发起的跨站表单**一定**带 Origin，所以这条不会误伤
		// 真实用户，却能让「来源未知的写请求」在到达业务逻辑前就被挡下。
		// （本地要用 curl 调试就显式加 `-H 'Origin: http://localhost'`。）
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if sameOrigin(u, siteurl.FromRequest(c)) {
		return true
	}
	for _, allowed := range strings.Split(conf.EnvConfig().AllowedOrigins, ",") {
		if allowed = strings.TrimSpace(allowed); allowed != "" && strings.EqualFold(allowed, origin) {
			return true
		}
	}
	// dev/test：页面与接口经常不同端口、不同网卡地址（127.0.0.1 / 局域网 IP /
	// 容器内网名），无法枚举成白名单。此时放宽到「不含公网域名」的来源。
	if devLike() && devLikelyOrigin(u) {
		return true
	}
	return false
}

// sameOrigin 判断来源与本次请求是否**完全同源**：主机名 + 端口（缺省端口按各自协议补齐）。
//
// 协议从 `siteurl.FromRequest` 取（TLS / X-Forwarded-Proto / 都不带时按 http），
// 不在这里另拼一遍：X-Forwarded-Host 的白名单校验只在那一个函数里，
// 谁再拼一次就等于又开一条「伪造 XFH 骗过同源判定」的口子。
func sameOrigin(u *url.URL, reqOrigin string) bool {
	req, err := url.Parse(reqOrigin)
	if err != nil || req.Host == "" {
		return false
	}
	a := hostnameOnly(u.Host)
	if a == "" || a != hostnameOnly(req.Host) {
		return false
	}
	return portOf(u.Host, u.Scheme) == portOf(req.Host, req.Scheme)
}

// portOf 取 host:port 里的端口；未显式写端口时按协议补默认端口（http=80 / https=443）。
//
// 补默认端口是必需的：`https://app.example`（浏览器不带 :443）与
// 反代回传的 `Host: app.example` 必须判为同源，否则 https 站点全部 403。
func portOf(hostport, scheme string) string {
	if _, p, err := net.SplitHostPort(strings.TrimSpace(hostport)); err == nil && p != "" {
		return p
	}
	if strings.EqualFold(strings.TrimSpace(scheme), "http") {
		return "80"
	}
	return "443"
}

// hostnameOnly 从 host:port 中取主机名并归一（小写、去尾点、去 IPv6 方括号）。
func hostnameOnly(hostport string) string {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return ""
	}
	if parsed, _, err := net.SplitHostPort(h); err == nil {
		h = parsed
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	return strings.TrimSuffix(h, ".")
}

// devLikelyOrigin 判断一个来源主机是否是「本地开发可能出现的地址」。
//
// 只放行回环、私有网段、单标签主机名与 `*.local`（mDNS）。
// 公网域名仍必须显式配置 `LD_ALLOWED_ORIGINS` —— 否则开发机上打开一个
// 恶意网页就能对着 dev 实例发登录 CSRF。
func devLikelyOrigin(u *url.URL) bool {
	h := hostnameOnly(u.Host)
	if h == "" {
		return false
	}
	if h == "localhost" || strings.HasSuffix(h, ".local") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	// 单标签主机名（docker 服务名、内网短名）：不含点且不是 IPv6 形态。
	return !strings.Contains(h, ".") && !strings.Contains(h, ":")
}

// devLike 与 conf 的环境判定一致（dev/test 视为开发态；空值按 dev 处理）。
func devLike() bool {
	switch strings.ToLower(strings.TrimSpace(conf.EnvConfig().AppEnv)) {
	case "", "dev", "test":
		return true
	}
	return false
}

// originFromReferer 从 Referer 提取来源（Origin 缺失时的兜底）。
func originFromReferer(referer string) string {
	referer = strings.TrimSpace(referer)
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
