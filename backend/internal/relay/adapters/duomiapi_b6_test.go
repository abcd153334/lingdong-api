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

	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// 本文件覆盖 B6-1：多米的图像（nano / gptimage）与音乐（suno.music / suno.lyrics）。
// 关注点：路径正确、出站字段白名单、本地拒绝、任务号提取、产物收集与状态判定。

// startDuomiCapture 起一个假上游，并把**原始请求体与 Content-Type** 交给断言。
//
// 不复用 startDuomi：那个辅助函数自己会读走 r.Body（rec.body/rec.raw），
// 回调里再读只能拿到空串 —— 而本文件恰恰要断言表单/JSON 的原文。
func startDuomiCapture(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, raw string)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		reply(w, r, string(buf))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// ---------------------------------------------------------------- 图像提交

func TestDuomiNanoT2ISubmit(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"nano-1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	id, req, err := submitDuomi(t, cfg, "nano-banana-2",
		`{"model":"nano-banana-2","prompt":"a cat","aspect_ratio":"3:4","size":"2K"}`)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if id != "nano-1" {
		t.Fatalf("任务号应为 nano-1，实际 %q", id)
	}
	if rec.path != duomiNanoT2IPath {
		t.Fatalf("路径应为 %s，实际 %s", duomiNanoT2IPath, rec.path)
	}
	if rec.method != http.MethodPost {
		t.Fatalf("方法应为 POST，实际 %s", rec.method)
	}
	// 出站白名单：只带 model/prompt/aspect_ratio/image_size，不带平台内部字段。
	for k := range rec.body {
		switch k {
		case "model", "prompt", "aspect_ratio", "image_size":
		default:
			t.Fatalf("出站体出现不该下发的字段 %q（白名单重建被破坏）", k)
		}
	}
	if got := rec.body["aspect_ratio"]; got != "3:4" {
		t.Fatalf("aspect_ratio 应为 3:4，实际 %v", got)
	}
	if got := rec.body["image_size"]; got != "2K" {
		t.Fatalf("image_size 应为 2K，实际 %v", got)
	}
	if req.SubKind != duomiProtoNano {
		t.Fatalf("SubKind 应回写 %s，实际 %q", duomiProtoNano, req.SubKind)
	}
}

// TestDuomiNanoImageURLsSwitchEndpoint 是这条链路的**唯一判定**：
// 带参考图走编辑端点、不带走文生图端点，且两者共用同一套 model 与同一个查询端点。
func TestDuomiNanoImageURLsSwitchEndpoint(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"edit-1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	// data: 内联必须本地拒绝（上游只收公网直链）。
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "nano-banana-2",
		`{"model":"nano-banana-2","prompt":"改背景","image_urls":["data:image/png;base64,AAA"]}`), "data:")
	// 官方上限 10 张：本地拦，省一次白跑的出站。
	eleven := `{"model":"nano-banana-2","prompt":"x","image_urls":[` +
		`"https://cdn/1.png","https://cdn/2.png","https://cdn/3.png","https://cdn/4.png","https://cdn/5.png",` +
		`"https://cdn/6.png","https://cdn/7.png","https://cdn/8.png","https://cdn/9.png","https://cdn/10.png","https://cdn/11.png"]}`
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "nano-banana-2", eleven), "最多 10 张")
	if rec.hits.Load() != 0 {
		t.Fatalf("本地拒绝前不得出网，实际 %d 次", rec.hits.Load())
	}

	id, req, err := submitDuomi(t, cfg, "nano-banana-2",
		`{"model":"nano-banana-2","prompt":"改背景","image_urls":["https://cdn/a.png","https://cdn/b.png"]}`)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if id != "edit-1" || req.SubKind != duomiProtoNano {
		t.Fatalf("任务号/SubKind 不符：%q %q", id, req.SubKind)
	}
	if rec.path != duomiNanoEditPath {
		t.Fatalf("带图提交应走编辑端点 %s，实际 %s", duomiNanoEditPath, rec.path)
	}
	if n := len(rec.body["image_urls"].([]any)); n != 2 {
		t.Fatalf("image_urls 应下发 2 张，实际 %d", n)
	}
}

