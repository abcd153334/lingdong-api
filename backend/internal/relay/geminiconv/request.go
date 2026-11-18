package geminiconv

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/relay/conv"
)

// 本文件是 Gemini 请求 → OpenAI Chat 请求的**唯一**映射点。
//
// 三类处置规则（先定规则再逐字段，避免逐个拍脑袋）：
//   - A 类：无法等价表达、且影响输出结构或计费 → 400，文案点名"该参数无法在 OpenAI 兼容上游表达"。
//     静默丢弃的后果是"参数看着生效、实际没生效"，用户拿到的结果与预期不符却查不出原因；
//     更糟的是缓存类参数（cachedContent）被忽略后仍按全价收费。
//   - B 类：只影响采样/过滤偏好、不改变输出结构 → 接受并忽略，逐条进文档的「不生效字段表」。
//   - C 类：纯客户端元数据 → 忽略，不入表。

// ── Gemini 请求结构（只声明我们真的会读的字段） ──

type geminiRequest struct {
	Model     string          `json:"model"`
	Contents  []geminiContent `json:"contents"`
	System    *geminiContent  `json:"systemInstruction"`
	Tools     []geminiTool    `json:"tools"`
	ToolConf  *geminiToolConf `json:"toolConfig"`
	GenConfig *geminiGenConf  `json:"generationConfig"`
	// 以下字段只用于判定「要不要 400」。
	CachedContent  string          `json:"cachedContent"`
	SafetySettings json.RawMessage `json:"safetySettings"`
	Labels         json.RawMessage `json:"labels"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart 同时用于**解析请求**与**渲染响应**，所以每个字段都带 omitempty：
// 响应里出现 `"thought":false,"inlineData":null` 这种空壳，Google SDK 会当成
// "显式声明了该字段"，部分版本会在 functionCall 与 inlineData 同时存在时直接报错。
// omitempty 不影响解析（缺失与零值在请求侧本来就等价）。
type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
	InlineData       *geminiBlob     `json:"inlineData,omitempty"`
	FileData         *geminiFileData `json:"fileData,omitempty"`
	FunctionCall     *geminiFuncCall `json:"functionCall,omitempty"`
	FunctionResp     *geminiFuncResp `json:"functionResponse,omitempty"`
	ExecutableCode   json.RawMessage `json:"executableCode,omitempty"`
	CodeExecResult   json.RawMessage `json:"codeExecutionResult,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFileData struct {
	MimeType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type geminiFuncCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiFuncResp struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiTool struct {
	FunctionDecls []geminiFuncDecl `json:"functionDeclarations"`
	// 服务端工具：无法在 OpenAI 兼容上游表达。
	GoogleSearch  json.RawMessage `json:"googleSearch"`
	CodeExecution json.RawMessage `json:"codeExecution"`
	URLContext    json.RawMessage `json:"urlContext"`
}

type geminiFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type geminiToolConf struct {
	FunctionCallingConfig *struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames"`
	} `json:"functionCallingConfig"`
}

type geminiGenConf struct {
	Temperature      *float64        `json:"temperature"`
	TopP             *float64        `json:"topP"`
	TopK             *int64          `json:"topK"`
	MaxOutputTokens  *int64          `json:"maxOutputTokens"`
	CandidateCount   *int64          `json:"candidateCount"`
	StopSequences    []string        `json:"stopSequences"`
	PresencePenalty  *float64        `json:"presencePenalty"`
	FrequencyPenalty *float64        `json:"frequencyPenalty"`
	Seed             *int64          `json:"seed"`
	ResponseMimeType string          `json:"responseMimeType"`
	ResponseSchema   json.RawMessage `json:"responseSchema"`
	ResponseModals   []string        `json:"responseModalities"`
	ThinkingConfig   *struct {
		ThinkingBudget  *int64 `json:"thinkingBudget"`
		IncludeThoughts bool   `json:"includeThoughts"`
	} `json:"thinkingConfig"`
}

// ── OpenAI Chat 请求结构（只声明我们会写的字段） ──

