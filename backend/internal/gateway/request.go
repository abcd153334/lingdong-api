package gateway

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// requestIDKey 与 middleware 包写入的键保持一致（`request_id`）。
//
// 为什么在这里再写一次字面量而不是 import middleware：gateway 是叶子包，
// middleware 要 import 它（调 SetFormat），反向依赖会成环。键名由一条测试
// 守住（两端都断言字面量），改一处不会静默失效。
const requestIDKey = "request_id"

// RequestIDOf 返回本次请求的链路上标识（无则空串）。
//
// 优先读 gin context，回落读请求头：NoRoute / 中间件早期失败时 context 里还没有，
// 而 logger 中间件已经把 id 回写进 `X-Request-Id`，那条路径也能给出同一个值。
func RequestIDOf(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(requestIDKey); ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return strings.TrimSpace(c.GetHeader("X-Request-Id"))
}
