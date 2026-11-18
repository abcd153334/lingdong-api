package task

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/crypto"
	"lingdong-api/internal/relay"
)

// ---------------------------------------------------------------- 纯函数用例

// TestParseSubmitStoresInputRefs 锁定「素材 type + 链接」落库：没有它，重试就只能发纯文本。
func TestParseSubmitStoresInputRefs(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"p","seconds":5,"resolution":"720p","seed":42,
		"input_reference":[
			{"type":"image","image_url":"https://cdn.example.com/1.png"},
			{"type":"audio","audio_url":"https://cdn.example.com/a.mp3"},
			{"type":"first_frame","image_url":"https://cdn.example.com/f.png"},
			{"type":"last_frame","image_url":"https://cdn.example.com/l.png"}
		]}`)
	req, err := ParseSubmit(body)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if req.Params.HasInline {
		t.Fatalf("http(s) 素材不应标记为内联")
	}
	wantTypes := []string{"image", "audio", "first_frame", "last_frame"}
	if len(req.Params.InputRef) != len(wantTypes) {
		t.Fatalf("input_ref 数量 = %d，期望 %d", len(req.Params.InputRef), len(wantTypes))
	}
	for i, want := range wantTypes {
		if req.Params.InputRef[i].Type != want {
			t.Fatalf("第 %d 个 type = %q，期望 %q", i, req.Params.InputRef[i].Type, want)
		}
		if strings.TrimSpace(req.Params.InputRef[i].URL) == "" {
			t.Fatalf("第 %d 个 URL 为空", i)
		}
	}
	if req.Params.Seed == nil || *req.Params.Seed != 42 {
		t.Fatalf("seed 未落库：%v", req.Params.Seed)
	}
	// 计费口径：首尾帧独立成 frame 类别，**不计素材费**（P2-8）。
	//
	// 首尾帧是视频自身的首/末帧，上游已含在"档位×秒数"的价里；计成 image 就是
	// 每单多加 N 张 ref_image_price —— 而图生视频普遍带首帧，等于普遍多收。
	wantKinds := []string{"image", "audio", "frame", "frame"}
	if len(req.Params.References) != len(wantKinds) {
		t.Fatalf("references 数量 = %d", len(req.Params.References))
	}
	for i, want := range wantKinds {
		if req.Params.References[i].Kind != want {
			t.Fatalf("第 %d 个 kind = %q，期望 %q（会改变计费）", i, req.Params.References[i].Kind, want)
		}
	}
}

// TestParseSubmitExtractsNativeContent：原生体（content[]）的素材必须落进计费视图。
//
// 这是"不落库就等于免费"的那一类缺陷：参考图价目配了，但只要用户按官方文档写
// content[]，References 就是空的 —— 素材费全免，而且 SSRF 校验整条被绕过
// （原生体会被原样转发，上游替平台去抓内网地址）。
func TestParseSubmitExtractsNativeContent(t *testing.T) {
	body := []byte(`{"model":"m","content":[
		{"type":"text","text":"一只猫"},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/1.png"},"role":"reference_image"},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/2.png"},"role":"reference_image"},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/f.png"},"role":"first_frame"},
		{"type":"image_url","image_url":{"url":"asset://img-preset"},"role":"reference_image"},
		{"type":"video_url","video_url":{"url":"https://cdn.example.com/v.mp4"},"role":"reference_video"},
		{"type":"audio_url","audio_url":{"url":"https://cdn.example.com/a.mp3"},"role":"reference_audio"},
		{"type":"draft_task","draft_task":{"id":"draft-1"}}
	]}`)
	req, err := ParseSubmit(body)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	// text / draft_task 不是素材，不占位置；其余 6 项按原顺序。
	wantKinds := []string{"image", "image", "frame", "image", "video", "audio"}
	if len(req.Params.References) != len(wantKinds) {
		t.Fatalf("references = %d 项，期望 %d：%+v", len(req.Params.References), len(wantKinds), req.Params.References)
	}
	for i, want := range wantKinds {
		if got := req.Params.References[i].Kind; got != want {
			t.Fatalf("第 %d 个 kind = %q，期望 %q", i, got, want)
		}
	}
	// asset:// 是上游素材库 ID，不是外链：必须落库（可重放）且不被 SSRF 拒。
	if got := req.Params.InputRef[3]; got.URL != "asset://img-preset" || got.Type != "reference_image" {
		t.Fatalf("素材库 ID 未落库或类型被改写：%+v", got)
	}
	u := usageOfRequest(req)
	if u.RefImages != 3 || u.RefVideos != 1 || u.RefAudios != 1 {
		t.Fatalf("计费口径不对：图 %d / 视频 %d / 音频 %d（期望 3/1/1）", u.RefImages, u.RefVideos, u.RefAudios)
	}
}

// TestParseSubmitRejectsMixedRefStyles：content[] 与 input_reference 互斥。
//
// 同时接受会造出两个真相源：计费按 content、出站按 input_reference
// （Ark 原生通道只认 content，input_reference 会被当成未知字段原样发过去）。
func TestParseSubmitRejectsMixedRefStyles(t *testing.T) {
	_, err := ParseSubmit([]byte(`{"model":"m","content":[{"type":"text","text":"猫"}],
		"input_reference":[{"type":"image","image_url":"https://cdn.example.com/1.png"}]}`))
	if !errors.Is(err, ErrInvalidParam) || !strings.Contains(err.Error(), "二选一") {
		t.Fatalf("want ErrInvalidParam(二选一)，got %v", err)
	}
}

// TestParseSubmitRejectsUnknownContentType：不认识的内容类型必须 400，绝不静默跳过。
func TestParseSubmitRejectsUnknownContentType(t *testing.T) {
	_, err := ParseSubmit([]byte(`{"model":"m","content":[{"type":"hologram","hologram":{"url":"https://cdn/x"}}]}`))
	if !errors.Is(err, ErrInvalidParam) || !strings.Contains(err.Error(), "hologram") {
		t.Fatalf("want ErrInvalidParam(含类型名)，got %v", err)
	}
}

// TestParseSubmitNativeInlineMarksInline：原生体里的内联素材同样要记 has_inline
// （否则重试会静默丢掉素材，而结算仍按素材计费）。
func TestParseSubmitNativeInlineMarksInline(t *testing.T) {
	req, err := ParseSubmit([]byte(`{"model":"m","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"},"role":"first_frame"}]}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !req.Params.HasInline || len(req.Params.InputRef) != 0 {
		t.Fatalf("内联素材处理不对：has_inline=%v refs=%+v", req.Params.HasInline, req.Params.InputRef)
	}
	if len(req.Refs) != 1 || req.Refs[0].Kind != "frame" {
		t.Fatalf("内联素材仍要计入 references：%+v", req.Refs)
	}
}

