package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingdong-api/internal/relay"
)

// metasoRec 记录一次上游请求的观测值。
type metasoRec struct {
	path  string
	query string
	auth  string
	body  map[string]any
	raw   string
}

// startMetaso 起一个假的上游；reply 决定状态码与响应体。
func startMetaso(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *metasoRec, *atomic.Int32) {
	t.Helper()
	rec := &metasoRec{}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.auth = r.Header.Get("Authorization")
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec.raw = string(buf)
		_ = json.Unmarshal(buf, &rec.body)
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec, &hits
}

func metasoJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func metasoCfg(base string, opts []byte) *relay.ChannelConfig {
	return &relay.ChannelConfig{BaseURL: base, APIKey: "mk-123", Timeout: 5 * time.Second, Options: opts}
}

func submitMetaso(t *testing.T, cfg *relay.ChannelConfig, body string) (string, error) {
	t.Helper()
	return Metaso{}.SubmitTask(context.Background(), cfg, &relay.TaskSubmitRequest{Body: []byte(body)})
}

func wantMetasoCaller(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望参数错误，实际成功")
	}
	if !errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("期望 ErrCallerParam，实际 %v", err)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("文案应包含 %q，实际 %q", contains, err.Error())
	}
}

// ---------------------------------------------------------------- 契约

func TestMetasoContracts(t *testing.T) {
	var a relay.Adapter = Metaso{}
	var _ relay.TaskAdapter = Metaso{}
	var _ relay.EndpointSupporter = Metaso{}
	var _ relay.OptionsSupporter = Metaso{}
	var _ relay.DeclarativeSupporter = Metaso{}

	if a.Name() != "metaso" {
		t.Fatalf("名称不对：%s", a.Name())
	}
	if caps := a.Caps(); len(caps) != 1 || caps[0] != "video" {
		t.Fatalf("能力子集应为 [video]，实际 %v", caps)
	}
	es := a.(relay.EndpointSupporter)
	for _, k := range []relay.EndpointKind{
		relay.KindChat, relay.KindCompletions, relay.KindEmbeddings,
		relay.KindImagesGenerations, relay.KindImagesEdits, relay.KindImagesVariations,
		relay.KindAudioSpeech, relay.KindAudioTranscriptions, relay.KindAudioTranslations,
		relay.KindModerations, relay.KindRerank, relay.KindResponses,
	} {
		if es.SupportsEndpoint(k) {
			t.Fatalf("同步端点 %v 必须声明不支持，否则会接走 chat", k)
		}
	}
	if !relay.TaskEndpointSupported("metaso", "video/generations") {
		t.Fatalf("必须承接 video/generations")
	}
	if !relay.TaskEndpointSupported("metaso", "video/regenerations") {
		t.Fatalf("metaso 是唯一有再生成语义的上游，必须承接 video/regenerations")
	}
	if relay.TaskEndpointSupported("metaso", "audio/music/generations") {
		t.Fatalf("不得声明承接音乐端点（上游没有该接口）")
	}
	if a.(relay.DeclarativeSupporter).SupportsDeclarativeConfig() {
		t.Fatalf("不应支持声明式配置")
	}
	if !relay.SupportsAdapterOptions("metaso") {
		t.Fatalf("必须声明支持渠道私有配置")
	}
	// 同步方法必须全部返回「不支持端点」，而不是空实现成功。
	ctx := context.Background()
	if _, err := a.Chat(ctx, nil, nil); !errors.Is(err, errMetasoNoEndpoint) {
		t.Fatalf("Chat 应返回 NoEndpoint，实际 %v", err)
	}
	if _, err := a.Completions(ctx, nil, nil); !errors.Is(err, errMetasoNoEndpoint) {
		t.Fatalf("Completions 应返回 NoEndpoint，实际 %v", err)
	}
	if _, err := a.Embeddings(ctx, nil, nil); !errors.Is(err, errMetasoNoEndpoint) {
		t.Fatalf("Embeddings 应返回 NoEndpoint，实际 %v", err)
	}
	if _, err := a.StreamChat(ctx, nil, nil, nil); !errors.Is(err, errMetasoNoEndpoint) {
		t.Fatalf("StreamChat 应返回 NoEndpoint，实际 %v", err)
	}
	if _, err := a.Forward(ctx, nil, nil, ""); !errors.Is(err, errMetasoNoEndpoint) {
		t.Fatalf("Forward 应返回 NoEndpoint，实际 %v", err)
	}
}

