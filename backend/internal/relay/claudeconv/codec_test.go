package claudeconv

import (
	"encoding/json"
	"strings"
	"testing"
)

// ───────────────────── 请求转换 ─────────────────────

func TestClaudeToOpenAISystemArray(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":100,
		"system":[{"type":"text","text":"你是助手"},{"type":"text","text":"回答要简短"}],
		"messages":[{"role":"user","content":"你好"}]}`)
	out, err := (Codec{}).ToUpstream(body)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var got openAIRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("消息数=%d，期望 system + user 两条", len(got.Messages))
	}
	if got.Messages[0].Role != "system" {
		t.Fatalf("首条必须是 system，实际 %q", got.Messages[0].Role)
	}
	if s, _ := got.Messages[0].Content.(string); s != "你是助手\n\n回答要简短" {
		t.Fatalf("多段 system 应以空行拼接，实际 %q", s)
	}
}

func TestClaudeToOpenAISystemRejectsImage(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,
		"system":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}],
		"messages":[{"role":"user","content":"hi"}]}`)
	_, err := (Codec{}).ToUpstream(body)
	ce, ok := err.(*ConvError)
	if !ok || ce.Status != 400 {
		t.Fatalf("system 里的图片块应 400，实际 %v", err)
	}
}

func TestClaudeToOpenAIToolRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"m","max_tokens":100,
		"tools":[{"name":"read_file","description":"读文件","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":"读一下 a.txt"},
			{"role":"assistant","content":[
				{"type":"text","text":"我来读"},
				{"type":"tool_use","id":"tu_1","name":"read_file","input":{"path":"a.txt"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"文件内容"}]},
				{"type":"text","text":"继续"}]}]}`)
	out, err := (Codec{}).ToUpstream(body)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var got openAIRequest
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "read_file" {
		t.Fatalf("工具定义未转换: %+v", got.Tools)
	}
	if !strings.Contains(string(got.Tools[0].Function.Parameters), "path") {
		t.Fatal("input_schema 应映射为 parameters")
	}

	// 期望消息序列：user → assistant(带 tool_calls) → tool → user(继续)
	roles := make([]string, 0, len(got.Messages))
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "tool", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("角色序列=%v，期望 %v（tool_result 必须拆成独立 tool 消息且保持顺序）", roles, want)
	}
	asst := got.Messages[1]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "tu_1" {
		t.Fatalf("assistant 的 tool_use 应转成 tool_calls: %+v", asst.ToolCalls)
	}
	if asst.ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("arguments 应是字符串化 JSON，实际 %q", asst.ToolCalls[0].Function.Arguments)
	}
	if got.Messages[2].ToolCallID != "tu_1" {
		t.Fatalf("tool 消息应带 tool_call_id: %+v", got.Messages[2])
	}
	// 文本在 tool_result 之后仍回到 user 消息（顺序不能乱）。
	if s, _ := got.Messages[3].Content.(string); s != "继续" {
		t.Fatalf("tool_result 之后的文本应留在 user 消息，实际 %q", s)
	}
}

func TestClaudeToOpenAIToolResultWithImage(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"text","text":"截图"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}]}]}`)
	out, err := (Codec{}).ToUpstream(body)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var got openAIRequest
	_ = json.Unmarshal(out, &got)
	// tool 消息 + 另起的 user 图片消息
	if len(got.Messages) != 2 {
		t.Fatalf("消息数=%d，期望 tool + user(图片)", len(got.Messages))
	}
	if got.Messages[0].Role != "tool" {
		t.Fatalf("首条应为 tool，实际 %q", got.Messages[0].Role)
	}
	if got.Messages[1].Role != "user" {
		t.Fatalf("图片应另起 user 消息，实际 %q", got.Messages[1].Role)
	}
	parts, ok := got.Messages[1].Content.([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("user 消息应含一个图片块，实际 %#v", got.Messages[1].Content)
	}
	part := parts[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatalf("图片块类型应为 image_url，实际 %v", part["type"])
	}
	url := part["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("base64 图片应转成 data URI，实际 %q", url)
	}
}

