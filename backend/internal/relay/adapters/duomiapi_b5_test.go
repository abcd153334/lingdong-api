package adapters

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// TestDuomiKlingPathIsShared 验证「提交 / 轮询 / 探活」三处用的是同一套路径拼接（P0-13）。
//
// 改造前只有提交做了 TrimRight，运营把 prefix 写成不带尾斜杠时：
// 提交成功、轮询拼成 `.../videosomni-video/{id}` 永远 404 → 任务生成好了也拿不到，
// 一直 running 到超时退费。这类缺陷只在轮询阶段暴露，必须在单测里锁死。
func TestDuomiKlingPathIsShared(t *testing.T) {
	cases := []struct {
		prefix string
		seg    string
		id     string
		want   string
	}{
		{"/api/video/kling/v1/videos/", "text2video", "abc", "/api/video/kling/v1/videos/text2video/abc"},
		// 不带尾斜杠：必须与带斜杠产出完全一致
		{"/api/video/kling/v1/videos", "text2video", "abc", "/api/video/kling/v1/videos/text2video/abc"},
		// 多个尾斜杠
		{"/api/video/kling/v1/videos///", "text2video", "abc", "/api/video/kling/v1/videos/text2video/abc"},
		// 空前缀走内置默认
		{"", "omni-video", "abc", duomiKlingVideoPrefix + "omni-video/abc"},
		// 提交（无 id）
		{"/api/video/kling/v1/videos", "multi-image2video", "", "/api/video/kling/v1/videos/multi-image2video"},
		// seg 前后有多余斜杠
		{"/api/video/kling/v1/videos", "/omni-video/", "abc", "/api/video/kling/v1/videos/omni-video/abc"},
	}
	for _, c := range cases {
		if got := duomiKlingPath(c.prefix, c.seg, c.id); got != c.want {
			t.Fatalf("duomiKlingPath(%q,%q,%q)=%q，期望 %q", c.prefix, c.seg, c.id, got, c.want)
		}
	}

	// 端到端一致性：同一份配置下，提交路径与轮询路径必须同段。
	opts := &duomiOptions{paths: map[string]string{"kling.prefix": "/api/video/kling/v1/videos"}}
	submit := duomiKlingPath(opts.paths["kling.prefix"], duomiKlingOmniVideo, "")
	poll, _ := duomiPollTarget(opts, "kling.omni-video", "t-1")
	if !strings.HasPrefix(poll, submit+"/") {
		t.Fatalf("轮询路径 %q 必须以提交路径 %q + \"/\" 开头", poll, submit)
	}
}

