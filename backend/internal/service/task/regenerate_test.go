package task

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
	// 再生成本轮改走「端点能力」判据（relay.TaskEndpointSupported），
	// 而能力来自适配器注册表：不导入就没有 metaso，判定恒为 false。
	_ "lingdong-api/internal/relay/adapters"
)

// TestParseRegenerateValidation 覆盖再生成请求体的结构校验（纯函数，无 DB）。
func TestParseRegenerateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 model", `{"resolution":"2k","source_task_id":"task_01HZZZZZZZZZZZZZZZZZZZZZZV"}`, "参数不合法"},
		{"两者都没有", `{"model":"m","resolution":"2k"}`, "source_task_id 或 base_video 之一"},
		{"两者同时有", `{"model":"m","resolution":"2k","source_task_id":"task_01HZZZZZZZZZZZZZZZZZZZZZZV","input_reference":[{"type":"base_video","video_url":"https://cdn.example.com/b.mp4"}]}`, "二选一"},
		{"任务号非法", `{"model":"m","resolution":"2k","source_task_id":"abc"}`, "参数不合法"},
	}
	for _, c := range cases {
		_, err := ParseRegenerate([]byte(c.body))
		if err == nil {
			t.Fatalf("%s: 应失败", c.name)
		}
		if !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("%s: 期望 ErrInvalidParam，实际 %v", c.name, err)
		}
		if !hasSubstr(err.Error(), c.want) {
			t.Fatalf("%s: 文案应含 %q，实际 %q", c.name, c.want, err.Error())
		}
	}
	// 缺 resolution 不再在解析期拒绝：配了 resolution_fixed 的模型（可灵）本就不传档位，
	// 在这里必填会让再生成直接 400。档位由 validateModality 决定。
	if _, err := ParseRegenerate([]byte(`{"model":"m","source_task_id":"task_01HZZZZZZZZZZZZZZZZZZZZZZV"}`)); err != nil {
		t.Fatalf("缺 resolution 不应在解析期被拒: %v", err)
	}
}

// TestParseRegenerateCarriesTaskRef：source_task_id 只接受本平台任务号。
//
// 解析层**不查库**（归属与存在性在 applyRegeneration 的 SQL 里判），
// 所以这里只断言"原文被原样带下去"与"形态错当场 400"。
func TestParseRegenerateCarriesTaskRef(t *testing.T) {
	ref := "task_01HZZZZZZZZZZZZZZZZZZZZZZX"
	req, err := ParseRegenerate([]byte(`{"model":"m","resolution":"2k","source_task_id":"` + ref + `"}`))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if req.SourceTaskRef != ref {
		t.Fatalf("want %s, got %s", ref, req.SourceTaskRef)
	}
	if req.Params.Kind != KindRegeneration {
		t.Fatalf("params.Kind 应为 regeneration，实际 %q", req.Params.Kind)
	}
	// 数字号是改造前的写法：必须当场拒绝，而不是带下去让 SQL 查不到（那会变成 404 而不是 400）。
	if _, err := ParseRegenerate([]byte(`{"model":"m","resolution":"2k","source_task_id":42}`)); err == nil {
		t.Fatal("数字任务号应被拒绝")
	}
}

