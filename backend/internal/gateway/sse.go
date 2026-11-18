package gateway

import "bytes"

// Frame 构造一个 SSE 帧的字节（**唯一**的帧格式实现）。
//
// 格式必须逐字节稳定：`data: <payload>\n\n`，有事件名时先写 `event: <name>\n`。
// OpenAI 端点的流式输出正被大量客户端逐字解析，顺手「优化」一次格式就会
// 打掉一片；而 Claude 端点的 `event:` 行是它区分块类型的唯一依据。
//
// 为什么单独成函数：帧格式此前写在 relay 的闭包里，既不能被测试直接断言，
// 也不能被第二个写出点复用（编码器与原始帧都经过它）。
func Frame(event string, payload []byte) []byte {
	// 去掉 payload 尾部的换行：SSE 里换行就是「一行结束」，payload 自带一个
	// 尾随 \n 会提前终止 data 字段，产生一个空帧 —— 客户端按空行切帧，
	// 于是每帧后面多出一个空事件（解析器报 unexpected end of JSON input）。
	// 我们的 payload 恒为单行 JSON，这种修剪不丢任何内容。
	payload = bytes.TrimRight(payload, "\r\n")
	buf := make([]byte, 0, len(payload)+len(event)+16)
	if event != "" {
		buf = append(buf, "event: "...)
		buf = append(buf, event...)
		buf = append(buf, '\n')
	}
	if len(payload) > 0 {
		buf = append(buf, "data: "...)
		buf = append(buf, payload...)
	}
	buf = append(buf, "\n\n"...)
	return buf
}

// StreamHeaders 写出流式响应的固定头（在第一次写字节之前调用）。
//
// 三条都不能少：Content-Type 决定客户端是否按流解析；no-cache 防止中间层缓存；
// X-Accel-Buffering: no 关掉 Nginx 的响应缓冲 —— 少了它，帧会被攒成一块，
// 表现为「生成完了才一次性出现」的假卡顿。
func StreamHeaders() map[string]string {
	return map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	}
}
