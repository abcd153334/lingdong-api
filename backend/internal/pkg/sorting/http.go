package sorting

// 本文件是 `sort` 参数的 **HTTP 接入点**：解析 + 失败即写 400。
//
// 为什么放在 sorting 包而不是各 handler 里：调用方有 5 个（且分布在不同包，
// controller 与 relay 互相不可见），任何"各写一遍错误响应"的写法都会分叉 ——
// 一处回 `参数不合法`、一处回 `sort 不支持字段 x`，接入方看到两套话术。
//
// 依赖面：gin + errs + response。核心的 Parse 仍然只依赖标准库，
// 本文件只是把它接到 HTTP 上，所以"解析逻辑"依旧可以脱离 gin 单测。

import (
	"errors"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
)

// Query 读取 `sort` 查询参数。
//
// 返回值 ok=false 表示**响应已经写好**（400 + 具体原因 + 可选字段清单），
// 调用方直接 return 即可。这是 Go 里"错误已处理"的常见约定，
// 比让每个 handler 判 error 再各自拼消息少一份重复。
//
// 400 而不是忽略：静默忽略会让调用方以为排序生效了 —— 尤其在字段拼错、
// 但返回结果"看起来也对"的时候。
func Query(c *gin.Context, columns map[string]string) (Order, bool) {
	o, err := Parse(c.Query("sort"), columns)
	if err != nil {
		msg := errs.InvalidParam.Message
		var fe *FieldError
		if errors.As(err, &fe) {
			// 把"能传什么"直接回给调用方：他自己就能改对，
			// 不需要来问"那支持哪些字段"。
			msg = fe.Error()
		}
		response.FailDefMsg(c, errs.InvalidParam, msg)
		return Order{}, false
	}
	return o, true
}