// TestDuomiNanoEmptyImageURLsIsT2I 空数组与不传等价：不能报「必须提供参考图」，
// 否则同一个请求换个写法就换了一种结果。
func TestDuomiNanoEmptyImageURLsIsT2I(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"t2i-1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	if _, _, err := submitDuomi(t, cfg, "nano-banana-2",
		`{"model":"nano-banana-2","prompt":"a","image_urls":[]}`); err != nil {
		t.Fatalf("空数组应等价于不传：%v", err)
	}
	if rec.path != duomiNanoT2IPath {
		t.Fatalf("空数组应走文生图端点，实际 %s", rec.path)
	}
	if _, ok := rec.body["image_urls"]; ok {
		t.Fatal("空数组不应下发 image_urls")
	}
}

func TestDuomiGPTImageSubmit(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"img-task-1"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	id, req, err := submitDuomi(t, cfg, "gpt-image-2",
		`{"model":"gpt-image-2","prompt":"a cat","size":"1024x1024","quality":"high","background":"transparent","image":"https://cdn/in.png"}`)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if id != "img-task-1" || req.SubKind != duomiProtoGPTImage {
		t.Fatalf("任务号/SubKind 不符：%q %q", id, req.SubKind)
	}
	if rec.path != duomiGPTImageSubmit {
		t.Fatalf("路径应为 %s，实际 %s", duomiGPTImageSubmit, rec.path)
	}
	if got := rec.query.Get("async"); got != "true" {
		t.Fatalf("必须带 async=true（否则上游走同步、拿不到任务号），实际 %q", got)
	}
	// 官方 schema 里没有 n，但「上游支不支持」由上游回答：客户端写了就原值下发，
	// 平台只保证计费按张数口径走（冻结按 n、结算按实际交付）。这条用例只验证「没写就不发」。
	if _, ok := rec.body["n"]; ok {
		t.Fatalf("客户端没写 n 时不该凭空下发：%v", rec.body["n"])
	}
	if got := rec.body["size"]; got != "1024x1024" {
		t.Fatalf("size 应原样透传，实际 %v", got)
	}
}

// TestDuomiGPTImageForwardsN n 不再本地判死：用户要几张由上游决定。
//
// 计费侧不受影响：冻结按 n 算（billing.ImageCountHint），结算按上游实际交付张数
// （billing.ResolveImageCount），用户显式写了 n 时还会收敛到 n（capImageCountToDeclared）。
func TestDuomiGPTImageForwardsN(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"n2"}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	if _, _, err := submitDuomi(t, cfg, "gpt-image-2",
		`{"model":"gpt-image-2","prompt":"a","n":2}`); err != nil {
		t.Fatalf("n=2 应原值下发、不再本地拒绝：%v", err)
	}
	if got, ok := rec.body["n"]; !ok || got != float64(2) {
		t.Fatalf("n 应原值下发，实际 %#v", got)
	}
}

// TestDuomiGPTImageMaskURLPassthrough 平台只有 mask_url 一个名字：出站不许出现 mask。
func TestDuomiGPTImageMaskURLPassthrough(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"t2"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	if _, _, err := submitDuomi(t, cfg, "gpt-image-2",
		`{"model":"gpt-image-2","prompt":"a","mask_url":"https://cdn/m.png"}`); err != nil {
		t.Fatalf("mask_url 应被接受：%v", err)
	}
	if got := rec.body["mask_url"]; got != "https://cdn/m.png" {
		t.Fatalf("mask_url 应原样下发，实际 %#v", got)
	}
	if _, ok := rec.body["mask"]; ok {
		t.Fatal("出站不该出现 mask：平台字段名只有 mask_url")
	}
	// 蒙版仍必须是公网直链：data: 内联上游取不到，早拒比让任务失败省一次成本。
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "gpt-image-2",
		`{"model":"gpt-image-2","prompt":"a","mask_url":"data:image/png;base64,AAA"}`), "data:")
}

// TestDuomiGPTImageMaskAliasNormalized 入站写 `mask` 时归一成 mask_url，出站不留 mask。
func TestDuomiGPTImageMaskAliasNormalized(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"t2"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	if _, _, err := submitDuomi(t, cfg, "gpt-image-2",
		`{"model":"gpt-image-2","prompt":"a","mask":"https://cdn/m.png"}`); err != nil {
		t.Fatalf("mask 别名应被接受：%v", err)
	}
	if got := rec.body["mask_url"]; got != "https://cdn/m.png" {
		t.Fatalf("mask 归一后应以 mask_url 下发，实际 %#v", got)
	}
	if _, ok := rec.body["mask"]; ok {
		t.Fatal("出站不该出现 mask")
	}
}