type openAIRequest struct {
	Model            string          `json:"model"`
	Messages         []openAIMessage `json:"messages"`
	MaxTokens        int64           `json:"max_tokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	N                int64           `json:"n,omitempty"`
	Stop             []string        `json:"stop,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	Tools            []openAITool    `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
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

// openAIPart 是多模态 content 数组里的一项。
type openAIPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *openAIImageU `json:"image_url,omitempty"`
}

type openAIImageU struct {
	URL string `json:"url"`
}

// ToUpstream 把 Gemini 请求体转成 OpenAI Chat 请求体。
//
// 返回的 error 一定是 *conv.Error 且 Status=400（调用方在**冻结之前**回拒绝，一分钱不动）。
func (c Codec) ToUpstream(body []byte) ([]byte, error) {
	if c.Embed {
		return c.embedToUpstream(body)
	}
	var req geminiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, conv.BadRequest("gemini_invalid_json", "请求体不是合法 JSON：%s", err.Error())
	}
	out := openAIRequest{Model: strings.TrimSpace(c.Model), Stream: c.Stream}
	if out.Model == "" {
		out.Model = strings.TrimSpace(req.Model)
	}
	if out.Model == "" {
		return nil, conv.BadRequest("gemini_model_required", "缺少 model：请在路径里写 /v1beta/models/<model>:generateContent")
	}

	// A 类字段必须在解析 contents 之前拦掉：先拦才能给出"哪个参数不行"，
	// 而不是让用户在一个更晚的、更含糊的错误上打转。
	if strings.TrimSpace(req.CachedContent) != "" {
		return nil, conv.BadRequest("gemini_cached_content_unsupported",
			"不支持 cachedContent：平台无法应用上下文缓存引用，静默忽略会按全价收费，请去掉它")
	}

	msgs, err := toMessages(req.Contents)
	if err != nil {
		return nil, err
	}
	if sys, err := systemMessage(req.System); err != nil {
		return nil, err
	} else if sys != nil {
		msgs = append([]openAIMessage{*sys}, msgs...)
	}
	if len(msgs) == 0 {
		return nil, conv.BadRequest("gemini_empty_contents", "contents 不能为空")
	}
	out.Messages = msgs

	tools, err := toTools(req.Tools)
	if err != nil {
		return nil, err
	}
	out.Tools = tools
	choice, err := toToolChoice(req.ToolConf)
	if err != nil {
		return nil, err
	}
	out.ToolChoice = choice

	if g := req.GenConfig; g != nil {
		out.Temperature = g.Temperature
		out.TopP = g.TopP
		// topK 是 B 类：只影响采样分布，OpenAI 兼容上游没有对应字段。
		// 接受并忽略，文档的「不生效字段表」里逐条写明（不静默、也不报错）。
		if g.MaxOutputTokens != nil && *g.MaxOutputTokens > 0 {
			out.MaxTokens = *g.MaxOutputTokens
		}
		if g.CandidateCount != nil && *g.CandidateCount > 0 {
			out.N = *g.CandidateCount
		}
		out.Stop = clipStop(g.StopSequences)
		out.PresencePenalty = g.PresencePenalty
		out.FrequencyPenalty = g.FrequencyPenalty
		out.Seed = g.Seed
		if len(g.ResponseModals) > 0 {
			// 只支持纯文本：图片输出是另一条链路（parts[].inlineData 的 base64），
			// 与 OpenAI 兼容上游的形状不同，硬转等于伪造能力。
			for _, m := range g.ResponseModals {
				if !strings.EqualFold(strings.TrimSpace(m), "TEXT") {
					return nil, conv.BadRequest("gemini_response_modality_unsupported",
						"responseModalities 只支持 [\"TEXT\"]：图片输出请改用 /v1/images/generations")
				}
			}
		}
		rf, err := responseFormatOf(g.ResponseMimeType, g.ResponseSchema)
		if err != nil {
			return nil, err
		}
		out.ResponseFormat = rf
		if tc := g.ThinkingConfig; tc != nil && tc.ThinkingBudget != nil {
			// 离散化到 reasoning_effort：上游只认这几档，精度必然丢失 —— 文档写明。
			out.ReasoningEffort = reasoningEffortOf(*tc.ThinkingBudget)
		}
	}
	return json.Marshal(out)
}

