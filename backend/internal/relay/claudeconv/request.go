package claudeconv

import (
	"bytes"
	"encoding/json"
	"strings"
)

// maxStopSequences 是上游对 stop 序列数量的上限。
//
// 超过就截前 N 条而不是报错：多给几条停止词是用户的正常意图，
// 为此把整条请求打回 400 属于"我方实现限制"外溢成用户错误。
const maxStopSequences = 4

// maxUserIDLen 是 OpenAI `user` 字段的长度上限（超过会被部分实现直接 400）。
const maxUserIDLen = 64

// ── Anthropic 请求结构（只声明我们真的会读的字段） ──

type claudeRequest struct {
	Model       string          `json:"model"`
	MaxTokens   *int64          `json:"max_tokens"`
	System      json.RawMessage `json:"system"`
	Messages    []claudeMessage `json:"messages"`
	Tools       []claudeTool    `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	StopSeqs    []string        `json:"stop_sequences"`
	Stream      bool            `json:"stream"`
	Metadata    *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// image
	Source *claudeSource `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeSource struct {
	Type      string `json:"type"`       // base64 | url
	MediaType string `json:"media_type"` // image/jpeg 等（base64 时必填）
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type claudeTool struct {
	// Type 在 Anthropic 协议里通常省略（自定义工具）；
	// 带值时只接受 "custom" —— 它表示"这是用户自定义函数"，语义与 OpenAI 的 function 一致。
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ── OpenAI 请求结构（输出目标） ──

type openAIRequest struct {
	Model             string          `json:"model"`
	Messages          []openAIMessage `json:"messages"`
	MaxTokens         int64           `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stop              []string        `json:"stop,omitempty"`
	Tools             []openAITool    `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	User              string          `json:"user,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIToolFunc `json:"function"`
}

type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *openAIImageU `json:"image_url,omitempty"`
}

type openAIImageU struct {
	URL string `json:"url"`
}

// ToUpstream 把 Anthropic Messages 请求体转成 OpenAI Chat 请求体。
//
// 返回的 error 一定是 *ConvError 且 Status=400（调用方据此在**冻结之前**回拒绝，
// 一分钱不动）。无法等价表达的内容一律 400 而不是静默丢弃：
// 静默丢弃会让用户以为参数生效了（thinking 被计费却拿不到、document 被忽略却按图片计费）。
func (Codec) ToUpstream(body []byte) ([]byte, error) {
	var req claudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, badRequest("messages_invalid_json", "请求体不是合法 JSON：%s", err.Error())
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, badRequest("messages_model_required", "缺少 model 字段")
	}
	if req.MaxTokens == nil || *req.MaxTokens <= 0 {
		return nil, badRequest("messages_max_tokens_required",
			"缺少 max_tokens：该协议要求显式声明输出上限，请填一个正整数")
	}

	out := openAIRequest{
		Model:       req.Model,
		MaxTokens:   *req.MaxTokens,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        clipStop(req.StopSeqs),
	}
	if req.Metadata != nil {
		out.User = clipUserID(req.Metadata.UserID)
	}

	msgs, err := toOpenAIMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if sys, err := systemMessage(req.System); err != nil {
		return nil, err
	} else if sys != nil {
		msgs = append([]openAIMessage{*sys}, msgs...)
	}
	if len(msgs) == 0 {
		return nil, badRequest("messages_empty", "messages 不能为空")
	}
	out.Messages = msgs

	tools, err := toOpenAITools(req.Tools)
	if err != nil {
		return nil, err
	}
	out.Tools = tools

	choice, parallel, err := toToolChoice(req.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = choice
	out.ParallelToolCalls = parallel

	return json.Marshal(out)
}

// systemMessage 把顶层 system 转成首条系统消息。
//
// 支持两种形态：字符串，或 [{type:"text",text}] 数组（多段用空行拼接）。
// 出现非文本块直接 400：图片/文档型 system 无法等价表达，硬转会改变语义。
func systemMessage(raw json.RawMessage) (*openAIMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return &openAIMessage{Role: "system", Content: s}, nil
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, badRequest("messages_system_invalid", "system 只能是字符串或文本块数组")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return nil, badRequest("messages_system_unsupported_block",
				"system 里只支持文本块，不支持 %s", b.Type)
		}
		parts = append(parts, b.Text)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return &openAIMessage{Role: "system", Content: strings.Join(parts, "\n\n")}, nil
}

// toOpenAIMessages 展开 Claude 的消息数组。
//
// 三处容易写错：
//  1. tool_result 必须**拆成独立的 role=tool 消息**（OpenAI 不允许它挂在 user 里）；
//     一条 user 消息可能同时含文本与多个 tool_result，拆开后顺序不能乱；
//  2. tool_result 里夹带的图片无法进 tool 消息，只能另起一条 user 消息；
//  3. assistant 的 tool_use 要变成 tool_calls，且 arguments 必须是**字符串**化的 JSON。
func toOpenAIMessages(msgs []claudeMessage) ([]openAIMessage, error) {
	out := make([]openAIMessage, 0, len(msgs)*2)
	for _, m := range msgs {
		switch m.Role {
		case "user":
			converted, err := expandUserMessage(m.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, converted...)
		case "assistant":
			converted, err := expandAssistantMessage(m.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, converted...)
		default:
			// Claude 只有 user / assistant 两种角色（system 在顶层）。
			// 其它值原样透传反而危险：上游会按自己的规则解释，行为不可预期。
			return nil, badRequest("messages_role_invalid", "不支持的 role：%s（只支持 user / assistant）", m.Role)
		}
	}
	return out, nil
}

// expandUserMessage 把一条 user 消息展开成 1..N 条 OpenAI 消息。
func expandUserMessage(content json.RawMessage) ([]openAIMessage, error) {
	if s, ok, err := asPlainString(content); err != nil {
		return nil, err
	} else if ok {
		return []openAIMessage{{Role: "user", Content: s}}, nil
	}
	blocks, err := asBlocks(content)
	if err != nil {
		return nil, err
	}

	out := make([]openAIMessage, 0, len(blocks))
	var parts []openAIPart // 当前累积的 user 内容（文本 + 图片）
	var trailingImages []openAIPart
	flushUser := func() {
		if len(parts) == 0 {
			return
		}
		out = append(out, openAIMessage{Role: "user", Content: renderParts(parts)})
		parts = nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			parts = append(parts, openAIPart{Type: "text", Text: b.Text})
		case "image":
			p, err := imagePart(b.Source)
			if err != nil {
				return nil, err
			}
			parts = append(parts, p)
		case "tool_result":
			// 先落地已累积的 user 内容，保证「文本 → 工具结果 → 后续文本」的顺序不乱。
			flushUser()
			text, images, err := toolResultContent(b.Content)
			if err != nil {
				return nil, err
			}
			if b.IsError {
				text = "ERROR: " + text
			}
			out = append(out, openAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    text,
			})
			trailingImages = append(trailingImages, images...)
		case "document", "thinking", "redacted_thinking", "server_tool_use", "web_search_tool_result":
			// 明确拒绝而不是丢弃：这些块要么会产生费用（thinking），要么改变语义
			// （document 被当成图片计费/忽略），静默丢弃等于让用户为看不见的东西付钱。
			return nil, badRequest("messages_unsupported_block",
				"暂不支持 %s 内容块：请移除后再调用（本平台不会静默忽略它）", b.Type)
		default:
			return nil, badRequest("messages_unknown_block", "无法识别的 %s 内容块", b.Type)
		}
	}
	flushUser()
	// tool 消息里的图片无法表达，另起一条 user 消息追加在本轮末尾（顺序仍可读）。
	if len(trailingImages) > 0 {
		out = append(out, openAIMessage{Role: "user", Content: renderParts(trailingImages)})
	}
	if len(out) == 0 {
		return nil, badRequest("messages_empty_content", "user 消息的内容为空")
	}
	return out, nil
}

// expandAssistantMessage 把 assistant 消息转成一条 OpenAI assistant 消息。
func expandAssistantMessage(content json.RawMessage) ([]openAIMessage, error) {
	if s, ok, err := asPlainString(content); err != nil {
		return nil, err
	} else if ok {
		return []openAIMessage{{Role: "assistant", Content: s}}, nil
	}
	blocks, err := asBlocks(content)
	if err != nil {
		return nil, err
	}

	var texts []string
	var calls []openAIToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_use":
			args, err := argumentsOf(b.Input)
			if err != nil {
				return nil, err
			}
			calls = append(calls, openAIToolCall{
				ID: b.ID, Type: "function",
				Function: openAIFunctionCall{Name: b.Name, Arguments: args},
			})
		case "thinking", "redacted_thinking":
			// 历史里的思考块不参与上游推理：它只对 Anthropic 自己有意义。
			// 这里丢弃是安全的（不是本轮新生成的、不产生费用），但仍要求客户端
			// 在**当前请求**里不主动发送 thinking 配置（见 toOpenAITools 的说明）。
			continue
		default:
			return nil, badRequest("messages_unsupported_block",
				"assistant 消息里暂不支持 %s 内容块", b.Type)
		}
	}
	msg := openAIMessage{Role: "assistant"}
	if s := strings.Join(texts, ""); s != "" {
		msg.Content = s
	}
	if len(calls) > 0 {
		msg.ToolCalls = calls
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		return nil, badRequest("messages_empty_content", "assistant 消息的内容为空")
	}
	return []openAIMessage{msg}, nil
}

// imagePart 把 Claude 的 image 块转成 OpenAI 的 image_url 块。
func imagePart(src *claudeSource) (openAIPart, error) {
	if src == nil {
		return openAIPart{}, badRequest("messages_image_invalid", "image 块缺少 source")
	}
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return openAIPart{}, badRequest("messages_image_invalid", "image 块缺少 data")
		}
		mt := src.MediaType
		if mt == "" {
			return openAIPart{}, badRequest("messages_image_invalid", "base64 图片必须带 media_type")
		}
		switch mt {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
		default:
			return openAIPart{}, badRequest("messages_image_type",
				"不支持的图片类型 %s（只支持 image/jpeg、image/png、image/gif、image/webp）", mt)
		}
		return openAIPart{Type: "image_url", ImageURL: &openAIImageU{
			URL: "data:" + mt + ";base64," + src.Data,
		}}, nil
	case "url":
		if strings.TrimSpace(src.URL) == "" {
			return openAIPart{}, badRequest("messages_image_invalid", "image 块的 url 为空")
		}
		return openAIPart{Type: "image_url", ImageURL: &openAIImageU{URL: src.URL}}, nil
	default:
		// source.type=file 是 Anthropic 的文件引用（需要 files API），无法等价表达。
		return openAIPart{}, badRequest("messages_image_source",
			"不支持的图片来源 %s（只支持 base64 与 url）", src.Type)
	}
}

// toolResultContent 拆出工具结果的文本与图片。
func toolResultContent(raw json.RawMessage) (string, []openAIPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	if s, ok, err := asPlainString(raw); err != nil {
		return "", nil, err
	} else if ok {
		return s, nil, nil
	}
	blocks, err := asBlocks(raw)
	if err != nil {
		return "", nil, err
	}
	var texts []string
	var images []openAIPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			p, err := imagePart(b.Source)
			if err != nil {
				return "", nil, err
			}
			images = append(images, p)
		default:
			return "", nil, badRequest("messages_tool_result_block",
				"工具结果里暂不支持 %s 内容块", b.Type)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

// toOpenAITools 转换工具定义。
func toOpenAITools(tools []claudeTool) ([]openAITool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]openAITool, 0, len(tools))
	for _, t := range tools {
		// Anthropic 的内置工具（computer_2024xxxx / web_search_20250305 等）用
		// type 字段标明版本，平台无法实现，也不能当成同名自定义函数塞给上游。
		if t.Type != "" && t.Type != "custom" {
			return nil, badRequest("messages_tool_type",
				"暂不支持内置工具 %s：只支持自定义工具（type 省略或 custom）", t.Type)
		}
		if strings.TrimSpace(t.Name) == "" {
			return nil, badRequest("messages_tool_name", "工具定义缺少 name")
		}
		out = append(out, openAITool{
			Type: "function",
			Function: openAIToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out, nil
}

// toToolChoice 把 Anthropic 的 tool_choice 转成 OpenAI 写法。
//
// 返回的第二个值只在 disable_parallel_tool_use=true 时非 nil。
func toToolChoice(raw json.RawMessage) (json.RawMessage, *bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var c struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, nil, badRequest("messages_tool_choice_invalid", "tool_choice 格式不正确")
	}
	var parallel *bool
	if c.DisableParallelToolUse {
		f := false
		parallel = &f
	}
	switch c.Type {
	case "auto":
		return json.RawMessage(`"auto"`), parallel, nil
	case "any":
		// Anthropic 的 any = 必须调工具，对应 OpenAI 的 required。
		return json.RawMessage(`"required"`), parallel, nil
	case "none":
		return json.RawMessage(`"none"`), parallel, nil
	case "tool":
		if strings.TrimSpace(c.Name) == "" {
			return nil, nil, badRequest("messages_tool_choice_invalid", "tool_choice 为 tool 时必须给 name")
		}
		b, _ := json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": c.Name},
		})
		return b, parallel, nil
	default:
		return nil, nil, badRequest("messages_tool_choice_invalid",
			"不支持的 tool_choice.type：%s", c.Type)
	}
}

// argumentsOf 把 tool_use 的 input 序列化成 OpenAI 要求的**字符串**。
//
// 空值/null 一律给 "{}"：上游对 arguments 为空串的处理不一致
// （有的当空对象、有的直接 400），给 "{}" 是唯一确定的行为。
func argumentsOf(input json.RawMessage) (string, error) {
	if len(input) == 0 || string(input) == "null" {
		return "{}", nil
	}
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return "{}", nil
	}
	if trimmed[0] != '{' {
		return "", badRequest("messages_tool_input",
			"tool_use.input 必须是 JSON 对象（实际是 %s）", string(trimmed[:1]))
	}
	var probe map[string]any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return "", badRequest("messages_tool_input", "tool_use.input 不是合法 JSON 对象")
	}
	return string(trimmed), nil
}

// asPlainString 判断 content 是否为普通字符串形态。
func asPlainString(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", true, nil
	}
	if trimmed[0] != '"' {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", false, badRequest("messages_content_invalid", "content 不是合法字符串")
	}
	return s, true, nil
}

// asBlocks 把 content 解析成内容块数组。
func asBlocks(raw json.RawMessage) ([]claudeBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, badRequest("messages_content_invalid",
			"content 只能是字符串或内容块数组")
	}
	return blocks, nil
}

// renderParts 把累积的内容块渲染成 OpenAI 的 content。
//
// 纯文本时返回字符串而不是数组：多数上游对字符串形态兼容性最好，
// 而数组形态只在真的带图时才需要。
func renderParts(parts []openAIPart) any {
	if len(parts) == 0 {
		return ""
	}
	onlyText := true
	for _, p := range parts {
		if p.Type != "text" {
			onlyText = false
			break
		}
	}
	if onlyText {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	return parts
}

// clipStop 截断 stop 序列（超出的部分忽略而不是报错）。
func clipStop(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		out = append(out, s)
		if len(out) == maxStopSequences {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clipUserID 截断调用方标记（上游对长度有硬限制）。
func clipUserID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxUserIDLen {
		return s
	}
	return s[:maxUserIDLen]
}