// TestDuomiKlingMultiNoModeAndStringDuration 覆盖 kling-v1.6-multi 的两条硬约束（P0-11）。
//
//   - 该型号不接受 mode：下发空串会被上游当非法枚举直接 400；
//   - duration 只认字符串写法：下发数字同样 400。
//
// 两者都会被归因成「渠道故障」，把一条健康渠道熔断，所以必须精确断言请求体。
func TestDuomiKlingMultiNoModeAndStringDuration(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"k-1"}}`)
	})
	body := `{"prompt":"用这三张图做个转场","seconds":5,"input_reference":[
		{"type":"image","image_url":"https://cdn/a.png"},
		{"type":"image","image_url":"https://cdn/b.png"}]}`
	if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v1.6-multi", body); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if _, ok := rec.body["mode"]; ok {
		t.Fatalf("kling.multi 不能下发 mode（上游会 400）：%+v", rec.body)
	}
	if got := rec.body["duration"]; got != "5" {
		t.Fatalf("kling-v1.6-multi 的 duration 必须是字符串 \"5\"，实际 %#v", got)
	}
	if rec.path != "/api/video/kling/v1/videos/multi-image2video" {
		t.Fatalf("路径不对：%s", rec.path)
	}
}

// TestDuomiKlingStdProSendsMode 验证 std/pro 型号仍然下发 mode（不能因为修多图把这里改坏）。
func TestDuomiKlingStdProSendsMode(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"k-2"}}`)
	})
	if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-pro",
		`{"prompt":"海边日落","seconds":5}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["mode"] != "pro" {
		t.Fatalf("pro 型号必须下发 mode=pro，实际 %+v", rec.body)
	}
	if got := rec.body["duration"]; got != float64(5) {
		t.Fatalf("未声明 DurationAsString 的型号应下发数字时长，实际 %#v", got)
	}
}

// TestDuomiKlingRejectsModeInOverride 验证 model_overrides 给 kling.multi 配 mode 会在保存期被拒。
func TestDuomiKlingRejectsModeInOverride(t *testing.T) {
	_, err := parseDuomiOptions([]byte(`{"model_overrides":{"my-multi":{"protocol":"kling.multi","mode":"pro"}}}`))
	if err == nil {
		t.Fatal("kling.multi + mode 必须在保存期被拒绝")
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Fatalf("错误文案应指明 mode：%v", err)
	}
	// 不带 mode 的合法配置
	if _, err := parseDuomiOptions([]byte(`{"model_overrides":{"my-multi":{"protocol":"kling.multi"}}}`)); err != nil {
		t.Fatalf("合法覆盖不该报错：%v", err)
	}
}

// TestDuomiOmniSendsImages 是 P0-12 的核心回归：omni 以前只校验、**一张图都不下发**。
//
// 第二轮修正（对标源代码 1.3.7）：下发键名必须是 image_url，且首尾帧只能靠
// image_list[].type 表达 —— omni **没有** image / image_tail 两个字段。
// 上游实测：缺 image_url 返回 code=1102「第1张图片缺少image_url字段」。
func TestDuomiOmniSendsImages(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"o-1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	// 首帧 + 尾帧 → image_list 两项，带 first_frame / end_frame
	body := `{"prompt":"镜头推进","seconds":5,"input_reference":[
		{"type":"first_frame","image_url":"https://cdn/f.png"},
		{"type":"last_frame","image_url":"https://cdn/l.png"}]}`
	if _, _, err := submitDuomi(t, cfg, "kling-v3-omni", body); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	list, ok := rec.body["image_list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("首尾帧应下发 image_list 两项，实际 %+v", rec.body["image_list"])
	}
	first, _ := list[0].(map[string]any)
	last, _ := list[1].(map[string]any)
	if first["image_url"] != "https://cdn/f.png" || first["type"] != "first_frame" {
		t.Fatalf("首帧项应为 {image_url, type:first_frame}，实际 %+v", first)
	}
	if last["image_url"] != "https://cdn/l.png" || last["type"] != "end_frame" {
		t.Fatalf("尾帧项应为 {image_url, type:end_frame}（不是 last_frame），实际 %+v", last)
	}
	// omni 绝不能出现 image / image_tail —— 它们是 image2video 的字段，在上游会被静默忽略
	for _, bad := range []string{"image", "image_tail"} {
		if _, has := rec.body[bad]; has {
			t.Fatalf("omni 不能下发顶层 %s（上游无此字段，会被静默忽略=白丢素材）：%+v", bad, rec.body)
		}
	}

	// 三张普通参考图 → image_list 三项，不带 type
	body = `{"prompt":"镜头推进","seconds":5,"input_reference":[
		{"type":"image","image_url":"https://cdn/1.png"},
		{"type":"image","image_url":"https://cdn/2.png"},
		{"type":"image","image_url":"https://cdn/3.png"}]}`
	if _, _, err := submitDuomi(t, cfg, "kling-v3-omni", body); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	list, ok = rec.body["image_list"].([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("三张图应下发 image_list（3 项），实际 %+v", rec.body["image_list"])
	}
	for i, item := range list {
		m, _ := item.(map[string]any)
		if m["image_url"] != []string{"https://cdn/1.png", "https://cdn/2.png", "https://cdn/3.png"}[i] {
			t.Fatalf("第 %d 项 image_url 不对（键名必须是 image_url）：%+v", i+1, m)
		}
		if _, has := m["type"]; has {
			t.Fatalf("普通参考图不该带 type：%+v", m)
		}
	}

	// 参考视频/音频必须明确拒绝（静默丢弃 = 收了钱不用素材）
	body = `{"prompt":"改编这个视频","seconds":5,"input_reference":[
		{"type":"video","video_url":"https://cdn/v.mp4"}]}`
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "kling-v3-omni", body), "不支持参考视频或音频")
}

// TestDuomiOmniBodyMatchesSource 锁死 omni 请求体与源代码 1.3.7 的逐字段一致性。
//
// 任何一处漂移都会重新变成「报文能发、素材不生效」，所以这里断言完整形状，
// 而不只是断言「某个键存在」。
func TestDuomiOmniBodyMatchesSource(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"o-2"}}`)
	})
	body := `{"prompt":"  镜头推进  ","seconds":10,"input_reference":[
		{"type":"image","image_url":"https://cdn/1.png"}],
		"aspect_ratio":"9:16"}`
	if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-omni", body); err != nil {
		t.Fatalf("提交失败：%v", err)
	}

	if rec.body["model_name"] != "kling-v3-omni" {
		t.Fatalf("model_name 不对：%+v", rec.body["model_name"])
	}
	if rec.body["prompt"] != "镜头推进" {
		t.Fatalf("prompt 应去首尾空白，实际 %#v", rec.body["prompt"])
	}
	if rec.body["mode"] != "std" {
		t.Fatalf("mode 必须来自档案(std)，实际 %#v", rec.body["mode"])
	}
	// duration 是**字符串**（源代码 String(duration)）；数字会被上游 400
	if got := rec.body["duration"]; got != "10" {
		t.Fatalf("duration 必须是字符串 \"10\"，实际 %#v", got)
	}
	if got, ok := rec.body["multi_shot"]; !ok || got != false {
		t.Fatalf("multi_shot 必须恒为 false（显式声明单镜头），实际 %#v", got)
	}
	if rec.body["sound"] != "on" {
		t.Fatalf("v3 系必须下发 sound=on，实际 %#v", rec.body["sound"])
	}
	if rec.body["aspect_ratio"] != "9:16" {
		t.Fatalf("aspect_ratio 应原样透传，实际 %#v", rec.body["aspect_ratio"])
	}
	if _, has := rec.body["resolution"]; has {
		t.Fatalf("omni 没有 resolution 参数：%+v", rec.body)
	}
}