// TestDuomiGPTImageImageArray image 的两种形态（字符串 / 数组）都按原形态下发。
func TestDuomiGPTImageImageArray(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"t2"}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	if _, _, err := submitDuomi(t, cfg, "gpt-image-2",
		`{"model":"gpt-image-2","prompt":"a","image":["https://cdn/1.png","https://cdn/2.png"]}`); err != nil {
		t.Fatalf("数组形态 image 应被接受：%v", err)
	}
	if n := len(rec.body["image"].([]any)); n != 2 {
		t.Fatalf("image 数组应原样保留 2 项，实际 %d", n)
	}
}

// ---------------------------------------------------------------- 图像轮询

func TestDuomiImagePollShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  string
		urls    int
		wantErr bool
	}{
		{"data 数组 + 状态数值", `{"code":0,"data":{"status":"completed","images":["https://cdn/1.png"]}}`, "succeeded", 1, false},
		{"data 裸字符串", `{"code":0,"data":"https://cdn/2.png"}`, "succeeded", 1, false},
		{"对象数组", `{"status":"succeeded","data":[{"url":"https://cdn/3.png"},{"url":"https://cdn/4.png"}]}`, "succeeded", 2, false},
		{"仍在生成", `{"code":0,"data":{"status":"running","progress":40}}`, "running", 0, false},
		{"失败带原因", `{"code":0,"data":{"status":"failed","error":{"message":"内容审核未通过"}}}`, "failed", 0, false},
		{"无状态但有产物", `{"data":[{"image_url":"https://cdn/5.png"}]}`, "succeeded", 1, false},
		{"无状态无产物", `{"code":0,"data":{}}`, "", 0, false},
	}
	for _, c := range cases {
		st, err := parseDuomiImageTask([]byte(c.body))
		if c.wantErr != (err != nil) {
			t.Fatalf("%s：错误不符 %v", c.name, err)
		}
		if st.Status != c.status {
			t.Fatalf("%s：状态应为 %q，实际 %q", c.name, c.status, st.Status)
		}
		if len(st.ResultURLs) != c.urls {
			t.Fatalf("%s：产物应为 %d 个，实际 %d（%v）", c.name, c.urls, len(st.ResultURLs), st.ResultURLs)
		}
		if c.urls > 0 && st.ResultURL != st.ResultURLs[0] {
			t.Fatalf("%s：ResultURL 应等于首个产物", c.name)
		}
	}
}