// ---------------------------------------------------------------- 生成请求组装

// TestMetasoSubmitSuccess 覆盖「发什么、发到哪、怎么鉴权」。
func TestMetasoSubmitSuccess(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"task-abc"}`)
	})
	id, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{
		"model":"metaso-h3","prompt":"一只橘猫弹钢琴","seconds":6,"resolution":"720p","aspect_ratio":"16:9",
		"input_reference":[{"type":"image","image_url":"https://cdn.example.com/a.png"}]
	}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != "task-abc" {
		t.Fatalf("want task-abc, got %q", id)
	}
	if rec.path != "/api/minimax/v2/video_generation" {
		t.Fatalf("路径不对：%s", rec.path)
	}
	if rec.auth != "Bearer mk-123" {
		t.Fatalf("鉴权头不对：%q", rec.auth)
	}
	if got := rec.body["model"]; got != metasoDefaultModelAlias {
		t.Fatalf("上游模型应兜底为 %s，实际 %v", metasoDefaultModelAlias, got)
	}
	if got := rec.body["resolution"]; got != "768P" {
		t.Fatalf("720p 应映射为 768P，实际 %v", got)
	}
	if got := rec.body["duration"]; got != float64(6) {
		t.Fatalf("duration 应为 6，实际 %v", got)
	}
	if got := rec.body["ratio"]; got != "16:9" {
		t.Fatalf("ratio 应为 16:9，实际 %v", got)
	}
	if got := rec.body["fast_mode"]; got != false {
		t.Fatalf("fast_mode 只能来自渠道配置（默认 false），实际 %v", got)
	}
	content, _ := rec.body["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content 应为 [text, image]，实际 %v", rec.body["content"])
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "一只橘猫弹钢琴" {
		t.Fatalf("首项必须是 text：%v", first)
	}
	second, _ := content[1].(map[string]any)
	if second["type"] != "image_url" || second["role"] != "reference_image" {
		t.Fatalf("图片项 type/role 不对：%v", second)
	}
	img, _ := second["image_url"].(map[string]any)
	if img["url"] != "https://cdn.example.com/a.png" {
		t.Fatalf("图片 URL 不对：%v", img)
	}
}

// TestMetasoContentOrder 锁定 content 的固定顺序：换顺序会让同一份输入产出不同结果。
func TestMetasoContentOrder(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{
		"model":"metaso-h3","prompt":"p","seconds":5,"resolution":"1080p",
		"input_reference":[
			{"type":"reference_audio","audio_url":"https://e/a.mp3"},
			{"type":"video","video_url":"https://e/v.mp4"},
			{"type":"image","image_url":"https://e/i2.png"},
			{"type":"last_frame","image_url":"https://e/last.png"},
			{"type":"first_frame","image_url":"https://e/first.png"},
			{"type":"image","image_url":"https://e/i1.png"}
		]
	}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	content, _ := rec.body["content"].([]any)
	want := []string{"text", "first_frame", "last_frame", "reference_image", "reference_image", "reference_video", "reference_audio"}
	if len(content) != len(want) {
		t.Fatalf("content 长度应为 %d，实际 %d：%v", len(want), len(content), content)
	}
	for i, w := range want {
		item, _ := content[i].(map[string]any)
		got := item["type"].(string)
		if w != "text" {
			got = item["role"].(string)
		}
		if got != w {
			t.Fatalf("第 %d 项应为 %s，实际 %s（%v）", i, w, got, item)
		}
	}
}

// TestMetasoFastModeOnlyFromChannel：请求体里的 fast_mode 一律不读。
func TestMetasoFastModeOnlyFromChannel(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	_, err := submitMetaso(t, metasoCfg(srv.URL, []byte(`{"fast_mode":true}`)),
		`{"model":"x","prompt":"p","seconds":5,"resolution":"720p","fast_mode":false}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := rec.body["fast_mode"]; got != true {
		t.Fatalf("fast_mode 必须取渠道配置 true，实际 %v", got)
	}
}