// TestDuomiOmniImageListSemantics 覆盖素材语义矩阵。
//
// 关键点：**不能按张数分派**。「首帧+尾帧」(2 张) 与「两张普通参考图」(2 张)
// 张数相同但报文形态相反 —— 前者带 type、后者不带。
func TestDuomiOmniImageListSemantics(t *testing.T) {
	cases := []struct {
		name      string
		refs      string
		wantLen   int
		wantTypes []string // "" 表示该位置不应有 type
		wantErr   string
	}{
		{
			name:      "无图",
			refs:      `[]`,
			wantLen:   0,
			wantTypes: nil,
		},
		{
			name:      "单张参考图",
			refs:      `[{"type":"image","image_url":"https://cdn/1.png"}]`,
			wantLen:   1,
			wantTypes: []string{""},
		},
		{
			name:      "两张普通参考图（不带 type）",
			refs:      `[{"type":"image","image_url":"https://cdn/1.png"},{"type":"image","image_url":"https://cdn/2.png"}]`,
			wantLen:   2,
			wantTypes: []string{"", ""},
		},
		{
			name:      "首帧+尾帧（带 type）",
			refs:      `[{"type":"first_frame","image_url":"https://cdn/f.png"},{"type":"last_frame","image_url":"https://cdn/l.png"}]`,
			wantLen:   2,
			wantTypes: []string{"first_frame", "end_frame"},
		},
		{
			name:      "只有首帧=图生视频",
			refs:      `[{"type":"first_frame","image_url":"https://cdn/f.png"}]`,
			wantLen:   1,
			wantTypes: []string{""},
		},
		{
			name:      "首帧+普通图=多参考（首帧排最前）",
			refs:      `[{"type":"first_frame","image_url":"https://cdn/f.png"},{"type":"image","image_url":"https://cdn/1.png"}]`,
			wantLen:   2,
			wantTypes: []string{"", ""},
		},
		{
			name:    "只有尾帧",
			refs:    `[{"type":"last_frame","image_url":"https://cdn/l.png"}]`,
			wantErr: "不支持只传尾帧",
		},
		{
			name: "首尾帧+普通图混传",
			refs: `[{"type":"first_frame","image_url":"https://cdn/f.png"},
			        {"type":"image","image_url":"https://cdn/1.png"},
			        {"type":"last_frame","image_url":"https://cdn/l.png"}]`,
			wantErr: "请二选一",
		},
	}
	for _, c := range cases {
		body := `{"prompt":"p","seconds":5,"input_reference":` + c.refs + `}`
		if c.wantErr != "" {
			wantDuomiCaller(t, mustDuomiErr(t, duomiCfg("http://127.0.0.1:1", nil), "kling-v3-omni", body), c.wantErr)
			continue
		}

		srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
			duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"o-3"}}`)
		})
		if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-omni", body); err != nil {
			t.Fatalf("%s：提交失败 %v", c.name, err)
		}
		raw, has := rec.body["image_list"]
		if c.wantLen == 0 {
			if has {
				t.Fatalf("%s：无图时不该下发 image_list，实际 %+v", c.name, raw)
			}
			continue
		}
		list, ok := raw.([]any)
		if !ok || len(list) != c.wantLen {
			t.Fatalf("%s：image_list 应为 %d 项，实际 %+v", c.name, c.wantLen, raw)
		}
		for i, item := range list {
			m, _ := item.(map[string]any)
			gotType, _ := m["type"].(string)
			if gotType != c.wantTypes[i] {
				t.Fatalf("%s：第 %d 项 type 应为 %q，实际 %q（整项 %+v）", c.name, i+1, c.wantTypes[i], gotType, m)
			}
		}
	}
}

// TestDuomiOmniImagePassthrough 守「omni 参考图张数不设上限」。
//
// 曾经这里锁死「最多 7 张」：那是适配器抄下来的一条模型能力，上游一放宽
// （或同一模型 ID 换了上游）平台就先把用户挡住，而理由听起来像"模型不支持"。
func TestDuomiOmniImagePassthrough(t *testing.T) {
	mk := func(n int) string {
		parts := make([]string, 0, n)
		for i := 0; i < n; i++ {
			parts = append(parts, fmt.Sprintf(`{"type":"image","image_url":"https://cdn/%d.png"}`, i))
		}
		return `{"prompt":"p","seconds":5,"input_reference":[` + strings.Join(parts, ",") + `]}`
	}

	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"o-4"}}`)
	})
	if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-omni", mk(7)); err != nil {
		t.Fatalf("7 张应放行：%v", err)
	}
	if list, ok := rec.body["image_list"].([]any); !ok || len(list) != 7 {
		t.Fatalf("7 张应下发 7 项，实际 %+v", rec.body["image_list"])
	}
	// 8 张同样不拦：张数上限是模型方的事实，多传由它报错，平台不替它判定。
	if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-omni", mk(8)); err != nil {
		t.Fatalf("8 张应原样下发而不是拦截：%v", err)
	}
}

