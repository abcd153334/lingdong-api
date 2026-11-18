package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingdong-api/internal/relay"
)

// duomiRec 记录一次上游请求的观测值。
type duomiRec struct {
	method string
	path   string
	query  url.Values
	auth   string
	ct     string
	body   map[string]any
	raw    string
	hits   *atomic.Int32
}

// startDuomi 起一个假上游；reply 决定状态码与响应体。
func startDuomi(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *duomiRec) {
	t.Helper()
	var hits atomic.Int32
	rec := &duomiRec{hits: &hits}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.query = r.URL.Query()
		rec.auth = r.Header.Get("Authorization")
		rec.ct = r.Header.Get("Content-Type")
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec.raw = string(buf)
		_ = json.Unmarshal(buf, &rec.body)
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func duomiJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func duomiCfg(base string, opts []byte) *relay.ChannelConfig {
	return &relay.ChannelConfig{BaseURL: base, APIKey: "dm-123", Timeout: 5 * time.Second, Options: opts}
}

// submitDuomi 提交并返回 (taskID, req)，req 用于断言回写的 SubKind。
func submitDuomi(t *testing.T, cfg *relay.ChannelConfig, modelID, body string) (string, *relay.TaskSubmitRequest, error) {
	t.Helper()
	req := &relay.TaskSubmitRequest{ModelID: modelID, Body: []byte(body)}
	id, err := DuoMi{}.SubmitTask(context.Background(), cfg, req)
	return id, req, err
}

func wantDuomiCaller(t *testing.T, err error, contains string) {
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

func TestDuoMiContracts(t *testing.T) {
	var a relay.Adapter = DuoMi{}
	var _ relay.TaskAdapter = DuoMi{}
	var _ relay.DetailedTaskPoller = DuoMi{}
	var _ relay.EndpointSupporter = DuoMi{}
	var _ relay.OptionsSupporter = DuoMi{}
	var _ relay.DeclarativeSupporter = DuoMi{}

	if a.Name() != "duomiapi" {
		t.Fatalf("名称不对：%s", a.Name())
	}
	caps := a.Caps()
	for _, c := range caps {
		if c == "tts" {
			t.Fatalf("不能声明 tts，否则同步 /audio/speech 会选到该渠道：%v", caps)
		}
	}
	es := a.(relay.EndpointSupporter)
	// 图像的两个**同步**端点是唯二放开的：多米上游只有异步，但 OpenAI SDK 只调同步，
	// 所以由适配器内部做「提交 → 轮询 → 转 OpenAI 响应」的桥接（见 duomiapi_sync_image.go）。
	for _, k := range []relay.EndpointKind{
		relay.KindImagesGenerations, relay.KindImagesEdits,
	} {
		if !es.SupportsEndpoint(k) {
			t.Fatalf("图像同步端点 %v 必须声明支持，否则标准客户端无法出图", k)
		}
	}
	// 其余全部保持「不支持」：多米的 chat / 语音 / 嵌入都没有对应子协议，
	// 一旦放开就会被选路选中，然后在上游 404 —— 用户看到的是「渠道不支持」而不是「模型不支持」。
	for _, k := range []relay.EndpointKind{
		relay.KindChat, relay.KindCompletions, relay.KindEmbeddings,
		relay.KindImagesVariations,
		relay.KindAudioSpeech, relay.KindAudioTranscriptions, relay.KindAudioTranslations,
		relay.KindModerations, relay.KindRerank, relay.KindResponses,
	} {
		if es.SupportsEndpoint(k) {
			t.Fatalf("同步端点 %v 必须声明不支持，否则会接走 chat", k)
		}
	}
	for _, key := range []string{
		"video/generations", "images/generations",
		"audio/music/generations", "audio/lyrics",
	} {
		if !relay.TaskEndpointSupported("duomiapi", key) {
			t.Fatalf("多米必须承接 %s（多路径视频 + nano/gpt-image + SUNO）", key)
		}
	}
	if relay.TaskEndpointSupported("duomiapi", "video/regenerations") {
		t.Fatalf("多米没有再生成语义，不得声明 video/regenerations")
	}
	if a.(relay.DeclarativeSupporter).SupportsDeclarativeConfig() {
		t.Fatalf("不应支持声明式配置")
	}
	if !relay.SupportsAdapterOptions("duomiapi") {
		t.Fatalf("必须声明支持渠道私有配置")
	}
	// 同步方法必须全部返回「不支持端点」，而不是空实现成功。
	ctx := context.Background()
	cfg := duomiCfg("https://x", nil)
	if _, err := (DuoMi{}).Chat(ctx, cfg, nil); !errors.Is(err, errDuoMiNoEndpoint) {
		t.Fatalf("Chat 应返回不支持：%v", err)
	}
	if _, err := (DuoMi{}).Forward(ctx, cfg, nil, "/x"); !errors.Is(err, errDuoMiNoEndpoint) {
		t.Fatalf("Forward 应返回不支持：%v", err)
	}
}

// TestDuoMiPollTaskOfDispatch 校验 relay.PollTaskOf 能识别新接口（分派点唯一性）。
func TestDuoMiPollTaskOfDispatch(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"t1","state":"succeeded","data":{"videos":[{"url":"https://cdn.example.com/v.mp4"}]}}`)
	})
	ta, ok := relay.GetTaskAdapter("duomiapi")
	if !ok {
		t.Fatalf("适配器未注册")
	}
	st, err := relay.PollTaskOf(context.Background(), ta, duomiCfg(srv.URL, nil), &relay.TaskPollRequest{
		UpstreamTaskID: "4fc6d48c-d064-dfef-f8de-6bac185c663f", SubKind: duomiSubSeedance,
	})
	if err != nil {
		t.Fatalf("轮询失败：%v", err)
	}
	if st.Status != "succeeded" || len(st.ResultURLs) != 1 {
		t.Fatalf("状态或产物不符：%+v", st)
	}
	if rec.path != "/api/v3/contents/generations/tasks/4fc6d48c-d064-dfef-f8de-6bac185c663f" {
		t.Fatalf("查询路径不对：%s", rec.path)
	}
}

// ---------------------------------------------------------------- seedance

func TestDuoMiSeedanceSubmit(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"abc-123"}`)
	})
	body := `{"model":"seedance-2.0","prompt":"一只猫","seconds":5,"resolution":"720p","aspect_ratio":"16:9",
		"input_reference":[
			{"type":"first_frame","image_url":"https://cdn.example.com/f.png"},
			{"type":"image","image_url":"https://cdn.example.com/a.png"},
			{"type":"video","video_url":"https://cdn.example.com/b.mp4"},
			{"type":"audio","audio_url":"https://cdn.example.com/c.mp3"}
		],
		"generate_audio":false,"metadata":{"watermark":true}}`
	id, req, err := submitDuomi(t, duomiCfg(srv.URL, nil), "seedance-2.0", body)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if id != "abc-123" {
		t.Fatalf("任务号不对：%s", id)
	}
	if req.SubKind != duomiSubSeedance {
		t.Fatalf("SubKind 应为 %s，实际 %s", duomiSubSeedance, req.SubKind)
	}
	if rec.method != "POST" || rec.path != "/api/v3/contents/generations/tasks" {
		t.Fatalf("请求不对：%s %s", rec.method, rec.path)
	}
	if rec.auth != "Bearer dm-123" {
		t.Fatalf("鉴权头不对：%q", rec.auth)
	}
	if rec.body["model"] != "doubao-seedance-2-0-260128" {
		t.Fatalf("model 应替换为上游别名，实际 %v", rec.body["model"])
	}
	if rec.body["duration"] != float64(5) || rec.body["resolution"] != "720p" || rec.body["ratio"] != "16:9" {
		t.Fatalf("时长/档位/比例不符：%+v", rec.body)
	}
	// 顶层显式字段优先于 metadata（P1-25）：generate_audio=false 必须原样下发，
	// 不能被「未配置就不下发」的 nil 判定吃掉——false 与「未指定」是两件事。
	if rec.body["generate_audio"] != false {
		t.Fatalf("顶层 generate_audio 应下发 false：%+v", rec.body)
	}
	if rec.body["watermark"] != true {
		t.Fatalf("允许清单内的键应下发：%+v", rec.body)
	}
	content, _ := rec.body["content"].([]any)
	if len(content) != 5 {
		t.Fatalf("content 应有 text + 4 个素材，实际 %d：%+v", len(content), content)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "一只猫" {
		t.Fatalf("首项应为 text：%+v", first)
	}
	second, _ := content[1].(map[string]any)
	if second["role"] != "first_frame" {
		t.Fatalf("第二个素材应为首帧：%+v", second)
	}
	// 素材顺序：首帧 → reference_image → reference_video → reference_audio
	if role(content[2].(map[string]any)) != "reference_image" ||
		role(content[3].(map[string]any)) != "reference_video" ||
		role(content[4].(map[string]any)) != "reference_audio" {
		t.Fatalf("素材顺序不符（会让 prompt 里的指代错位）：%+v", content)
	}
	// 出站体不得含平台内部字段
	for _, k := range []string{"seconds", "size", "input_reference"} {
		if _, has := rec.body[k]; has {
			t.Fatalf("出站体不应含平台字段 %s：%+v", k, rec.body)
		}
	}
}