func TestDuomiImagePollIgnoresNonHTTP(t *testing.T) {
	// base64 / 相对路径无法转存，收集了只会产生「产物存在但下载必失败」的坏数据。
	st, err := parseDuomiImageTask([]byte(`{"status":"succeeded","data":{"images":["data:image/png;base64,AAA","/files/1.png"]}}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(st.ResultURLs) != 0 {
		t.Fatalf("非 http 链接不应计入产物，实际 %v", st.ResultURLs)
	}
}

// TestDuomiImagePollSurfacesDataMsg nano 的失败原因在 `data.msg`（`data.data` 为 null）。
//
// 官方失败样本：{state:"error", status:"2", data:null, msg:"Gemini could not generate…"}。
// 只读 `message` 会让这类任务永远显示「未返回原因」，用户无从判断是自己提示词的问题
// 还是平台故障。
func TestDuomiImagePollSurfacesDataMsg(t *testing.T) {
	fail := `{"code":200,"msg":"success","data":{"task_id":"t1","state":"error","data":null,` +
		`"msg":"Gemini could not generate an image with the given prompt.","status":"2"}}`
	st, err := parseDuomiImageTask([]byte(fail))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "failed" {
		t.Fatalf("状态应为 failed，实际 %q", st.Status)
	}
	if !strings.Contains(st.Error, "could not generate") {
		t.Fatalf("失败原因应取 data.msg，实际 %q", st.Error)
	}

	// 成功响应里的 msg（"success" / 空）不能被当成错误说明。
	ok := `{"code":200,"msg":"success","data":{"task_id":"t2","state":"succeeded","status":"3",` +
		`"data":{"images":[{"url":"https://cdn/a.png"}]},"msg":""}}`
	st2, err := parseDuomiImageTask([]byte(ok))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st2.Status != "succeeded" || st2.Error != "" {
		t.Fatalf("成功响应不应带错误说明：status=%q err=%q", st2.Status, st2.Error)
	}
}

// ---------------------------------------------------------------- 音乐

func TestDuomiMusicSubmitMultipart(t *testing.T) {
	var gotCT, gotBody, gotPath string
	srv, _ := startDuomiCapture(t, func(w http.ResponseWriter, r *http.Request, raw string) {
		gotCT, gotBody, gotPath = r.Header.Get("Content-Type"), raw, r.URL.Path
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"music-1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	id, req, err := submitDuomi(t, cfg, "chirp-v4",
		`{"model":"chirp-v4","prompt":"lofi jazz","tags":"lofi,jazz","title":"Rainy"}`)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if id != "music-1" || req.SubKind != duomiProtoSunoMusic {
		t.Fatalf("任务号/SubKind 不符：%q %q", id, req.SubKind)
	}
	if gotPath != duomiSunoSubmitPath {
		t.Fatalf("路径应为 %s，实际 %s", duomiSunoSubmitPath, gotPath)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data") {
		t.Fatalf("SUNO 生成接口只收 multipart，实际 %q", gotCT)
	}
	for _, want := range []string{`name="custom_mode"`, `name="make_instrumental"`, `name="prompt"`, "lofi jazz", `name="mv"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("表单缺少 %q：%s", want, gotBody)
		}
	}
	// 非自定义模式下不建议下发 tags/title（上游会忽略，传了徒增噪音）。
	if strings.Contains(gotBody, `name="tags"`) {
		t.Fatalf("非自定义模式不应下发 tags：%s", gotBody)
	}
}

