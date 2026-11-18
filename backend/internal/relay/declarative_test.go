package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

func TestParseAdapterConfigRejectsUnknownField(t *testing.T) {
	// 拼写错误（request_patchs）必须在保存时报错，否则运营以为配了其实没配。
	if _, err := ParseAdapterConfig([]byte(`{"request_patchs":{}}`)); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
}

func TestParseAdapterConfigEmptyIsZero(t *testing.T) {
	for _, raw := range []string{"", "null", "  ", "{}"} {
		cfg, err := ParseAdapterConfig([]byte(raw))
		if err != nil {
			t.Fatalf("%q 应合法：%v", raw, err)
		}
		if !cfg.IsZero() {
			t.Fatalf("%q 应解析为零值，实际 %+v", raw, cfg)
		}
	}
}

func TestValidateAuthHeader(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		prefix  string
		wantErr bool
	}{
		{"默认可空", "", "", false},
		{"x-api-key", "x-api-key", "none", false},
		{"含空格拒绝", "x api key", "", true},
		{"头注入拒绝", "X-Key\r\nX-Evil: 1", "", true},
		{"禁 scheme 头", "host", "", true},
		{"禁 content-type", "content-type", "", true},
		{"禁 proxy-", "proxy-authorization", "", true},
		{"前缀换行拒绝", "x-api-key", "Token\r\n", true},
		{"前缀过长拒绝", "Authorization", strings.Repeat("a", 33), true},
	}
	for _, c := range cases {
		err := validateAuth(c.header, c.prefix)
		if (err != nil) != c.wantErr {
			t.Errorf("%s：期望错误=%v，实际 %v", c.name, c.wantErr, err)
		}
	}
}

func TestValidateUpstreamPath(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		canon   string
		wantErr bool
	}{
		{"普通路径", "/v4/chat/completions", PathChatCompletions, false},
		{"带 query", "/v1/models?api-version=2024-02-01", PathModels, false},
		{"userinfo 攻击", "/v4/x@evil.com", PathChatCompletions, true},
		{"协议相对", "//evil.com/x", PathChatCompletions, true},
		{"绝对 URL", "https://evil.com/x", PathChatCompletions, true},
		{"路径穿越", "/a/../b", PathChatCompletions, true},
		{"反斜杠", `/a\b`, PathChatCompletions, true},
		{"fragment", "/a#b", PathChatCompletions, true},
		{"空格", "/a b", PathChatCompletions, true},
		{"不以斜杠开头", "a/b", PathChatCompletions, true},
		{"{id} 只能用于轮询", "/v1/video/{id}", PathVideoSubmit, true},
		{"{id} 轮询合法", "/api/task/{id}", PathVideoPoll, false},
	}
	for _, c := range cases {
		err := validateUpstreamPath(c.canon, c.value)
		if (err != nil) != c.wantErr {
			t.Errorf("%s：期望错误=%v，实际 %v", c.name, c.wantErr, err)
		}
	}
}

func TestValidatePathOverridesRejectsUnknownEndpoint(t *testing.T) {
	err := ValidateAdapterConfig(AdapterConfig{
		PathOverrides: map[string]string{"/v2/whatever": "/x"},
	})
	if err == nil {
		t.Fatal("非 canonical 端点应被拒绝（否则永远不生效且无人知道）")
	}
}

// TestApplyRequestPatchNullDeletes 锁住 P0-1：null 是字面量而不是 nil，用错判断删除永不生效。
func TestApplyRequestPatchNullDeletes(t *testing.T) {
	body := []byte(`{"temperature":0.7,"model":"gpt-4o","stream":false}`)
	out, err := ApplyRequestPatch(body, map[string]json.RawMessage{
		"temperature": json.RawMessage(`null`),
	}, "")
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	if strings.Contains(string(out), "temperature") {
		t.Fatalf("null 应删除该键，实际：%s", out)
	}
	if !strings.Contains(string(out), `"model":"gpt-4o"`) {
		t.Fatalf("其它键应保留，实际：%s", out)
	}
}

