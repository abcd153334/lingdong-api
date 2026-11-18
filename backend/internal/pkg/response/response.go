// Package response 是**控制面（/api/*）**的统一响应信封。
//
// 与协议面（/v1、/v1beta、/dashboard）的分工：
//   - 控制面：`{code:0,message:"success",data}`，前端 parseResponse 按 code 判成败；
//   - 协议面：OpenAI `{error:{...}}` / Claude `{type:"error",...}` / 兼容封套，
//     由 internal/gateway 统一写出（**不在本包**）。
//
// 为什么本包不再有 FailOpenAI 系列：两面临时共用过一个包，结果协议面的信封
// 从 7 个地方各自拼装，状态码与业务码在端点之间悄悄漂移。协议面收口到
// gateway 之后，这里只留控制面信封 —— 谁想写 /v1 响应就得去 gateway，
// 编译期就挡住了第二条路。
package response

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/errs"
)

// Body 是统一响应体（step-00 §6.3）：成功 code=0，失败 code!=0。
// ErrorCode 是机器可判读的业务错误码（C-06，v2.0 §4.3）：由 errs.Def.Code 下发，成功时省略。
type Body struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	ErrorCode string `json:"error_code,omitempty"`
	Data      any    `json:"data"`
}

// OK 返回成功响应，code=0。
func OK(c *gin.Context, data any) {
	c.JSON(200, Body{Code: 0, Message: "success", Data: data})
}

// Encode 产出与 OK **逐字节相同**的成功响应体，供需要 ETag 的端点使用。
//
// 为什么需要它：ETag 必须对实际写出的字节取哈希，而 OK 把序列化交给了 c.JSON，
// 调用方拿不到字节。若各端点自己拼一遍 `{code,message,data}`，就等于有了第二份
// 信封定义 —— 迟早与 OK 分叉（少一个字段、换一个 key）。所以信封只在这里拼一次，
// 由 response_test.go 的逐字节比对守住。
//
// 必须用 json.Marshal 而不是 json.NewEncoder().Encode：后者会多写一个换行，
// 结果既影响 ETag，也影响 Content-Length。
func Encode(data any) ([]byte, error) {
	return json.Marshal(Body{Code: 0, Message: "success", Data: data})
}

// OKMsg 返回带自定义 message 的成功响应。
func OKMsg(c *gin.Context, message string, data any) {
	c.JSON(200, Body{Code: 0, Message: message, Data: data})
}

// FailDef 按错误码定义返回失败响应（同时下发业务错误码 error_code）。
func FailDef(c *gin.Context, def errs.Def) {
	gateway.MarkErrorCode(c, def.Code)
	c.AbortWithStatusJSON(def.HTTP, Body{Code: def.HTTP, Message: def.Message, ErrorCode: def.Code, Data: nil})
}

// FailDefMsg 与 FailDef 相同，但用 msg 覆盖文案（错误码不变）。
//
// 用在"调用方能自己改对"的场景：错误码仍是 `invalid_param`（前端可以据此定位
// 是哪一类问题），而文案里带上具体原因与可选项（如 sort 支持的字段清单）。
// 只改 message、不动 code —— 反过来做（自定义 code）会让新增的写法漏登记到
// errs，前端错误码表里就多出一行"后端有话说，前端只能显示泛化提示"。
func FailDefMsg(c *gin.Context, def errs.Def, msg string) {
	gateway.MarkErrorCode(c, def.Code)
	c.AbortWithStatusJSON(def.HTTP, Body{Code: def.HTTP, Message: msg, ErrorCode: def.Code, Data: nil})
}

// FailDefData 与 FailDef 相同，但额外下发结构化 data。
// 只有"拒绝原因里带可操作清单"的场景才用它（例如缺哪些探测目标）：
// 把这些信息塞进 message 会让前端只能整段展示，无法逐条给按钮。
func FailDefData(c *gin.Context, def errs.Def, data any) {
	gateway.MarkErrorCode(c, def.Code)
	c.AbortWithStatusJSON(def.HTTP, Body{Code: def.HTTP, Message: def.Message, ErrorCode: def.Code, Data: data})
}

// NotFound 是控制面 404 兜底（统一错误结构）。
func NotFound(c *gin.Context) {
	FailDef(c, errs.NotFound)
}
