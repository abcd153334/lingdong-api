package middleware

import (
	"github.com/gin-gonic/gin"
)

// PublicNoStore 给「可缓存的公开端点」兜一个 no-store 默认值。
//
// 为什么需要它（而不是在每个 handler 里设）：
//   - 限流拒绝（429）由中间件直接写出，此时 handler 根本没运行，只有这里设的头
//     能覆盖到；
//   - 404（应用不存在）与 500（快照不可用）同样是"不该被缓存"的，一旦被浏览器
//     或中间代理记住，应用恢复后终端还会拿着旧 404 反复失败。
//
// 成功响应会由 writeETag 覆盖成 `public, max-age=N` —— 所以这里的 no-store
// 只是默认值，不是最终值。
//
// 必须挂在 RateLimitN **之前**（gin 的组中间件先于路由中间件执行）。
func PublicNoStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}
