package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	relayspec "lingdong-api/internal/relay/spec"
	"lingdong-api/internal/service/billing"
	specsvc "lingdong-api/internal/service/spec"
)

// indexttsSpec 是 AutoDL IndexTTS 的真实形状：上游只有「提交 + 查询」两个接口，
// 平台对外是同步的 /v1/audio/speech（二进制），计费按秒（¥0.001/秒，最低 ¥0.01）。
const indexttsSpec = `{
  "slug":"autodl-indextts","label":"AutoDL IndexTTS","vendor":"autodl",
  "endpoints":{
    "audio/speech":{
      "method":"POST","path":"/comfyui_workflow/{upstream_model}",
      "omit_model":true,"response_content_type":"audio/wav",
      "request_fields":[
        {"name":"reference_audio","type":"url","required":true,"desc":"参考音色的音频地址"}
      ],
      "body":{
        "copy":{
          "text":"$req.input",
          "prompt_simple":"$req.reference_audio",
          "emo_ref_audio":"$req.reference_audio"
        },
        "const":{"output_format":"wav"}
      },
      "extract":{"task_id":"$.data.task_id"},
      "errors":{"success":"body_code","code_path":"$.code","ok_codes":["Success"],"message_path":"$.msg"},
      "async":{"mode":"bridge","poll":"audio/speech@poll","timeout_secs":20,"interval_ms":200}
    },
    "audio/speech@poll":{
      "method":"GET","path":"/comfyui_workflow/result/{task_id}",
      "extract":{
        "status":"$.data.status","error":"$.msg",
        "result_urls":{"path":"$.data.results[*].url","where":{"type":"audio"}}
      },
      "status_map":{"succeeded":["SUCCESS","completed"],"failed":["FAILED"],"running":["RUNNING"]}
    }
  }
}`

// indexttsPlan 是助手应当产出的完整提案：供应商 → 规格 → 渠道 → 模型 → 绑定。
func indexttsPlan() string {
	spec := indexttsSpec
	return `{
	  "summary":"接入 AutoDL IndexTTS 语音合成（异步桥接 + 按秒计费）",
	  "operations":[
	    {"op":"create","entity":"vendor","key":"autodl","data":{"name":"AutoDL"}},
	    {"op":"create","entity":"adapter_spec","key":"autodl-indextts","data":{
	      "label":"AutoDL IndexTTS","vendor":"autodl","spec":` + spec + `}},
	    {"op":"create","entity":"channel","key":"autodl-tts","data":{
	      "name":"AutoDL-TTS","adapter_id":"spec:autodl-indextts","base_url":"%BASE%",
	      "keys":["sk-autodl"],"groups":["default"]}},
	    {"op":"create","entity":"model","key":"indextts","data":{
	      "display_name":"IndexTTS 语音合成","vendor":"autodl","modality":"audio",
	      "billing_mode":"spec","enabled":true,"endpoints":["audio/speech"],
	      "spec_prices":{"kind":"per_second", "max_seconds": 60,"per_second_price":1000,"min_price":10000}}},
	    {"op":"replace","entity":"model_bindings","key":"indextts","data":{
	      "bindings":[{"channel_key":"autodl-tts","upstream_model":"indextts"}]}}
	  ]
	}`
}

