package relay

import (
	"bytes"
	"encoding/json"
)

// 本文件是「未声明字段透传」的唯一实现。
//
// 背景：适配器把客户端请求映射成上游报文时，过去一律按结构体白名单重建
// （`duomiapi.go` 文件头也曾把它写成硬约束）。后果是**上游加了新参数、平台还没跟上**
// 时，用户按官方文档传的字段会被静默吃掉：`upstream_raw` 里看不到、账单上看不出来、
// 用户以为生效了。典型例子是 `mask_url`（蒙版）与 `output_format`（输出格式）。
//
// 现在只有一条规则：
//
//	适配器映射写过的键，以映射为准；适配器没写的顶层键，原样补进上游报文。
//
// 「白名单」就是本次 payload 里实际出现的键，不是另一份手写清单 —— 因此不存在
// 「清单与实现漂移」这种第二真相源。失败方向被刻意设计成**多发**而不是少发：
// 漏写一个 localOnly 只会多送一个上游可能忽略的字段，不会把用户的参数吃掉。

// ForwardUnmapped 把 raw 里「dst 还没有、且不在 localOnly」的顶层键原样补进 dst。
//
// 参数：
//   - dst：调用方已经映射好的上游报文；
//   - raw：客户端请求体原文（**不是**经过任何结构体往返的副本）；
//   - localOnly：平台已消费但绝不下发的键，以及已被改名/翻译过的原始键。
//
// 规则（顺序即优先级）：
//  1. dst 已有的键一律跳过 —— 映射优先，天然排除 model / stream / 已翻译的尺寸；
//  2. localOnly 里的键跳过 —— 例如 response_format（平台本地决定返回 b64 还是 url），
//     以及 size/resolution（已翻译成上游单一表示）；
//  3. 值为 null 的跳过 —— JSON 语义等同未设置，转发 null 会被多数上游判非法；
//  4. 其余键以**原始字节**写入，不做 float64 往返（`seed` 这类大整数不能变精度）。
//
// 不返回错误：dst 是调用方已经算好的合法报文，这里最坏的结果是「少补一个键」，
// 不该让一次正常请求变成 400。raw 不是 JSON 对象时静默返回。
func ForwardUnmapped(dst map[string]any, raw []byte, localOnly ...string) {
	if dst == nil || len(bytes.TrimSpace(raw)) == 0 {
		return
	}
	var src map[string]json.RawMessage
	if err := json.Unmarshal(raw, &src); err != nil || src == nil {
		return
	}
	var skip map[string]struct{}
	if len(localOnly) > 0 {
		skip = make(map[string]struct{}, len(localOnly))
		for _, k := range localOnly {
			skip[k] = struct{}{}
		}
	}
	for k, v := range src {
		if _, ok := dst[k]; ok {
			continue
		}
		if skip != nil {
			if _, ok := skip[k]; ok {
				continue
			}
		}
		val := bytes.TrimSpace(v)
		if len(val) == 0 || string(val) == "null" {
			continue
		}
		dst[k] = json.RawMessage(append([]byte(nil), val...))
	}
}

// imageOutboundBody 返回图片端点在某条候选上实际应该发出的报文。
//
// 只有实现了 ImageStreamer 的候选才走 SSE 分支（dispatch 里的 stream && isImageKind），
// 其余候选必须不带 stream：上游回 SSE 时响应侧按 JSON 解析会失败，被判成 0 张后
// 兜底记 1 张 —— 客户端拿到一坨事件流、账单按 1 张收、全程无报错。
//
// 纯函数：候选能力由调用方用类型断言算出（`_, ok := adapter.(ImageStreamer)`）。
func imageOutboundBody(body []byte, isStreamer bool) []byte {
	if isStreamer {
		return body
	}
	return StripTopLevelKey(body, "stream")
}

// StripTopLevelKey 返回删掉某个顶层键后的报文；键不存在或 body 不是 JSON 对象时原样返回。
func StripTopLevelKey(body []byte, key string) []byte {
	if len(body) == 0 {
		return body
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	if _, ok := m[key]; !ok {
		return body
	}
	delete(m, key)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
