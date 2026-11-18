package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"lingdong-api/internal/relay"
)

// 本文件是 AI 编排的 LLM 调用链：**零计费、不进 usages、复用平台渠道与密钥池**。
//
// 三条不能破的约束：
//  1. 不新建供应商配置。模型与密钥就是"默认对话模型 + 它的绑定渠道"，
//     另存一份 base_url/api_key 等于第二个真相源（默认模型换渠道后 AI 还在用旧 key）。
//  2. 不走 /v1 的 HTTP 入口。那条路强绑计费预扣与 usages 落库，管理台调用
//     既不该扣额度也不该出现在用户的调用记录里。
//  3. 出字后绝不换渠道重试：前端会看到两段内容拼接，比直接失败更糟。

const (
	// llmTimeout 是单次 LLM 调用的上限（一轮工具循环里的每一次）。
	llmTimeout = 120 * time.Second
	// llmMaxCandidates 是首次出字前最多尝试的候选渠道数。
	llmMaxCandidates = 2
)

var (
	// ErrModelUnavailable 表示当前没有可用的对话模型（未配置/未绑定/适配器不支持对话）。
	ErrModelUnavailable = errors.New("agent: 没有可用的对话模型")
	// ErrUpstreamEmpty 表示上游 200 但一帧数据都没给。
	ErrUpstreamEmpty = errors.New("agent: 上游未返回任何内容")
)

// ToolCall 是一次完整的工具调用（分片已合并）。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
	// Truncated 表示上游声明本轮输出被长度上限截断（finish_reason=length）。
	//
	// 这一位必须单独带着走：截断导致的半截 JSON 与"模型写坏了 JSON"在文本上
	// 完全同形，若不加区分，模型会以为是自己数错括号，反复重写同样大小的参数。
	Truncated bool
}

// chatOutcome 是一轮 LLM 调用的结果。
type chatOutcome struct {
	Content   string
	ToolCalls []ToolCall
	// Reasoning 是推理型模型的思维链（`reasoning_content`）。
	//
	// 它与 Content 必须分开：思维链是过程、不是答复，混进 Content 会让
	// 落库的助手消息里出现一整段"自问自答"，而前端想折叠它也无从下手。
	Reasoning  string
	TokensIn   int
	TokensOut  int
	Estimated  bool
	Frames     int
	UpstreamID string
}

// Message 是发给上游的一条消息（只保留对话所需字段）。
type Message struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Extra      json.RawMessage `json:"-"`
}

// wireToolCall 是请求体里的 tool_calls 形状（assistant 回显用）。
type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec 是 function calling 的工具声明。
type ToolSpec struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// chatRequest 是发给上游的请求体。
type chatRequest struct {
	Model     string     `json:"model"`
	Messages  []Message  `json:"messages"`
	Tools     []ToolSpec `json:"tools,omitempty"`
	Stream    bool       `json:"stream"`
	MaxTokens int        `json:"max_tokens,omitempty"`
}

