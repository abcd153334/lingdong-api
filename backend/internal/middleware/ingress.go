package middleware

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/gateway"
)

// IngressFormat 把「本次请求命中的端点线格式」写入 gin.Context。
//
// 挂载位置有硬要求：**路由匹配之后、鉴权之前**。
//   - 必须在鉴权之前：401/403/429 也由 response 包写出，晚了这些响应会退回
//     OpenAI 形状，Claude 客户端解析不到 {type:"error"}，只能显示"未知错误"；
//   - 必须在路由匹配之后：线格式是端点属性，只能从已匹配的路由（c.FullPath）反查。
//
// 判定用 ProtocolOfRequestPath 而不是 KindOfPath：前者带"最长路径前缀"兜底，
// 让 /v1/messages/count_tokens 这类**同协议、未单独登记**的子路径也拿到正确形状。
// 只做精确匹配的话，那个接口的 501 会退回 OpenAI 形状 —— 恰好是客户端最需要
// 读懂错误的时候。
func IngressFormat() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 未匹配路由（gin 的 NoRoute）时 FullPath 是空串，只能回落到原始路径 ——
		// 否则未注册的 /v1beta/* 与 /suno/* 会被判成 OpenAI 形状，
		// 而那两个面恰恰是「路径写错时最需要读懂错误体」的场景。
		path := c.FullPath()
		if path == "" && c.Request != nil {
			path = c.Request.URL.Path
		}
		gateway.SetFormat(c, apidoc.ProtocolOfRequestPath(path))
		c.Next()
	}
}