// reasoningEffortOf 把 Gemini 的 thinkingBudget（token 数）离散成 OpenAI 的四档。
//
// 阈值与 new-api 一致（0 / 4K / 16K），这样"同一份请求换网关"的结果可预期。
func reasoningEffortOf(budget int64) string {
	switch {
	case budget <= 0:
		return "minimal"
	case budget <= 4096:
		return "low"
	case budget <= 16384:
		return "medium"
	default:
		return "high"
	}
}

// responseFormatOf 把 responseMimeType/Schema 折成 OpenAI 的 response_format。
//
// 只认 application/json：其它 mime 无法等价表达（text/plain 是默认行为，不用写），
// 写错一个字就静默不生效，所以直接 400 让他改。
func responseFormatOf(mime string, schema json.RawMessage) (json.RawMessage, error) {
	switch strings.TrimSpace(mime) {
	case "":
		return nil, nil
	case "application/json", "application/json; charset=utf-8":
	default:
		return nil, conv.BadRequest("gemini_response_mime_unsupported",
			"responseMimeType 只支持 application/json（收到 %q）", mime)
	}
	if len(schema) == 0 || string(schema) == "null" {
		return json.Marshal(map[string]string{"type": "json_object"})
	}
	// 有 schema 时原样透传：上游认就生效，不认就由它自己 400 —— 网关不替它改写 schema。
	return json.Marshal(map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "gemini_response",
			"schema": schema,
		},
	})
}