// TestApplyRegenerationResolvesSource 是权限与口径的核心用例：
// 平台任务号 → 上游任务号、秒数取源任务、body 里换成上游号。
func TestApplyRegenerationResolvesSource(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)

	src := &model.Task{
		UserID: uid, KeyID: kid, Model: "metaso-h3", Modality: "video", Kind: KindVideo,
		Status: StatusSucceeded, UpstreamTaskID: "up-src-9", Platform: "metaso",
		PricingSnapshot: model.MustJSON(billing.Snapshot{}),
		RequestParams:   model.MustJSON(params{Model: "metaso-h3", Resolution: "720p", Seconds: 10}),
	}
	if err := gdb.Create(src).Error; err != nil {
		t.Fatalf("造源任务失败: %v", err)
	}

	req, err := ParseRegenerate([]byte(`{"model":"metaso-h3-regen","resolution":"2k","source_task_id":"` + src.PublicID + `"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := applyRegeneration(ctx, SubmitInput{Key: &model.APIKey{ID: kid, UserID: uid}, Req: req}); err != nil {
		t.Fatalf("applyRegeneration 失败: %v", err)
	}

	if req.Seconds != 10 || req.Params.Seconds != 10 {
		t.Fatalf("秒数应取源任务的 10，实际 req=%d params=%d", req.Seconds, req.Params.Seconds)
	}
	if req.Params.SourceUpstream != "up-src-9" {
		t.Fatalf("应记录上游任务号，实际 %q", req.Params.SourceUpstream)
	}
	if req.Params.Kind != KindRegeneration {
		t.Fatalf("Kind 应为 regeneration，实际 %q", req.Params.Kind)
	}
	var doc map[string]any
	if err := json.Unmarshal(req.Body, &doc); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if doc["source_task_id"] != "up-src-9" {
		t.Fatalf("请求体里的 source_task_id 必须换成上游号，实际 %v", doc["source_task_id"])
	}
	if _, ok := doc["seconds"]; ok {
		t.Fatalf("再生成不该带 seconds：%v", doc)
	}
}

// TestApplyRegenerationRejections 覆盖权限与状态边界，全部零上游请求。
func TestApplyRegenerationRejections(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)
	other, _ := seedUserAndKey(t, gdb, 100000)

	mk := func(userID uint64, status, platform, upstream string) *model.Task {
		row := &model.Task{
			UserID: userID, KeyID: kid, Model: "metaso-h3", Modality: "video", Kind: KindVideo,
			Status: status, Platform: platform, UpstreamTaskID: upstream,
			PricingSnapshot: model.MustJSON(billing.Snapshot{}),
			RequestParams:   model.MustJSON(params{Resolution: "720p", Seconds: 8}),
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造任务失败: %v", err)
		}
		return row
	}

	cases := []struct {
		name string
		row  *model.Task
		want error
		msg  string
	}{
		{"非本人", mk(other, StatusSucceeded, "metaso", "up-1"), ErrNotFound, "源任务不存在"},
		{"未完成", mk(uid, StatusRunning, "metaso", "up-2"), ErrInvalidParam, "只有已完成的任务"},
		{"无上游号", mk(uid, StatusSucceeded, "metaso", ""), ErrInvalidParam, "缺少上游任务号"},
		{"跨上游", mk(uid, StatusSucceeded, "autodl", "up-3"), ErrInvalidParam, "不支持再生成"},
	}
	for _, c := range cases {
		req, err := ParseRegenerate([]byte(`{"model":"metaso-h3-regen","resolution":"2k","source_task_id":"` + c.row.PublicID + `"}`))
		if err != nil {
			t.Fatalf("%s: 解析失败: %v", c.name, err)
		}
		err = applyRegeneration(ctx, SubmitInput{Key: &model.APIKey{ID: kid, UserID: uid}, Req: req})
		if !errors.Is(err, c.want) {
			t.Fatalf("%s: 期望 %v，实际 %v", c.name, c.want, err)
		}
		if !hasSubstr(err.Error(), c.msg) {
			t.Fatalf("%s: 文案应含 %q，实际 %q", c.name, c.msg, err.Error())
		}
	}
	// 不存在的任务号也必须报「源任务不存在」（不区分「不存在」与「非本人」）
	req, _ := ParseRegenerate([]byte(`{"model":"metaso-h3-regen","resolution":"2k","source_task_id":"task_00000000000000000000000009"}`))
	if err := applyRegeneration(ctx, SubmitInput{Key: &model.APIKey{ID: kid, UserID: uid}, Req: req}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的源任务应报 ErrNotFound，实际 %v", err)
	}
}

// TestApplyRegenerationScopedToApp 源任务必须按**应用**收敛，而不是只按用户。
//
// 只按 user_id 查时，应用 A 的托管密钥可以把同一用户在应用 B 下完成的任务
// 当成源任务：用 B 的上游任务号重建，再用自己的 key 取回派生产物 ——
// 跨应用内容越权，且 A 还能试出 B 有哪些任务号（存在性 oracle）。
func TestApplyRegenerationScopedToApp(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 100000)

	// 源任务属于「平台直连」（key 的 app_id=0）。
	src := &model.Task{
		UserID: uid, KeyID: kid, Model: "metaso-h3", Modality: "video", Kind: KindVideo,
		Status: StatusSucceeded, UpstreamTaskID: "up-app-src", Platform: "metaso",
		PricingSnapshot: model.MustJSON(billing.Snapshot{}),
		RequestParams:   model.MustJSON(params{Model: "metaso-h3", Resolution: "720p", Seconds: 8}),
	}
	if err := gdb.Create(src).Error; err != nil {
		t.Fatalf("造源任务失败: %v", err)
	}

	// 同一用户在应用 7 下的托管密钥。
	other := &model.APIKey{
		UserID: uid, AppID: 7, Kind: model.KeyKindHosted,
		KeyCipher: "x", KeyHash: "h-app7-" + itoa64(src.ID), Name: "app7", Status: 1, Group: "default",
	}
	if err := gdb.Create(other).Error; err != nil {
		t.Fatalf("造应用密钥失败: %v", err)
	}

	body := `{"model":"metaso-h3-regen","resolution":"2k","source_task_id":"` + src.PublicID + `"}`

	// 应用 7 的密钥：必须查不到（与「不存在」同码，不泄露跨应用任务是否存在）。
	req, err := ParseRegenerate([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	err = applyRegeneration(ctx, SubmitInput{
		Key: &model.APIKey{ID: other.ID, UserID: uid, AppID: 7, Kind: model.KeyKindHosted}, Req: req,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨应用源任务应报 ErrNotFound，实际 %v", err)
	}

	// 本应用（app_id=0）仍可再生成：修复不能把正常路径一起挡掉。
	req2, err := ParseRegenerate([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := applyRegeneration(ctx, SubmitInput{Key: &model.APIKey{ID: kid, UserID: uid}, Req: req2}); err != nil {
		t.Fatalf("同应用再生成不应失败: %v", err)
	}
	if req2.Params.SourceUpstream != "up-app-src" {
		t.Fatalf("应带回源任务的上游号，实际 %q", req2.Params.SourceUpstream)
	}
}

// TestApplyRegenerationBaseVideo：base_video 形态靠探测拿秒数。
func TestApplyRegenerationBaseVideo(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	withTestDB(t)
	ctx := context.Background()
	url, _ := startVideoServer(t, map[string][]byte{"/base.mp4": mp4WithSeconds(12)})

	req, err := ParseRegenerate([]byte(`{"model":"metaso-h3-regen","resolution":"2k","input_reference":[{"type":"base_video","video_url":"https://cdn.example.com/base.mp4"}]}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 解析入口的 SSRF 校验只认公网地址（localhost 会被拒），这里把链接换成测试服务地址，
	// 只验证 applyRegeneration 的探测与秒数回填。
	req.Params.InputRef[0].URL = url + "/base.mp4"
	if err := applyRegeneration(ctx, SubmitInput{Key: &model.APIKey{ID: 1, UserID: 1}, Req: req}); err != nil {
		t.Fatalf("applyRegeneration 失败: %v", err)
	}
	if req.Seconds != 12 {
		t.Fatalf("秒数应取探测值 12，实际 %d", req.Seconds)
	}
	if req.Params.SourceUpstream != "" {
		t.Fatalf("base_video 形态不该有上游任务号，实际 %q", req.Params.SourceUpstream)
	}
}

// TestReplayedBodyRegeneration：重放体只含 model / source_task_id / resolution。
func TestReplayedBodyRegeneration(t *testing.T) {
	row := &model.Task{
		Model: "metaso-h3-regen",
		RequestParams: model.MustJSON(params{
			Model: "metaso-h3-regen", Resolution: "2k", Seconds: 10,
			Kind: KindRegeneration, SourceUpstream: "up-src-9",
		}),
	}
	var doc map[string]any
	if err := json.Unmarshal(replayedBody(row), &doc); err != nil {
		t.Fatalf("重放体不是 JSON: %v", err)
	}
	if doc["model"] != "metaso-h3-regen" || doc["source_task_id"] != "up-src-9" || doc["resolution"] != "2k" {
		t.Fatalf("重放体字段不对：%v", doc)
	}
	for _, k := range []string{"seconds", "prompt", "duration", "input_reference"} {
		if _, ok := doc[k]; ok {
			t.Fatalf("再生成重放体不该带 %s：%v", k, doc)
		}
	}
	// 生成任务：重放体就是提交时的原文（不重建）
	gen := &model.Task{
		Model:         "metaso-h3",
		RequestParams: model.MustJSON(params{Model: "metaso-h3", Resolution: "720p", Seconds: 5}),
		SubmitBody:    model.JSON(`{"model":"metaso-h3","prompt":"x","resolution":"720p","seconds":5}`),
	}
	var g map[string]any
	if err := json.Unmarshal(replayedBody(gen), &g); err != nil {
		t.Fatalf("重放体不是 JSON: %v", err)
	}
	if g["seconds"] != float64(5) || g["resolution"] != "720p" {
		t.Fatalf("生成重放体字段不对：%v", g)
	}
}

func itoa64(v uint64) string { return strconv.FormatUint(v, 10) }

func hasSubstr(s, sub string) bool {
	if sub == "" {
		return true
	}
	return strings.Contains(s, sub)
}
