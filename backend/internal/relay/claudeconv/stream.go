package claudeconv

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Codec 是 Anthropic Messages 协议的编解码器。
//
// 无状态：可以做成值类型随意复制；有状态的只有流式编码器（每次请求新建）。
type Codec struct{}

// NewStream 为一次真实尝试创建流式编码器。
//
// 必须每尝试新建：编码器持有块序号与"已关闭"状态，复用会让第二次尝试
// 直接从半截开始（表现为"换了一条线路后回答从中间接着写"）。
func (Codec) NewStream(model string, write WriteFunc) StreamEncoder {
	return &streamEncoder{model: model, write: write, tools: map[int]*toolBlock{}}
}

// toolBlock 是单个工具调用的流式状态。
type toolBlock struct {
	clientIndex int
	id          string
	name        string
	started     bool
	// pending 是在拿到 id+name 之前缓冲的 arguments 分片。
	//
	// 为什么要缓冲：Anthropic 的 content_block_start 必须带 id 与 name，
	// 而 OpenAI 的流里它们可能比第一个 arguments 分片更晚到。
	// 不缓冲就只能先发一个空 name 的块 —— 客户端会把它当成无名工具丢掉。
	pending []string
}

// streamEncoder 把 OpenAI chunk 序列翻译成 Anthropic 事件序列。
type streamEncoder struct {
	model string
	write WriteFunc

	started    bool
	finished   bool
	nextIndex  int
	textOpen   bool
	textIndex  int
	tools      map[int]*toolBlock
	stopReason string

	// usage 逐帧取最大：标准实现只在末帧给，但兼容网关常见"每帧都给、且中间帧缺字段"。
	promptTokens     int64
	completionTokens int64
	cachedTokens     int64
}

