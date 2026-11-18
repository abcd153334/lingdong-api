package spec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingdong-api/internal/relay"
)

// indexTTSSpec 是 AutoDL IndexTTS 的真实形状：只有「提交 + 查询」两个端点，
// 平台对外是同步的 /v1/audio/speech（二进制）。桥接就是为这类厂商存在的。
const indexTTSSpec = `{
  "slug": "autodl-indextts",
  "label": "AutoDL IndexTTS",
  "vendor": "autodl",
  "endpoints": {
    "audio/speech": {
      "method": "POST",
      "path": "/tts/submit",
      "omit_model": true,
      "response_content_type": "audio/wav",
      "body": {
        "copy": {
          "text": "$req.input",
          "prompt_text": "$req.prompt_text",
          "emo_ref_audio": ["$req.reference_audio", "$req.voice"],
          "emo_random": "$req.emo_random",
          "emo_control_method": "$req.emo_control_method",
          "emo_alpha": "$req.emo_alpha"
        },
        "const": {"emo_vector": null, "output_format": "wav"}
      },
      "extract": {"task_id": "$.data.task_id"},
      "async": {"mode": "bridge", "poll": "audio/speech@poll", "timeout_secs": 10, "interval_ms": 200}
    },
    "audio/speech@poll": {
      "method": "GET",
      "path": "/tts/result/{task_id}",
      "extract": {
        "status": "$.data.status",
        "error": "$.data.message",
        "result_urls": "$.data.audios[*]",
        "duration": "$.data.duration"
      },
      "status_map": {"succeeded": ["done", "SUCCESS"], "failed": ["failed", "ERROR"]}
    }
  }
}`

// TestBridgeTTSRoutesByPlatformPath 锁死三件事：
//  1. Forward 收到的是**平台路径**（/v1/audio/speech），必须能路由到规格声明的**上游路径**；
//  2. 桥接：提交 → 轮询 → 下载产物 → 按同步二进制返回（含 Content-Type 与时长）；
//  3. 请求体：copy 候选链取第一个有值的来源；omit_model 时不能出现 model 字段。
//
// 改造前这三条全都不成立：路由永远失败（拿平台路径比上游路径），
// 而 audio/speech 声明 async 会被"只有异步端点才能声明 async"直接拒绝。
func TestBridgeTTSRoutesByPlatformPath(t *testing.T) {
	var submitBody map[string]any
	var polls int32
	var gotPollPath string
	audio := []byte("RIFF....WAVEfake-audio-bytes")

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tts/submit":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &submitBody)
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"t-77"}}`))
		case strings.HasPrefix(r.URL.Path, "/tts/result/"):
			gotPollPath = r.URL.Path
			if atomic.AddInt32(&polls, 1) == 1 {
				_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"status":"done","duration":1.6,"audios":["` + srv.URL + `/files/a.wav"]}}`))
		case r.URL.Path == "/files/a.wav":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(audio)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, indexTTSSpec)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-x"}

	// 调用方给的是 OpenAI 风格字段（input/voice），上游要的是 text/emo_*。
	req := &relay.RelayRequest{
		Model: "indextts", ModelID: "indextts",
		Body: json.RawMessage(`{"input":"你好世界","voice":"ref-a.wav","prompt_text":"参考","emo_random":false,"emo_control_method":2,"emo_alpha":0.8}`),
	}
	raw, err := a.Forward(context.Background(), cfg, req, "/v1/audio/speech")
	if err != nil {
		t.Fatalf("桥接调用失败: %v", err)
	}
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", raw.StatusCode)
	}
	if string(raw.Body) != string(audio) {
		t.Fatalf("产物字节不符: %q", raw.Body)
	}
	if raw.ContentType != "audio/wav" {
		t.Fatalf("响应类型应取上游 Content-Type，实际 %q", raw.ContentType)
	}
	// 1.6 秒必须四舍五入成 2 秒（按秒计费的唯一来源）。
	if raw.DurationSecs != 2 {
		t.Fatalf("时长应四舍五入为 2 秒，实际 %d", raw.DurationSecs)
	}
	if gotPollPath != "/tts/result/t-77" {
		t.Fatalf("轮询路径未替换 {task_id}: %q", gotPollPath)
	}

	// 请求体：候选链取到了 voice；text 来自 input；绝不能有 model。
	if submitBody["text"] != "你好世界" {
		t.Fatalf("text 未映射: %+v", submitBody)
	}
	if submitBody["emo_ref_audio"] != "ref-a.wav" {
		t.Fatalf("audio 参考候选链未生效: %+v", submitBody)
	}
	if _, exists := submitBody["model"]; exists {
		t.Fatalf("omit_model=true 时不应写 model: %+v", submitBody)
	}
	if submitBody["output_format"] != "wav" || submitBody["emo_alpha"] != 0.8 {
		t.Fatalf("const/copy 合成结果不符: %+v", submitBody)
	}
}