func TestDuomiMusicCustomModeCarriesLyrics(t *testing.T) {
	var gotBody string
	srv, _ := startDuomiCapture(t, func(w http.ResponseWriter, _ *http.Request, raw string) {
		gotBody = raw
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"m2"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	if _, _, err := submitDuomi(t, cfg, "chirp-v4",
		`{"model":"chirp-v4","prompt":"秋天","lyrics":"第一句\n第二句","tags":"folk","title":"秋"}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if !strings.Contains(gotBody, `name="custom_mode"`) || !strings.Contains(gotBody, "true") {
		t.Fatalf("给了歌词应自动进入自定义模式：%s", gotBody)
	}
	if !strings.Contains(gotBody, "第一句") {
		t.Fatalf("自定义模式下歌词应放进 prompt 字段：%s", gotBody)
	}
	if !strings.Contains(gotBody, `name="tags"`) || !strings.Contains(gotBody, "folk") {
		t.Fatalf("自定义模式应下发 tags：%s", gotBody)
	}
}

func TestDuomiMusicRejectsInstrumentalWithLyrics(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("互斥参数应本地拒绝")
	})
	cfg := duomiCfg(srv.URL, nil)
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "chirp-v4",
		`{"model":"chirp-v4","prompt":"jazz","lyrics":"歌词","instrumental":true}`), "互斥")
	if rec.hits.Load() != 0 {
		t.Fatalf("本地拒绝前不得出网，实际 %d 次", rec.hits.Load())
	}
}

func TestDuomiMusicMultiArtifactKeepsWaiting(t *testing.T) {
	// 一首完成、一首还在 streaming：必须继续 running，否则第二首永远丢。
	body := `{"code":0,"data":[{"status":"complete","audio_url":"https://cdn/1.mp3","image_url":"https://cdn/cover1.jpg"},{"status":"streaming","audio_url":""}]}`
	st, err := parseDuomiMusicTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "running" {
		t.Fatalf("仍有曲目在生成时必须 running，实际 %q", st.Status)
	}
	if len(st.ResultURLs) != 1 || st.ResultURLs[0] != "https://cdn/1.mp3" {
		t.Fatalf("已完成的曲目应保留，实际 %v", st.ResultURLs)
	}

	// 两首都完成：成功且两个产物。
	both := `{"code":0,"data":[{"status":"complete","audio_url":"https://cdn/1.mp3"},{"status":"complete","audio_url":"https://cdn/2.mp3"}]}`
	st2, _ := parseDuomiMusicTask([]byte(both))
	if st2.Status != "succeeded" || len(st2.ResultURLs) != 2 {
		t.Fatalf("两首都完成应 succeeded + 2 产物，实际 %q %v", st2.Status, st2.ResultURLs)
	}
	// 封面图不是产物。
	for _, u := range st2.ResultURLs {
		if strings.Contains(u, "cover") {
			t.Fatalf("封面图不应计入产物：%v", st2.ResultURLs)
		}
	}
}

func TestDuomiLyricsSubmitAndPoll(t *testing.T) {
	var gotBody map[string]any
	var gotCT, gotPath string
	srv, _ := startDuomiCapture(t, func(w http.ResponseWriter, r *http.Request, raw string) {
		gotCT, gotPath = r.Header.Get("Content-Type"), r.URL.Path
		var v map[string]any
		_ = json.Unmarshal([]byte(raw), &v)
		gotBody = v
		duomiJSON(w, 200, `{"code":200,"data":"lyrics-task-1"}`) // 裸字符串任务号
	})
	cfg := duomiCfg(srv.URL, nil)

	id, req, err := submitDuomi(t, cfg, "suno-lyrics", `{"model":"suno-lyrics","prompt":"写一首关于秋天的歌"}`)
	if err != nil {
		t.Fatalf("歌词提交失败：%v", err)
	}
	if id != "lyrics-task-1" {
		t.Fatalf("裸字符串 data 应能取到任务号，实际 %q", id)
	}
	if req.SubKind != duomiProtoSunoLyrics {
		t.Fatalf("SubKind 应为 %s，实际 %q", duomiProtoSunoLyrics, req.SubKind)
	}
	if gotPath != duomiSunoLyricsPath {
		t.Fatalf("路径应为 %s，实际 %s", duomiSunoLyricsPath, gotPath)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("歌词接口发 JSON，实际 Content-Type %q", gotCT)
	}
	if gotBody["prompt"] != "写一首关于秋天的歌" {
		t.Fatalf("prompt 未正确下发：%v", gotBody)
	}
	// 上游不吃 model，下发会被当未知模型拒绝。
	if _, ok := gotBody["model"]; ok {
		t.Fatalf("歌词接口不应下发 model：%v", gotBody)
	}

	st, perr := parseDuomiLyricsTask([]byte(`{"code":200,"data":{"data":{"ret_a":{"text":"秋风吹过"}}}}`))
	if perr != nil {
		t.Fatalf("解析失败：%v", perr)
	}
	if st.Status != "succeeded" || st.ResultText != "秋风吹过" {
		t.Fatalf("歌词应解析为文本产物，实际 %q %q", st.Status, st.ResultText)
	}

	// 生成中（无状态、无正文）：不能判失败——歌词要跑几十秒，早期轮询必然是空的。
	st2, _ := parseDuomiLyricsTask([]byte(`{"code":200,"data":{"data":{"ret_a":{}}}}`))
	if st2.Status == "failed" || st2.Status == "succeeded" {
		t.Fatalf("无状态无正文应保持未终结，实际 %q", st2.Status)
	}
	// 上游自报成功但正文为空 → 必须判失败（否则用户付费拿到空文件）。
	st3, _ := parseDuomiLyricsTask([]byte(`{"code":200,"data":{"status":"complete","data":{"ret_a":{}}}}`))
	if st3.Status != "failed" || st3.Error == "" {
		t.Fatalf("成功但空正文应判失败并带原因，实际 %q %q", st3.Status, st3.Error)
	}
}

func TestDuomiLyricsPromptLimit(t *testing.T) {
	srv, _ := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("超长 prompt 应本地拒绝")
	})
	cfg := duomiCfg(srv.URL, nil)
	long := strings.Repeat("字", duomiSunoMaxLyricsPrompt+1)
	wantDuomiCaller(t, mustDuomiErr(t, cfg, "suno-lyrics",
		`{"model":"suno-lyrics","prompt":"`+long+`"}`), "过长")
}

// ---------------------------------------------------------------- 路由与开关

func TestDuomiPollTargetRoutesNewProtocols(t *testing.T) {
	opts, err := parseDuomiOptions(nil)
	if err != nil {
		t.Fatalf("默认配置解析失败：%v", err)
	}
	cases := []struct{ subKind, want string }{
		// 文生图与图生图共用同一个查询端点：官方只定义了 GET /api/gemini/nano-banana/{id}
		// （带 `-edit` 的 GET 由 nginx 404，实测），因此子协议只有一个值。
		{duomiProtoNano, duomiNanoT2IPath + "/id1"},
		{duomiProtoGPTImage, duomiGPTImagePoll + "/id1"},
		{duomiProtoSunoMusic, duomiSunoFeedPath + "?task_id=id1"},
		{duomiProtoSunoLyrics, duomiSunoFeedPath + "?task_id=id1"},
		// muse 的查询前缀与提交路径**没有关系**：写成 submit + "/{id}" 会 404 到超时退费。
		{duomiProtoMuse, duomiMusePollPrefix + "/id1"},
	}
	for _, c := range cases {
		got, _ := duomiPollTarget(opts, c.subKind, "id1")
		if got != c.want {
			t.Fatalf("%s 的轮询路径应为 %q，实际 %q", c.subKind, c.want, got)
		}
	}
	// 任务号必须转义（它是查询参数）。
	got, _ := duomiPollTarget(opts, duomiProtoSunoMusic, "a-b_c")
	if got != duomiSunoFeedPath+"?task_id=a-b_c" {
		t.Fatalf("任务号转义不符：%q", got)
	}
	if _, err := url.Parse(got); err != nil {
		t.Fatalf("轮询路径不是合法 URL：%v", err)
	}
}

func TestDuomiNewModelGate(t *testing.T) {
	t.Setenv("LD_DUOMI_NEW_MODELS", "0")
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("开关关闭时不应出网")
	})
	cfg := duomiCfg(srv.URL, nil)

	err := mustDuomiErr(t, cfg, "gpt-image-2", `{"model":"gpt-image-2","prompt":"a"}`)
	var ce *relay.ConfigError
	if !asConfigError(err, &ce) {
		t.Fatalf("开关关闭应报 ConfigError（运营开关，不是调用方参数错），实际 %v", err)
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("开关关闭时不得出网，实际 %d 次", rec.hits.Load())
	}
	// 视频模型不受该开关影响。
	if err := duomiCheckNewModelGate(duomiProtoSeedance); err != nil {
		t.Fatalf("视频协议不应被新模型开关拦截：%v", err)
	}
	// muse 同样是视频协议：它出厂停用，止血走「停用模型/绑定」（即时生效、无需重启），
	// 不挂这个粗粒度总闸 —— 否则老实例上遗留的 LD_DUOMI_NEW_MODELS=0 会让它静默不可用。
	if err := duomiCheckNewModelGate(duomiProtoMuse); err != nil {
		t.Fatalf("muse 不应被新模型开关拦截：%v", err)
	}
}

func TestDuomiImageSizeTierNotBilled(t *testing.T) {
	// 图像按张计费，档位不影响价格 → 不需要像视频那样校验档位一致性；
	// 但档位仍必须下发（这里翻成 nano 系的 image_size），上游据此决定出图分辨率。
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"t"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	if _, _, err := submitDuomi(t, cfg, "nano-banana-pro",
		`{"model":"nano-banana-pro","prompt":"a","size":"4K"}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["image_size"] != "4K" {
		t.Fatalf("image_size 应原样下发，实际 %v", rec.body["image_size"])
	}
}

func TestDuomiSubmitConfigErrorOnBadOptions(t *testing.T) {
	// 配置脏了必须在出网前失败（OptionsErr）。
	cfg := &relay.ChannelConfig{BaseURL: "http://127.0.0.1:1", APIKey: "k", Options: []byte(`{"paths":{"unknown.key":"/x"}}`)}
	_, _, err := submitDuomi(t, cfg, "gpt-image-2", `{"model":"gpt-image-2","prompt":"a"}`)
	if err == nil {
		t.Fatal("配置错必须报错")
	}
	if !errors.Is(err, relay.ErrCallerParam) && !isConfigErr(err) {
		t.Fatalf("应为配置类错误，实际 %v", err)
	}
}

// isConfigErr 判断是否为 ConfigError（避免依赖具体字段）。
func isConfigErr(err error) bool {
	var ce *relay.ConfigError
	return asConfigError(err, &ce)
}

// 编译期确认：新协议常量与 path key 都在枚举里（漏登会让保存配置直接 400）。
func TestDuomiProtocolAndPathEnumsCoverNewModels(t *testing.T) {
	for _, p := range []string{duomiProtoNano, duomiProtoGPTImage, duomiProtoSunoMusic, duomiProtoSunoLyrics, duomiProtoMuse} {
		if !duomiProtocols[p] {
			t.Fatalf("协议 %s 未登记到 duomiProtocols", p)
		}
	}
	for _, k := range []string{"nano.submit", "nano.edit", "nano.poll", "gptimage.submit", "gptimage.poll", "suno.submit", "suno.feed", "suno.lyrics", "muse.submit", "muse.poll"} {
		if !duomiPathKeys[k] {
			t.Fatalf("路径键 %s 未登记到 duomiPathKeys", k)
		}
	}
	// 种子模型必须能解析出协议（否则提交时报「未知模型」）。
	// nano-banana-edit **不在**这个清单里：它是官方端点，不是一个模型。
	//
	// 遍历全表而不是手抄清单：手抄版只列了 10 个（seedance / kling 系列全漏在外面），
	// 是一份必然漂移的第三真相源。新增内置型号只要漏填 Protocol，这里立刻红。
	for id, p := range duomiModels {
		if p.Protocol == "" {
			t.Fatalf("内置模型 %s 缺少协议档案", id)
		}
	}
}

// TestDuomiMusicDropsStreamTokens 串流令牌不能当产物。
//
// 实测（2026-10-24）：`https://audiopipe.suno.ai/?item_id=<clip>` 恒 403
// `{"detail":"Invalid or expired stream URL"}`。它一旦进了产物清单，
// 转存必然全部失败 → 任务被判失败并全额退款，用户明明已生成好两首歌却什么都拿不到。
func TestDuomiMusicDropsStreamTokens(t *testing.T) {
	body := `{"code":0,"data":[{"status":"complete",
		"audio_url":"https://cdn2.suno.show/media/export/a.mp3",
		"stream_audio_url":"https://audiopipe.suno.ai/?item_id=a"},
		{"status":"complete",
		"audio_url":"https://cdn2.suno.show/media/export/b.mp3",
		"source_audio_url":"https://audiopipe.suno.ai/?item_id=b"}]}`
	st, err := parseDuomiMusicTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "succeeded" {
		t.Fatalf("两首都完成应 succeeded，实际 %q", st.Status)
	}
	if len(st.ResultURLs) != 2 {
		t.Fatalf("应只保留 2 条可下载链接，实际 %v", st.ResultURLs)
	}
	for _, u := range st.ResultURLs {
		if duomiIsMusicStreamURL(u) {
			t.Fatalf("串流令牌不应进入产物：%v", st.ResultURLs)
		}
	}
}

// TestDuomiMusicWaitsForExportURL 上游判完成但只给了串流令牌时，必须继续等导出链接。
//
// SUNO 是「先能串流、后能导出」：此刻收尾就是 0 产物 → 转存全失败 → 整单退款。
func TestDuomiMusicWaitsForExportURL(t *testing.T) {
	body := `{"code":0,"data":[{"status":"complete","stream_audio_url":"https://audiopipe.suno.ai/?item_id=a"}]}`
	st, err := parseDuomiMusicTask([]byte(body))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != "running" {
		t.Fatalf("只有串流令牌时必须保持 running，实际 %q", st.Status)
	}
	if len(st.ResultURLs) != 0 {
		t.Fatalf("不应留下产物，实际 %v", st.ResultURLs)
	}
}

var _ = context.Background

// TestDuomiNanoImageSizeDispatchesBySource 守「出站 image_size 由 Source 决定」。
//
// 破坏组合（改造前）：`size=16:9` + `aspect_ratio=4:3` → Ratio 被覆盖成 4:3，
// 而旧判据是 `Ratio != "" && Unknown == Ratio` → 不成立 → 落到「原文透传」→
// 把 "16:9" 塞进上游 image_size（上游只认 1K/2K/4K，直接 400）。
func TestDuomiNanoImageSizeDispatchesBySource(t *testing.T) {
	cases := []struct {
		name string
		in   billing.ImageSize
		want string
	}{
		{"档位", billing.ImageSize{Source: billing.ImageSizeSourceTier, DeclaredTier: "2K", Raw: "2k"}, "2K"},
		{"像素", billing.ImageSize{Source: billing.ImageSizeSourcePixels, Width: 2048, Height: 1152, Raw: "2048x1152"}, "2048x1152"},
		{"纯画幅", billing.ImageSize{Source: billing.ImageSizeSourceRatio, Ratio: "16:9", Raw: "16:9"}, ""},
		// 关键回归：Ratio 被 aspect_ratio 覆盖、Unknown 仍是 size 原文。
		{"画幅 + 冲突画幅", billing.ImageSize{Source: billing.ImageSizeSourceRatio, Ratio: "4:3", Unknown: "16:9", Raw: "16:9"}, ""},
		{"auto", billing.ImageSize{Source: billing.ImageSizeSourceAuto, Auto: true, Raw: "auto"}, ""},
		{"私有写法", billing.ImageSize{Source: billing.ImageSizeSourceOther, Unknown: "portrait_4_3", Raw: "portrait_4_3"}, "portrait_4_3"},
		{"没写", billing.ImageSize{}, ""},
	}
	for _, c := range cases {
		if got := duomiNanoImageSize(c.in); got != c.want {
			t.Fatalf("%s：got %q want %q", c.name, got, c.want)
		}
	}
}

// TestDuomiSunoFormKeepsOrderAndAppendsUnmapped SUNO 的表单对字段顺序敏感
// （continue_at / continue_clip_id 必须跟在 mv 之后），所以透传只能**追加在末尾**。
//
// 如果按名字排序，运营商配置的续写参数会被挪到 mv 前面，上游按位置解析就会错位，
// 而且不会报错 —— 只会生成一段接不上的音频。
func TestDuomiSunoFormKeepsOrderAndAppendsUnmapped(t *testing.T) {
	fields := [][2]string{
		{"prompt", "la la"},
		{"custom_mode", "true"},
		{"mv", "chirp-v4"},
		{"continue_at", "30"},
	}
	raw := []byte(`{"model":"suno-v4","prompt":"p","mv":"chirp-v4","continue_at":30,` +
		`"continue_clip_id":"clip-1","weird_vendor_flag":"on","lyrics":"la"}`)

	got := duomiFormFieldsWithUnmapped(fields, raw, duomiSunoMusicLocalOnly...)

	// ① 已有字段顺序原样保留。
	if len(got) < 4 || got[0][0] != "prompt" || got[1][0] != "custom_mode" ||
		got[2][0] != "mv" || got[3][0] != "continue_at" {
		t.Fatalf("已有字段顺序被改动：%v", got)
	}
	// ② 已映射 / localOnly 的键不重复追加。
	//    model 不在表单里（上游用 workflow/模型编号，由平台决定），因此出现 0 次。
	seen := map[string]int{}
	for _, kv := range got {
		seen[kv[0]]++
	}
	for _, k := range []string{"prompt", "mv", "continue_at"} {
		if seen[k] != 1 {
			t.Fatalf("键 %q 出现 %d 次，应为 1：%v", k, seen[k], got)
		}
	}
	if seen["model"] != 0 {
		t.Fatalf("model 属平台注入，不该出现在表单里：%v", got)
	}
	// ③ 未映射的键追加在末尾，值保持原类型语义（数字转成文本）。
	if seen["continue_clip_id"] != 1 || seen["weird_vendor_flag"] != 1 {
		t.Fatalf("未映射键应被追加：%v", got)
	}
	last := got[len(got)-2:]
	if last[0][0] > last[1][0] {
		t.Fatalf("追加部分应按名字排序：%v", last)
	}
	if last[0][0] != "continue_clip_id" || last[0][1] != "clip-1" {
		t.Fatalf("追加值不符：%v", last)
	}
}