// TestReplayResendsSubmitBody：重试必须**原文重发** tasks.submit_body。
//
// 改造前重试按 request_params 快照重建请求体，重建必然丢字段 —— 客户端的
// output_format、厂商新增参数、音乐任务的 lyrics 全都消失。症状是「首次成功、
// 重试 400」或「重试出来的东西和第一次不一样」，而两次代码单看都正确。
func TestReplayResendsSubmitBody(t *testing.T) {
	body := []byte(`{"model":"vid-v","prompt":"p","seconds":5,"resolution":"720p","aspect_ratio":"16:9",
		"watermark":false,"generate_audio":true,"output_format":"mov","frames":121}`)
	req, err := ParseSubmit(body)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	row := &model.Task{
		Model: "vid-v", Modality: "video",
		RequestParams: model.MustJSON(req.Params), SubmitBody: model.JSON(req.Body),
	}

	var got map[string]any
	if err := json.Unmarshal(replayedBody(row), &got); err != nil {
		t.Fatalf("重放体不是 JSON: %v", err)
	}
	// 未声明的键一个都不能少（这正是重建路径做不到的）。
	for k, want := range map[string]any{
		"watermark": false, "generate_audio": true, "output_format": "mov", "frames": float64(121),
		"seconds": float64(5), "aspect_ratio": "16:9", "prompt": "p",
	} {
		if got[k] != want {
			t.Fatalf("重放体字段 %s = %v，期望 %v（全量：%v）", k, got[k], want, got)
		}
	}

	// 没有 submit_body（本列上线前创建的行）：返回空对象而不是猜一份报文。
	// 让适配器报「请求体为空」比用重建体去赌上游行为更诚实。
	empty := &model.Task{Model: "vid-v", Modality: "video", RequestParams: model.MustJSON(req.Params)}
	if out := string(replayedBody(empty)); out != "{}" {
		t.Fatalf("无 submit_body 时应返回 {}，实际 %s", out)
	}
}

