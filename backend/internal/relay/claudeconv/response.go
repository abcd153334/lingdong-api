package claudeconv

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// claudeResponse 是 Anthropic 的非流式响应。
type claudeResponse struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	Role         string              `json:"role"`
	Model        string              `json:"model"`
	Content      []claudeRespBlock   `json:"content"`
	StopReason   string              `json:"stop_reason"`
	StopSequence *string             `json:"stop_sequence"`
	Usage        claudeResponseUsage `json:"usage"`
}

type claudeRespBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type claudeResponseUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ToClient 把 OpenAI Chat 响应体翻译成 Anthropic Messages 响应体。
//
// 返回 *ConvError 且 Status=502 表示"上游给的东西我们翻译不了"：
// 调用方必须**释放冻结**，不能按坏体结算 —— 用户没拿到可用结果就不该付钱。
// 这类响应在对话补全端点上今天会被当成"成功但无 usage"按估算收费，
// 是同一类缺陷的新入口，必须在这里堵死。
func (Codec) ToClient(body []byte, model string) ([]byte, *ConvError) {
	var up openAIChatResponse
	if err := json.Unmarshal(body, &up); err != nil {
		return nil, badUpstream("upstream_invalid_response", "收到的响应不是合法 JSON")
	}
	if len(up.Choices) == 0 {
		return nil, badUpstream("upstream_invalid_response", "收到的响应里没有 choices")
	}
	ch := up.Choices[0]
	if ch.Message == nil {
		return nil, badUpstream("upstream_invalid_response", "收到的响应里没有 message")
	}

	blocks := make([]claudeRespBlock, 0, 1+len(ch.Message.ToolCalls))
	if s := contentText(ch.Message.Content); s != "" {
		blocks = append(blocks, claudeRespBlock{Type: "text", Text: s})
	}
	for _, tc := range ch.Message.ToolCalls {
		input, cerr := decodeArguments(tc.Function.Arguments)
		if cerr != nil {
			return nil, cerr
		}
		blocks = append(blocks, claudeRespBlock{
			Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input,
		})
	}
	if len(blocks) == 0 {
		return nil, badUpstream("upstream_invalid_response", "收到的响应既没有正文也没有工具调用")
	}

	out := claudeResponse{
		ID:         messageID(up.ID),
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    blocks,
		StopReason: stopReasonOf(ch.FinishReason, len(ch.Message.ToolCalls) > 0),
		Usage:      usageOf(up.Usage.PromptTokens, up.Usage.CompletionTokens, up.Usage.PromptTokensDetails),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, badUpstream("convert_failed", "响应序列化失败")
	}
	return b, nil
}

// openAIChatResponse 是 OpenAI Chat 的响应（只声明我们读的字段）。
type openAIChatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      *openAIRespMessage `json:"message"`
		FinishReason string             `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int64            `json:"prompt_tokens"`
		CompletionTokens    int64            `json:"completion_tokens"`
		PromptTokensDetails *openAITokenInfo `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// openAITokenInfo 是 prompt_tokens_details（只关心缓存命中量）。
type openAITokenInfo struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type openAIRespMessage struct {
	Content   any `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// contentText 把 content 的两种形态（字符串 / [{type:text,text}]）统一成字符串。
func contentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, it := range t {
			if m, ok := it.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// decodeArguments 把 OpenAI 的 arguments 字符串解析成 Anthropic 要求的**对象**。
//
// 空串给 {}：上游对"无参数工具"的写法不一致（空串 / "{}" / null），
// 空串直接透传会让 Anthropic 客户端拿到一个非对象值而解析失败。
// 非法 JSON 必须 502：这不是"没有参数"，是上游给了坏数据。
func decodeArguments(s string) (json.RawMessage, *ConvError) {
	trimmed := bytes.TrimSpace([]byte(s))
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage(`{}`), nil
	}
	if trimmed[0] != '{' {
		return nil, badUpstream("upstream_invalid_response",
			"工具参数不是 JSON 对象，无法转换为该协议要求的格式")
	}
	var probe map[string]any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, badUpstream("upstream_invalid_response",
			"工具参数不是合法 JSON，无法转换为该协议要求的格式")
	}
	return json.RawMessage(trimmed), nil
}

// stopReasonOf 把 finish_reason 映射为 Anthropic 的 stop_reason。
//
// 缺省按 end_turn：上游不给 finish_reason 时，报"正常结束"比报错更安全
// （客户端拿到完整正文，只是不知道为什么停）。
func stopReasonOf(finish string, hasTools bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "stop", "":
		if finish == "" && hasTools {
			// 有的上游只在 tool_calls 存在时不带 finish_reason。
			return "tool_use"
		}
		return "end_turn"
	default:
		return "end_turn"
	}
}

// usageOf 把 OpenAI 用量换算成 Anthropic 用量。
//
// **必须减缓存**：OpenAI 的 prompt_tokens 已含命中缓存的部分，
// 而 Anthropic 的 input_tokens **不含**。直接透传会让客户端把同一批输入
// 与 cache_read 各算一次，用户看到的"本次消耗"凭空翻倍。
func usageOf(prompt, completion int64, details *openAITokenInfo) claudeResponseUsage {
	cached := int64(0)
	if details != nil {
		cached = details.CachedTokens
	}
	input := prompt - cached
	if input < 0 {
		input = 0
	}
	return claudeResponseUsage{
		InputTokens:          input,
		OutputTokens:         completion,
		CacheReadInputTokens: cached,
		// openai 协议里没有"缓存写入"这个通道（只有命中量），
		// 所以这里恒为 0，而不是把 cached 重复计一次写入。
		CacheCreationInputTokens: 0,
	}
}

// messageID 由上游 id 派生一个稳定的 Anthropic 风格 id。
//
// 用哈希而不是随机：同一次调用的响应 id 在重放/日志对照时必须一致，
// 随机 id 会让"客户端报障给的 id"在我们的调用记录里查不到。
func messageID(upstreamID string) string {
	sum := sha256.Sum256([]byte(upstreamID))
	return "msg_" + hex.EncodeToString(sum[:])[:24]
}

// randomMessageID 生成流式场景的响应 id（此时还没有上游 id 可用）。
//
// 必须是随机的：按 model 或时间派生会让并发请求撞 id，
// 而客户端会拿 id 做去重与报障引用。
func randomMessageID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 系统随机源不可用属于进程级异常；退化成时间戳也比撞 id 好。
		return "msg_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "msg_" + hex.EncodeToString(b[:])
}