// TestMetasoContextIRFromChannel：context_ir_enabled 只认渠道配置，nil 时不下发。
func TestMetasoContextIRFromChannel(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	body := `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`
	if _, err := submitMetaso(t, metasoCfg(srv.URL, nil), body); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if _, ok := rec.body["context_ir_enabled"]; ok {
		t.Fatalf("未配置时不应下发 context_ir_enabled：%v", rec.body)
	}
	if _, err := submitMetaso(t, metasoCfg(srv.URL, []byte(`{"context_ir_enabled":true}`)), body); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := rec.body["context_ir_enabled"]; got != true {
		t.Fatalf("应从渠道配置下发 true，实际 %v", got)
	}
}

// TestMetasoMetadataAllowlist 是「计费完整性命中」的防线：
// metadata 只能带 aigc_watermark，其余键必须 400 且零出站。
func TestMetasoMetadataAllowlist(t *testing.T) {
	srv, rec, hits := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	base := `"model":"x","prompt":"p","seconds":5,"resolution":"720p"`
	for _, bad := range []string{
		`{"billing_policy":"pay_as_you_go"}`,
		`{"context_ir_enabled":true}`,
		`{"callback_url":"https://evil.example.com/cb"}`,
		`{"fast_mode":true}`,
		`{"model":"other"}`,
		`{"resolution":"2K"}`,
		`{"duration":15}`,
		`{"source_task_id":"t9"}`,
	} {
		_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{`+base+`,"metadata":`+bad+`}`)
		wantMetasoCaller(t, err, "metadata 仅支持 aigc_watermark")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("负例不应产生任何出站请求，实际 %d 次（最后一次 body=%s）", n, rec.raw)
	}
	// 唯一放行的键
	if _, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{`+base+`,"metadata":{"aigc_watermark":true}}`); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got := rec.body["aigc_watermark"]; got != true {
		t.Fatalf("aigc_watermark 应透传，实际 %v", got)
	}
	// 类型不对也要拒
	_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{`+base+`,"metadata":{"aigc_watermark":"yes"}}`)
	wantMetasoCaller(t, err, "布尔值")
}

// TestMetasoDurationBounds 覆盖时长边界（含 2K 收紧到 15 秒）。
func TestMetasoDurationBounds(t *testing.T) {
	srv, _, hits := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	cases := []struct {
		res  string
		secs int
		ok   bool
	}{
		{"720p", 3, false},
		{"720p", 4, true},
		{"720p", 30, true},
		{"720p", 31, false},
		{"2k", 15, true},
		{"2k", 16, false},
		{"1080p", 16, true}, // 1080P 不是 2K 档，按 30 秒上限
	}
	for _, c := range cases {
		body := `{"model":"x","prompt":"p","seconds":` + strconv.Itoa(c.secs) + `,"resolution":"` + c.res + `"}`
		_, err := submitMetaso(t, metasoCfg(srv.URL, nil), body)
		if c.ok && err != nil {
			t.Fatalf("%s %ds 应通过，实际 %v", c.res, c.secs, err)
		}
		if !c.ok {
			wantMetasoCaller(t, err, "秒的整数")
		}
	}
	_ = hits
}

// TestMetasoResolutionMap 覆盖四档映射与「未开通即拒绝」。
func TestMetasoResolutionMap(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	cases := map[string]string{"480p": "480P", "720p": "768P", "1080p": "1080P", "2k": "2K"}
	for tier, upstream := range cases {
		if _, err := submitMetaso(t, metasoCfg(srv.URL, nil),
			`{"model":"x","prompt":"p","seconds":5,"resolution":"`+tier+`"}`); err != nil {
			t.Fatalf("%s 应通过，实际 %v", tier, err)
		}
		if got := rec.body["resolution"]; got != upstream {
			t.Fatalf("%s 应映射为 %s，实际 %v", tier, upstream, got)
		}
	}
	// 运营把 1080p 清空 → 该档拒绝，且不静默降档
	_, err := submitMetaso(t, metasoCfg(srv.URL, []byte(`{"resolution_map":{"1080p":""}}`)),
		`{"model":"x","prompt":"p","seconds":5,"resolution":"1080p"}`)
	wantMetasoCaller(t, err, "未开通 1080p 档")
	// 未配置的档位键直接报错
	if err := (Metaso{}).ValidateAdapterOptions([]byte(`{"resolution_map":{"4k":"2K"}}`)); err == nil {
		t.Fatal("未知档位键应被拒绝")
	}
	if err := (Metaso{}).ValidateAdapterOptions([]byte(`{"resolution_map":{"720p":"4K"}}`)); err == nil {
		t.Fatal("白名单外的上游值应被拒绝")
	}
}

// TestMetasoRefRejections 覆盖素材层的拒绝矩阵，且全部零出站。
func TestMetasoRefRejections(t *testing.T) {
	srv, _, hits := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	base := `"model":"x","prompt":"p","seconds":5,"resolution":"720p"`
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"未知类型", `[{"type":"hologram","url":"https://e/x"}]`, "不支持的素材类型"},
		{"内联 data:", `[{"type":"image","image_url":"data:image/png;base64,AAAA"}]`, "/v1/files"},
		{"缺链接", `[{"type":"image"}]`, "缺少链接"},
		{"首帧重复", `[{"type":"first_frame","image_url":"https://e/1.png"},{"type":"first_frame","image_url":"https://e/2.png"}]`, "first_frame 只能有 1 个"},
		{"尾帧重复", `[{"type":"last_frame","image_url":"https://e/1.png"},{"type":"last-frame","image_url":"https://e/2.png"}]`, "last_frame 只能有 1 个"},
	}
	for _, c := range cases {
		_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{`+base+`,"input_reference":`+c.ref+`}`)
		if err == nil || !errors.Is(err, relay.ErrCallerParam) || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 期望含 %q 的参数错误，实际 %v", c.name, c.want, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("负例不应出站，实际 %d 次", n)
	}
}