// toMessages 把 contents 转成 OpenAI 消息数组。
//
// 关键：functionResponse 的上游形态是 `role:"tool"` 的独立消息，而 Gemini 把它
// 混在同一个 content 的 parts 里。同一段 parts 因此可能展开成**多条**消息
// （前面的文本/工具调用归 assistant，后面的函数结果各归一条 tool）。
func toMessages(contents []geminiContent) ([]openAIMessage, error) {
	var out []openAIMessage
	// callIDs 记录「本次会话里出现过的函数名 → 生成的 call id」。
	// functionResponse 只带 name 不带 id，而 OpenAI 的 tool 消息必须回指一个 id；
	// 找不到就 400（编一个 id 会让上游把结果挂到不存在的调用上，静默答非所问）。
	callIDs := map[string]string{}

	for _, ct := range contents {
		role, err := roleOf(ct.Role)
		if err != nil {
			return nil, err
		}
		var assistant *openAIMessage
		var texts []string
		var images []openAIPart

		assistantCall := func() *openAIMessage {
			if assistant == nil {
				assistant = &openAIMessage{Role: "assistant"}
			}
			return assistant
		}
		flush := func() {
			if assistant == nil {
				return
			}
			switch {
			case len(assistant.ToolCalls) > 0 && len(texts) == 0 && len(images) == 0:
				// 纯工具调用：content 留空（OpenAI 规范允许 null）。
			case len(images) > 0:
				parts := make([]openAIPart, 0, len(images)+len(texts))
				if t := strings.Join(texts, ""); t != "" {
					parts = append(parts, openAIPart{Type: "text", Text: t})
				}
				parts = append(parts, images...)
				assistant.Content = parts
			default:
				assistant.Content = strings.Join(texts, "")
			}
			out = append(out, *assistant)
			assistant = nil
			texts, images = nil, nil
		}

		for _, p := range ct.Parts {
			switch {
			case p.ExecutableCode != nil || p.CodeExecResult != nil:
				return nil, conv.BadRequest("gemini_code_execution_unsupported",
					"不支持 executableCode / codeExecutionResult，请改用自定义工具")
			case p.FunctionCall != nil:
				if role != "assistant" {
					return nil, conv.BadRequest("gemini_function_call_role",
						"functionCall 只能出现在 role=model 的 content 里（收到 %q）", ct.Role)
				}
				msg := assistantCall()
				args := "{}"
				if len(p.FunctionCall.Args) > 0 && string(p.FunctionCall.Args) != "null" {
					args = string(p.FunctionCall.Args)
				}
				id := "call_" + p.FunctionCall.Name
				msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
					ID: id, Type: "function",
					Function: openAIFunctionCall{Name: p.FunctionCall.Name, Arguments: args},
				})
				callIDs[p.FunctionCall.Name] = id
			case p.FunctionResp != nil:
				// 上游形态：独立的一条 role=tool 消息。
				flush()
				id, ok := callIDs[p.FunctionResp.Name]
				if !ok {
					return nil, conv.BadRequest("gemini_function_response_unmatched",
						"functionResponse(%q) 找不到对应的 functionCall：请在同一次请求里先给出调用", p.FunctionResp.Name)
				}
				content := "{}"
				if len(p.FunctionResp.Response) > 0 && string(p.FunctionResp.Response) != "null" {
					content = string(p.FunctionResp.Response)
				}
				out = append(out, openAIMessage{Role: "tool", ToolCallID: id, Content: content})
			case p.InlineData != nil:
				url, err := inlineDataURL(p.InlineData)
				if err != nil {
					return nil, err
				}
				if role == "assistant" {
					// 模型侧不接受图片入参：Gemini 允许回显，OpenAI 不允许，静默丢弃会改变上下文。
					return nil, conv.BadRequest("gemini_inline_data_role",
						"inlineData 只能出现在 role=user 的 content 里")
				}
				images = append(images, openAIPart{Type: "image_url", ImageURL: &openAIImageU{URL: url}})
			case p.FileData != nil:
				if err := checkImageMime(p.FileData.MimeType); err != nil {
					return nil, err
				}
				if strings.TrimSpace(p.FileData.FileURI) == "" {
					return nil, conv.BadRequest("gemini_file_uri_required", "fileData 缺少 fileUri")
				}
				images = append(images, openAIPart{Type: "image_url", ImageURL: &openAIImageU{URL: p.FileData.FileURI}})
			case p.Text != "":
				if p.Thought {
					// 思考内容不回流：它属于上一轮的推理草稿，混进对话会污染上下文。
					// （thoughtSignature 同理，属 C 类元数据，直接忽略。）
					continue
				}
				texts = append(texts, p.Text)
			}
		}
		// 纯文本 / 纯图片的 user 消息在这里落地；assistant 的残留由 flush 处理。
		if role == "assistant" {
			flush()
			continue
		}
		if assistant != nil {
			flush()
		}
		if len(texts) > 0 || len(images) > 0 {
			if len(images) == 0 {
				out = append(out, openAIMessage{Role: role, Content: strings.Join(texts, "")})
			} else {
				parts := make([]openAIPart, 0, len(images)+1)
				if t := strings.Join(texts, ""); t != "" {
					parts = append(parts, openAIPart{Type: "text", Text: t})
				}
				parts = append(parts, images...)
				out = append(out, openAIMessage{Role: role, Content: parts})
			}
		}
	}
	return out, nil
}

// roleOf 把 Gemini 角色折成 OpenAI 角色；未知角色 400。
//
// Gemini 只有 user / model 两种（另有少数 SDK 发 assistant，兼容处理）。
// 未知角色静默当 user 会让"系统提示"或工具结果错位，是最难查的一类错账。
func roleOf(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "", "user":
		return "user", nil
	case "model", "assistant":
		return "assistant", nil
	default:
		return "", conv.BadRequest("gemini_unknown_role",
			"未知的 content.role=%q：只支持 user 与 model", role)
	}
}

// inlineDataURL 把 base64 内联数据折成 data URL。
func inlineDataURL(b *geminiBlob) (string, error) {
	if err := checkImageMime(b.MimeType); err != nil {
		return "", err
	}
	if strings.TrimSpace(b.Data) == "" {
		return "", conv.BadRequest("gemini_inline_data_empty", "inlineData 缺少 data")
	}
	return "data:" + b.MimeType + ";base64," + b.Data, nil
}