func TestClaudeToOpenAIRejectsThinking(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":[
			{"type":"thinking","thinking":"想一想"}]}]}`)
	_, err := (Codec{}).ToUpstream(body)
	ce, ok := err.(*ConvError)
	if !ok || ce.Status != 400 {
		t.Fatalf("thinking 块应 400，实际 %v", err)
	}
	if !strings.Contains(ce.Message, "thinking") {
		t.Fatalf("文案应点名不支持的类型，实际 %q", ce.Message)
	}
}

func TestClaudeToOpenAIRejectsUnknownBlock(t *testing.T) {
	for _, typ := range []string{"document", "server_tool_use", "web_search_tool_result"} {
		body := []byte(`{"model":"m","max_tokens":10,"messages":[
			{"role":"user","content":[{"type":"` + typ + `"}]}]}`)
		if _, err := (Codec{}).ToUpstream(body); err == nil {
			t.Fatalf("%s 应被拒绝（静默丢弃会让用户为看不见的东西付钱）", typ)
		}
	}
}

func TestClaudeToOpenAITopK(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := (Codec{}).ToUpstream(body)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if strings.Contains(string(out), "top_k") {
		t.Fatalf("top_k 不得出现在上游请求里（不伪造）：%s", out)
	}
}

func TestClaudeToOpenAIRequiresMaxTokens(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		_, err := (Codec{}).ToUpstream([]byte(body))
		ce, ok := err.(*ConvError)
		if !ok || ce.Status != 400 || ce.Code != "messages_max_tokens_required" {
			t.Fatalf("缺 max_tokens 应 400，实际 %v", err)
		}
	}
}

func TestClaudeToOpenAIToolChoice(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"none"}`, `"none"`},
		{`{"type":"tool","name":"read_file"}`, `{"function":{"name":"read_file"},"type":"function"}`},
	}
	for _, c := range cases {
		body := []byte(`{"model":"m","max_tokens":10,"tool_choice":` + c.in +
			`,"messages":[{"role":"user","content":"hi"}]}`)
		out, err := (Codec{}).ToUpstream(body)
		if err != nil {
			t.Fatalf("%s 转换失败: %v", c.in, err)
		}
		var got struct {
			ToolChoice json.RawMessage `json:"tool_choice"`
		}
		_ = json.Unmarshal(out, &got)
		// 对象形态的键序由 encoding/json 决定，逐字节比较不稳；统一解析后比较。
		var a, b any
		_ = json.Unmarshal(got.ToolChoice, &a)
		_ = json.Unmarshal([]byte(c.want), &b)
		aj, _ := json.Marshal(a)
		bj, _ := json.Marshal(b)
		if string(aj) != string(bj) {
			t.Fatalf("%s → %s，期望 %s", c.in, aj, bj)
		}
	}
}