// TestDuomiKlingSoundByModel 锁死 sound 只发给 v3 系。
//
// 给 kling-v1-6 发 sound 会被上游当非法枚举拒，而那条 400 会被归因成
// 「渠道故障」，把一条健康渠道熔断。
func TestDuomiKlingSoundByModel(t *testing.T) {
	cases := []struct {
		model     string
		body      string
		wantSound bool
	}{
		{"kling-v3-std", `{"prompt":"p","seconds":5}`, true},
		{"kling-v3-pro", `{"prompt":"p","seconds":5}`, true},
		{"kling-v3-omni", `{"prompt":"p","seconds":5}`, true},
		{"kling-v1.6-multi", `{"prompt":"p","seconds":5,"input_reference":[
			{"type":"image","image_url":"https://cdn/1.png"}]}`, false},
	}
	for _, c := range cases {
		srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
			duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"s-1"}}`)
		})
		if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), c.model, c.body); err != nil {
			t.Fatalf("%s：提交失败 %v", c.model, err)
		}
		got, has := rec.body["sound"]
		if has != c.wantSound {
			t.Fatalf("%s：sound 存在性应为 %v，实际 %+v", c.model, c.wantSound, rec.body)
		}
		if c.wantSound && got != "on" {
			t.Fatalf("%s：sound 应为 \"on\"，实际 %#v", c.model, got)
		}
	}
}

// TestDuomiKlingSound 覆盖型号版本判定（含 v2 边界与「认不出版本」）。
//
// 旧实现判「含 v3」，于是 kling-v4-pro 静默不下发 sound：成片无声、账单照收、
// 无日志无指标。这里锁死的是「按版本号递增判定」这个语义。
func TestDuomiKlingSound(t *testing.T) {
	cases := []struct {
		id       string
		wantOn   bool
		wantUnkn bool
	}{
		{"kling-v3", true, false},
		{"kling-v3-omni", true, false},
		{"KLING-V3-PRO", true, false},
		{"kling-v4-pro", true, false}, // 本次修复的目标：新版本必须默认开
		{"kling-v5", true, false},
		{"kling-v1-6", false, false},
		{"kling-v2-5-turbo", false, false},
		{"kling-v2.6", false, false},
		// 认不出版本：按支持处理（无声的代价大于多发一个字段），但必须留痕。
		{"kling-hd", true, true},
		{"kling-omni-hd", true, true},
		{"", false, false},
	}
	for _, c := range cases {
		on, unkn := duomiKlingSound(c.id)
		if on != c.wantOn || unkn != c.wantUnkn {
			t.Errorf("duomiKlingSound(%q)=(%v,%v)，期望 (%v,%v)", c.id, on, unkn, c.wantOn, c.wantUnkn)
		}
	}
}

// TestDuomiOmniDurationRange 锁死 omni 的 3–15 秒区间与字符串写法。
func TestDuomiOmniDurationRange(t *testing.T) {
	for _, secs := range []int{3, 5, 10, 15} {
		srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
			duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"d-1"}}`)
		})
		body := fmt.Sprintf(`{"prompt":"p","seconds":%d}`, secs)
		if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-omni", body); err != nil {
			t.Fatalf("%d 秒应放行：%v", secs, err)
		}
		if got := rec.body["duration"]; got != fmt.Sprintf("%d", secs) {
			t.Fatalf("%d 秒的 duration 必须是字符串，实际 %#v", secs, got)
		}
	}
	// 秒数超出「常见区间」也原样下发：该型号到底认哪几个秒数是厂商文档的事实，
	// 平台替它判定只会在上游放宽时把自己人挡在门外。
	for _, secs := range []int{2, 16} {
		srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
			duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"d-1"}}`)
		})
		body := fmt.Sprintf(`{"prompt":"p","seconds":%d}`, secs)
		if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-omni", body); err != nil {
			t.Fatalf("%d 秒应原样下发：%v", secs, err)
		}
		if got := rec.body["duration"]; got != fmt.Sprintf("%d", secs) {
			t.Fatalf("%d 秒的 duration 必须原样透传（字符串），实际 %#v", secs, got)
		}
	}
}

func mustDuomiErr(t *testing.T, cfg *relay.ChannelConfig, modelID, body string) error {
	t.Helper()
	_, _, err := submitDuomi(t, cfg, modelID, body)
	return err
}

// TestDuomiKlingPromptAndMaterialRules 覆盖 P2-11 / P2-12 的本地拒绝。
func TestDuomiKlingPromptAndMaterialRules(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"x"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	// 可灵上限 3072（seedance 是 20000）：超了必须本地 400，不能等上游 502。
	long := strings.Repeat("字", duomiKlingMaxPromptRunes+1)
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "kling-v3-std", `{"prompt":"`+long+`","seconds":5}`), "提示词过长")

	// 恰好到上限必须放行（边界不算越界）
	ok := strings.Repeat("字", duomiKlingMaxPromptRunes)
	if _, _, err := submitDuomi(t, cfg, "kling-v3-std", `{"prompt":"`+ok+`","seconds":5}`); err != nil {
		t.Fatalf("恰好到上限应放行：%v", err)
	}

	// 图生视频必须带 prompt（P2-12）
	i2v := `{"seconds":5,"input_reference":[{"type":"first_frame","image_url":"https://cdn/f.png"}]}`
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "kling-v3-std", i2v), "图生视频需要 prompt")

	// 只传尾帧必须拒绝：否则会拼成 image == image_tail（同一张图）
	tailOnly := `{"prompt":"动起来","seconds":5,"input_reference":[{"type":"last_frame","image_url":"https://cdn/l.png"}]}`
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "kling-v3-std", tailOnly), "不支持只传尾帧")

	// 首尾帧同图必须拒绝
	same := `{"prompt":"动起来","seconds":5,"input_reference":[
		{"type":"first_frame","image_url":"https://cdn/x.png"},
		{"type":"last_frame","image_url":"https://cdn/x.png"}]}`
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "kling-v3-std", same), "首帧与尾帧不能是同一张图")

	_ = rec
}

// TestDuomiKlingRejectsMetadata 验证可灵收到 metadata 直接 400（P1-25）。
//
// 静默忽略会让用户以为 generate_audio / watermark 生效了 —— 那是改上游成本的字段，
// 「以为生效了其实没生效」比「明确拒绝」危险得多。
func TestDuomiKlingRejectsMetadata(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"x"}}`)
	})
	err := mustDuomiErr(t, duomiCfg(srv.URL, nil), "kling-v3-std",
		`{"prompt":"p","seconds":5,"metadata":{"generate_audio":true,"watermark":false}}`)
	wantDuomiCaller(t, err, "不支持 metadata 透传")
	if !strings.Contains(err.Error(), "generate_audio") || !strings.Contains(err.Error(), "watermark") {
		t.Fatalf("错误文案应列出被拒的键（且稳定排序），实际 %v", err)
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("参数错必须零出站")
	}

	// seed 不参与「kling 不支持 metadata」的判定（它是平台已知字段）
	if _, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "kling-v3-std",
		`{"prompt":"p","seconds":5,"metadata":{"seed":42}}`); err != nil {
		t.Fatalf("只带 seed 不该被拒：%v", err)
	}
}