func role(m map[string]any) string {
	if s, ok := m["role"].(string); ok {
		return s
	}
	return ""
}

func TestDuoMiSeedanceRejects(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"x"}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	cases := []struct {
		name, model, body, want string
	}{
		{"未知模型", "nope-1", `{"model":"nope-1","prompt":"a","seconds":5,"resolution":"720p"}`, "未知的多米模型"},
		{"内联素材", "seedance-2.0", `{"model":"seedance-2.0","prompt":"a","seconds":5,"resolution":"720p",
			"input_reference":[{"type":"image","image_url":"data:image/png;base64,AAAA"}]}`, "data:"},
		{"未知 metadata", "seedance-2.0", `{"model":"seedance-2.0","prompt":"a","seconds":5,"resolution":"720p",
			"metadata":{"billing_policy":"x"}}`, "metadata.billing_policy"},
	}
	for _, c := range cases {
		before := rec.hits.Load()
		_, _, err := submitDuomi(t, cfg, c.model, c.body)
		if err == nil {
			t.Fatalf("%s：应失败", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：文案应含 %q，实际 %q", c.name, c.want, err.Error())
		}
		if rec.hits.Load() != before {
			t.Fatalf("%s：参数错必须零出站", c.name)
		}
	}
	// 没写秒数不再拒绝：**不下发** duration，由模型方用自己的默认值。
	// 平台侧按预算冻结、结算以上游实际时长为准，账实仍然一致。
	{
		srv2, rec2 := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
			duomiJSON(w, 200, `{"id":"x"}`)
		})
		if _, _, err := submitDuomi(t, duomiCfg(srv2.URL, nil), "seedance-2.0",
			`{"model":"seedance-2.0","prompt":"a","resolution":"720p"}`); err != nil {
			t.Fatalf("没写秒数不应拒绝：%v", err)
		}
		if _, ok := rec2.body["duration"]; ok {
			t.Fatalf("没写秒数时不得编造 duration，实际 %#v", rec2.body["duration"])
		}
	}
	// 未知模型属配置错，不是用户参数错
	_, _, err := submitDuomi(t, cfg, "nope-1", `{"model":"nope-1","prompt":"a","seconds":5,"resolution":"720p"}`)
	if errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("未知模型应报渠道配置错而不是参数错：%v", err)
	}
}