func TestClaudeToOpenAIStopClipped(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":10,"stop_sequences":["a","b","c","d","e","f"],
		"messages":[{"role":"user","content":"hi"}]}`)
	out, err := (Codec{}).ToUpstream(body)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var got openAIRequest
	_ = json.Unmarshal(out, &got)
	if len(got.Stop) != maxStopSequences {
		t.Fatalf("stop 应截到 %d 条，实际 %d", maxStopSequences, len(got.Stop))
	}
}

// ───────────────────── 响应转换 ─────────────────────

func TestOpenAIToClaudeUsageMath(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"},
		"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1000,"completion_tokens":50,
		"prompt_tokens_details":{"cached_tokens":400}}}`)
	out, cerr := (Codec{}).ToClient(body, "claude-x")
	if cerr != nil {
		t.Fatalf("转换失败: %v", cerr)
	}
	var got claudeResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got.Usage.InputTokens != 600 {
		t.Fatalf("input_tokens=%d，期望 600（prompt 1000 减去缓存 400）", got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 400 {
		t.Fatalf("cache_read=%d，期望 400", got.Usage.CacheReadInputTokens)
	}
	if got.Usage.OutputTokens != 50 {
		t.Fatalf("output_tokens=%d，期望 50", got.Usage.OutputTokens)
	}
	if got.Model != "claude-x" {
		t.Fatalf("model 应回填客户端请求的名字，实际 %q", got.Model)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "hi" {
		t.Fatalf("正文块不正确: %+v", got.Content)
	}
}

func TestOpenAIToClaudeStopReason(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens",
		"tool_calls": "tool_use", "function_call": "tool_use",
		"content_filter": "refusal", "": "end_turn",
	}
	for finish, want := range cases {
		body := []byte(`{"id":"x","choices":[{"message":{"content":"a"},"finish_reason":"` + finish + `"}]}`)
		out, cerr := (Codec{}).ToClient(body, "m")
		if cerr != nil {
			t.Fatalf("%s 转换失败: %v", finish, cerr)
		}
		var got claudeResponse
		_ = json.Unmarshal(out, &got)
		if got.StopReason != want {
			t.Fatalf("finish_reason=%q → %q，期望 %q", finish, got.StopReason, want)
		}
	}
}

func TestOpenAIToClaudeToolArgumentsEmpty(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"message":{"content":null,
		"tool_calls":[{"id":"c1","function":{"name":"f","arguments":""}}]},"finish_reason":"tool_calls"}]}`)
	out, cerr := (Codec{}).ToClient(body, "m")
	if cerr != nil {
		t.Fatalf("空 arguments 应被容忍: %v", cerr)
	}
	var got claudeResponse
	_ = json.Unmarshal(out, &got)
	if len(got.Content) != 1 || got.Content[0].Type != "tool_use" {
		t.Fatalf("应产出 tool_use 块: %+v", got.Content)
	}
	if string(got.Content[0].Input) != "{}" {
		t.Fatalf("空 arguments 应给 {}，实际 %s", got.Content[0].Input)
	}
}

func TestOpenAIToClaudeInvalidToolArguments(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"message":{"content":null,
		"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"not-json"}}]}}]}`)
	_, cerr := (Codec{}).ToClient(body, "m")
	if cerr == nil || cerr.Status != 502 {
		t.Fatalf("非法参数应 502（释放冻结），实际 %v", cerr)
	}
}

func TestOpenAIToClaudeRejectsEmptyChoices(t *testing.T) {
	for _, body := range []string{`{"id":"x","choices":[]}`, `{"id":"x"}`, `not json`} {
		if _, cerr := (Codec{}).ToClient([]byte(body), "m"); cerr == nil || cerr.Status != 502 {
			t.Fatalf("无 choices 的响应应 502，实际 %v（body=%s）", cerr, body)
		}
	}
}

// ───────────────────── 流式编码 ─────────────────────

type captured struct {
	events []string
	data   []string
}

func (c *captured) write(event string, data []byte) error {
	c.events = append(c.events, event)
	c.data = append(c.data, string(data))
	return nil
}

func newCapture(t *testing.T) (*streamEncoder, *captured) {
	t.Helper()
	c := &captured{}
	return (Codec{}).NewStream("claude-x", c.write).(*streamEncoder), c
}

