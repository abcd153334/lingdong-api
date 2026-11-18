package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// 本文件覆盖「渠道级声明式配置」在适配器上的接线。
// 每个用例都断言**上游实际收到了什么**（路径/头/请求体），而不是只看我方返回值——
// 这类配置的失败模式几乎都是"请求发歪了但响应看起来正常"。

func TestSupportsDeclarativeConfig(t *testing.T) {
	if !(OpenAI{}).SupportsDeclarativeConfig() {
		t.Fatal("内置 openai 适配器应声明支持声明式配置")
	}
	if !relay.SupportsDeclarativeConfig("openai") {
		t.Fatal("未注册适配器时 SupportsDeclarativeConfig 应为 false；已注册应为 true")
	}
}

func TestChatCustomAuthHeader(t *testing.T) {
	var gotAPIKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{AuthHeader: "x-api-key", AuthPrefix: "none"},
	}
	if _, err := (OpenAI{}).Chat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m"}`),
	}); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if gotAPIKey != "sk-1" {
		t.Fatalf("x-api-key 应为原始密钥，实际 %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("自定义头时不应再发 Authorization，实际 %q", gotAuth)
	}
}

func TestChatPathOverride(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{PathOverrides: map[string]string{
			relay.PathChatCompletions: "/v4/chat/completions?api-version=2024-02-01",
		}},
	}
	if _, err := (OpenAI{}).Chat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m"}`),
	}); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if gotPath != "/v4/chat/completions" {
		t.Fatalf("路径覆盖未生效：%s", gotPath)
	}
	if gotQuery != "api-version=2024-02-01" {
		t.Fatalf("query 未保留：%s", gotQuery)
	}
}

func TestChatRequestPatch(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{RequestPatch: map[string]json.RawMessage{
			"temperature":    json.RawMessage(`null`),
			"thinking.type":  json.RawMessage(`"enabled"`),
			"upstream_model": json.RawMessage(`"$model"`),
		}},
	}
	if _, err := (OpenAI{}).Chat(context.Background(), cfg, &relay.RelayRequest{
		Model: "upstream-m",
		Body:  json.RawMessage(`{"model":"alias","temperature":0.7,"max_tokens":100}`),
	}); err != nil {
		t.Fatalf("请求失败：%v", err)
	}

	var sent map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("上游收到的不是合法 JSON：%v", err)
	}
	if _, ok := sent["temperature"]; ok {
		t.Fatalf("temperature 应被删除，实际：%s", gotBody)
	}
	if _, ok := sent["max_tokens"]; !ok {
		t.Fatalf("未改写的字段应保留，实际：%s", gotBody)
	}
	var model string
	_ = json.Unmarshal(sent["model"], &model)
	if model != "upstream-m" {
		t.Fatalf("model 必须仍是绑定表的上游别名，实际 %q", model)
	}
	if !strings.Contains(gotBody, `"upstream_model":"upstream-m"`) {
		t.Fatalf("$model 占位符未替换，实际：%s", gotBody)
	}
}

func TestStreamPatchCannotDisableUsage(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{RequestPatch: map[string]json.RawMessage{
			"stream_options.include_usage": json.RawMessage(`null`),
		}},
	}
	if _, err := (OpenAI{}).StreamChat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`),
	}, func(relay.SSEFrame) error { return nil }); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if !strings.Contains(gotBody, `"include_usage":true`) {
		// patch 在 withIncludeUsage 之前，运营删掉它也会被重新写入：计费安全优先。
		t.Fatalf("include_usage 必须为 true，实际：%s", gotBody)
	}
}

func TestChatResponsePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"answer":"hi","usage":{"input_tokens":5,"output_tokens":2}}}`))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{ResponsePath: &relay.ResponsePath{
			Content:          "data.answer",
			PromptTokens:     "data.usage.input_tokens",
			CompletionTokens: "data.usage.output_tokens",
		}},
	}
	resp, err := (OpenAI{}).Chat(context.Background(), cfg, &relay.RelayRequest{
		Model: "upstream-m", Body: json.RawMessage(`{"model":"m"}`),
	})
	if err != nil {
		t.Fatalf("重构失败：%v", err)
	}
	if !strings.Contains(string(resp.Body), `"object":"chat.completion"`) {
		t.Fatalf("应返回标准 chat 形状，实际：%s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), `"content":"hi"`) {
		t.Fatalf("正文映射错误：%s", resp.Body)
	}
	u := relay.ParseUsage(resp.Body)
	if u.PromptTokens != 5 || u.CompletionTokens != 2 {
		t.Fatalf("usage 未正确映射：prompt=%d completion=%d", u.PromptTokens, u.CompletionTokens)
	}
}

func TestChatResponsePathMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{ResponsePath: &relay.ResponsePath{Content: "data.answer"}},
	}
	if _, err := (OpenAI{}).Chat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m"}`),
	}); err == nil {
		t.Fatal("映射路径缺失必须报错：回落透传会让客户端拿到非 OpenAI 形状且按估算计费")
	}
}

