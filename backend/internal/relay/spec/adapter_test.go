package spec

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// mustAdapter 解析一份规格并构造适配器（任何一步失败即 Fatal）。
func mustAdapter(t *testing.T, raw string) *Adapter {
	t.Helper()
	s, err := decodeJSON(t, raw)
	if err != nil {
		t.Fatalf("规格应合法: %v", err)
	}
	return New(s)
}

// TestAdapterChatPassthrough 断言透传模式：
//  1. 请求体原样送到上游（含调用方的自定义字段），只替换 model 为上游真名；
//  2. 密钥按规格写法放头；
//  3. 响应体原样返回。
//
// 「只替换 model」是关键：平台对外模型名与上游真名经常不同（运营会起别名），
// 不替换会让上游 404 model_not_found，而错误信息看起来像是渠道配错了。
func TestAdapterChatPassthrough(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[]}`))
	}))
	defer srv.Close()

	a := mustAdapter(t, `{"slug":"relay-a","label":"中转 A","endpoints":{
		"chat/completions":{"path":"/v1/chat/completions","passthrough":true}}}`)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-test"}

	resp, err := a.Chat(context.Background(), cfg, &relay.RelayRequest{
		Model: "upstream-real-name",
		Body:  json.RawMessage(`{"model":"local-alias","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`),
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("上游路径不符: %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("鉴权头不符: %q", gotAuth)
	}
	if gotBody["model"] != "upstream-real-name" {
		t.Fatalf("model 未替换成上游名: %v", gotBody["model"])
	}
	if _, ok := gotBody["tools"]; !ok {
		t.Fatal("透传模式下不得丢弃调用方字段（tools 丢了）")
	}
	if !strings.Contains(string(resp.Body), "chatcmpl-1") {
		t.Fatalf("响应体未原样返回: %s", resp.Body)
	}
}

