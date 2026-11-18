package middleware

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/gateway"
)

// GeminiSurface 把 /v1beta 整棵树的线格式标记为 Gemini。
//
// 为什么按**前缀**而不是按匹配到的端点：`POST /v1beta/models/:model` 一条路由承接
// 四个动作，每个动作的线格式相同（都是 Gemini），逐个判只会多一个出错点。
//
// 唯一的例外是 `/v1beta/openai/*`：那一层存在的意义就是「OpenAI SDK 只改 base_url」，
// 换掉错误体形状等于改了它的前提 —— SDK 会进不了自己的错误分支，把限流/额度不足
// 显示成「未知错误」。所以这里按路径前缀排除，而不是按端点类型判断。
//
// 挂载位置有硬要求：**路由匹配之后、鉴权之前**（与 IngressFormat 同理）。
// 挂晚了 401/403/429 就永远是 OpenAI 形状 —— 而那恰好是客户端最需要读懂的错误。
func GeminiSurface() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 判定只走 apidoc.ProtocolOfRequestPath：NoRoute 兜底、/v1 的 IngressFormat
		// 与这里必须用**同一个**函数，否则「已匹配路由」与「未匹配路径」会给出
		// 两种错误体形状，而用户只会看到"有时候报错看不懂"。
		path := c.FullPath()
		if path == "" && c.Request != nil {
			path = c.Request.URL.Path
		}
		gateway.SetFormat(c, apidoc.ProtocolOfRequestPath(path))
		c.Next()
	}
}
