package relay

import "lingdong-api/internal/relay/conv"

// ingressCodec 是入站线格式的编解码器。
//
// nil 表示「入站与上游同格式」（逐字节透传，今天的 OpenAI 端点全都是这种）。
// 非 nil 时 relay 只做三件事：进请求前解码、出响应前编码、流式逐帧编码，
// 其余判定（定价、选路、冻结、结算）一律作用在**解码后**的 OpenAI 请求体上。
//
// 为什么用接口而不是直接依赖某个 Codec：
// relay 的管线不该知道有几种协议；接第二个协议时只加一个实现，
// 不需要在 relayRequest / dispatch / streamDispatch 里再加分支。
//
// 错误类型与写出句柄来自 conv 叶子包（而不是某个具体协议包）：否则第二个协议的
// 编码器必须 import 第一个协议的包才能实现这个接口。
type ingressCodec interface {
	// ToUpstream 把客户端请求体转成 OpenAI Chat 请求体。
	// 返回的 error 一定是 *conv.Error 且 Status=400（未冻结，一分钱不动）。
	ToUpstream(body []byte) ([]byte, error)
	// ToClient 把 OpenAI 成功响应体转成客户端格式。
	// 返回 *conv.Error 且 Status=502 表示上游给了无法翻译的形状 → 必须释放冻结。
	ToClient(body []byte, model string) ([]byte, *conv.Error)
	// NewStream 每次真实尝试新建（重试不得复用，避免块序号与已关闭状态串味）。
	NewStream(model string, write conv.WriteFunc) conv.StreamEncoder
}