// TestMetasoPromptRequired：空 prompt 直接 400，不打上游。
func TestMetasoPromptRequired(t *testing.T) {
	srv, _, hits := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{"model":"x","prompt":"  ","seconds":5,"resolution":"720p"}`)
	wantMetasoCaller(t, err, "prompt 不能为空")
	if hits.Load() != 0 {
		t.Fatal("不应出站")
	}
}

// TestMetasoRatio：非法画幅拒绝，缺省 adaptive。
func TestMetasoRatio(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	body := `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`
	if _, err := submitMetaso(t, metasoCfg(srv.URL, nil), body); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// 文生视频：上游不接受 adaptive，缺省必须是具体画幅（实测 400「ratio 不能为空或 adaptive」）。
	if rec.body["ratio"] != metasoDefaultTextRatio {
		t.Fatalf("文生视频缺省应为 %s，实际 %v", metasoDefaultTextRatio, rec.body["ratio"])
	}
	_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{"model":"x","prompt":"p","seconds":5,"resolution":"720p","aspect_ratio":"2:1"}`)
	wantMetasoCaller(t, err, "画幅比例")
	// 显式 adaptive 也是错的：本地就该说清楚「要具体画幅」，不要等上游 400 再透传。
	_, err = submitMetaso(t, metasoCfg(srv.URL, nil), `{"model":"x","prompt":"p","seconds":5,"resolution":"720p","ratio":"adaptive"}`)
	wantMetasoCaller(t, err, "文生视频必须指定具体画幅")
}

// ---------------------------------------------------------------- 提交响应与错误分档

func TestMetasoSubmitResponseShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"顶层 task_id", `{"task_id":"a-1"}`, "a-1"},
		{"task.id", `{"task":{"id":"b-2"}}`, "b-2"},
		{"task.task_id", `{"task":{"task_id":"c-3"}}`, "c-3"},
	}
	for _, c := range cases {
		srv, _, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
			metasoJSON(w, 200, c.body)
		})
		got, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`)
		if err != nil || got != c.want {
			t.Fatalf("%s: want %q/nil, got %q/%v", c.name, c.want, got, err)
		}
	}
}

// TestMetasoSubmitMissingTaskID：200 但没有 task_id 必须判失败，否则任务空转到超时。
func TestMetasoSubmitMissingTaskID(t *testing.T) {
	srv, _, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"status":"queued"}`)
	})
	_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`)
	if err == nil || !strings.Contains(err.Error(), "缺少 task_id") {
		t.Fatalf("want 缺少 task_id，got %v", err)
	}
}

// TestMetasoErrorEnvelopeOn200：上游用 200 + 错误信封表达业务拒绝。
func TestMetasoErrorEnvelopeOn200(t *testing.T) {
	srv, _, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"type":"error","http_code":400,"message":"content risk"}`)
	})
	_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`)
	if !errors.Is(err, relay.ErrCallerParam) || !strings.Contains(err.Error(), "content risk") {
		t.Fatalf("want CallerParam/content risk，got %v", err)
	}
}

// TestMetasoHTTPErrorTiers 覆盖状态码分档。
func TestMetasoHTTPErrorTiers(t *testing.T) {
	cases := []struct {
		status  int
		caller  bool
		contain string
	}{
		{400, true, "拒绝了本次参数"},
		{422, true, "拒绝了本次参数"},
		{401, false, "鉴权失败"},
		{403, false, "鉴权失败"},
		{402, false, "余额不足"},
		{404, false, "路径不存在"},
		{405, false, "路径不存在"},
		{429, false, "限流"},
		{503, false, "上游返回 503"},
	}
	for _, c := range cases {
		srv, _, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
			metasoJSON(w, c.status, `{"message":"boom"}`)
		})
		_, err := submitMetaso(t, metasoCfg(srv.URL, nil), `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`)
		if err == nil {
			t.Fatalf("%d: 应失败", c.status)
		}
		if errors.Is(err, relay.ErrCallerParam) != c.caller {
			t.Fatalf("%d: caller 归因错误：%v", c.status, err)
		}
		if !strings.Contains(err.Error(), c.contain) {
			t.Fatalf("%d: 文案应含 %q，实际 %q", c.status, c.contain, err.Error())
		}
	}
}

// TestMetasoOptionsFailClosed：脏配置必须在组装请求前失败（拿默认值跑等于按错价格出网）。
func TestMetasoOptionsFailClosed(t *testing.T) {
	srv, _, hits := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"t1"}`)
	})
	cfg := metasoCfg(srv.URL, nil)
	cfg.OptionsErr = &relay.ConfigError{Scope: "渠道适配器配置错误", Field: "adapter_options", Msg: "不是合法的 JSON"}
	_, err := submitMetaso(t, cfg, `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`)
	if err == nil || !strings.Contains(err.Error(), "adapter_options") {
		t.Fatalf("want OptionsErr，got %v", err)
	}
	// 空密钥同样在出站前失败
	if _, err := submitMetaso(t, &relay.ChannelConfig{BaseURL: srv.URL}, `{"model":"x","prompt":"p","seconds":5,"resolution":"720p"}`); err == nil {
		t.Fatal("空密钥应失败")
	}
	if hits.Load() != 0 {
		t.Fatalf("不应出站，实际 %d 次", hits.Load())
	}
}

// ---------------------------------------------------------------- 轮询

