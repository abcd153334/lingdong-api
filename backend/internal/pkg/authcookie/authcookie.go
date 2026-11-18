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
	secure := conf.EnvConfig().CookieSecure
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
	secure := conf.EnvConfig().CookieSecure
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