// TestApplyRequestPatchKeepsRawNumbers 锁住 P0-2：不能经 map[string]any（int64 会丢精度、1.0 会变 1）。
func TestApplyRequestPatchKeepsRawNumbers(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"temperature":1.0,"scale":1e10}`)
	out, err := ApplyRequestPatch(body, map[string]json.RawMessage{
		"extra_body": json.RawMessage(`{"enable_thinking":true}`),
	}, "")
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	s := string(out)
	for _, want := range []string{"9007199254740993", "1.0", "1e10"} {
		if !strings.Contains(s, want) {
			t.Fatalf("应原样保留 %q，实际：%s", want, s)
		}
	}
}

func TestApplyRequestPatchNestedSet(t *testing.T) {
	body := []byte(`{"extra_body":{"existing":1}}`)
	out, err := ApplyRequestPatch(body, map[string]json.RawMessage{
		"extra_body.enable_thinking": json.RawMessage(`true`),
	}, "")
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("结果不是合法 JSON：%v", err)
	}
	eb := m["extra_body"]
	if eb["existing"] != float64(1) || eb["enable_thinking"] != true {
		t.Fatalf("嵌套写入不正确：%v", eb)
	}
}

func TestApplyRequestPatchModelPlaceholder(t *testing.T) {
	body := []byte(`{"model":"old"}`)
	out, err := ApplyRequestPatch(body, map[string]json.RawMessage{
		"model_name": json.RawMessage(`"$model"`),
	}, "glm-4")
	if err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	if !strings.Contains(string(out), `"model_name":"glm-4"`) {
		t.Fatalf("$model 应被替换，实际：%s", out)
	}
}

func TestApplyRequestPatchIntermediateTypeConflict(t *testing.T) {
	body := []byte(`{"thinking":"enabled"}`)
	_, err := ApplyRequestPatch(body, map[string]json.RawMessage{
		"thinking.type": json.RawMessage(`"x"`),
	}, "")
	if err == nil {
		t.Fatal("中间层不是对象时应报 ConfigError（不能静默覆盖）")
	}
	var ce *ConfigError
	if !asConfigError(err, &ce) {
		t.Fatalf("应是 ConfigError，实际 %T", err)
	}
}

func TestApplyRequestPatchSkipsNonObjectBody(t *testing.T) {
	body := []byte(`[1,2,3]`)
	out, err := ApplyRequestPatch(body, map[string]json.RawMessage{"a": json.RawMessage(`1`)}, "")
	if err != nil {
		t.Fatalf("非对象体应跳过而不是报错：%v", err)
	}
	if string(out) != string(body) {
		t.Fatalf("非对象体应原样返回，实际：%s", out)
	}
}

func TestValidateRequestPatchReservedAndConflict(t *testing.T) {
	if err := ValidateAdapterConfig(AdapterConfig{
		RequestPatch: map[string]json.RawMessage{"model": json.RawMessage(`"x"`)},
	}); err == nil {
		t.Fatal("改写 model 应被拒绝（计费与上游模型会不一致）")
	}
	if err := ValidateAdapterConfig(AdapterConfig{
		RequestPatch: map[string]json.RawMessage{"stream": json.RawMessage(`true`)},
	}); err == nil {
		t.Fatal("改写 stream 应被拒绝（分叉已定，改了会收到 SSE 却按 JSON 解析）")
	}
	if err := ValidateAdapterConfig(AdapterConfig{
		RequestPatch: map[string]json.RawMessage{
			"thinking":    json.RawMessage(`{"type":"enabled"}`),
			"thinking.x":  json.RawMessage(`1`),
			"temperature": json.RawMessage(`0.1`),
		},
	}); err == nil {
		t.Fatal("祖先-后代冲突应被拒绝（应用顺序不确定）")
	}
	if err := ValidateAdapterConfig(AdapterConfig{
		RequestPatch: map[string]json.RawMessage{"messages.0.content": json.RawMessage(`"x"`)},
	}); err == nil {
		t.Fatal("请求改写不支持数组定位")
	}
}

func TestValidateResponsePathPairedTokens(t *testing.T) {
	if err := ValidateAdapterConfig(AdapterConfig{ResponsePath: &ResponsePath{Content: "data.answer"}}); err != nil {
		t.Fatalf("只配 content 应合法：%v", err)
	}
	if err := ValidateAdapterConfig(AdapterConfig{ResponsePath: &ResponsePath{
		Content: "data.answer", PromptTokens: "a.b",
	}}); err == nil {
		t.Fatal("token 必须成对配置")
	}
	if err := ValidateAdapterConfig(AdapterConfig{ResponsePath: &ResponsePath{}}); err == nil {
		t.Fatal("content 必填")
	}
}

func TestExtractByPath(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi"}}],"n":3}`)
	if raw, ok := ExtractByPath(body, "choices.0.message.content"); !ok || string(raw) != `"hi"` {
		t.Fatalf("数组下标取值失败：%s", raw)
	}
	if _, ok := ExtractByPath(body, "choices.9.message.content"); ok {
		t.Fatal("越界应返回 false")
	}
	if _, ok := ExtractByPath(body, "choices.x"); ok {
		t.Fatal("非数字下标应返回 false")
	}
	if _, ok := ExtractByPath(body, "n.deep"); ok {
		t.Fatal("对标量继续下降应返回 false")
	}
}

