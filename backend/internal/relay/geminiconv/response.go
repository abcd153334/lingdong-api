package geminiconv

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/relay/conv"
)

// 本文件是 OpenAI Chat 响应 → Gemini 响应的**唯一**映射点。
//
// 只改 id/model 是不够的：Google SDK 解析的是 `candidates[].content.parts[].text`，
// 给它一个 OpenAI 的 `{choices:[{message:{content}}]}` 是「200 但拿不到数据」，
// 属于最坏的一种修复（接口通了、功能没通）。

// openAICall 是 OpenAI 消息里的一个工具调用（ToClient 与流式共用的形状）。
type openAICall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// geminiResponse 是对外的响应体。
type geminiResponse struct {
	Candidates     []geminiCandidate   `json:"candidates,omitempty"`
	UsageMetadata  *geminiUsage        `json:"usageMetadata,omitempty"`
	PromptFeedback *geminiPromptFeedbk `json:"promptFeedback,omitempty"`
	ModelVersion   string              `json:"modelVersion,omitempty"`
}

type geminiCandidate struct {
	Content      geminiCandidateContent `json:"content"`
	FinishReason string                 `json:"finishReason,omitempty"`
	Index        int                    `json:"index"`
}

type geminiCandidateContent struct {
	Role string `json:"role"`
	// 只有终止帧没有 parts：给 `parts:null` 会让部分 SDK 把这一帧当成损坏内容。
	Parts []geminiPart `json:"parts,omitempty"`
}

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount,omitempty"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount,omitempty"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
}

type geminiPromptFeedbk struct {
	BlockReason string `json:"blockReason,omitempty"`
}

// ToClient 把 OpenAI 响应体转成 Gemini 响应体。
//
// 返回 *conv.Error 且 Status=502 表示上游给了无法翻译的形状 → relay 已冻结，必须释放。
func (c Codec) ToClient(body []byte, model string) ([]byte, *conv.Error) {
	if c.Embed {
		return c.embedToClient(body, model)
	}
	var in struct {
		Choices []struct {
			Message struct {
				Content          any          `json:"content"`
				ReasoningContent any          `json:"reasoning_content"`
				ToolCalls        []openAICall `json:"tool_calls"`
			} `json:"message"`
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
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, conv.BadUpstream("gemini_response_invalid", "服务响应不是合法 JSON：%s", err.Error())
	}

	out := geminiResponse{ModelVersion: "models/" + model}
	for i, ch := range in.Choices {
		parts, cerr := partsOfMessage(ch.Message.Content, ch.Message.ReasoningContent, ch.Message.ToolCalls)
		if cerr != nil {
			return nil, cerr
		}
		out.Candidates = append(out.Candidates, geminiCandidate{
			Content:      geminiCandidateContent{Role: "model", Parts: parts},
			FinishReason: finishReasonOf(ch.FinishReason),
			Index:        i,
		})
	}
	if u := in.Usage; u != nil {
		reasoning := int64(0)
		if u.CompletionTokensDetails != nil {
			reasoning = u.CompletionTokensDetails.ReasoningTokens
		}
		// candidatesTokenCount 只算"可见输出"：Gemini 把思考 token 单列在
		// thoughtsTokenCount 里，两者相加才等于上游的 completion_tokens。
		candidates := u.CompletionTokens - reasoning
		if candidates < 0 {
			candidates = 0
		}
		meta := &geminiUsage{
			PromptTokenCount:     u.PromptTokens,
			CandidatesTokenCount: candidates,
			ThoughtsTokenCount:   reasoning,
			TotalTokenCount:      u.PromptTokens + u.CompletionTokens,
		}
		if u.PromptTokensDetails != nil {
			meta.CachedContentTokenCount = u.PromptTokensDetails.CachedTokens
		}
		out.UsageMetadata = meta
	}
	// 上游把"被安全策略拦下"表达成 0 choices + finish_reason=content_filter：
	// Gemini 的对应形态是 promptFeedback.blockReason 且没有 candidates。
	if len(out.Candidates) == 0 {
		out.PromptFeedback = &geminiPromptFeedbk{BlockReason: "SAFETY"}
	}
	return mustMarshal(out)
}

// partsOfMessage 把一条 assistant 消息折成 Gemini 的 parts。
//
// 顺序有意义：思考 → 正文 → 工具调用。Gemini 的 functionCall 必须带完整 args 对象，
// 而 OpenAI 给的是 JSON 字符串 —— 解析失败是**上游违约**，502 而不是 400。
func partsOfMessage(content, reasoning any, calls []openAICall) ([]geminiPart, *conv.Error) {
	var parts []geminiPart
	if t := anyToText(reasoning); t != "" {
		parts = append(parts, geminiPart{Text: t, Thought: true})
	}
	if t := anyToText(content); t != "" {
		parts = append(parts, geminiPart{Text: t})
	}
	for _, tc := range calls {
		args := json.RawMessage(`{}`)
		if strings.TrimSpace(tc.Function.Arguments) != "" {
			raw := strings.TrimSpace(tc.Function.Arguments)
			if !json.Valid([]byte(raw)) {
				return nil, conv.BadUpstream("gemini_tool_args_invalid",
					"服务返回的工具参数不是合法 JSON（工具 %s），本次不计费", tc.Function.Name)
			}
			args = json.RawMessage(raw)
		}
		parts = append(parts, geminiPart{FunctionCall: &geminiFuncCall{Name: tc.Function.Name, Args: args}})
	}
	// 一条空消息也要给一个空文本 part：Gemini 客户端遇到 parts 缺失会认为响应损坏。
	if len(parts) == 0 {
		parts = append(parts, geminiPart{Text: ""})
	}
	return parts, nil
}

// anyToText 把 content / reasoning_content 折成纯文本。
//
// 两者都可能是字符串，也可能是 parts 数组（部分上游的兼容实现）；数组里只取 text。
func anyToText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// finishReasonOf 把 OpenAI 的 finish_reason 折成 Gemini 枚举。
//
// 未知值给 OTHER 而不是留空：Gemini 客户端按这个字段决定"是否还要继续生成"，
// 空值在部分 SDK 里被当成"未结束"。
func finishReasonOf(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls", "function_call":
		return "STOP"
	case "content_filter":
		return "SAFETY"
	case "recitation":
		return "RECITATION"
	case "":
		return ""
	default:
		return "OTHER"
	}
}

// mustMarshal 序列化响应；失败只可能是不可序列化的类型（本文件里不存在），
// 真发生了也要给出 502 而不是空体（空体会让 SDK 报"响应无内容"，更难查）。
func mustMarshal(v any) ([]byte, *conv.Error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, conv.BadUpstream("gemini_response_marshal", "响应序列化失败：%s", err.Error())
	}
	return raw, nil
}
