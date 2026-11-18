// Package claudeconv 是 Anthropic Messages 协议与 OpenAI Chat 协议之间的唯一转换处。
//
// 为什么单独一个包、且不 import relay：
//   - relay 要调用它（入站解码、出站编码），反向 import 会成环；
//   - 转换规则必须能**脱离 HTTP 与数据库单测**：协议转换错一位（比如 usage 的
//     input_tokens 该不该减缓存）在真机上只表现为"账单金额不对"，极难定位。
//
// 转换方向刻意只有一条：Claude 入站 → OpenAI Chat 中枢 → 上游。
// 上游线格式目前只有 OpenAI 一种（所有能承接对话的适配器都经同一个 Chat 方法），
// 所以不需要 N×M 的矩阵；将来若接入 Claude 原生上游，只需再加一个编码器。
package claudeconv

import "lingdong-api/internal/relay/conv"

// 本文件是 conv 包三个类型在 Claude 侧的类型别名。
//
// 为什么用别名而不是包一层新类型：它们必须在整个仓库里是**同一个类型** ——
// ingressCodec 的签名用的是 conv.WriteFunc，而 claudeconv.StreamEncoder 的实现
// 要能直接赋给它；包一层就需要 in/out 转换，转换点漏一个就是编译错，写对了也是噪音。
//
// 保留别名的第二个理由：claudeconv 内部的 35 处 badRequest/badUpstream 与
// 测试里的 `err.(*ConvError)` 断言全部零改动。

// ConvError 是转换失败的结构化原因（同 conv.Error）。
type ConvError = conv.Error

// WriteFunc 是编码器写出一个事件的方式（同 conv.WriteFunc）。
type WriteFunc = conv.WriteFunc

// StreamEncoder 把上游流式帧翻译成客户端事件流（同 conv.StreamEncoder）。
type StreamEncoder = conv.StreamEncoder

// badRequest 构造请求侧错误（400，未冻结）。
func badRequest(code, format string, args ...any) *ConvError {
	return conv.BadRequest(code, format, args...)
}

// badUpstream 构造响应侧错误（502，已冻结需释放）。
func badUpstream(code, format string, args ...any) *ConvError {
	return conv.BadUpstream(code, format, args...)
}