func TestMetasoPoll(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status string
		url    string
		errMsg string
	}{
		{"成功", `{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"}}}`, "succeeded", "https://cdn/v.mp4", ""},
		{"成功平铺", `{"status":"Success","url":"https://cdn/v.mp4"}`, "succeeded", "https://cdn/v.mp4", ""},
		{"成功数组", `{"task":{"status":"completed","content":[{"url":"https://cdn/v.mp4"}]}}`, "succeeded", "https://cdn/v.mp4", ""},
		{"成功 output", `{"task":{"status":"done","output":{"url":"https://cdn/v.mp4"}}}`, "succeeded", "https://cdn/v.mp4", ""},
		{"成功无产物", `{"task":{"status":"succeeded"}}`, "failed", "", "上游返回成功但缺少视频产物"},
		{"失败", `{"task":{"status":"failed","error":{"message":"审核不通过"}}}`, "failed", "", "审核不通过"},
		{"失败无原因", `{"task":{"status":"failure"}}`, "failed", "", "上游任务失败"},
		{"已取消", `{"task":{"status":"cancelled"}}`, "canceled", "", ""},
		{"处理中", `{"task":{"status":"processing"}}`, "processing", "", ""},
		{"未知状态", `{"task":{"status":"weird"}}`, "weird", "", ""},
	}
	for _, c := range cases {
		srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
			metasoJSON(w, 200, c.body)
		})
		got, err := Metaso{}.PollTask(context.Background(), metasoCfg(srv.URL, nil), "task-1")
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", c.name, err)
		}
		if got.Status != c.status || got.ResultURL != c.url || got.Error != c.errMsg {
			t.Fatalf("%s: want %s/%q/%q, got %s/%q/%q", c.name, c.status, c.url, c.errMsg, got.Status, got.ResultURL, got.Error)
		}
		if rec.path != "/api/minimax/v2/query/video_generation/task-1" {
			t.Fatalf("%s: 查询路径不对：%s", c.name, rec.path)
		}
	}
}

// TestMetasoPollCustomQueryPath：query_path 是运行时可调项（再生成路径不确定时的逃生门）。
func TestMetasoPollCustomQueryPath(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task":{"status":"succeeded","url":"https://cdn/v.mp4"}}`)
	})
	cfg := metasoCfg(srv.URL, []byte(`{"query_path":"/api/minimax/v2/query/video_regeneration/"}`))
	if _, err := (Metaso{}).PollTask(context.Background(), cfg, "task-9"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rec.path != "/api/minimax/v2/query/video_regeneration/task-9" {
		t.Fatalf("query_path 未生效：%s", rec.path)
	}
}

// TestMetasoPollIgnoresBrokenOptions：配置改坏不能让在途任务全部失败退费。
func TestMetasoPollIgnoresBrokenOptions(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task":{"status":"running"}}`)
	})
	cfg := metasoCfg(srv.URL, []byte(`{"fast_mode":`))
	cfg.OptionsErr = &relay.ConfigError{Field: "adapter_options", Msg: "不是合法的 JSON"}
	got, err := Metaso{}.PollTask(context.Background(), cfg, "task-1")
	if err != nil {
		t.Fatalf("轮询不应因脏配置失败：%v", err)
	}
	if got.Status != "running" {
		t.Fatalf("want running, got %s", got.Status)
	}
	if rec.path != "/api/minimax/v2/query/video_generation/task-1" {
		t.Fatalf("应回落到默认查询路径，实际 %s", rec.path)
	}
}