func TestDuoMiSeedancePollShapes(t *testing.T) {
	// schema 形态：state + data.videos
	srv, _ := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"t","state":"succeeded","progress":100,"data":{"videos":[{"url":"https://cdn.example.com/a.mp4"}]}}`)
	})
	st, err := (DuoMi{}).PollTaskDetailed(context.Background(), duomiCfg(srv.URL, nil),
		&relay.TaskPollRequest{UpstreamTaskID: "t1", SubKind: duomiSubSeedance})
	if err != nil || st.Status != "succeeded" || st.ResultURLs[0] != "https://cdn.example.com/a.mp4" {
		t.Fatalf("schema 形态解析失败：%+v %v", st, err)
	}

	// example 形态：status + content.video_url
	srv2, _ := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"t","status":"succeeded","content":{"video_url":"https://cdn.example.com/b.mp4"}}`)
	})
	st, err = (DuoMi{}).PollTaskDetailed(context.Background(), duomiCfg(srv2.URL, nil),
		&relay.TaskPollRequest{UpstreamTaskID: "t1", SubKind: duomiSubSeedance})
	if err != nil || st.Status != "succeeded" || st.ResultURLs[0] != "https://cdn.example.com/b.mp4" {
		t.Fatalf("example 形态解析失败：%+v %v", st, err)
	}

	// queued / running
	srv3, _ := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"t","state":"pending","progress":0}`)
	})
	st, _ = (DuoMi{}).PollTaskDetailed(context.Background(), duomiCfg(srv3.URL, nil),
		&relay.TaskPollRequest{UpstreamTaskID: "t1", SubKind: duomiSubSeedance})
	if st.Status != "queued" {
		t.Fatalf("pending 应归一为 queued，实际 %s", st.Status)
	}

	// 成功但无产物 → failed（宁可退费，也不能结算成功却拿不到文件）
	srv4, _ := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"t","state":"succeeded"}`)
	})
	st, _ = (DuoMi{}).PollTaskDetailed(context.Background(), duomiCfg(srv4.URL, nil),
		&relay.TaskPollRequest{UpstreamTaskID: "t1", SubKind: duomiSubSeedance})
	if st.Status != "failed" {
		t.Fatalf("成功但无产物应判失败，实际 %s", st.Status)
	}
}