func TestChatResponsePathSkippedOnUpstreamError(t *testing.T) {
	const errBody = `{"code":5001,"msg":"upstream boom"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(errBody))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{ResponsePath: &relay.ResponsePath{Content: "data.answer"}},
	}
	resp, err := (OpenAI{}).Chat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m"}`),
	})
	if err != nil {
		t.Fatalf("错误体不应触发重构失败：%v", err)
	}
	if string(resp.Body) != errBody || resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("错误响应应原样透传，实际 %d %s", resp.StatusCode, resp.Body)
	}
}

func TestStreamChatIgnoresResponsePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		// 配了 response_path 也不该影响流式：上游 SSE 可能是标准的，只是非流式结构不同。
		Decl: relay.AdapterConfig{ResponsePath: &relay.ResponsePath{Content: "data.answer"}},
	}
	var frames []string
	if _, err := (OpenAI{}).StreamChat(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m","stream":true}`),
	}, func(f relay.SSEFrame) error {
		if !f.Done {
			frames = append(frames, string(f.Data))
		}
		return nil
	}); err != nil {
		t.Fatalf("流式不应因 response_path 报错：%v", err)
	}
	if len(frames) != 1 || !strings.Contains(frames[0], `"choices"`) {
		t.Fatalf("流式帧应原样透传，实际：%v", frames)
	}
}

func TestTestUsesPathOverride(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/v9/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{PathOverrides: map[string]string{relay.PathModels: "/v9/models"}},
	}
	if err := (OpenAI{}).Test(context.Background(), cfg); err != nil {
		t.Fatalf("连通测试应走覆盖后的路径：%v", err)
	}
	if gotPath != "/v9/models" {
		t.Fatalf("实际路径 %q", gotPath)
	}
}

func TestTestErrorMentionsPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{PathOverrides: map[string]string{relay.PathModels: "/v9/models"}},
	}
	err := (OpenAI{}).Test(context.Background(), cfg)
	if err == nil {
		t.Fatal("应失败")
	}
	if !strings.Contains(err.Error(), "/v9/models") {
		t.Fatalf("错误文案应带实际请求路径，实际：%v", err)
	}
}

// TestForwardMultipartSkipsPatch 断言 multipart 端点遇到 request_patch 时 **fail-closed**。
//
// 为什么不能再「静默跳过」：运营在界面上配了 request_patch（比如强行注入 size/quality
// 来对齐计费档位），multipart 端点上这条配置不可能生效。旧实现直接跳过并照常转发，
// 表现是「配置看起来生效了、账单却是另一个档位」——静默失败比明确报错危险得多。
// 现在返回 *relay.ConfigError，管理台保存期就能定位到具体字段。
func TestForwardMultipartSkipsPatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("配置错误时不应发出上游请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "local-alias")
	_ = mw.WriteField("prompt", "hi")
	_ = mw.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{RequestPatch: map[string]json.RawMessage{
			"injected": json.RawMessage(`"should-not-appear"`),
		}},
	}
	_, err := (OpenAI{}).Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "upstream-m", Body: buf.Bytes(), ContentType: mw.FormDataContentType(),
	}, relay.PathImagesEdits)
	var ce *relay.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("multipart + request_patch 应返回 ConfigError，实际: %v", err)
	}
	if ce.Field != "adapter_config.request_patch" {
		t.Fatalf("错误应定位到 adapter_config.request_patch，实际: %+v", ce)
	}
}

// TestForwardMultipartRewritesModelWithoutPatch 断言「没有 request_patch 时」multipart
// 仍按原逻辑替换 model 字段并原样转发（不能因为上面的 fail-closed 误伤正常配置）。
func TestForwardMultipartRewritesModelWithoutPatch(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "local-alias")
	_ = mw.WriteField("prompt", "hi")
	_ = mw.Close()

	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-1"}
	if _, err := (OpenAI{}).Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "upstream-m", Body: buf.Bytes(), ContentType: mw.FormDataContentType(),
	}, relay.PathImagesEdits); err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if !strings.Contains(gotBody, "upstream-m") {
		t.Fatalf("multipart 的 model 仍应被替换为上游别名，实际：%s", gotBody)
	}
	if strings.Contains(gotBody, "local-alias") {
		t.Fatalf("对外模型名不应泄漏给上游，实际：%s", gotBody)
	}
}

// TestTestUsesCustomAuthHeader 断言连通测试也走自定义鉴权头。
//
// 漏了这条的后果：运营配好 x-api-key 后点「连通测试」一直 401，会误判渠道坏了。
func TestTestUsesCustomAuthHeader(t *testing.T) {
	var gotAPIKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("api-key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &relay.ChannelConfig{
		BaseURL: srv.URL, APIKey: "sk-1",
		Decl: relay.AdapterConfig{AuthHeader: "api-key", AuthPrefix: "none"},
	}
	if err := (OpenAI{}).Test(context.Background(), cfg); err != nil {
		t.Fatalf("连通测试失败：%v", err)
	}
	if gotAPIKey != "sk-1" || gotAuth != "" {
		t.Fatalf("连通测试未使用自定义鉴权头：api-key=%q authorization=%q", gotAPIKey, gotAuth)
	}
}