// TestBridgeTimeoutIsBounded 锁死等待上限：上游永远 running 时必须按上限放弃，
// 返回 SyncWaitTimeoutError（网关据此 504 + 释放），而不是无限挂着。
func TestBridgeTimeoutIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tts/submit" {
			_, _ = w.Write([]byte(`{"data":{"task_id":"t-1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"running"}}`))
	}))
	defer srv.Close()

	spec := strings.Replace(indexTTSSpec, `"timeout_secs": 10`, `"timeout_secs": 1`, 1)
	a := mustAdapter(t, spec)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-x"}
	started := time.Now()
	_, err := a.Forward(context.Background(), cfg, &relay.RelayRequest{Body: json.RawMessage(`{"input":"hi"}`)}, "/v1/audio/speech")
	if err == nil {
		t.Fatal("应当超时失败")
	}
	var to *relay.SyncWaitTimeoutError
	if !errors.As(err, &to) {
		t.Fatalf("应为 SyncWaitTimeoutError，实际 %T: %v", err, err)
	}
	if to.TaskID != "t-1" {
		t.Fatalf("超时错误必须带上游任务号，实际 %q", to.TaskID)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("等待超出上限：%s", elapsed)
	}
}

// TestBridgeFailedStatusIsError 锁死"上游失败不能当成功"：
// 返回 0 字节音频 + 照常扣费是这类桥接最贵的 bug。
func TestBridgeFailedStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tts/submit" {
			_, _ = w.Write([]byte(`{"data":{"task_id":"t-9"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"ERROR","message":"内容违规"}}`))
	}))
	defer srv.Close()

	a := mustAdapter(t, indexTTSSpec)
	_, err := a.Forward(context.Background(), &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"},
		&relay.RelayRequest{Body: json.RawMessage(`{"input":"hi"}`)}, "/v1/audio/speech")
	if err == nil {
		t.Fatal("上游失败必须回错")
	}
	if !strings.Contains(err.Error(), "内容审核") {
		t.Fatalf("审核类失败应折成调用方错误，实际: %v", err)
	}
}

// TestSpecValidationRejectsBridgeOnAsyncEndpoint 锁死形态与相位：
// 异步提交端点不能用 bridge（那是分钟级任务，一个 HTTP 请求等不到）。
func TestSpecValidationRejectsBridgeOnAsyncEndpoint(t *testing.T) {
	_, err := decodeJSON(t, `{"slug":"vid-x","label":"vid","endpoints":{
		"video/generations":{
			"path":"/v/submit","extract":{"task_id":"$.id"},
			"async":{"mode":"bridge","poll":"video/generations@poll"}},
		"video/generations@poll":{
			"path":"/v/status/{task_id}","extract":{"status":"$.s","result_urls":"$.u"},
			"status_map":{"succeeded":["ok"],"failed":["bad"]}}}}`)
	if err == nil {
		t.Fatal("异步端点声明 bridge 应被拒绝")
	}
	if !strings.Contains(err.Error(), "mode=task") {
		t.Fatalf("报错应指明只能用 task，实际: %v", err)
	}
}

// TestSpecValidationRequiresResultForBridge 锁死 bridge 的必备项：
// 没有产物链接就没有同步响应体，配出来必然返回空。
//
// 早前这条校验写成「必须 extract.download=true」，而那个开关运行期从不读取 ——
// 运营写了 false 被拒、写 true 也没改变任何行为，属于死开关。
func TestSpecValidationRequiresResultForBridge(t *testing.T) {
	_, err := decodeJSON(t, `{"slug":"aud-x","label":"aud","endpoints":{
		"audio/speech":{
			"path":"/submit","extract":{"task_id":"$.id"},
			"async":{"mode":"bridge","poll":"audio/speech@poll"}},
		"audio/speech@poll":{
			"path":"/r/{task_id}","extract":{"status":"$.s"},
			"status_map":{"succeeded":["ok"],"failed":["bad"]}}}}`)
	if err == nil {
		t.Fatal("缺少产物链接应被拒绝")
	}
	if !strings.Contains(err.Error(), "result") {
		t.Fatalf("报错应指向 result/result_urls，实际: %v", err)
	}
}