// ---------------------------------------------------------------- kling

func TestDuoMiKlingSegments(t *testing.T) {
	seg := `{"id":"k1"}`
	cases := []struct {
		name      string
		model     string
		body      string
		wantPath  string
		wantSub   string
		wantModel string
	}{
		{"文生视频", "kling-v3-std",
			`{"model":"kling-v3-std","prompt":"hi","seconds":5,"resolution":"720p"}`,
			"/api/video/kling/v1/videos/text2video", "kling.text2video", "kling-v3"},
		{"图生视频", "kling-v3-pro",
			`{"model":"kling-v3-pro","prompt":"hi","seconds":10,"resolution":"1080p",
			  "input_reference":[{"type":"image","image_url":"https://cdn.example.com/a.png"}]}`,
			"/api/video/kling/v1/videos/image2video", "kling.image2video", "kling-v3"},
		{"多图参考", "kling-v1.6-multi",
			`{"model":"kling-v1.6-multi","prompt":"hi","seconds":5,"resolution":"720p",
			  "input_reference":[
			    {"type":"image","image_url":"https://cdn.example.com/a.png"},
			    {"type":"image","image_url":"https://cdn.example.com/b.png"}]}`,
			"/api/video/kling/v1/videos/multi-image2video", "kling.multi-image2video", "kling-v1-6"},
		{"omni", "kling-v3-omni",
			`{"model":"kling-v3-omni","prompt":"hi","seconds":3,"resolution":"1080p"}`,
			"/api/video/kling/v1/videos/omni-video", "kling.omni-video", "kling-v3-omni"},
	}
	for _, c := range cases {
		srv, rec := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
			duomiJSON(w, 200, seg)
		})
		id, req, err := submitDuomi(t, duomiCfg(srv.URL, nil), c.model, c.body)
		if err != nil {
			t.Fatalf("%s：提交失败 %v", c.name, err)
		}
		if id != "k1" {
			t.Fatalf("%s：任务号不对 %s", c.name, id)
		}
		if rec.path != c.wantPath {
			t.Fatalf("%s：路径应为 %s，实际 %s", c.name, c.wantPath, rec.path)
		}
		if req.SubKind != c.wantSub {
			t.Fatalf("%s：SubKind 应为 %s，实际 %s", c.name, c.wantSub, req.SubKind)
		}
		if rec.body["model_name"] != c.wantModel {
			t.Fatalf("%s：model_name 应为 %s，实际 %v", c.name, c.wantModel, rec.body["model_name"])
		}
		// 可灵没有 resolution 参数，绝不能发给上游
		if _, has := rec.body["resolution"]; has {
			t.Fatalf("%s：不应下发 resolution（上游无此参数）：%+v", c.name, rec.body)
		}
	}
}

