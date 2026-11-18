package middleware

import (
	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
)

// TokenAuthIfProtocol 只对**协议面**路径执行 TokenAuth。
//
// 用于 NoRoute：未注册的 `/v1/xxx`、`/v1beta/xxx`、`/suno/xxx` 必须与已注册端点
// 走同一条入口链 —— 先鉴权、再 404。以前这些路径绕过鉴权直接 404，后果有三：
//  1. 任何人都能不用 KEY 免费探测「某个端点是否存在」（404 与 401 的差异就是探针）；
//  2. 请求不经过 AccessLog，没有 request_id，出问题时连一次访问痕迹都没有；
//  3. 同一个客户端在「已注册」与「拼错一个字母」两种情况下拿到不同的错误形状与状态码。
//
// 其余面必须跳过：`/f/` 是签名直链域（本来就不需要 KEY）、`/api/*` 是控制面
// （有自己的会话鉴权），一律套 TokenAuth 会把它们从 404 变成 401。
func TokenAuthIfProtocol() gin.HandlerFunc {
	// 复用同一个闭包实例：TokenAuth 内部没有请求级状态，构造一次即可。
	base := TokenAuth()
	return func(c *gin.Context) {
		if c != nil && c.Request != nil && apidoc.IsProtocolSurface(c.Request.URL.Path) {
			// TokenAuth 自己在末尾调用 c.Next()，因此这里直接 return。
			base(c)
			return
		}
		c.Next()
	}
}
