package middleware

import (
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/response"
)

// Recover 捕获 panic，记录堆栈并返回统一 500 错误体。
func Recover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.L().Error("请求 panic",
					zap.Any("error", r),
					zap.String("path", c.Request.URL.Path),
					zap.ByteString("stack", debug.Stack()),
				)
				// 已经写出过响应（典型是 SSE 已 flush 若干帧）时**不能再写错误体**：
				// 状态码已经发出去了，追加一段 JSON 会让客户端解析器报协议错误，
				// 而它会把这个错误归因成"上游返回了坏数据" —— 比 500 更难查。
				if c.Writer.Written() {
					c.Abort()
					return
				}
				writePanicBody(c)
				c.Abort()
			}
		}()
		c.Next()
	}
}

// writePanicBody 按请求所属面写错误体。
//
// 这里**不能无条件用 response.FailDef**：协议面（/v1、/v1beta、/suno、/dashboard）
// 的客户端按 OpenAI/Claude/Gemini 形状解析错误，收到 `{code,message,error_code}`
// 只会显示"未知错误"，连重试建议都给不出。而 Recover 是全局中间件、拿不到路由归属，
// 所以只能：① 看中间件是否已写入线格式；② 没写入时按路径判面。
func writePanicBody(c *gin.Context) {
	if _, ok := gateway.FormatExplicit(c); ok {
		gateway.FailDef(c, errs.Internal)
		return
	}
	if c.Request != nil && apidoc.IsProtocolSurface(c.Request.URL.Path) {
		// panic 可能就发生在 IngressFormat 自身：此时 context 里还没有线格式，
		// 只能按路径反查（ProtocolOfRequestPath 对未登记路径也有兜底）。
		gateway.SetFormat(c, apidoc.ProtocolOfRequestPath(c.Request.URL.Path))
		gateway.FailDef(c, errs.Internal)
		return
	}
	response.FailDef(c, errs.Internal)
}