// TestMetasoPollRejectsBadTaskID：任务号字符集受限，防路径穿越与查询注入。
func TestMetasoPollRejectsBadTaskID(t *testing.T) {
	srv, _, hits := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{}`)
	})
	for _, id := range []string{"", "a/b", "a?x=1", "../../etc/passwd", strings.Repeat("a", 200)} {
		if _, err := (Metaso{}).PollTask(context.Background(), metasoCfg(srv.URL, nil), id); err == nil {
			t.Fatalf("%q 应被拒绝", id)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("不应出站，实际 %d 次", hits.Load())
	}
}

// ---------------------------------------------------------------- 探活

func TestMetasoTest(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"404 错误信封=可达", 404, `{"type":"error","http_code":404,"message":"task not found"}`, false},
		{"404 非信封=路径错", 404, `<html>not found</html>`, true},
		{"200=可达", 200, `{"task":{}}`, false},
		{"400=可达", 400, `{"message":"bad"}`, false},
		{"429=可达", 429, `{"message":"slow down"}`, false},
		{"401=鉴权失败", 401, `{"message":"bad token"}`, true},
		{"403=鉴权失败", 403, `{"message":"forbidden"}`, true},
		{"405=路径错", 405, ``, true},
		{"500=故障", 500, `{"message":"boom"}`, true},
	}
	for _, c := range cases {
		srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
			metasoJSON(w, c.status, c.body)
		})
		err := Metaso{}.Test(context.Background(), metasoCfg(srv.URL, nil))
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: wantErr=%v, got %v", c.name, c.wantErr, err)
		}
		if rec.path != "/api/minimax/v2/query/video_generation/__probe__" {
			t.Fatalf("%s: 探活路径不对：%s", c.name, rec.path)
		}
	}
	// 配置脏 → 探活直接失败（不拿默认值假装健康）
	cfg := metasoCfg("http://127.0.0.1:1", []byte(`{"resolution_map":{"9k":"2K"}}`))
	if err := (Metaso{}).Test(context.Background(), cfg); err == nil {
		t.Fatal("脏配置应探活失败")
	}
}

// ---------------------------------------------------------------- 配置解析

func TestMetasoOptionParsing(t *testing.T) {
	// 空/默认
	for _, raw := range []string{"", "null", "{}", "  "} {
		opts, err := parseMetasoOptions([]byte(raw))
		if err != nil {
			t.Fatalf("%q 应解析成功：%v", raw, err)
		}
		if opts.queryPath != metasoDefaultQueryPath || opts.fastMode || len(opts.resolutionMap) != 4 {
			t.Fatalf("%q 默认值不对：%+v", raw, opts)
		}
	}
	// 未知键被拒（DisallowUnknownFields）
	if _, err := parseMetasoOptions([]byte(`{"workflows":{}}`)); err == nil {
		t.Fatal("未知键应被拒绝")
	}
	// 两个对象拼在一起被拒
	if _, err := parseMetasoOptions([]byte(`{"fast_mode":true}{"fast_mode":false}`)); err == nil {
		t.Fatal("尾部多余内容应被拒绝")
	}
	// query_path 校验
	for _, bad := range []string{
		`{"query_path":"api/x"}`,                               // 缺前导 /
		`{"query_path":"/api?x=1"}`, `{"query_path":"/api#f"}`, // 带查询/片段
		`{"query_path":"/api"}`, // P2-03：缺尾斜杠（轮询拼接任务号会拼错）
	} {
		if _, err := parseMetasoOptions([]byte(bad)); err == nil {
			t.Fatalf("%s 应被拒绝", bad)
		}
	}
	// 超长被拒
	if _, err := parseMetasoOptions([]byte(`{"query_path":"/` + strings.Repeat("a", 300) + `"}`)); err == nil {
		t.Fatal("超长 query_path 应被拒绝")
	}
	// resolution_map 跨档映射必须被拒（P2-09）：按 1080p 的价拿到 2K 的成片 = 价格倒挂。
	for _, bad := range []string{
		`{"resolution_map":{"720p":"2K"}}`,
		`{"resolution_map":{"1080p":"2K"}}`,
		`{"resolution_map":{"2k":"1080P"}}`,
		`{"resolution_map":{"480p":"768P"}}`,
	} {
		if _, err := parseMetasoOptions([]byte(bad)); err == nil {
			t.Fatalf("跨档映射应被拒绝：%s", bad)
		}
	}
	// 同档等价映射与「清空（未开通）」合法
	opts, err := parseMetasoOptions([]byte(`{"fast_mode":true,"query_path":"/q/","resolution_map":{"720p":"768P","2k":""}}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !opts.fastMode || opts.queryPath != "/q/" || opts.resolutionMap["720p"] != "768P" || opts.resolutionMap["2k"] != "" {
		t.Fatalf("覆盖值不对：%+v", opts)
	}
}

// ---------------------------------------------------------------- 再生成