// TestDuomiAuthPrefixNormalize 覆盖 auth_prefix 的三条归一（P2-07）。
func TestDuomiAuthPrefixNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"-", ""},                       // 显式空前缀
		{"Bearer ", "Bearer "},          // 标准
		{"Bearer", "Bearer "},           // 缺尾随空格
		{"Bearer Bearer ", "Bearer "},   // 重复拼接
		{"bearer Bearer x", "Bearer x"}, // 大小写混用 + 重复
		{"Token", "Token "},             // 自定义前缀补空格
		{"Token ", "Token "},            // 已有空格不重复加
		{"BearerToken", "BearerToken "}, // 这是自定义前缀，不能剥掉 "Bearer"
	}
	for _, c := range cases {
		got, err := normalizeDuomiAuthPrefix(c.raw)
		if err != nil {
			t.Fatalf("normalize(%q) 报错：%v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("normalize(%q)=%q，期望 %q", c.raw, got, c.want)
		}
	}
	if _, err := normalizeDuomiAuthPrefix("Bear\ner"); err == nil {
		t.Fatal("含换行必须被拒绝（防头注入）")
	}
	// 端到端：配成 "Bearer" 也必须能发出正确的头
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"x"}}`)
	})
	if _, _, err := submitDuomi(t, duomiCfg(srv.URL, []byte(`{"auth_prefix":"Bearer"}`)), "kling-v3-std",
		`{"prompt":"p","seconds":5}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.auth != "Bearer dm-123" {
		t.Fatalf("鉴权头应归一为 \"Bearer dm-123\"，实际 %q", rec.auth)
	}
}

