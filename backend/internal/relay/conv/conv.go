// Package conv 是**协议转换的公用类型**（错误 + 写出句柄 + 流式编码器接口）。
//
// 为什么单独一个叶子包：ingressCodec（relay/ingress.go）是通用接口，任何协议
// 编解码器都要实现它。若这些类型长在 claudeconv 上，Gemini 编码器就必须 import
// claudeconv —— 那是「协议」与「Claude」绑死的结构性错误，日后接入第三种协议会
// 出现 A import B、B import A 的镜像依赖。
//
// 本包零依赖（只 import fmt），因此任何 codec 包都能引用。
package conv

import "fmt"

// Error 是转换失败的结构化原因。
//
// 分两类，处置完全不同：
//   - 请求侧（Status=400）：客户端的请求表达不了，**未冻结**，一分钱不动；
//   - 响应侧（Status=502）：上游返回了我们无法翻译的形状（例如 tool arguments 不是
//     JSON 对象），必须释放冻结而不是按坏体结算 —— 用户没拿到可用结果，不该付钱。
type Error struct {
	// Status 是对外 HTTP 状态码（400 / 502）。
	Status int
	// Code 是内部错误码（进访问日志与 usages.error_msg，不进业务判断）。
	Code string
	// Message 是给用户看的一句话：必须说清"哪里不行、怎么改"，
	// 而不是"转换失败"这种只有我们知道发生了什么的话。
	Message string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// BadRequest 构造请求侧错误（400，未冻结）。
func BadRequest(code, format string, args ...any) *Error {
	return &Error{Status: 400, Code: code, Message: fmt.Sprintf(format, args...)}
}

// BadUpstream 构造响应侧错误（502，已冻结需释放）。
func BadUpstream(code, format string, args ...any) *Error {
	return &Error{Status: 502, Code: code, Message: fmt.Sprintf(format, args...)}
}

// WriteFunc 是编码器写出一个事件的方式（event 为空表示不写 event 行）。
//
// 由 relay 的流式分支提供：它负责设响应头、Flush、以及客户端断开时返回 error
// （编码器据此中止，不再继续产出）。
type WriteFunc func(event string, data []byte) error

// StreamEncoder 把上游的 OpenAI 流式帧翻译成客户端格式的事件流。
//
// 有状态、按请求新建：重试时必须重新 NewStream，复用会把上一次的块序号与
// 已关闭状态带进新一次尝试（表现为"第二个渠道的流一开始就是半截"）。
type StreamEncoder interface {
	// OnChunk 处理一帧上游数据（OpenAI chunk 的 JSON，或 [DONE] 标记）。
	// 返回 error 表示写出失败（通常是客户端断开），调用方应停止转发。
	OnChunk(data []byte) error
	// Close 正常收尾：补齐各自协议的终止事件（Claude 的 message_stop、
	// Gemini 的收尾帧）。**终止语义只由编码器负责** —— relay 在 codec != nil
	// 时不补 [DONE]，所以 Close 不写就等于客户端一直等。
	Close() error
	// Fail 异常收尾：输出一条错误事件（客户端据此知道流断了而不是正常结束）。
	Fail(message string) error
}