func TestDuoMiKlingModeFromProfileOnly(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"k1"}}`)
	})
	// 请求体里塞 mode=pro 也不能改变档案（否则能用 std 价买 pro 画质）
	_, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-std",
		`{"model":"kling-v3-std","prompt":"hi","seconds":5,"mode":"pro"}`)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["mode"] != "std" {
		t.Fatalf("mode 必须来自档案，实际 %v", rec.body["mode"])
	}
}

// TestDuoMiKlingPassesParamsThrough 守「时长 / 档位 / 素材张数一律不拦」。
//
// 这些限制曾经是适配器里的硬编码（Durations 5/10、FixedTier、MaxTier、最多 4 张）：
// 上游一放宽、或同一模型 ID 换了上游，平台就先把自己的用户挡在门外，
// 而且拒绝理由听起来像"模型不支持"，实际是代码没跟上。
func TestDuoMiKlingPassesParamsThrough(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"k1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	cases := []struct{ name, body string }{
		{"时长不在 5/10", `{"model":"kling-v3-std","prompt":"hi","seconds":7}`},
		{"档位与型号默认不同", `{"model":"kling-v3-std","prompt":"hi","seconds":5,"resolution":"1080p"}`},
		{"omni 时长超常见区间", `{"model":"kling-v3-omni","prompt":"hi","seconds":16}`},
		{"多图超过 4 张", `{"model":"kling-v1.6-multi","prompt":"hi","seconds":5,
			"input_reference":[
			  {"type":"image","image_url":"https://cdn.example.com/1.png"},
			  {"type":"image","image_url":"https://cdn.example.com/2.png"},
			  {"type":"image","image_url":"https://cdn.example.com/3.png"},
			  {"type":"image","image_url":"https://cdn.example.com/4.png"},
			  {"type":"image","image_url":"https://cdn.example.com/5.png"}]}`},
	}
	for _, c := range cases {
		before := rec.hits.Load()
		if _, _, err := submitDuomi(t, cfg, "", c.body); err != nil {
			t.Fatalf("%s：应透传而不是拦截：%v", c.name, err)
		}
		if rec.hits.Load() != before+1 {
			t.Fatalf("%s：应真实出站一次", c.name)
		}
	}
}

func TestDuoMiKlingPoll(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{
			"task_id":"t","task_status":"succeed","task_status_msg":null,
			"task_result":{"images":null,"videos":[{"id":"v","url":"https://cdn.example.com/k.mp4","duration":"5"}]}}}`)
	})
	// SubKind 决定查询段：这条用例锁死「提交走 image2video、轮询也必须走 image2video」
	st, err := (DuoMi{}).PollTaskDetailed(context.Background(), duomiCfg(srv.URL, nil),
		&relay.TaskPollRequest{UpstreamTaskID: "abc-1", SubKind: duomiSubKlingImg})
	if err != nil {
		t.Fatalf("轮询失败：%v", err)
	}
	if st.Status != "succeeded" || st.ResultURLs[0] != "https://cdn.example.com/k.mp4" {
		t.Fatalf("解析不符：%+v", st)
	}
	if rec.path != "/api/video/kling/v1/videos/image2video/abc-1" {
		t.Fatalf("查询路径不对：%s", rec.path)
	}

	// 业务码失败（如任务不存在）→ failed，而不是等到超时退费
	srv2, _ := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"code":1203,"message":"task not found","data":null}`)
	})
	st, err = (DuoMi{}).PollTaskDetailed(context.Background(), duomiCfg(srv2.URL, nil),
		&relay.TaskPollRequest{UpstreamTaskID: "abc-1", SubKind: duomiSubKlingText})
	if err != nil || st.Status != "failed" {
		t.Fatalf("业务码失败应归一为 failed：%+v %v", st, err)
	}
}

// ---------------------------------------------------------------- 错误分档

func TestDuoMiErrorClasses(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		caller   bool
		contains string
	}{
		{400, `{"code":1201,"message":"bad param"}`, true, "参数"},
		{422, `{"message":"invalid"}`, true, "参数"},
		{401, `{"message":"no auth"}`, false, "鉴权失败"},
		{402, `{"message":"no money"}`, false, "余额不足"},
		{404, `{"message":"no path"}`, false, "路径不存在"},
		{429, `{"message":"slow down"}`, false, "限流"},
		{500, `{"message":"boom"}`, false, "500"},
	}
	for _, c := range cases {
		srv, _ := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
			duomiJSON(w, c.status, c.body)
		})
		_, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "seedance-2.0",
			`{"model":"seedance-2.0","prompt":"a","seconds":5,"resolution":"720p"}`)
		if err == nil {
			t.Fatalf("HTTP %d 应失败", c.status)
		}
		if got := errors.Is(err, relay.ErrCallerParam); got != c.caller {
			t.Fatalf("HTTP %d 的 caller 归因应为 %v，实际 %v（%v）", c.status, c.caller, got, err)
		}
		if !strings.Contains(err.Error(), c.contains) {
			t.Fatalf("HTTP %d 文案应含 %q，实际 %q", c.status, c.contains, err.Error())
		}
	}
}

// ---------------------------------------------------------------- 配置

func TestDuomiOptionsParse(t *testing.T) {
	// 空配置 = 全默认
	opts, err := parseDuomiOptions(nil)
	if err != nil || opts.authPrefix != duomiDefaultAuthPrefix || !opts.metadataAllow["generate_audio"] || !opts.metadataAllow["watermark"] {
		t.Fatalf("空配置应回落默认：%+v %v", opts, err)
	}
	// auth_prefix "-" = 空前缀
	opts, err = parseDuomiOptions([]byte(`{"auth_prefix":"-"}`))
	if err != nil || opts.authPrefix != "" {
		t.Fatalf("auth_prefix \"-\" 应等于空前缀：%q %v", opts.authPrefix, err)
	}
	// 合法覆盖（用**未内置**的对外模型 ID：内置模型不允许改协议，见下方 bad 用例）
	opts, err = parseDuomiOptions([]byte(`{"model_overrides":{"my-kling-pro":{"protocol":"kling","mode":"pro"}}}`))
	if err != nil || opts.modelOverrides["my-kling-pro"].Mode != "pro" {
		t.Fatalf("覆盖表解析失败：%+v %v", opts, err)
	}
	// 非法：未知字段 / 非法协议 / 非法档位 / 非法路径 / 已知模型改协议
	bad := []string{
		`{"pathsX":{}}`,
		`{"model_overrides":{"m":{"protocol":"nano.old"}}}`,
		`{"model_overrides":{"m":{"protocol":"nano.edit"}}}`,
		`{"paths":{"seedance.submit":"api/x"}}`,
		`{"paths":{"unknown.key":"/x"}}`,
		`{"model_overrides":{"seedance-2.0":{"protocol":"kling"}}}`,
		`{"auth_prefix":"a\nb"}`,
		`{"metadata_allow":["billing_policy"]}`,
	}
	for _, raw := range bad {
		if _, err := parseDuomiOptions([]byte(raw)); err == nil {
			t.Fatalf("配置 %s 应被拒绝", raw)
		}
	}
}

func TestDuomiAuthPrefixApplied(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
		duomiJSON(w, 200, `{"id":"x"}`)
	})
	_, _, err := submitDuomi(t, duomiCfg(srv.URL, []byte(`{"auth_prefix":"-"}`)), "seedance-2.0",
		`{"model":"seedance-2.0","prompt":"a","seconds":5,"resolution":"720p"}`)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.auth != "dm-123" {
		t.Fatalf("空前缀应只发裸 token，实际 %q", rec.auth)
	}
}

// TestDuomiConfigErrorBlocksSubmit 校验配置脏了必须 fail-closed（不允许按默认配置出网）。
func TestDuomiConfigErrorBlocksSubmit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		duomiJSON(w, 200, `{"id":"x"}`)
	}))
	t.Cleanup(srv.Close)
	cfg := duomiCfg(srv.URL, nil)
	cfg.OptionsErr = &relay.ConfigError{Field: "adapter_options", Msg: "坏了"}
	if _, _, err := submitDuomi(t, cfg, "seedance-2.0",
		`{"model":"seedance-2.0","prompt":"a","seconds":5,"resolution":"720p"}`); err == nil {
		t.Fatalf("配置脏了应拒绝提交")
	}
	if hits.Load() != 0 {
		t.Fatalf("配置错必须零出站")
	}
}

// ---------------------------------------------------------------- 探活

func TestDuomiTest(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"200", 200, `{"code":0,"data":null}`, false},
		{"404+信封", 404, `{"code":1203,"message":"task not found"}`, false},
		{"404+非信封", 404, `<html>not found</html>`, true},
		{"401", 401, `{"message":"no auth"}`, true},
		{"429+信封", 429, `{"code":1302,"message":"rate limited"}`, false},
		{"429+非信封", 429, `{"message":"slow"}`, true},
		{"400+信封", 400, `{"code":1201,"message":"bad"}`, false},
		{"400+非信封", 400, `{"message":"bad"}`, true},
	}
	for _, c := range cases {
		srv, _ := startDuomi(t, func(w http.ResponseWriter, r *http.Request) {
			duomiJSON(w, c.status, c.body)
		})
		err := (DuoMi{}).Test(context.Background(), duomiCfg(srv.URL, nil))
		if (err != nil) != c.wantErr {
			t.Fatalf("%s：wantErr=%v，实际 %v", c.name, c.wantErr, err)
		}
	}
}

// ---------------------------------------------------------------- 纯函数

func TestDuomiNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"succeed": "succeeded", "succeeded": "succeeded", "3": "succeeded", "done": "succeeded",
		"error": "failed", "2": "failed",
		"submitted": "queued", "1": "queued", "pending": "queued",
		"processing": "running", "in_progress": "running", "whatever": "running",
		"canceled": "canceled",
	}
	for in, want := range cases {
		if got := duomiNormalizeStatus(in); got != want {
			t.Fatalf("%s want %s, got %s", in, want, got)
		}
	}
}

// TestDuomiResolutionValueIsLiteral 守「分辨率原样下发」。
//
// 这里曾把 `1280x720` 归一成 `720p`、把 `3840x2160` 归一成 `1080p`（短边映射）。
// 那等于替用户改参数：用户按文档要精确像素，平台却按自己的档位表猜一个枚举值，
// 猜错的后果是「按 A 档收钱、按 B 档出图」，账单上完全看不出来。
func TestDuomiResolutionValueIsLiteral(t *testing.T) {
	cases := map[string]string{
		"720P": "720P", "1280x720": "1280x720", "1280×720": "1280×720",
		" 1080p ": "1080p", "": "",
	}
	for in, want := range cases {
		if got := duomiResolutionValue(in); got != want {
			t.Fatalf("%q 应原样下发，实际 %q", in, got)
		}
	}
}

func TestDuomiExtractTaskIDShapes(t *testing.T) {
	ok := map[string]string{
		`{"id":"a-1"}`:                        "a-1",
		`{"task_id":"b-2"}`:                   "b-2",
		`{"data":{"task_id":"c-3"}}`:          "c-3",
		`{"data":[{"task_id":"d-4"}]}`:        "d-4",
		`{"code":0,"data":{"task_id":"e-5"}}`: "e-5",
	}
	for body, want := range ok {
		got, err := duomiExtractTaskID([]byte(body))
		if err != nil || got != want {
			t.Fatalf("%s want %s, got %s (%v)", body, want, got, err)
		}
	}
	for _, body := range []string{`{"code":0}`, `{"id":"../etc"}`, `{}`} {
		if _, err := duomiExtractTaskID([]byte(body)); err == nil {
			t.Fatalf("%s 应报错", body)
		}
	}
}

// TestDuomiEnvelopeErrorCarriesMsg 锁定「业务拒绝必须带回上游原话」。
//
// nano 系失败形状是 `{"code":400,"msg":"...","data":null}` —— 原因在 `msg`。
// 只读 `message` 时，所有业务拒绝都会退化成「上游返回业务错误」，
// 用户与运维都拿不到可行动的信息（2026-11-04 实测丢了整句
// `gemini-3.1-flash-lite-image does not support 2K or 4K resolutions`）。
func TestDuomiEnvelopeErrorCarriesMsg(t *testing.T) {
	bodies := []string{
		`{"code":400,"msg":"upstream said no","data":null}`,
		`{"code":400,"message":"msg field wins","msg":"ignored"}`,
		`{"code":400,"error":{"code":400,"msg":"nested msg"}}`,
		`{"code":400,"error":{"code":400,"message":"nested message"}}`,
	}
	wants := []string{"upstream said no", "msg field wins", "nested msg", "nested message"}
	for i, body := range bodies {
		err, bad := duomiEnvelopeError([]byte(body))
		if !bad || err == nil {
			t.Fatalf("应识别为业务失败：%s", body)
		}
		if !strings.Contains(err.Error(), wants[i]) {
			t.Fatalf("文案应含 %q，实际 %q", wants[i], err.Error())
		}
		if strings.Contains(err.Error(), "上游返回业务错误") {
			t.Fatalf("有原话时不得落到兜底文案：%s", err.Error())
		}
	}
	// 没有任何原因字段时才允许兜底（保证错误形状不退化）。
	err, bad := duomiEnvelopeError([]byte(`{"code":500}`))
	if !bad || !strings.Contains(err.Error(), "上游返回业务错误") {
		t.Fatalf("无原因字段应回兜底文案，实际 %v", err)
	}
}