// TestIndexTTSEndToEnd 走完整链路：提案 → apply → 规格注册 → 桥接调用 → 按秒结算。
//
// 这条用例是「AI 能接入任何非标/异步接口」的验收锚点。它一旦红，说明下面某处断了：
//  1. 规格 DSL 能表达「同步端点 + 上游异步」（async.mode=bridge）；
//  2. 平台路径 /v1/audio/speech 能路由到规格声明的上游路径 /tts/submit；
//  3. 桥接能把「提交 → 查询 → 下载产物」折成一次同步二进制响应；
//  4. 音频按秒计费能落库、能按产物时长结算（含最低消费）。
func TestIndexTTSEndToEnd(t *testing.T) {
	// 上游是 httptest 起的回环服务：生产策略默认拒绝私网地址，测试必须显式放行。
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	gdb := withAgentDB(t)

	audio := []byte("RIFF\x24\x00\x00\x00WAVEfake")
	var polls int32
	var submitted map[string]any
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/comfyui_workflow/indextts":
			// 占位符 {upstream_model} 必须被替换成绑定里的 upstream_model。
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &submitted)
			_, _ = w.Write([]byte(`{"code":"Success","data":{"task_id":"t-tts-1"},"msg":""}`))
		case r.URL.Path == "/comfyui_workflow/result/t-tts-1":
			if atomic.AddInt32(&polls, 1) == 1 {
				_, _ = w.Write([]byte(`{"code":"Success","data":{"status":"RUNNING","results":[]}}`))
				return
			}
			// results 混排：封面图排第 0 位，音频排第 1 位。where 过滤必须挑到音频。
			_, _ = w.Write([]byte(`{"code":"Success","data":{"status":"completed","duration":196,"results":[` +
				`{"url":"` + srv.URL + `/files/1.png","type":"image"},` +
				`{"url":"` + srv.URL + `/files/1.wav","type":"audio"}]}}`))
		case r.URL.Path == "/files/1.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNGDATA"))
		case r.URL.Path == "/files/1.wav":
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(audio)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	prop := createProposal(t, strings.ReplaceAll(indexttsPlan(), "%BASE%", srv.URL))
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}

	// ① 规格落库且能解析成运行时适配器（保存期校验 + 注册表两处都要过）。
	sp, rerr := specsvc.ResolveBySlug("autodl-indextts")
	if rerr != nil {
		t.Fatalf("规格未注册：渠道运行期会找不到适配器（%v）", rerr)
	}
	adapter := relayspec.New(sp)

	// ② 渠道与模型落库形态正确。
	var ch model.Channel
	if err := gdb.Where("name = ?", "AutoDL-TTS").First(&ch).Error; err != nil {
		t.Fatalf("渠道未落库: %v", err)
	}
	if ch.AdapterID != "spec:autodl-indextts" {
		t.Fatalf("渠道适配器应为 spec:autodl-indextts，实际 %s", ch.AdapterID)
	}
	var m model.Model
	if err := gdb.Where("id = ?", "indextts").First(&m).Error; err != nil {
		t.Fatalf("模型未落库: %v", err)
	}
	if m.BillingMode != model.BillingModeSpec {
		t.Fatalf("模型应为按规格计费，实际 %s", m.BillingMode)
	}
	if m.SpecPrices.Kind != billing.SpecKindPerSecond || m.SpecPrices.PerSecondPrice == nil {
		t.Fatalf("按秒价目未落库: %+v", m.SpecPrices)
	}

	// ③ 桥接调用：平台路径 → 上游提交 → 轮询 → 下载。
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, APIKey: "sk-autodl"}
	req := &relay.RelayRequest{
		Model: "indextts", ModelID: "indextts",
		Body: json.RawMessage(`{"input":"你好，这是验收文本","reference_audio":"https://cdn.example.com/ref.mp3"}`),
	}
	raw, err := adapter.Forward(context.Background(), cfg, req, "/v1/audio/speech")
	if err != nil {
		t.Fatalf("桥接调用失败: %v", err)
	}
	if string(raw.Body) != string(audio) {
		t.Fatalf("产物字节不符: %q", raw.Body)
	}
	if raw.ContentType != "audio/wav" {
		t.Fatalf("响应类型应为 audio/wav，实际 %q", raw.ContentType)
	}
	// 上游返回的 data.duration=196 是**任务耗时**，绝不能当成音频时长：
	// 接进来会把 4 秒音频按 196 秒计费（≈49 倍）。适配器这里必须留 0，
	// 由 relay 用产物字节头探测真实时长。
	if raw.DurationSecs != 0 {
		t.Fatalf("适配器不应从任务耗时推断音频时长，实际 %d", raw.DurationSecs)
	}
	if submitted["text"] != "你好，这是验收文本" {
		t.Fatalf("input 应映射到 text: %+v", submitted)
	}
	if submitted["prompt_simple"] != "https://cdn.example.com/ref.mp3" ||
		submitted["emo_ref_audio"] != "https://cdn.example.com/ref.mp3" {
		t.Fatalf("reference_audio 应映射到上游的音频 URL 字段: %+v", submitted)
	}
	if _, exists := submitted["model"]; exists {
		t.Fatalf("omit_model=true 时不应写 model: %+v", submitted)
	}

	// ④ 结算：¥0.001/秒（1000 微），最低 ¥0.01（10000 微）。
	snap := billing.Snapshot{BillingMode: m.BillingMode, SpecPrice: m.SpecPrices}
	cases := []struct {
		secs int64
		want int64
	}{
		{20, 20000}, // 20 秒 × 1000
		{3, 10000},  // 3 秒 = 3000 → 抬到最低消费 10000
		{10, 10000}, // 恰好等于最低消费
		{4, 10000},  // 桥接实测的 4 秒也应按最低消费收
	}
	for _, c := range cases {
		got, err := billing.Charge(snap, billing.Usage{AudioDurationSecs: c.secs})
		if err != nil {
			t.Fatalf("%d 秒结算失败: %v", c.secs, err)
		}
		if got != c.want {
			t.Fatalf("%d 秒应收 %d 微，实际 %d", c.secs, c.want, got)
		}
	}
}