func TestReconstructChatCompletion(t *testing.T) {
	body := []byte(`{"data":{"answer":"hi","usage":{"input_tokens":5,"output_tokens":2}}}`)
	out, err := ReconstructChatCompletion(body, &ResponsePath{
		Content:          "data.answer",
		PromptTokens:     "data.usage.input_tokens",
		CompletionTokens: "data.usage.output_tokens",
	}, "glm-4")
	if err != nil {
		t.Fatalf("重构失败：%v", err)
	}

	// 关键：重构体必须能被 ParseUsage 正确解析，否则结算会退化成估算。
	u := ParseUsage(out)
	if u.PromptTokens != 5 || u.CompletionTokens != 2 {
		t.Fatalf("usage 解析错误：prompt=%d completion=%d", u.PromptTokens, u.CompletionTokens)
	}
	var m struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("重构体不是合法 JSON：%v", err)
	}
	if m.Object != "chat.completion" || m.Model != "glm-4" || len(m.Choices) != 1 ||
		m.Choices[0].Message.Content != "hi" {
		t.Fatalf("重构体形状不对：%+v", m)
	}
}

func TestReconstructChatCompletionContentBlocks(t *testing.T) {
	body := []byte(`{"message":{"content":[{"type":"text","text":"a"},{"type":"image","source":{}},{"type":"text","text":"b"}]}}`)
	out, err := ReconstructChatCompletion(body, &ResponsePath{Content: "message.content"}, "m")
	if err != nil {
		t.Fatalf("重构失败：%v", err)
	}
	if !strings.Contains(string(out), `"content":"ab"`) {
		t.Fatalf("应拼接 text blocks，实际：%s", out)
	}
	// 未声明 token 映射时不写 usage，让结算走估算兜底（可见），而不是写个假的 0。
	if strings.Contains(string(out), `"usage"`) {
		t.Fatalf("未配置 token 时不应写 usage：%s", out)
	}
}

func TestReconstructChatCompletionMissingPath(t *testing.T) {
	_, err := ReconstructChatCompletion([]byte(`{"data":{}}`), &ResponsePath{Content: "data.answer"}, "m")
	if err == nil {
		t.Fatal("路径缺失应报错，不能回落透传")
	}
	var ce *ConfigError
	if !asConfigError(err, &ce) {
		t.Fatalf("应是 ConfigError，实际 %T", err)
	}
}

// TestConfigErrorIsInternal 锁住 R4：配置错不能被当成上游故障（否则会熔断好渠道）。
func TestConfigErrorIsInternal(t *testing.T) {
	if got := classify(0, &ConfigError{Field: "x", Msg: "y"}, false); got != outcomeInternal {
		t.Fatalf("配置错误应归 outcomeInternal，实际 %v", got)
	}
}

func TestCanonicalPathsCoverAdapters(t *testing.T) {
	kinds := []endpointKind{
		kindChat, kindCompletions, kindEmbeddings, kindImagesGenerations, kindImagesEdits,
		kindImagesVariations, kindAudioSpeech, kindAudioTranscriptions, kindAudioTranslations,
		kindModerations, kindRerank, kindResponses,
	}
	for _, k := range kinds {
		p := specOf(k).path
		if p == "" {
			continue
		}
		if !canonicalPaths[p] {
			t.Errorf("端点 %v 的路径 %q 不在 canonicalPaths 里（加端点时忘了同步白名单）", k, p)
		}
	}
	// 专用方法（path 为空）的三条路径也必须可覆盖，否则它们永远改不了路径。
	for _, p := range []string{PathModels, PathChatCompletions, PathCompletions, PathEmbeddings, PathVideoSubmit, PathVideoPoll} {
		if !canonicalPaths[p] {
			t.Errorf("路径 %q 应在白名单里", p)
		}
	}
}

func TestBuildURL(t *testing.T) {
	cfg := &ChannelConfig{BaseURL: "https://api.example.com/"}
	got, err := BuildURL(cfg, PathChatCompletions, "")
	if err != nil || got != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("默认拼接错误：%q %v", got, err)
	}

	cfg.Decl = AdapterConfig{PathOverrides: map[string]string{PathChatCompletions: "/v4/chat"}}
	got, _ = BuildURL(cfg, PathChatCompletions, "")
	if got != "https://api.example.com/v4/chat" {
		t.Fatalf("覆盖未生效：%q", got)
	}

	cfg.Decl = AdapterConfig{PathOverrides: map[string]string{PathVideoPoll: "/api/task/{id}"}}
	got, _ = BuildURL(cfg, PathVideoPoll, "abc/def")
	if got != "https://api.example.com/api/task/abc%2Fdef" {
		t.Fatalf("占位符替换错误：%q", got)
	}

	// 合法覆盖值拼出来的 host 必须仍是渠道 Base URL（R1 的核心断言）。
	cfg.Decl = AdapterConfig{PathOverrides: map[string]string{PathChatCompletions: "/v4/chat?x=1"}}
	raw, _ := BuildURL(cfg, PathChatCompletions, "")
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "api.example.com" {
		t.Fatalf("host 被改变：%q", raw)
	}
}

