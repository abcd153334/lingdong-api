package geminiconv

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/relay/conv"
)

// 本文件守住 Gemini ↔ OpenAI 的字段映射。这些用例的意义在真机上体现为
// "参数生效了 / 没生效"与"金额对 / 不对"，出错时几乎无法从现象反推字段，
// 所以只能在这里逐条钉住。

func TestToUpstreamBasic(t *testing.T) {
	body := `{
		"systemInstruction":{"parts":[{"text":"你是助手"},{"text":"回答要简短"}]},
		"contents":[{"role":"user","parts":[{"text":"你好"}]}],
		"generationConfig":{"temperature":0.5,"topP":0.8,"topK":40,"maxOutputTokens":128,
			"stopSequences":["END"],"presencePenalty":0.1,"frequencyPenalty":0.2,"seed":7}
	}`
	out, err := Codec{Model: "gemini-2.0-flash"}.ToUpstream([]byte(body))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物不是合法 JSON: %v", err)
	}
	if got["model"] != "gemini-2.0-flash" {
		t.Fatalf("model 应取自路径，实际 %v", got["model"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应展开成 system+user 两条消息，实际 %d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "你是助手\n回答要简短" {
		t.Fatalf("systemInstruction 映射不符: %+v", sys)
	}
	if got["max_tokens"] != float64(128) || got["temperature"] != 0.5 || got["top_p"] != 0.8 {
		t.Fatalf("generationConfig 映射不符: %+v", got)
	}
	// topK 是 B 类：接受并忽略（不进产物）。
	if _, exists := got["top_k"]; exists {
		t.Fatalf("topK 不应出现在产物里: %+v", got)
	}
	if got["seed"] != float64(7) {
		t.Fatalf("seed 应透传: %+v", got)
	}
}

func TestToUpstreamToolRoundTrip(t *testing.T) {
	body := `{
		"contents":[
			{"role":"user","parts":[{"text":"北京天气"}]},
			{"role":"model","parts":[{"functionCall":{"name":"weather","args":{"city":"北京"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"temp":3}}},{"text":"谢谢"}]}
		],
		"tools":[{"functionDeclarations":[{"name":"weather","description":"查天气",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["weather"]}}
	}`
	out, err := Codec{Model: "m"}.ToUpstream([]byte(body))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var got struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice any `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物解析失败: %v", err)
	}
	// user / assistant(带 tool_calls) / tool / user(文本)
	if len(got.Messages) != 4 {
		t.Fatalf("消息条数不符: %d → %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[1].Role != "assistant" || len(got.Messages[1].ToolCalls) != 1 {
		t.Fatalf("functionCall 应转成 assistant.tool_calls: %+v", got.Messages[1])
	}
	if got.Messages[1].ToolCalls[0].Function.Name != "weather" ||
		!strings.Contains(got.Messages[1].ToolCalls[0].Function.Arguments, "北京") {
		t.Fatalf("工具名/参数不符: %+v", got.Messages[1].ToolCalls[0])
	}
	if got.Messages[2].Role != "tool" || got.Messages[2].ToolCallID != got.Messages[1].ToolCalls[0].ID {
		t.Fatalf("functionResponse 应转成 role=tool 且 id 对齐: %+v", got.Messages[2])
	}
	if got.Messages[3].Role != "user" || got.Messages[3].Content != "谢谢" {
		t.Fatalf("同段文本应在 functionResponse 之后单独成消息: %+v", got.Messages[3])
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Function.Name != "weather" {
		t.Fatalf("tools 映射不符: %+v", got.Tools)
	}
	// ANY + 单个 allowed → 精确指定那个函数
	var ch map[string]any
	raw, _ := json.Marshal(got.ToolChoice)
	_ = json.Unmarshal(raw, &ch)
	if ch["type"] != "function" {
		t.Fatalf("tool_choice 应为精确函数: %s", raw)
	}
}

func TestToUpstreamImages(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[
		{"text":"这是什么"},
		{"inlineData":{"mimeType":"image/png","data":"AAAA"}}
	]}]}`
	out, err := Codec{Model: "m"}.ToUpstream([]byte(body))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var got struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物解析失败: %v", err)
	}
	parts := got.Messages[0].Content
	if len(parts) != 2 || parts[1].Type != "image_url" ||
		parts[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Fatalf("inlineData 应折成 data URL: %+v", parts)
	}
}

func TestToUpstreamRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"缓存内容", `{"cachedContent":"cachedContents/x","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
		{"服务端工具", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"googleSearch":{}}]}`},
		{"代码执行", `{"contents":[{"role":"user","parts":[{"executableCode":{"language":"PYTHON"}}]}]}`},
		{"非图片内联", `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"audio/mp3","data":"AA"}}]}]}`},
		{"未知角色", `{"contents":[{"role":"system","parts":[{"text":"hi"}]}]}`},
		{"非文本 responseModalities", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`},
		{"非 json 的 responseMimeType", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"text/xml"}}`},
		{"多个 allowedFunctions", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["a","b"]}}}`},
		{"空 contents", `{"contents":[]}`},
	}
	for _, c := range cases {
		_, err := Codec{Model: "m"}.ToUpstream([]byte(c.body))
		if err == nil {
			t.Errorf("%s 应被拒绝（静默忽略会改变语义/计费）", c.name)
			continue
		}
		ce, ok := err.(*conv.Error)
		if !ok || ce.Status != 400 {
			t.Errorf("%s 应为 400 转换错误，实际 %T %v", c.name, err, err)
		}
	}
}

func TestToClient(t *testing.T) {
	body := `{"choices":[{"message":{"content":"你好","reasoning_content":"先想想",
		"tool_calls":[{"id":"call_1","function":{"name":"weather","arguments":"{\"city\":\"北京\"}"}}]},
		"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":100,"completion_tokens":40,
			"prompt_tokens_details":{"cached_tokens":25},
			"completion_tokens_details":{"reasoning_tokens":10}}}`
	out, cerr := Codec{}.ToClient([]byte(body), "gemini-2.0-flash")
	if cerr != nil {
		t.Fatalf("转换失败: %v", cerr)
	}
	var got geminiResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物解析失败: %v", err)
	}
	if got.ModelVersion != "models/gemini-2.0-flash" {
		t.Fatalf("modelVersion 不符: %s", got.ModelVersion)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("候选/终止原因不符: %+v", got.Candidates)
	}
	parts := got.Candidates[0].Content.Parts
	if len(parts) != 3 {
		t.Fatalf("应有三段（thought/正文/工具调用），实际 %+v", parts)
	}
	if !parts[0].Thought || parts[0].Text != "先想想" {
		t.Fatalf("thinking 应折成 thought part: %+v", parts[0])
	}
	if parts[1].Text != "你好" {
		t.Fatalf("正文映射不符: %+v", parts[1])
	}
	if parts[2].FunctionCall == nil || parts[2].FunctionCall.Name != "weather" ||
		!strings.Contains(string(parts[2].FunctionCall.Args), "北京") {
		t.Fatalf("工具调用映射不符: %+v", parts[2].FunctionCall)
	}
	u := got.UsageMetadata
	if u == nil || u.PromptTokenCount != 100 || u.CandidatesTokenCount != 30 ||
		u.ThoughtsTokenCount != 10 || u.CachedContentTokenCount != 25 || u.TotalTokenCount != 140 {
		t.Fatalf("usageMetadata 不符: %+v", u)
	}
}

func TestToClientBadToolArgsIs502(t *testing.T) {
	body := `{"choices":[{"message":{"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{oops"}}]}}]}`
	_, cerr := Codec{}.ToClient([]byte(body), "m")
	if cerr == nil || cerr.Status != 502 {
		t.Fatalf("坏的 args 应为 502（上游违约，需释放冻结），实际 %+v", cerr)
	}
}

func TestToClientEmptyChoicesIsBlocked(t *testing.T) {
	out, cerr := Codec{}.ToClient([]byte(`{"choices":[]}`), "m")
	if cerr != nil {
		t.Fatalf("空候选不应报错: %v", cerr)
	}
	var got geminiResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物解析失败: %v", err)
	}
	if got.PromptFeedback == nil || got.PromptFeedback.BlockReason != "SAFETY" {
		t.Fatalf("空候选应报 SAFETY 拦截: %+v", got)
	}
}

// ── 流式 ──

type recWriter struct {
	events []string
	data   [][]byte
}

func (w *recWriter) write(event string, data []byte) error {
	w.events = append(w.events, event)
	w.data = append(w.data, append([]byte(nil), data...))
	return nil
}

func TestStreamTextAndClose(t *testing.T) {
	w := &recWriter{}
	enc := Codec{Model: "m"}.NewStream("m", w.write)
	chunks := []string{
		`{"choices":[{"delta":{"content":"你"}}]}`,
		`{"choices":[{"delta":{"content":"好"}}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
	}
	for _, c := range chunks {
		if err := enc.OnChunk([]byte(c)); err != nil {
			t.Fatalf("OnChunk 失败: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	// 2 个增量帧 + 1 个终止帧
	if len(w.data) != 3 {
		t.Fatalf("帧数不符: %d", len(w.data))
	}
	if w.events[0] != "" || w.events[2] != "" {
		t.Fatalf("Gemini 流不应带 event 行: %+v", w.events)
	}
	var first geminiResponse
	_ = json.Unmarshal(w.data[0], &first)
	if first.Candidates[0].Content.Parts[0].Text != "你" || first.Candidates[0].FinishReason != "" {
		t.Fatalf("增量帧不符: %s", w.data[0])
	}
	var last geminiResponse
	_ = json.Unmarshal(w.data[2], &last)
	if last.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("终止帧应带 finishReason: %s", w.data[2])
	}
	if last.UsageMetadata == nil || last.UsageMetadata.PromptTokenCount != 10 ||
		last.UsageMetadata.CandidatesTokenCount != 4 {
		t.Fatalf("终止帧应带 usageMetadata: %s", w.data[2])
	}
}

func TestStreamToolCallsBuffered(t *testing.T) {
	w := &recWriter{}
	enc := Codec{}.NewStream("m", w.write)
	// 分片：先 name 后 args，分两帧到达（拼起来才是 {"city":"北京"}）。
	first := `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`
	second := `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":"tool_calls"}]}`
	if err := enc.OnChunk([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if err := enc.OnChunk([]byte(second)); err != nil {
		t.Fatal(err)
	}
	// 分片阶段不应发出任何帧：Gemini 的 functionCall 是完整对象。
	if len(w.data) != 0 {
		t.Fatalf("分片阶段不应发帧，实际 %d 帧: %s", len(w.data), w.data[0])
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if len(w.data) != 2 {
		t.Fatalf("Close 应发出工具帧 + 终止帧，实际 %d", len(w.data))
	}
	var tool geminiResponse
	_ = json.Unmarshal(w.data[0], &tool)
	fc := tool.Candidates[0].Content.Parts[0].FunctionCall
	if fc == nil || fc.Name != "weather" || !strings.Contains(string(fc.Args), "北京") {
		t.Fatalf("工具调用应完整: %s", w.data[0])
	}
	var last geminiResponse
	_ = json.Unmarshal(w.data[1], &last)
	if last.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("tool_calls 应折成 STOP: %s", w.data[1])
	}
}

func TestStreamFailShape(t *testing.T) {
	w := &recWriter{}
	enc := Codec{}.NewStream("m", w.write)
	if err := enc.Fail("上游断了"); err != nil {
		t.Fatal(err)
	}
	if len(w.data) != 1 || !strings.Contains(string(w.data[0]), `"status":"INTERNAL"`) {
		t.Fatalf("Fail 应写出 Gemini 形状的错误帧: %s", w.data)
	}
	// 已失败后再 Close 不得再发帧（客户端会收到两条终止）。
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if len(w.data) != 1 {
		t.Fatalf("Fail 后 Close 不应再写: %d", len(w.data))
	}
}