// OnChunk 处理一帧上游数据。
func (e *streamEncoder) OnChunk(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[DONE]" {
		// 结束标记由 relay 在流正常结束后调用 Close 收尾，
		// 这里不立即发 message_stop：之后可能还有 usage 帧。
		return nil
	}

	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   any `json:"content"`
				Refusal   any `json:"refusal"`
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64            `json:"prompt_tokens"`
			CompletionTokens    int64            `json:"completion_tokens"`
			PromptTokensDetails *openAITokenInfo `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		// 上游给了非 JSON 帧：忽略而不是中止。中止会把一条本来能正常结束的流
		// 打成失败，而这类帧（心跳、注释）对客户端没有价值。
		return nil
	}

	if chunk.Usage != nil {
		e.promptTokens = maxI64(e.promptTokens, chunk.Usage.PromptTokens)
		e.completionTokens = maxI64(e.completionTokens, chunk.Usage.CompletionTokens)
		if chunk.Usage.PromptTokensDetails != nil {
			e.cachedTokens = maxI64(e.cachedTokens, chunk.Usage.PromptTokensDetails.CachedTokens)
		}
	}

	if err := e.ensureStarted(); err != nil {
		return err
	}

	for _, ch := range chunk.Choices {
		// 正文与拒答都按文本块输出：拒答也是一段要展示给用户的文字，
		// 丢掉它客户端只能显示空回答。
		if text := contentText(ch.Delta.Content); text != "" {
			if err := e.writeText(text); err != nil {
				return err
			}
		}
		if text := contentText(ch.Delta.Refusal); text != "" {
			if err := e.writeText(text); err != nil {
				return err
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if err := e.writeToolCall(idx, tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
				return err
			}
		}
		if ch.FinishReason != "" {
			e.stopReason = stopReasonOf(ch.FinishReason, len(e.tools) > 0)
		}
	}
	return nil
}

// Close 正常收尾：补齐 message_delta 与 message_stop。
func (e *streamEncoder) Close() error {
	if e.finished {
		return nil
	}
	if err := e.ensureStarted(); err != nil {
		return err
	}
	// 先关所有仍然开着的块：Anthropic 要求每个 content_block_start 都有对应的 stop，
	// 缺一个客户端就会一直等这个块的内容。
	if err := e.closeBlocks(); err != nil {
		return err
	}
	if e.stopReason == "" {
		if len(e.tools) > 0 {
			e.stopReason = "tool_use"
		} else {
			e.stopReason = "end_turn"
		}
	}
	delta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   e.stopReason,
			"stop_sequence": nil,
		},
		"usage": e.usagePayload(),
	}
	if err := e.emit("message_delta", delta); err != nil {
		return err
	}
	if err := e.emit("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	e.finished = true
	return nil
}

// Fail 异常收尾：只发一条 error 事件。
//
// 不再补 message_stop：客户端看到 error 就知道这次没结束，
// 而"先 error 再 message_stop"会让部分 SDK 把失败当成功。
func (e *streamEncoder) Fail(message string) error {
	if e.finished {
		return nil
	}
	e.finished = true
	return e.emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
}

// ensureStarted 懒发 message_start（首帧到达或收尾时）。
//
// input_tokens 写 0 是**诚实值**：上游此时还没给用量。
// 最终值由 message_delta 补全，文档也写明了这一点。
func (e *streamEncoder) ensureStarted() error {
	if e.started {
		return nil
	}
	e.started = true
	return e.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			// 流式起始时还没有上游 id（usage 与 id 都在末帧），
			// 只能生成一个：用随机值而不是按 model 派生，否则同一模型的
			// 所有流式响应 id 全一样，客户端按 id 去重会把两次调用合并。
			"id":            randomMessageID(),
			"type":          "message",
			"role":          "assistant",
			"model":         e.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_read_input_tokens":     0,
				"cache_creation_input_tokens": 0,
			},
		},
	})
}

// writeText 输出文本增量（懒开文本块）。
func (e *streamEncoder) writeText(text string) error {
	if !e.textOpen {
		e.textIndex = e.nextIndex
		e.nextIndex++
		e.textOpen = true
		if err := e.emit("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         e.textIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
	}
	return e.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": e.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

// writeToolCall 输出工具调用增量。
//
// 稠密编号：客户端看到的块序号必须是 0,1,2…，与上游的 tool_calls.index 解耦。
// 上游可能给 0/3（并发工具丢失中间项）或重复使用同一 index，
// 直接透传会让客户端按"块 3"去索引一个不存在的块。
func (e *streamEncoder) writeToolCall(upstreamIdx int, id, name, args string) error {
	tb, ok := e.tools[upstreamIdx]
	if !ok {
		tb = &toolBlock{}
		e.tools[upstreamIdx] = tb
	}
	if id != "" {
		tb.id = id
	}
	if name != "" {
		tb.name = name
	}
	if args != "" {
		tb.pending = append(tb.pending, args)
	}
	if tb.started {
		return e.flushToolArgs(tb)
	}
	// id 与 name 都到齐才能开块（Anthropic 的 content_block_start 必须带它们）。
	if tb.id == "" || tb.name == "" {
		return nil
	}
	// 文本块与工具块不能交错：先关文本块再开工具块。
	if e.textOpen {
		if err := e.emit("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": e.textIndex,
		}); err != nil {
			return err
		}
		e.textOpen = false
	}
	tb.clientIndex = e.nextIndex
	e.nextIndex++
	tb.started = true
	if err := e.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": tb.clientIndex,
		"content_block": map[string]any{
			"type": "tool_use", "id": tb.id, "name": tb.name, "input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	return e.flushToolArgs(tb)
}

// flushToolArgs 把缓冲的参数分片一次性发出（开块前缓冲的那些）。
func (e *streamEncoder) flushToolArgs(tb *toolBlock) error {
	if len(tb.pending) == 0 {
		return nil
	}
	payload := strings.Join(tb.pending, "")
	tb.pending = nil
	return e.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": tb.clientIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": payload},
	})
}

// closeBlocks 关闭所有仍开着的块（按开启顺序，文本在前）。
func (e *streamEncoder) closeBlocks() error {
	if e.textOpen {
		if err := e.emit("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": e.textIndex,
		}); err != nil {
			return err
		}
		e.textOpen = false
	}
	// 按 clientIndex 升序关闭：顺序与开启顺序一致，客户端不会收到"关了没开的块"。
	ordered := make([]*toolBlock, 0, len(e.tools))
	for _, tb := range e.tools {
		if tb.started {
			ordered = append(ordered, tb)
		}
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].clientIndex < ordered[i].clientIndex {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, tb := range ordered {
		if err := e.emit("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": tb.clientIndex,
		}); err != nil {
			return err
		}
	}
	return nil
}

// usagePayload 组装 Anthropic 用量（input 必须减掉缓存命中量）。
func (e *streamEncoder) usagePayload() map[string]any {
	input := e.promptTokens - e.cachedTokens
	if input < 0 {
		input = 0
	}
	return map[string]any{
		"input_tokens":                input,
		"output_tokens":               e.completionTokens,
		"cache_read_input_tokens":     e.cachedTokens,
		"cache_creation_input_tokens": 0,
	}
}

// emit 序列化并以 event+data 两行写出。
func (e *streamEncoder) emit(event string, payload any) error {
	if e.write == nil {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("claudeconv: 事件序列化失败: %w", err)
	}
	return e.write(event, b)
}

func maxI64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}