// checkImageMime 只接受图片：音频/PDF 在 OpenAI 兼容上游没有等价入参，
// 硬转成 image_url 会让上游按图片解码失败，报一个与真实原因无关的错。
func checkImageMime(mime string) error {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/") {
		return nil
	}
	return conv.BadRequest("gemini_non_image_part",
		"只支持图片（mimeType 需以 image/ 开头），收到 %q：音频/文档请改用 /v1/audio/* 或文件上传", mime)
}

// systemMessage 把 systemInstruction 转成首条系统消息（多段用换行拼接）。
func systemMessage(sys *geminiContent) (*openAIMessage, error) {
	if sys == nil || len(sys.Parts) == 0 {
		return nil, nil
	}
	var texts []string
	for _, p := range sys.Parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
			continue
		}
		return nil, conv.BadRequest("gemini_system_non_text",
			"systemInstruction 只支持文本：图片/工具块无法等价表达")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	return &openAIMessage{Role: "system", Content: strings.Join(texts, "\n")}, nil
}

// toTools 把 functionDeclarations 转成 OpenAI tools。
func toTools(tools []geminiTool) ([]openAITool, error) {
	var out []openAITool
	for _, t := range tools {
		if len(t.GoogleSearch) > 0 || len(t.CodeExecution) > 0 || len(t.URLContext) > 0 {
			return nil, conv.BadRequest("gemini_server_tool_unsupported",
				"不支持服务端工具（googleSearch / codeExecution / urlContext），请改用自定义工具")
		}
		for _, fd := range t.FunctionDecls {
			if strings.TrimSpace(fd.Name) == "" {
				return nil, conv.BadRequest("gemini_tool_name_required", "functionDeclarations 缺少 name")
			}
			params := fd.Parameters
			if len(params) == 0 || string(params) == "null" {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out = append(out, openAITool{
				Type: "function",
				Function: openAIToolFunc{
					Name: fd.Name, Description: fd.Description, Parameters: params,
				},
			})
		}
	}
	return out, nil
}

// toToolChoice 把 functionCallingConfig 折成 OpenAI 的 tool_choice。
//
// AUTO → auto、NONE → none、ANY → required。ANY + 单个 allowedFunctionNames 时
// 精确指定那个函数（Gemini 的常见用法）；多个则 400 —— OpenAI 的 tool_choice
// 无法表达"只允许这几个里的任意一个"。
func toToolChoice(conf *geminiToolConf) (json.RawMessage, error) {
	if conf == nil || conf.FunctionCallingConfig == nil {
		return nil, nil
	}
	c := conf.FunctionCallingConfig
	mode := strings.ToUpper(strings.TrimSpace(c.Mode))
	switch mode {
	case "", "AUTO":
		return nil, nil
	case "NONE":
		return json.Marshal("none")
	case "ANY", "REQUIRED":
		if len(c.AllowedFunctionNames) == 1 {
			return json.Marshal(map[string]any{
				"type":     "function",
				"function": map[string]string{"name": c.AllowedFunctionNames[0]},
			})
		}
		if len(c.AllowedFunctionNames) > 1 {
			return nil, conv.BadRequest("gemini_tool_choice_multiple",
				"functionCallingConfig 只能指定一个 allowedFunctionNames；请只保留一个")
		}
		return json.Marshal("required")
	default:
		return nil, conv.BadRequest("gemini_tool_choice_unknown",
			"未知的 functionCallingConfig.mode=%q", c.Mode)
	}
}

// clipStop 截断停止序列（上游上限 4 条）。
//
// 超过就截前 N 条而不是报错：多给几条停止词是用户的正常意图，
// 为此把整条请求打回 400 属于"我方实现限制"外溢成用户错误。
func clipStop(in []string) []string {
	const maxStop = 4
	if len(in) == 0 {
		return nil
	}
	if len(in) > maxStop {
		in = in[:maxStop]
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
