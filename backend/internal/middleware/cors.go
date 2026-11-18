package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
)

// CORS 设置跨域响应头并处理预检请求。
//
// 与改造前的区别：
//   - 不再无条件 `Access-Control-Allow-Origin: *`。会话票据放在 Cookie 里，
//     只有带 `Allow-Credentials: true` 的**具体来源**回显才能让它跨域生效，
//     而 `*` 与 credentials 在规范上互斥（浏览器会直接丢弃整个响应）。
//   - 同源请求（绝大多数部署：nginx 同源反代）**不返回任何 CORS 头**，
//     避免把一个"看起来允许所有来源"的响应留在网关日志里。
//
// 白名单来自 LD_ALLOWED_ORIGINS；为空表示只允许同源。
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		// Vary 必须始终带上：否则中间缓存会把「带 CORS 头」的响应
		// 复用给另一个来源，或反之。
		c.Header("Vary", "Origin")

		if origin != "" && !strings.EqualFold(origin, "null") &&
			originMatches(c, origin) {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			// Anthropic SDK（浏览器版）会带 x-api-key 与 anthropic-version，
			// 部分客户端还会带 anthropic-beta。少一个就会在预检阶段被浏览器拦下，
			// 而预检失败在页面上只表现为"请求没发出去"，看不出是 CORS。
			c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,X-API-Key,X-CSRF-Token,Anthropic-Version,Anthropic-Beta")
			c.Header("Access-Control-Max-Age", "86400")
		}

		if c.Request.Method == http.MethodOptions {
			// 预检不携带 Cookie，也不应该触发任何业务逻辑。
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// originMatches 判断来源是否同源或在白名单内。
func originMatches(c *gin.Context, origin string) bool {
	host := c.Request.Host
	if strings.EqualFold(origin, "http://"+host) || strings.EqualFold(origin, "https://"+host) {
		return true
	}
	for _, w := range strings.Split(conf.EnvConfig().AllowedOrigins, ",") {
		if w = strings.TrimSpace(w); w != "" && strings.EqualFold(origin, w) {
			return true
		}
	}
	return false
}
