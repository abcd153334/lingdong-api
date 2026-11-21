// Package authcookie 是认证相关 Cookie 的**唯一读写点**。
//
// 为什么单独一层：会话票、CSRF 令牌、验证码 id 三个 Cookie 的属性
// （Path / SameSite / HttpOnly / Secure / MaxAge）必须成对出现。
// 分散在 controller 与 middleware 里各写一遍 `c.SetCookie(...)` 时，
// 漏掉一个 HttpOnly 或漏掉一次清理，症状是「会话没了但 CSRF 还在」
// 或者「退出后 Cookie 仍然被浏览器发回」——两种都很难在测试里发现。
package authcookie

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/service/session"
)

// path 统一为根路径：门户与管理台共用同一票据，按路径划分会让 /admin 下拿不到会话。
const path = "/"

// SetSession 写入会话票与 CSRF 令牌。
//
// SameSite=Lax 而不是 Strict：Strict 会让「从搜索引擎/邮件点进来的链接」
// 一律不带 Cookie，用户看到的是「已登录却提示未登录」。
// Lax 下跨站的 POST/PUT/DELETE 不带 Cookie，配合 CSRF 双提交已经足够。
func SetSession(c *gin.Context, ticket, csrf string, maxAge int) {
	secure := secureFor(c)
	c.SetSameSite(http.SameSiteLaxMode)
	// 会话票必须 HttpOnly：它是凭证本身，XSS 不该能读走它。
	c.SetCookie(session.CookieName, ticket, maxAge, path, "", secure, true)
	// CSRF 令牌**不设** HttpOnly：前端要读出来放进请求头（双提交），
	// 这正是它的设计——攻击者能发请求但读不到这个值。
	c.SetCookie(session.CSRFCookieName, csrf, maxAge, path, "", secure, false)
}

// Clear 清除会话票与 CSRF 令牌（登出、会话失效、改密后）。
//
// 必须成对清除：只清会话票会留下一个永远对不上的 CSRF 值，
// 前端仍然会把它放进请求头，而服务端已经没有会话可校验。
func Clear(c *gin.Context) {
	secure := secureFor(c)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(session.CookieName, "", -1, path, "", secure, true)
	c.SetCookie(session.CSRFCookieName, "", -1, path, "", secure, false)
}

// Ticket 读取会话票据（空串表示未携带）。
func Ticket(c *gin.Context) string {
	v, err := c.Cookie(session.CookieName)
	if err != nil {
		return ""
	}
	return v
}

// CSRF 读取 CSRF 令牌。
func CSRF(c *gin.Context) string {
	v, err := c.Cookie(session.CSRFCookieName)
	if err != nil {
		return ""
	}
	return v
}

// secureFor 决定**这一次响应**的 Cookie 是否带 Secure。
//
// LD_COOKIE_SECURE=auto（默认）时按请求协议判断：这个开关存在的理由是把
// 「第一次登录」从"必须先配好证书"里解出来 —— prod 默认 Secure，而纯 http 入口下
// 浏览器不会回传带 Secure 的 Cookie，症状是"登录接口 200、之后全 401"，
// 是自查成本最高的一类故障。上了 TLS 之后同一个 auto 自动变成 Secure，不需要改配置。
//
// 空值（单测或工具没走 Load）按 auto 处理：零值不该让 Cookie 变成不可用。
func secureFor(c *gin.Context) bool {
	switch conf.EnvConfig().CookieSecureMode {
	case "true":
		return true
	case "false":
		return false
	}
	return requestIsHTTPS(c.Request)
}

// requestIsHTTPS 判定请求的实际协议。
//
// X-Forwarded-Proto 只在 auto 模式下用来**加**Secure：伪造它最多让攻击者自己的
// Cookie 不被发回（自伤），不会让别人的会话降级，所以这里不依赖可信代理判定。
func requestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if r.URL != nil && strings.EqualFold(r.URL.Scheme, "https") {
		return true
	}
	// 多层代理会逗号追加（"https,http"），协议看最左那段。
	xfp := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(xfp, ','); i >= 0 {
		xfp = xfp[:i]
	}
	return strings.EqualFold(strings.TrimSpace(xfp), "https")
}