// TestBuildBodyCopyFallbackSkipsEmpty 锁死候选链语义：空串视为没给，继续试下一个。
func TestBuildBodyCopyFallbackSkipsEmpty(t *testing.T) {
	a := mustAdapter(t, `{"slug":"aud-f","label":"aud","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"body":{"copy":{"ref":["$req.reference_audio","$req.voice"]}},
			"extract":{"result":"$.url"}}}}`)
	ep := a.byKey["audio/speech"]
	rq := &relay.RelayRequest{Body: json.RawMessage(`{"reference_audio":"","voice":"v2"}`)}
	src, err := a.prepareRequest(ep, rq)
	if err != nil {
		t.Fatalf("预处理失败: %v", err)
	}
	out, err := a.buildBody(nil, ep, rq, src)
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if got["ref"] != "v2" {
		t.Fatalf("空串来源不应截断候选链: %+v", got)
	}
}

// syncTTSSpec 是「上游本来同步」的 TTS：一次 POST 直接回音频字节，没有任务号。
//
// 这类厂商不需要桥接，但同样容易被写坏 —— 响应是二进制，若按 JSON 处理，
// Content-Type 会被写死成 application/json、体积会被卡在 8 MiB。
const syncTTSSpec = `{
  "slug": "sync-tts",
  "label": "同步 TTS",
  "endpoints": {
    "audio/speech": {
      "method": "POST",
      "path": "/tts",
      "omit_model": true,
      "body": {"copy": {"text": "$req.input", "voice": "$req.voice"}},
      "extract": {"result": "$.url"}
    }
  }
}`

// TestSyncBinaryKeepsUpstreamContentType 锁死「响应类型以上游为准」。
//
// 写死 application/json 时接口返回 200、字节也对，但播放器/浏览器按 JSON 解析
// → 用户看到"点了没声音"，而日志里一切正常。
func TestSyncBinaryKeepsUpstreamContentType(t *testing.T) {
	audio := []byte("RIFF....WAVEfake-audio")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav; charset=binary")
		_, _ = w.Write(audio)
	}))
	defer srv.Close()

	a := mustAdapter(t, syncTTSSpec)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-x"}
	raw, err := a.Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "tts-x", Body: json.RawMessage(`{"input":"你好","voice":"v1"}`),
	}, "/v1/audio/speech")
	if err != nil {
		t.Fatalf("同步 TTS 调用失败: %v", err)
	}
	if !strings.HasPrefix(raw.ContentType, "audio/wav") {
		t.Fatalf("响应类型必须取上游 Content-Type，实际 %q", raw.ContentType)
	}
	if string(raw.Body) != string(audio) {
		t.Fatalf("产物字节不符: %q", raw.Body)
	}
}

// TestSyncBinaryUsesBinaryLimit 锁死「二进制端点按 32 MiB 读」。
//
// 一段 10 分钟的 WAV 就有十几 MB：按 JSON 的 8 MiB 上限读会把正常音频
// 判成"上游体过大"（502），而用户只是朗读了一段长文。
func TestSyncBinaryUsesBinaryLimit(t *testing.T) {
	big := make([]byte, relay.MaxUpstreamBodyBytes+1024) // 8 MiB + 1 KiB
	copy(big, "RIFF....WAVE")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	a := mustAdapter(t, syncTTSSpec)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-x"}
	raw, err := a.Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "tts-x", Body: json.RawMessage(`{"input":"长文","voice":"v1"}`),
	}, "/v1/audio/speech")
	if err != nil {
		t.Fatalf("超过 JSON 上限但未超过二进制上限时不应报错: %v", err)
	}
	if len(raw.Body) != len(big) {
		t.Fatalf("应完整取回 %d 字节，实际 %d", len(big), len(raw.Body))
	}
}

// TestSyncJSONKeepsJSONLimit 反向锁死：JSON 端点不能因为放宽二进制上限
// 而变成"多大都读"——那是一次 OOM 换来的便利。
func TestSyncJSONKeepsJSONLimit(t *testing.T) {
	big := make([]byte, relay.MaxUpstreamBodyBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	const chatSpec = `{
  "slug": "chat-json",
  "label": "对话",
  "endpoints": {
    "chat/completions": {"method": "POST", "path": "/v1/chat/completions", "passthrough": true}
  }
}`
	a := mustAdapter(t, chatSpec)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-x"}
	_, err := a.Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: "m", Body: json.RawMessage(`{"model":"m","messages":[]}`),
	}, "/v1/chat/completions")
	if !errors.Is(err, relay.ErrUpstreamBodyTooLarge) {
		t.Fatalf("JSON 端点超 8 MiB 必须报 ErrUpstreamBodyTooLarge，实际 %v", err)
	}
}