func TestClaudeStreamEventOrder(t *testing.T) {
	enc, cap := newCapture(t)
	chunks := []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"世界"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, c := range chunks {
		if err := enc.OnChunk([]byte(c)); err != nil {
			t.Fatalf("处理帧失败: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(cap.events, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序列不正确:\n got=%v\nwant=%v", cap.events, want)
	}
}

func TestClaudeStreamToolIndexIsDense(t *testing.T) {
	enc, cap := newCapture(t)
	// 上游给了 index 0 与 3（中间缺失），客户端必须看到 1 与 2（文本块占了 0）。
	chunks := []string{
		`{"choices":[{"delta":{"content":"先说话"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":3,"id":"c3","function":{"name":"b","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	for _, c := range chunks {
		if err := enc.OnChunk([]byte(c)); err != nil {
			t.Fatalf("处理帧失败: %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	var indexes []int
	for i, e := range cap.events {
		if e == "content_block_start" {
			var payload struct {
				Index int `json:"index"`
			}
			_ = json.Unmarshal([]byte(cap.data[i]), &payload)
			indexes = append(indexes, payload.Index)
		}
	}
	if len(indexes) != 3 {
		t.Fatalf("应有 1 个文本块 + 2 个工具块，实际 %d 个: %v", len(indexes), indexes)
	}
	for i, idx := range indexes {
		if idx != i {
			t.Fatalf("块序号必须稠密（0,1,2…），实际 %v", indexes)
		}
	}
}

func TestClaudeStreamToolArgsBufferedUntilName(t *testing.T) {
	enc, cap := newCapture(t)
	// arguments 先到，id/name 后到：不能先开块，否则会发出无名工具。
	if err := enc.OnChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	for i, e := range cap.events {
		if e == "content_block_start" {
			t.Fatalf("id/name 未到就开了块: %v", cap.data[i])
		}
	}
	if err := enc.OnChunk([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f"}}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	// 开块后必须把之前缓冲的参数补发出来。
	foundArgs := false
	for i, e := range cap.events {
		if e == "content_block_delta" && strings.Contains(cap.data[i], `{\"a\":`) {
			foundArgs = true
		}
	}
	if !foundArgs {
		t.Fatalf("缓冲的参数分片应补发，实际事件: %v / %v", cap.events, cap.data)
	}
}

func TestClaudeStreamUsageInMessageDelta(t *testing.T) {
	enc, cap := newCapture(t)
	if err := enc.OnChunk([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`)); err != nil {
		t.Fatal(err)
	}
	// 末帧只带 usage（choices 为空）
	if err := enc.OnChunk([]byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,
		"prompt_tokens_details":{"cached_tokens":30}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	var start, delta map[string]any
	for i, e := range cap.events {
		var payload map[string]any
		_ = json.Unmarshal([]byte(cap.data[i]), &payload)
		switch e {
		case "message_start":
			start = payload
		case "message_delta":
			delta = payload
		}
	}
	if start == nil || delta == nil {
		t.Fatal("缺少 message_start 或 message_delta")
	}
	su := start["message"].(map[string]any)["usage"].(map[string]any)
	if su["input_tokens"].(float64) != 0 {
		t.Fatalf("message_start 的 input_tokens 应为 0（诚实值），实际 %v", su["input_tokens"])
	}
	du := delta["usage"].(map[string]any)
	if du["input_tokens"].(float64) != 70 { // 100 - 30
		t.Fatalf("message_delta 的 input_tokens=%v，期望 70", du["input_tokens"])
	}
	if du["output_tokens"].(float64) != 20 || du["cache_read_input_tokens"].(float64) != 30 {
		t.Fatalf("message_delta 用量不正确: %v", du)
	}
}

func TestClaudeSSEHasEventLines(t *testing.T) {
	// 直接验证事件名非空且不含 [DONE]（[DONE] 是 OpenAI 的结束标记，
	// Claude 客户端不认识它，收到会把整条流判成异常）。
	enc, cap := newCapture(t)
	if err := enc.OnChunk([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := enc.OnChunk([]byte(`[DONE]`)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	for _, e := range cap.events {
		if e == "" {
			t.Fatal("每个事件都必须有事件名")
		}
	}
	for _, d := range cap.data {
		if strings.Contains(d, "[DONE]") {
			t.Fatalf("不应出现 [DONE]: %s", d)
		}
	}
	if cap.events[0] != "message_start" {
		t.Fatalf("首事件必须是 message_start，实际 %q", cap.events[0])
	}
	if cap.events[len(cap.events)-1] != "message_stop" {
		t.Fatalf("末事件必须是 message_stop，实际 %q", cap.events[len(cap.events)-1])
	}
}

func TestClaudeStreamFailEmitsErrorEvent(t *testing.T) {
	enc, cap := newCapture(t)
	if err := enc.OnChunk([]byte(`{"choices":[{"delta":{"content":"半截"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Fail("连接中断"); err != nil {
		t.Fatal(err)
	}
	last := cap.events[len(cap.events)-1]
	if last != "error" {
		t.Fatalf("异常收尾应发 error 事件，实际 %q", last)
	}
	if strings.Contains(cap.data[len(cap.data)-1], "message_stop") {
		t.Fatal("异常收尾不应发 message_stop（客户端会把失败当成功）")
	}
}