// TestMetasoRegeneration 覆盖再生成的两条入口与拒绝矩阵。
func TestMetasoRegeneration(t *testing.T) {
	srv, rec, hits := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"regen-1"}`)
	})
	cfg := metasoCfg(srv.URL, nil)

	// 源任务形态
	id, err := (Metaso{}).SubmitTask(context.Background(), cfg, &relay.TaskSubmitRequest{
		Kind: relay.TaskKindRegeneration,
		Body: []byte(`{"model":"metaso-h3-regen","resolution":"2k","source_task_id":"up-42"}`),
	})
	if err != nil || id != "regen-1" {
		t.Fatalf("want regen-1/nil, got %q/%v", id, err)
	}
	if rec.path != "/api/minimax/v2/video_regeneration" {
		t.Fatalf("再生成路径不对：%s", rec.path)
	}
	if rec.body["source_task_id"] != "up-42" {
		t.Fatalf("source_task_id 应透传，实际 %v", rec.body)
	}
	for _, k := range []string{"duration", "ratio", "fast_mode"} {
		if _, ok := rec.body[k]; ok {
			t.Fatalf("再生成不应下发 %s：%v", k, rec.body)
		}
	}

	// base_video 形态
	before := hits.Load()
	if _, err := (Metaso{}).SubmitTask(context.Background(), cfg, &relay.TaskSubmitRequest{
		Kind: relay.TaskKindRegeneration,
		Body: []byte(`{"model":"metaso-h3-regen","resolution":"2k","prompt":"更清晰","input_reference":[{"type":"base_video","video_url":"https://e/base.mp4"}]}`),
	}); err != nil {
		t.Fatalf("base_video 形态应通过：%v", err)
	}
	if hits.Load() != before+1 {
		t.Fatal("base_video 形态应出站一次")
	}
	content, _ := rec.body["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content 应为 [text, video]，实际 %v", rec.body["content"])
	}
	item, _ := content[1].(map[string]any)
	if item["role"] != "base_video" {
		t.Fatalf("base_video role 不对：%v", item)
	}

	// 拒绝矩阵（全部零出站）
	before = hits.Load()
	bad := []string{
		`{"resolution":"2k"}`,
		`{"resolution":"2k","source_task_id":"a","input_reference":[{"type":"base_video","video_url":"https://e/b.mp4"}]}`,
		`{"resolution":"1080p","source_task_id":"a"}`,
		`{"resolution":"2k","source_task_id":"a/b"}`,
	}
	for _, body := range bad {
		if _, err := (Metaso{}).SubmitTask(context.Background(), cfg, &relay.TaskSubmitRequest{
			Kind: relay.TaskKindRegeneration, Body: []byte(body),
		}); !errors.Is(err, relay.ErrCallerParam) {
			t.Fatalf("%s: 期望参数错误，实际 %v", body, err)
		}
	}
	if hits.Load() != before {
		t.Fatalf("拒绝矩阵不应出站：%d → %d", before, hits.Load())
	}
}

// TestMetasoTierParsing 锁定档位归一（与 task.normalizeVideoResolution 同源规则）。
func TestMetasoTierParsing(t *testing.T) {
	cases := map[string]string{
		"480p": "480p", "720P": "720p", "1080p": "1080p", "2K": "2k",
		"1280x720": "720p", "720×1280": "720p", "854x480": "480p",
		"1024x1024": "1080p", "768x1366": "1080p",
	}
	for in, want := range cases {
		got, err := metasoTier(in, "")
		if err != nil || got != want {
			t.Fatalf("%q → want %s, got %s/%v", in, want, got, err)
		}
	}
	// size 兜底
	if got, err := metasoTier("", "1280x720"); err != nil || got != "720p" {
		t.Fatalf("size 兜底失效：%s/%v", got, err)
	}
	// 无法识别 → 参数错（不是静默 480p）
	for _, bad := range []string{"", "4k", "abc", "x720"} {
		if _, err := metasoTier(bad, ""); err == nil {
			t.Fatalf("%q 应被拒绝", bad)
		} else if !errors.Is(err, relay.ErrCallerParam) {
			t.Fatalf("%q 应为参数错误，实际 %v", bad, err)
		}
	}
}