// TestDuomiUnknownModelIsConfigError 验证未收录型号报配置错而不是参数错（P2-13）。
//
// 报 400 会让接入方以为是自己传错了 model，实际是运营没配 —— 责任指错人。
func TestDuomiUnknownModelIsConfigError(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"x"}`)
	})
	_, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "no-such-model", `{"prompt":"p","seconds":5}`)
	if err == nil {
		t.Fatal("未知模型必须报错")
	}
	if !strings.Contains(err.Error(), "未知的多米模型") {
		t.Fatalf("错误文案应说明是未登记：%v", err)
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("配置错必须零出站")
	}
	var ce *relay.ConfigError
	if !asConfigError(err, &ce) {
		t.Fatalf("必须是 ConfigError（错误归因成调用方会导致 400 并指错责任），实际 %T", err)
	}
}

func asConfigError(err error, target **relay.ConfigError) bool {
	for err != nil {
		if ce, ok := err.(*relay.ConfigError); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestDuomiSeedanceRatioAlias 验证 seedance 也接受 ratio 写法（P2-10）。
func TestDuomiSeedanceRatioAlias(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"s-1"}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	if _, _, err := submitDuomi(t, cfg, "seedance-2.0", `{"prompt":"p","seconds":5,"ratio":"9:16"}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["ratio"] != "9:16" {
		t.Fatalf("ratio 写法应生效，实际 %+v", rec.body)
	}
	// 两种写法同时给：ratio 优先
	if _, _, err := submitDuomi(t, cfg, "seedance-2.0", `{"prompt":"p","seconds":5,"ratio":"1:1","aspect_ratio":"16:9"}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["ratio"] != "1:1" {
		t.Fatalf("ratio 应优先于 aspect_ratio，实际 %+v", rec.body)
	}
}

// TestDuomiProbeRequiresEnvelope 验证 400/422/429 只有带信封才算健康（P2-06）。
func TestDuomiProbeRequiresEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"429+信封", 429, `{"code":1302,"message":"rate limited"}`, false},
		{"429+HTML", 429, `<html>slow down</html>`, true},
		{"400+信封", 400, `{"code":1201,"message":"bad"}`, false},
		{"400+HTML", 400, `<html>bad request</html>`, true},
		{"422+信封", 422, `{"code":1201,"message":"bad"}`, false},
	}
	for _, c := range cases {
		srv, _ := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
			duomiJSON(w, c.status, c.body)
		})
		err := (DuoMi{}).Test(context.Background(), duomiCfg(srv.URL, nil))
		if (err != nil) != c.wantErr {
			t.Fatalf("%s：wantErr=%v，实际 %v", c.name, c.wantErr, err)
		}
	}
}