// TestParseSubmitMarksInline 内联素材不落 URL，但必须记住「有内联」（否则重试会静默丢素材）。
func TestParseSubmitMarksInline(t *testing.T) {
	req, err := ParseSubmit([]byte(`{"model":"m","prompt":"p","seconds":5,"resolution":"480p",
		"input_reference":[{"type":"image","image_url":"data:image/png;base64,AAAA"}]}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !req.Params.HasInline {
		t.Fatalf("应标记 has_inline")
	}
	if len(req.Params.InputRef) != 0 {
		t.Fatalf("内联素材不应落 URL，实际 %+v", req.Params.InputRef)
	}
	if len(req.Params.References) != 1 {
		t.Fatalf("内联素材仍要计入 references（计费口径）")
	}
}

func TestParseSubmitSeedSources(t *testing.T) {
	req, err := ParseSubmit([]byte(`{"model":"m","prompt":"p","seconds":5,"resolution":"480p","metadata":{"seed":7}}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if req.Params.Seed == nil || *req.Params.Seed != 7 {
		t.Fatalf("metadata.seed 未生效：%v", req.Params.Seed)
	}
	for _, bad := range []string{
		`{"model":"m","prompt":"p","seconds":5,"resolution":"480p","seed":1.5}`,
		`{"model":"m","prompt":"p","seconds":5,"resolution":"480p","seed":"abc"}`,
		`{"model":"m","prompt":"p","seconds":5,"resolution":"480p","metadata":{"seed":1.5}}`,
	} {
		if _, err := ParseSubmit([]byte(bad)); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("非整数 seed 应拒绝：%s", bad)
		}
	}
}

// TestNormalizeFoldsFirstLastFrame 顶层首尾帧必须折叠进 input_reference 并删除原键。
func TestNormalizeFoldsFirstLastFrame(t *testing.T) {
	out, err := normalizeSubmitBody([]byte(`{"model":"m","prompt":"p","seconds":5,"resolution":"720p",
		"first_frame":"https://cdn.example.com/f.png","last_frame":"https://cdn.example.com/l.png","fps":24}`))
	if err != nil {
		t.Fatalf("归一失败：%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if _, ok := m["first_frame"]; ok {
		t.Fatalf("顶层 first_frame 必须删除（否则上游会收到两套写法）")
	}
	if _, ok := m["last_frame"]; ok {
		t.Fatalf("顶层 last_frame 必须删除")
	}
	refs, _ := m["input_reference"].([]any)
	if len(refs) != 2 {
		t.Fatalf("input_reference = %d 项，期望 2", len(refs))
	}
	first, _ := refs[0].(map[string]any)
	last, _ := refs[1].(map[string]any)
	if first["type"] != "first_frame" || first["image_url"] != "https://cdn.example.com/f.png" {
		t.Fatalf("首帧折叠不对：%v", first)
	}
	if last["type"] != "last_frame" || last["image_url"] != "https://cdn.example.com/l.png" {
		t.Fatalf("尾帧折叠不对：%v", last)
	}
	if _, ok := m["fps"]; !ok {
		t.Fatalf("其余字段必须原样保留")
	}
}

// 已有同 type 的 input_reference 时不得重复追加（否则会撞「首帧只能有 1 个」）。
func TestNormalizeFirstLastNoDuplicate(t *testing.T) {
	out, err := normalizeSubmitBody([]byte(`{"model":"m","prompt":"p","seconds":5,"resolution":"720p",
		"first_frame":"https://cdn.example.com/f.png",
		"input_reference":[{"type":"first_frame","image_url":"https://cdn.example.com/f.png"}]}`))
	if err != nil {
		t.Fatalf("归一失败：%v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	refs, _ := m["input_reference"].([]any)
	if len(refs) != 1 {
		t.Fatalf("input_reference = %d 项，期望 1（不应重复）", len(refs))
	}
}

// ---------------------------------------------------------------- 数据库集成用例

// callerParamAdapter 返回「调用方参数错」，并按调用顺序记录渠道 ID。
type callerParamAdapter struct {
	mu    sync.Mutex
	calls []uint64
}

func (*callerParamAdapter) Name() string                                     { return "fake-caller" }
func (*callerParamAdapter) Caps() []string                                   { return []string{"video"} }
func (*callerParamAdapter) Test(context.Context, *relay.ChannelConfig) error { return nil }
func (*callerParamAdapter) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("not implemented")
}
func (*callerParamAdapter) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("not implemented")
}
func (*callerParamAdapter) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("not implemented")
}
func (*callerParamAdapter) StreamChat(context.Context, *relay.ChannelConfig, *relay.RelayRequest, func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return nil, errors.New("not implemented")
}
func (*callerParamAdapter) Forward(context.Context, *relay.ChannelConfig, *relay.RelayRequest, string) (*relay.RawResponse, error) {
	return nil, errors.New("not implemented")
}
func (a *callerParamAdapter) SubmitTask(_ context.Context, cfg *relay.ChannelConfig, _ *relay.TaskSubmitRequest) (string, error) {
	a.mu.Lock()
	a.calls = append(a.calls, cfg.ID)
	a.mu.Unlock()
	return "", &relay.CallerParamError{Msg: "本模型不支持 1080p"}
}
func (*callerParamAdapter) PollTask(context.Context, *relay.ChannelConfig, string) (*relay.TaskStatus, error) {
	return &relay.TaskStatus{Status: "running"}, nil
}

// seedChannelFor 造一条指定适配器的可路由渠道。
func seedChannelFor(t *testing.T, gdb *gorm.DB, modelID, adapterID string, priority int) *model.Channel {
	t.Helper()
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-master-key")
	conf.Load()
	token, err := crypto.FernetEncrypt(crypto.FernetDerive("unit-test-master-key"), []byte("sk-test"))
	if err != nil {
		t.Fatalf("加密渠道密钥失败：%v", err)
	}
	keys, err := model.NewJSON([]string{token})
	if err != nil {
		t.Fatalf("序列化渠道密钥失败：%v", err)
	}
	ch := &model.Channel{
		Name: "caller-" + uuid.NewString()[:8], AdapterID: adapterID, BaseURL: "https://up.invalid",
		Keys: keys, Status: 1, Priority: priority, Weight: 1,
	}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("造渠道失败：%v", err)
	}
	if err := gdb.Create(&model.ModelBinding{ModelID: modelID, ChannelID: ch.ID, Enabled: true, Weight: 1}).Error; err != nil {
		t.Fatalf("造绑定失败：%v", err)
	}
	return ch
}