// StreamChat 跑一次流式对话，逐段回调正文与思维链增量。
//
// onDelta / onReasoning 返回错误（客户端断开）时立即中止；此时已产生的用量仍会回传，
// 调用方要按 aborted 记账 —— "断了就当作没发生"会让预算闸永远算不准。
func StreamChat(ctx context.Context, modelID string, messages []Message, tools []ToolSpec,
	maxTokens int, onDelta func(string) error, onReasoning func(string) error) (*chatOutcome, error) {

	if modelID == "" {
		return nil, ErrModelUnavailable
	}
	body, err := json.Marshal(chatRequest{
		// 平台对外模型 ID：upstream_model 由 RelayRequest.Model 单独传，
		// 混进 body 会让 request_patch / response_path 与计费口径一起漂。
		Model: modelID, Messages: messages, Tools: tools, Stream: true, MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, err
	}
	cands, err := relay.SelectCandidatesForAdmin(modelID)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, ErrModelUnavailable
	}

	var lastErr error
	tried := 0
	for i := range cands {
		if tried >= llmMaxCandidates {
			break
		}
		cand := cands[i]
		adapter, ok := relay.GetAdapter(cand.Channel.AdapterID)
		if !ok {
			lastErr = fmt.Errorf("agent: 渠道 %d 的适配器 %s 未注册", cand.Channel.ID, cand.Channel.AdapterID)
			continue
		}
		// 能力判定不能省：duomiapi 这类适配器存在但没有对话实现，
		// 只按"适配器已注册"放行的话，AI 会在运营敲完需求之后才失败。
		if sup, ok := adapter.(relay.EndpointSupporter); ok && !sup.SupportsEndpoint(relay.KindChat) {
			lastErr = fmt.Errorf("agent: 渠道 %d 的适配器不支持对话端点", cand.Channel.ID)
			continue
		}
		ref, err := relay.PickKey(cand.Channel, nil)
		if err != nil {
			lastErr = err
			continue
		}
		tried++
		cfg := relay.ChannelConfigFor(cand.Channel, ref.Key)
		req := &relay.RelayRequest{Model: cand.UpstreamModel, ModelID: modelID, Body: body}

		callCtx, cancel := context.WithTimeout(ctx, llmTimeout)
		out := &chatOutcome{}
		emitted := false
		agg := newToolAggregator()
		res, cerr := adapter.StreamChat(callCtx, cfg, req, func(f relay.SSEFrame) error {
			out.Frames++
			if u := f.Usage; u != nil {
				out.TokensIn = int(u.PromptTokens)
				out.TokensOut = int(u.CompletionTokens)
			}
			text, reason, err := agg.feed(f.Data)
			if err != nil {
				// 非 OpenAI 形状的帧：忽略而不是中断。规格适配器可能透传别的协议，
				// 直接失败会让"能跑但形状不同"的上游彻底不可用。
				return nil
			}
			if reason != "" {
				out.Reasoning += reason
				// 思维链一旦出字同样不能换渠道：换了就是两段不相干的推理拼在一起。
				emitted = true
				if onReasoning != nil {
					if err := onReasoning(reason); err != nil {
						return err
					}
				}
			}
			if text == "" {
				return nil
			}
			out.Content += text
			if !emitted {
				emitted = true
			}
			if onDelta != nil {
				return onDelta(text)
			}
			return nil
		})
		cancel()

		if res != nil {
			out.UpstreamID = res.UpstreamRequestID
			out.TokensIn = int(res.Usage.PromptTokens)
			out.TokensOut = int(res.Usage.CompletionTokens)
			if out.TokensIn == 0 && out.TokensOut == 0 && out.Content != "" {
				// 上游没给 usage：估算并标记，避免预算闸把免费调用算成 0。
				out.TokensIn = estimateTokens(messages)
				out.TokensOut = estimateTokensText(out.Content)
				out.Estimated = true
			}
			if res.ErrorMsg != "" && cerr != nil {
				out.Content = strings.TrimSpace(out.Content)
			}
		}
		out.ToolCalls = agg.result()

		if cerr != nil {
			// 已吐字：不换渠道（前端会看到两段拼接的内容），把错误如实回传。
			if emitted {
				return out, cerr
			}
			lastErr = cerr
			continue
		}
		if out.Frames == 0 && out.Content == "" && len(out.ToolCalls) == 0 && out.Reasoning == "" {
			lastErr = ErrUpstreamEmpty
			continue
		}
		return out, nil
	}
	if lastErr == nil {
		lastErr = ErrModelUnavailable
	}
	return nil, lastErr
}

// toolAggregator 按 index 合并 tool_calls 分片。
//
// 为什么必须按 index 聚合：OpenAI 兼容上游把一次调用拆成多帧，
// 每帧只带 `index` 与一段 `arguments` 字符串。按 name 聚合会在
// "同一轮里两次调用同名工具"时把参数拼串（得到一段非法 JSON）。
type toolAggregator struct {
	byIndex map[int]*ToolCall
	order   []int
	// finish 是本轮最后一次非空的 finish_reason（"stop"/"length"/…）。
	// 全仓只有这里能拿到它：早前解析出来就丢了，于是截断永远无法识别。
	finish string
}

