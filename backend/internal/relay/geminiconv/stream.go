package geminiconv

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/relay/conv"
)

// NewStream 为一次真实尝试创建流式编码器。
//
// 每尝试新建：编码器持有"已发终止帧"的状态，复用会让第二次尝试的客户端
// 收到两条 finishReason（Gemini 客户端会把它当成两次结束）。
func (c Codec) NewStream(model string, write conv.WriteFunc) conv.StreamEncoder {
	if model == "" {
		model = c.Model
	}
	return &streamEncoder{model: model, write: write}
}

// streamEncoder 把 OpenAI chunk 序列翻译成 Gemini 的 SSE 帧。
//
// 三条硬约束：
//  1. **不发 [DONE]**：Gemini 的流以 finishReason 收尾。relay 在 codec != nil 时不补
//     [DONE]，终止语义完全由这里的 Close 负责 —— Close 不写就是客户端一直等。
//  2. **tool_calls 必须缓冲到完整再发**：Gemini 的 functionCall 是完整对象，
//     把 OpenAI 的分片直接发出去，客户端会解析成半个函数（args 缺括号）。
//  3. 文本增量逐帧发：Gemini 的 parts[].text 是增量语义，攒起来发会让首字延迟 = 全量时间。
type streamEncoder struct {
	model string
	write conv.WriteFunc

	finished bool
	// 工具调用按 index 聚合（OpenAI 的流式分片用 index 标识第几个调用）。
	tools map[int]*streamCall
	order []int

	promptTokens     int64
	completionTokens int64
	cachedTokens     int64
	reasoningTokens  int64
	finishReason     string
}

// streamCall 是单个工具调用的聚合状态。
type streamCall struct {
	name string
	args strings.Builder
}

// OnChunk 处理一帧上游数据。
func (e *streamEncoder) OnChunk(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[DONE]" {
		// [DONE] 不等终止：真正的收尾在 Close（那时 usage 已经齐了）。
		return nil
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          any            `json:"content"`
				ReasoningContent any            `json:"reasoning_content"`
				ToolCalls        []openAICallDk `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		// 非 JSON 帧（心跳/注释）直接忽略：中止会把一条本来能正常结束的流打成失败。
		return nil
	}
	if u := chunk.Usage; u != nil {
		// 取最大值而不是最后一帧：兼容网关常见"每帧都报、中间帧缺字段"。
		e.promptTokens = maxI64(e.promptTokens, u.PromptTokens)
		e.completionTokens = maxI64(e.completionTokens, u.CompletionTokens)
		if u.PromptTokensDetails != nil {
			e.cachedTokens = maxI64(e.cachedTokens, u.PromptTokensDetails.CachedTokens)
		}
		if u.CompletionTokensDetails != nil {
			e.reasoningTokens = maxI64(e.reasoningTokens, u.CompletionTokensDetails.ReasoningTokens)
		}
	}
	for _, ch := range chunk.Choices {
		if t := anyToText(ch.Delta.ReasoningContent); t != "" {
			// 思考内容单独发一个 thought part：Gemini 客户端据此折叠展示，
			// 混进正文会让用户看到"模型在自言自语"。
			if err := e.emit([]geminiPart{{Text: t, Thought: true}}, ""); err != nil {
				return err
			}
		}
		if t := anyToText(ch.Delta.Content); t != "" {
			if err := e.emit([]geminiPart{{Text: t}}, ""); err != nil {
				return err
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if e.tools == nil {
				e.tools = map[int]*streamCall{}
			}
			cur, ok := e.tools[idx]
			if !ok {
				cur = &streamCall{}
				e.tools[idx] = cur
				e.order = append(e.order, idx)
			}
			if tc.Function.Name != "" {
				cur.name = tc.Function.Name
			}
			cur.args.WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != "" && e.finishReason == "" {
			e.finishReason = finishReasonOf(ch.FinishReason)
		}
	}
	return nil
}

// openAICallDk 是流式分片里的工具调用（index 是分片序号，必须是指针以区分 0 与缺失）。
type openAICallDk struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Close 正常收尾：把缓冲的工具调用一次发完，再发带 finishReason 与 usageMetadata 的末帧。
func (e *streamEncoder) Close() error {
	if e.finished {
		return nil
	}
	if len(e.order) > 0 {
		parts := make([]geminiPart, 0, len(e.order))
		for _, idx := range e.order {
			call := e.tools[idx]
			raw := strings.TrimSpace(call.args.String())
			if raw == "" {
				raw = "{}"
			}
			args := json.RawMessage(raw)
			if !json.Valid(args) {
				// 分片拼起来不是合法 JSON：这是上游违约（我们只做拼接，不改内容）。
				// 给一个空对象而不是把坏 JSON 发出去 —— 客户端拿到半截 args 会直接崩在解析上。
				args = json.RawMessage(`{}`)
			}
			parts = append(parts, geminiPart{FunctionCall: &geminiFuncCall{Name: call.name, Args: args}})
		}
		if err := e.emit(parts, ""); err != nil {
			return err
		}
	}
	reason := e.finishReason
	if reason == "" {
		reason = "STOP"
	}
	e.finished = true
	return e.emit(nil, reason)
}

// Fail 异常收尾：直接抛出 error 事件（Gemini 的错误流形状），客户端据此知道流断了。
func (e *streamEncoder) Fail(message string) error {
	if e.finished {
		return nil
	}
	e.finished = true
	raw, err := json.Marshal(map[string]any{
		"error": map[string]any{"code": 502, "message": message, "status": "INTERNAL"},
	})
	if err != nil {
		return err
	}
	return e.write("", raw)
}

// emit 写出一帧 Gemini chunk。
//
// finish="" 时是增量帧；非空时是终止帧（带 usageMetadata）。
// **绝不写 event: 行**：Gemini 客户端只解析 data 段，多一行 event 会被当成脏数据。
func (e *streamEncoder) emit(parts []geminiPart, finish string) error {
	chunk := geminiResponse{}
	cand := geminiCandidate{Content: geminiCandidateContent{Role: "model", Parts: parts}}
	if finish != "" {
		cand.FinishReason = finish
		reasoning := e.reasoningTokens
		candidates := e.completionTokens - reasoning
		if candidates < 0 {
			candidates = 0
		}
		chunk.UsageMetadata = &geminiUsage{
			PromptTokenCount:        e.promptTokens,
			CandidatesTokenCount:    candidates,
			ThoughtsTokenCount:      reasoning,
			CachedContentTokenCount: e.cachedTokens,
			TotalTokenCount:         e.promptTokens + e.completionTokens,
		}
	}
	// 增量帧也带上候选（finish="" 时 finishReason 省略）。
	chunk.Candidates = append(chunk.Candidates, cand)
	chunk.ModelVersion = "models/" + e.model
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return e.write("", raw)
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