// TestRetryCallerParamStopsAtFirstCandidate 参数错必须立即终止：
// 只打第一个候选、返回 ErrInvalidParam、全额释放且任务回到 failed。
func TestRetryCallerParamStopsAtFirstCandidate(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	price := int64(1000)

	ch1 := seedChannelFor(t, gdb, "vid-caller", "fake-caller", 1)
	ch2 := seedChannelFor(t, gdb, "vid-caller", "fake-caller", 2)

	ad := &callerParamAdapter{}
	if err := relay.RegisterAdapter(ad); err != nil {
		t.Fatalf("注册测试适配器失败：%v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-caller") })

	row := seedFailedTask(t, gdb, uid, kid, "vid-caller", price)
	_, err := Retry(ctx, row.ID, AllScope())
	if !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("参数错应返回 ErrInvalidParam，实际 %v", err)
	}
	if len(ad.calls) != 1 || ad.calls[0] != ch1.ID {
		t.Fatalf("参数错应立即终止，实际调用渠道 %v（期望只调用 %d）", ad.calls, ch1.ID)
	}
	if ch2.ID == 0 {
		t.Fatal("第二个候选应存在（否则本用例证明不了「不换候选」）")
	}

	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("重试失败后应回到 failed，实际 %s", got.Status)
	}
	if !strings.Contains(got.ErrorMsg, "1080p") {
		t.Fatalf("失败原因应可定位，实际 %q", got.ErrorMsg)
	}
	// 新会话已释放：用户额度回到初始，无冻结残留。
	assertQuota(t, gdb, uid, kid, 100000, 0, 0)
}

// TestRetryRejectsUnreplayableTasks 含内联素材 / 旧版本任务一律拒绝重试（且不产生新冻结）。
func TestRetryRejectsUnreplayableTasks(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	price := int64(1000)
	seedChannelFor(t, gdb, "vid-unreplayable", "fake-caller", 1)
	ad := &callerParamAdapter{}
	if err := relay.RegisterAdapter(ad); err != nil {
		t.Fatalf("注册测试适配器失败：%v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-caller") })

	cases := map[string]params{
		"内联素材": {Model: "vid-unreplayable", Prompt: "p", Seconds: 5, Resolution: "480p",
			References: []RefSummary{{Kind: "image"}}, HasInline: true},
		"旧任务缺链接": {Model: "vid-unreplayable", Prompt: "p", Seconds: 5, Resolution: "480p",
			References: []RefSummary{{Kind: "image"}, {Kind: "audio"}}},
	}
	for name, p := range cases {
		row := seedFailedTask(t, gdb, uid, kid, "vid-unreplayable", price)
		if err := gdb.Model(row).Update("request_params", model.MustJSON(p)).Error; err != nil {
			t.Fatalf("[%s] 写参数快照失败：%v", name, err)
		}
		if _, err := Retry(ctx, row.ID, AllScope()); !errors.Is(err, ErrNotRetryable) {
			t.Fatalf("[%s] 应拒绝重试，实际 %v", name, err)
		}
		var got model.Task
		if err := gdb.First(&got, row.ID).Error; err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusFailed {
			t.Fatalf("[%s] 状态不应被改动，实际 %s", name, got.Status)
		}
	}
	if len(ad.calls) != 0 {
		t.Fatalf("不可重放的任务不应发起上游请求，实际 %v", ad.calls)
	}
	assertQuota(t, gdb, uid, kid, 100000, 0, 0)
}