// TestAdapterSyncRebuildBody 断言白名单重建模式：请求体只含 const/copy 声明的字段。
//
// 为什么必须严格：重建模式的意义是"只把上游认识的字段送过去"。若混进调用方的
// 其它字段（例如 platform 私有参数），上游可能直接 400，而渠道看起来配置正常。
func TestAdapterSyncRebuildBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"url":"https://cdn.example/a.png"}]}`))
	}))
	defer srv.Close()

	a := mustAdapter(t, `{"slug":"img-b","label":"图像 B","endpoints":{
		"images/generations":{"path":"/v1/images/generations",
			"body":{"const":{"model":"gpt-image-2","response_format":"url"},
				"copy":{"prompt":"$req.prompt","size":"$req.size"}},
			"extract":{"result":"$.data[0].url"}}}}`)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"}

	_, err := a.Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "gpt-image-2",
		Body:  json.RawMessage(`{"prompt":"a cat","size":"1024x1024","n":4,"internal_trace":"x"}`),
	}, "/v1/images/generations")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if gotBody["prompt"] != "a cat" {
		t.Fatalf("copy 未生效: %v", gotBody["prompt"])
	}
	if gotBody["model"] != "gpt-image-2" || gotBody["response_format"] != "url" {
		t.Fatalf("const 未生效: %v", gotBody)
	}
	// n 未被 copy 声明 → 不得出现（它是计费维度，必须由调用方显式复制，不能凭空带过去）
	if _, ok := gotBody["n"]; ok {
		t.Fatal("未声明 copy 的字段不应出现在上游请求体里")
	}
	if _, ok := gotBody["internal_trace"]; ok {
		t.Fatal("重建模式不得携带平台私有字段")
	}
}

// TestAdapterSubmitAndPoll 断言异步链路：提交取任务号 → 轮询按 status_map 归一。
func TestAdapterSubmitAndPoll(t *testing.T) {
	pollCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos":
			_, _ = w.Write([]byte(`{"data":{"task_id":"up-123"}}`))
		case strings.HasPrefix(r.URL.Path, "/v1/videos/"):
			if !strings.HasSuffix(r.URL.Path, "/up-123") {
				t.Errorf("轮询未替换 {task_id}: %s", r.URL.Path)
			}
			pollCalls++
			if pollCalls == 1 {
				_, _ = w.Write([]byte(`{"data":{"status":"processing","progress":40}}`))
				return
			}
			if pollCalls == 2 {
				// 上游新增了一个我方未映射的状态：必须按 running 处理，不能判失败。
				_, _ = w.Write([]byte(`{"data":{"status":"throttled"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"status":"succeed","videos":[{"url":"https://cdn/v1.mp4"}]}}`))
		default:
			t.Errorf("意外请求: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, `{"slug":"video-c","label":"视频 C","endpoints":{
		"video/generations":{"path":"/v1/videos","body":{"const":{"model":"kling-v2"}},
			"extract":{"task_id":"$.data.task_id"},"async":{"poll":"video/generations@poll"}},
		"video/generations@poll":{"path":"/v1/videos/{task_id}",
			"extract":{"status":"$.data.status","progress":"$.data.progress","result_urls":"$.data.videos[*].url"},
			"status_map":{"queued":["waiting"],"running":["processing"],"succeeded":["succeed"],"failed":["failed"]}}}}`)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"}

	sub := &relay.TaskSubmitRequest{ModelID: "kling-v2", UpstreamModel: "kling-v2", Body: json.RawMessage(`{"prompt":"x"}`)}
	taskID, err := a.SubmitTask(context.Background(), cfg, sub)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if taskID != "up-123" {
		t.Fatalf("任务号提取错误: %q", taskID)
	}
	// 子协议标记必须回写：轮询靠它选端点（规格有多个异步端点时尤其关键）。
	if sub.SubKind != "video/generations" {
		t.Fatalf("SubKind 未回写: %q", sub.SubKind)
	}

	st1, err := a.PollTaskDetailed(context.Background(), cfg,
		&relay.TaskPollRequest{UpstreamTaskID: taskID, SubKind: sub.SubKind})
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if st1.Status != "running" || st1.Progress != 40 {
		t.Fatalf("首个轮询结果不符: %+v", st1)
	}

	st2, err := a.PollTaskDetailed(context.Background(), cfg,
		&relay.TaskPollRequest{UpstreamTaskID: taskID, SubKind: sub.SubKind})
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if st2.Status != "running" {
		t.Fatalf("未映射的状态应按 running 处理，实际 %q", st2.Status)
	}

	st3, err := a.PollTaskDetailed(context.Background(), cfg,
		&relay.TaskPollRequest{UpstreamTaskID: taskID, SubKind: sub.SubKind})
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if st3.Status != "succeeded" {
		t.Fatalf("终态映射错误: %+v", st3)
	}
	if len(st3.ResultURLs) != 1 || st3.ResultURLs[0] != "https://cdn/v1.mp4" {
		t.Fatalf("产物链接提取错误: %v", st3.ResultURLs)
	}
}

// TestAdapterBodyCodeSuccess 断言「HTTP 200 但业务失败」能被识别为上游错误。
//
// 这类上游（国内厂商很常见）如果只看状态码，会把失败当成功返回给用户，
// 结果就是「钱扣了、图没出」，且渠道健康度完全不下降。
func TestAdapterBodyCodeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":40001,"msg":"额度不足"}`))
	}))
	defer srv.Close()

	a := mustAdapter(t, `{"slug":"audio-d","label":"语音 D","endpoints":{
		"audio/speech":{"path":"/api/tts","body":{"copy":{"text":"$req.input"}},
			"extract":{"result":"$.data.audio_url"},
			"errors":{"success":"body_code","code_path":"$.code","ok_codes":["0"],"message_path":"$.msg"}}}}`)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"}

	_, err := a.Forward(context.Background(), cfg, &relay.RelayRequest{
		Body: json.RawMessage(`{"input":"hello"}`),
	}, "/api/tts")
	if err == nil {
		t.Fatal("业务失败应返回错误")
	}
	if !strings.Contains(err.Error(), "额度不足") {
		t.Fatalf("错误信息应带上游 msg，实际: %v", err)
	}
}

// TestAdapterUnsupportedEndpoint 断言未声明端点返回 ConfigError（不记渠道健康、不冷却密钥）。
//
// 归因必须正确：把「渠道配置漏了端点」记成渠道故障，会触发熔断把本来健康的渠道停掉；
// 归成调用方错误，则运营永远看不到"该渠道少了这个端点"。
func TestAdapterUnsupportedEndpoint(t *testing.T) {
	a := mustAdapter(t, `{"slug":"relay-a","label":"中转 A","endpoints":{
		"chat/completions":{"path":"/v1/chat/completions","passthrough":true}}}`)
	if _, err := a.Embeddings(context.Background(), &relay.ChannelConfig{BaseURL: "http://127.0.0.1:1"}, &relay.RelayRequest{}); err == nil {
		t.Fatal("未声明的端点应报错")
	} else if _, ok := err.(*relay.ConfigError); !ok {
		t.Fatalf("应为 ConfigError，实际 %T: %v", err, err)
	}
}
