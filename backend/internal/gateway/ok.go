package gateway

import "github.com/gin-gonic/gin"

// OK 写裸对象（OpenAI 风格：`/v1/tasks/{id}`、`/v1/models/{id}`、文件对象）。
//
// 「裸」不是省事：这些端点的客户端用 OpenAI SDK 直接反序列化成结构体，
// 外面套一层信封会让它把所有字段读成零值。
func OK(c *gin.Context, data any) {
	c.JSON(200, data)
}

// OKStatus 是带非 200 成功码的裸对象（例如文件上传的 201）。
func OKStatus(c *gin.Context, status int, data any) {
	c.JSON(status, data)
}

// OKNewAPI 写兼容协议的成功信封（`{code:"success",message:"",data}`）。
//
// 为什么必须有一个 helper：失败侧一直只有一处（serialize 的 NewAPI 分支），
// 成功侧却是手写的 `gin.H{"code":"success",...}`。两处一旦分叉 —— 例如给信封
// 加一个字段 —— 只有失败侧会跟着变，成功侧仍是旧形状，而客户端用的是同一份解析。
func OKNewAPI(c *gin.Context, data any) {
	c.JSON(200, NewAPIEnvelope{Code: "success", Message: "", Data: data})
}