func newToolAggregator() *toolAggregator {
	return &toolAggregator{byIndex: map[int]*ToolCall{}}
}

type deltaChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// 思维链增量。上游命名不统一：DeepSeek 与多数 OpenAI 兼容网关用
			// `reasoning_content`，也有网关只给 `reasoning`。两个字段都收，
			// 同一帧里取非空的那个 —— 只认一个会让一半供应商的思考块永远是空的。
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
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
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Error 是上游在流里回的错误帧（有些网关 200 + error 帧）。
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// feed 吃掉一帧，返回本帧的正文增量与思维链增量（两者可以同时为空）。
func (a *toolAggregator) feed(data []byte) (text, reasoning string, err error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return "", "", nil
	}
	var c deltaChunk
	if err := json.Unmarshal(trimmed, &c); err != nil {
		return "", "", err
	}
	var textBuf, reasonBuf strings.Builder
	for _, ch := range c.Choices {
		if ch.Delta.Content != "" {
			textBuf.WriteString(ch.Delta.Content)
		}
		if ch.Delta.ReasoningContent != "" {
			reasonBuf.WriteString(ch.Delta.ReasoningContent)
		} else if ch.Delta.Reasoning != "" {
			reasonBuf.WriteString(ch.Delta.Reasoning)
		}
		if ch.FinishReason != "" {
			a.finish = ch.FinishReason
		}
		for _, tc := range ch.Delta.ToolCalls {
			cur, ok := a.byIndex[tc.Index]
			if !ok {
				cur = &ToolCall{}
				a.byIndex[tc.Index] = cur
				a.order = append(a.order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			// 名字**首帧定名**，不累加：有的网关每帧都重发 function.name，
			// 累加会拼成 validate_draftvalidate_draft → unknown_tool，
			// 而症状和"模型调了不存在的工具"一模一样，排障方向会跑偏。
			if tc.Function.Name != "" && cur.Name == "" {
				cur.Name = tc.Function.Name
			}
			cur.Arguments += tc.Function.Arguments
		}
	}
	return textBuf.String(), reasonBuf.String(), nil
}

func (a *toolAggregator) result() []ToolCall {
	out := make([]ToolCall, 0, len(a.order))
	truncated := a.finish == "length"
	for _, idx := range a.order {
		cur := a.byIndex[idx]
		if cur == nil || strings.TrimSpace(cur.Name) == "" {
			continue
		}
		cur.Truncated = truncated
		out = append(out, *cur)
	}
	return out
}

// estimateToolsTokens 估算工具声明的 token 占用。
//
// 必须单独算：`tools` 不放进 messages（它是请求体的另一个字段），
// 但每一次请求都要带上它 —— 21 个工具声明的 JSON 实测 24KB，
// 按本文件的公式≈6k token。漏掉它，窗口预算就会系统性高估可用空间，
// 表现是"装配说装得下，上游说超了"。
func estimateToolsTokens(tools []ToolSpec) int {
	if len(tools) == 0 {
		return 0
	}
	buf, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return estimateTokensText(string(buf))
}

// estimateTokens 粗估输入 token。
//
// 口径：非 ASCII 按 2/3 token/字，ASCII 按 1/4 token/字节。
// **注意**：对中文这是**偏小**而不是偏大（多数中文分词 ≥0.6 token/字）。
// 之所以不调大：这个系数同时是预算闸的判据，一调大等于把所有会话的可用历史砍半，
// 而真实用量由上游 usage 回填（agent_runs.tokens_estimated 标明是否估算）。
func estimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTokensText(m.Content) + estimateTokensText(m.Name)
		for _, tc := range m.ToolCalls {
			total += estimateTokensText(tc.Function.Name) + estimateTokensText(tc.Function.Arguments)
		}
		total += 4 // 每条消息的角色/分隔符开销
	}
	return total
}

func estimateTokensText(s string) int {
	if s == "" {
		return 0
	}
	runes := 0
	ascii := 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			runes++
		}
	}
	n := runes*2/3 + ascii/4
	if n == 0 {
		n = 1
	}
	return n
}