func TestBuildURLPropagatesDeclErr(t *testing.T) {
	cfg := &ChannelConfig{BaseURL: "https://api.example.com", DeclErr: &ConfigError{Field: "adapter_config", Msg: "脏数据"}}
	if _, err := BuildURL(cfg, PathModels, ""); err == nil {
		t.Fatal("DeclErr 必须传播（禁止静默降级为无配置）")
	}
}

func TestSetAuthHeaderSemantics(t *testing.T) {
	cases := []struct {
		name       string
		decl       AdapterConfig
		wantHeader string
		wantValue  string
	}{
		{"默认", AdapterConfig{}, "Authorization", "Bearer sk-1"},
		{"自定义头 + none", AdapterConfig{AuthHeader: "x-api-key", AuthPrefix: "none"}, "x-api-key", "sk-1"},
		{"Token 前缀", AdapterConfig{AuthPrefix: "Token "}, "Authorization", "Token sk-1"},
		// 关键：后端**不自动补空格**（与 plugin.injectAuth 一致）。前端负责在生成配置时补。
		{"无尾空格原样拼接", AdapterConfig{AuthPrefix: "Bearer"}, "Authorization", "Bearersk-1"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", nil)
		cfg := &ChannelConfig{APIKey: "sk-1", Decl: c.decl}
		SetAuthHeader(req, cfg)
		if got := req.Header.Get(c.wantHeader); got != c.wantValue {
			t.Errorf("%s：期望 %q=%q，实际 %q", c.name, c.wantHeader, c.wantValue, got)
		}
		if c.wantHeader != "Authorization" && req.Header.Get("Authorization") != "" {
			t.Errorf("%s：不应同时设置 Authorization", c.name)
		}
	}
}

// asConfigError 是 errors.As 的薄封装，避免测试文件里到处写 var 声明。
func asConfigError(err error, target **ConfigError) bool {
	return errors.As(err, target)
}

// TestChannelConfigParsesDeclarativeConfig 锁住「配置从 JSONB 到运行态」这一跳。
//
// 两个方向都要测：
//   - 合法配置必须被解析（否则所有声明式能力都静默失效）；
//   - 脏数据必须留下 DeclErr（fail-closed），不能被当成"没配置"继续发请求。
func TestChannelConfigParsesDeclarativeConfig(t *testing.T) {
	ok := &model.Channel{
		ID: 1, Name: "c", BaseURL: "https://api.example.com",
		AdapterConfig: model.JSON(`{"auth_header":"x-api-key","auth_prefix":"none"}`),
	}
	cfg := channelConfig(ok, "sk-1")
	if cfg.DeclErr != nil {
		t.Fatalf("合法配置不应报错：%v", cfg.DeclErr)
	}
	if cfg.Decl.AuthHeader != "x-api-key" || cfg.Decl.AuthPrefix != "none" {
		t.Fatalf("配置未解析：%+v", cfg.Decl)
	}

	bad := &model.Channel{
		ID: 2, Name: "d", BaseURL: "https://api.example.com",
		AdapterConfig: model.JSON(`{"unknown_field":1}`),
	}
	if cfg := channelConfig(bad, "sk-1"); cfg.DeclErr == nil {
		t.Fatal("未知字段必须 fail-closed（记 DeclErr），不能静默当成无配置")
	}

	empty := &model.Channel{ID: 3, Name: "e", BaseURL: "https://api.example.com"}
	if cfg := channelConfig(empty, "sk-1"); cfg.DeclErr != nil || !cfg.Decl.IsZero() {
		t.Fatalf("未配置应得到零值且无错误：%+v %v", cfg.Decl, cfg.DeclErr)
	}
}

// TestParseUsageHandlesTopLevelAnthropic 是 P0-4 的回归锁。
//
// ParseUsage 本来就能读顶层 input_tokens/output_tokens，所以「Anthropic 形状」**不需要**
// 配 response_path；预设有责任说清楚，否则运营会多配一层，反而把 cache 字段丢掉。
func TestParseUsageHandlesTopLevelAnthropic(t *testing.T) {
	u := ParseUsage([]byte(`{"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":3}}`))
	if u.PromptTokens != 5 || u.CompletionTokens != 2 || u.CacheTokens != 3 {
		t.Fatalf("顶层 Anthropic usage 应被直接解析：%+v", u)
	}
	// 嵌在 data 下的同名结构不会被读到——这才是 response_path 的适用场景。
	u2 := ParseUsage([]byte(`{"data":{"usage":{"input_tokens":5,"output_tokens":2}}}`))
	if u2.PromptTokens != 0 || u2.CompletionTokens != 0 {
		t.Fatalf("嵌套 usage 不应被顶层解析命中：%+v", u2)
	}
}
