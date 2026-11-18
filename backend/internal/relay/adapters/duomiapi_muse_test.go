package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
)

// 多米的 muse 视频协议（POST /v1/videos/generations + GET /v1/videos/tasks/{id}）。
//
// 这一组用例守三件最容易在"看起来能跑通"的情况下出错的事：
//  1. **查询路径独立**：写成"提交路径 + /{id}"不会报错，只会让每次轮询 404，
//     最终超时退费 —— 已生成的视频被丢掉；
//  2. **参考图只从 input_reference 取**：官方字段 image_urls 由平台入口折叠进来
//     （task.ParseVideoSubmit），适配器层读到顶层数组一律不当参考图 ——
//     否则它会绕过 SSRF 校验与参考图计费；
//  3. **duration 原话下发**：平台不替上游裁剪成 10/20/30，也不在智能时长时补 0。

const museBody = `{"model":"muse-video","prompt":"广告","aspect_ratio":"9:16","duration":10,` +
	`"input_reference":[{"type":"image","image_url":"https://cdn.example.com/a.png"}]}`

// bodyKeys 返回出站报文的全部顶层键（用于"恰好这些键"的断言）。
func bodyKeys(body map[string]any) map[string]bool {
	out := make(map[string]bool, len(body))
	for k := range body {
		out[k] = true
	}
	return out
}

func TestDuomiMuseSubmitHappyPath(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"8cb280b3-442e-1db2-43f0-6e73616eb474"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	body := `{"model":"muse-video","prompt":"广告","aspect_ratio":"16:9","duration":20,` +
		`"input_reference":[` +
		`{"type":"last_frame","image_url":"https://cdn.example.com/last.png"},` +
		`{"type":"first_frame","image_url":"https://cdn.example.com/first.png"},` +
		`{"type":"image","image_url":"https://cdn.example.com/ref.png"}]}`
	id, req, err := submitDuomi(t, cfg, "muse-video", body)
	if err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if id != "8cb280b3-442e-1db2-43f0-6e73616eb474" {
		t.Fatalf("任务号解析错误：%q", id)
	}
	if req.SubKind != duomiSubMuse {
		t.Fatalf("SubKind 应回写 %q（否则轮询会查错端点），实际 %q", duomiSubMuse, req.SubKind)
	}
	if rec.method != http.MethodPost || rec.path != duomiMuseSubmitPath {
		t.Fatalf("提交应为 POST %s，实际 %s %s", duomiMuseSubmitPath, rec.method, rec.path)
	}

	// 出站键必须**恰好**这 5 个：多一个 resolution/size 就会让上游 400。
	want := map[string]bool{"model": true, "prompt": true, "aspect_ratio": true, "duration": true, "image_urls": true}
	if got := bodyKeys(rec.body); len(got) != len(want) {
		t.Fatalf("出站键应恰好 %v，实际 %v（多出的键见 raw=%s）", want, got, rec.raw)
	}
	for k := range want {
		if !bodyKeys(rec.body)[k] {
			t.Fatalf("出站缺键 %q：%s", k, rec.raw)
		}
	}
	if rec.body["model"] != "muse-video" {
		t.Fatalf("model 应为 muse-video，实际 %v", rec.body["model"])
	}
	if rec.body["aspect_ratio"] != "16:9" {
		t.Fatalf("aspect_ratio 应原话下发，实际 %v", rec.body["aspect_ratio"])
	}
	if rec.body["duration"] != float64(20) {
		t.Fatalf("duration 应为 20，实际 %v", rec.body["duration"])
	}

	// 顺序是约定的：首帧 → 普通图 → 尾帧（上游按位置引用素材）。
	urls, ok := rec.body["image_urls"].([]any)
	if !ok {
		t.Fatalf("image_urls 必须是数组：%s", rec.raw)
	}
	gotURLs := make([]string, 0, len(urls))
	for _, u := range urls {
		gotURLs = append(gotURLs, u.(string))
	}
	wantURLs := []string{
		"https://cdn.example.com/first.png",
		"https://cdn.example.com/ref.png",
		"https://cdn.example.com/last.png",
	}
	if strings.Join(gotURLs, ",") != strings.Join(wantURLs, ",") {
		t.Fatalf("image_urls 顺序应为 %v，实际 %v", wantURLs, gotURLs)
	}
}

// 适配器只把 input_reference 当参考图：顶层 image_urls 即使出现也不认，且一次都不出网。
//
// 平台入口（task.ParseVideoSubmit）已经把官方 image_urls 折叠成 input_reference，
// 所以正常链路上这里读不到顶层数组；这条用例守的是**兜底**：万一有别的调用方
// （旧任务重放、直调适配器）塞进来，它不会被当作素材透传 —— 那会同时绕过
// SSRF 校验与参考图计费，而账单照收。
func TestDuomiMuseIgnoresTopLevelImageURLs(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("没有可用参考图时不应出网")
	})
	cfg := duomiCfg(srv.URL, nil)

	_, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","aspect_ratio":"9:16","image_urls":["https://cdn.example.com/a.png"]}`)
	wantDuomiCaller(t, err, "参考图")
	if rec.hits.Load() != 0 {
		t.Fatalf("不得出网，实际 %d 次", rec.hits.Load())
	}
}

// 三个必填项各自缺失都要在出网前给出人话错误。
func TestDuomiMuseRequiresPromptRefsAspectRatio(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("参数缺失时不应出网")
	})
	cfg := duomiCfg(srv.URL, nil)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 prompt", `{"model":"muse-video","aspect_ratio":"16:9","input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`, "prompt"},
		{"缺参考图", `{"model":"muse-video","prompt":"广告","aspect_ratio":"16:9"}`, "参考图"},
		{"缺 aspect_ratio", `{"model":"muse-video","prompt":"广告","input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`, "aspect_ratio"},
	}
	for _, c := range cases {
		_, _, err := submitDuomi(t, cfg, "muse-video", c.body)
		if err == nil {
			t.Fatalf("%s：应报参数错误", c.name)
		}
		if !errors.Is(err, relay.ErrCallerParam) {
			t.Fatalf("%s：应为 CallerParamError，实际 %v", c.name, err)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：文案应包含 %q，实际 %q", c.name, c.want, err.Error())
		}
	}
	if rec.hits.Load() != 0 {
		t.Fatalf("不得出网，实际 %d 次", rec.hits.Load())
	}
}

// 视频/音频素材在 muse 里没有对应字段：必须明确拒绝，不能静默丢（丢了用户以为用上了，账单照收）。
func TestDuomiMuseRejectsNonImageRefs(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("非图片素材被拒时不应出网")
	})
	cfg := duomiCfg(srv.URL, nil)

	_, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","aspect_ratio":"16:9",`+
			`"input_reference":[{"type":"image","image_url":"https://cdn/a.png"},`+
			`{"type":"video","video_url":"https://cdn/b.mp4"}]}`)
	wantDuomiCaller(t, err, "只接受图片参考")
	if rec.hits.Load() != 0 {
		t.Fatalf("不得出网，实际 %d 次", rec.hits.Load())
	}
}

// duration 原话下发：15 既不 clamp 也不替换成最近档（10/20/30 的约束由上游判定）。
func TestDuomiMuseDurationVerbatim(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"t1"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	if _, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","aspect_ratio":"16:9","duration":15,`+
			`"input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["duration"] != float64(15) {
		t.Fatalf("duration 应原话下发 15，实际 %v", rec.body["duration"])
	}

	// 字符串写法等价（平台两种时长写法都认）。
	if _, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","aspect_ratio":"16:9","seconds":"15",`+
			`"input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["duration"] != float64(15) {
		t.Fatalf("seconds 字符串应折算成 duration=15，实际 %v", rec.body["duration"])
	}
}

// 智能时长 / 省略时长：**不下发任何时长字段**（补 0 或 -1 都会被上游 400）。
func TestDuomiMuseOmitsDurationWhenAuto(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"t1"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	for _, req := range []*relay.TaskSubmitRequest{
		{ModelID: "muse-video", Body: []byte(museBody), OmitDuration: true},
		{ModelID: "muse-video", Body: []byte(`{"model":"muse-video","prompt":"广告","aspect_ratio":"9:16",` +
			`"input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`), AutoDuration: true},
	} {
		rec.body = nil
		if _, err := (DuoMi{}).SubmitTask(context.Background(), cfg, req); err != nil {
			t.Fatalf("提交失败：%v", err)
		}
		if _, ok := rec.body["duration"]; ok {
			t.Fatalf("自动时长时不得下发 duration，实际 raw=%s", rec.raw)
		}
		if strings.Contains(rec.raw, "-1") {
			t.Fatalf("不得把 -1 下发给不认它的上游：%s", rec.raw)
		}
	}
}

// 上游没有 resolution：平台归一注入了也不能透传（那是替上游发一个它不认的参数）。
func TestDuomiMuseNeverSendsResolution(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"t1"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	if _, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","aspect_ratio":"9:16","resolution":"1080p","size":"1920x1080",`+
			`"input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	for _, k := range []string{"resolution", "size", "image_size"} {
		if _, ok := rec.body[k]; ok {
			t.Fatalf("%s 不该下发（上游没有这个字段）：%s", k, rec.raw)
		}
	}
	if rec.body["aspect_ratio"] != "9:16" {
		t.Fatalf("aspect_ratio 应原话保留，实际 %v", rec.body["aspect_ratio"])
	}
}

// ratio 是平台的别名写法：出站统一用上游的 aspect_ratio；两者冲突时以原生写法为准。
func TestDuomiMuseAspectRatioAlias(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"id":"t1"}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	if _, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","ratio":"9:16",`+
			`"input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["aspect_ratio"] != "9:16" {
		t.Fatalf("ratio 应翻译成 aspect_ratio，实际 %v", rec.body["aspect_ratio"])
	}
	if _, ok := rec.body["ratio"]; ok {
		t.Fatalf("ratio 不该同时下发（重复表达）：%s", rec.raw)
	}

	if _, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","aspect_ratio":"16:9","ratio":"9:16",`+
			`"input_reference":[{"type":"image","image_url":"https://cdn/a.png"}]}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["aspect_ratio"] != "16:9" {
		t.Fatalf("冲突时应以 aspect_ratio 为准，实际 %v", rec.body["aspect_ratio"])
	}
}

// data: 内联素材一律拒（上游要公网直链；内联体还会让重放丢素材）。
func TestDuomiMuseRejectsDataURI(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("内联素材被拒时不应出网")
	})
	cfg := duomiCfg(srv.URL, nil)

	_, _, err := submitDuomi(t, cfg, "muse-video",
		`{"model":"muse-video","prompt":"广告","aspect_ratio":"16:9",`+
			`"input_reference":[{"type":"image","image_url":"data:image/png;base64,AAA"}]}`)
	wantDuomiCaller(t, err, "data:")
	if rec.hits.Load() != 0 {
		t.Fatalf("不得出网，实际 %d 次", rec.hits.Load())
	}
}

// 查询路径与提交路径**没有前缀关系** —— 这是本协议最贵的一处。
func TestDuomiMusePollPathSeparate(t *testing.T) {
	opts, err := parseDuomiOptions(nil)
	if err != nil {
		t.Fatalf("默认配置解析失败：%v", err)
	}
	got, parse := duomiPollTarget(opts, duomiSubMuse, "id1")
	if got != duomiMusePollPrefix+"/id1" {
		t.Fatalf("查询路径应为 %q，实际 %q", duomiMusePollPrefix+"/id1", got)
	}
	if got == duomiMuseSubmitPath+"/id1" {
		t.Fatalf("查询路径绝不能是「提交路径 + /{id}」（会 404 到超时退费）：%q", got)
	}
	if parse == nil {
		t.Fatal("缺查询响应解析器")
	}

	// 渠道把 muse.poll 写成带尾斜杠：必须归一，不能拼出 //id1。
	custom, err := parseDuomiOptions([]byte(`{"paths":{"muse.poll":"/v2/videos/tasks/"}}`))
	if err != nil {
		t.Fatalf("自定义路径解析失败：%v", err)
	}
	if got, _ := duomiPollTarget(custom, duomiSubMuse, "id1"); got != "/v2/videos/tasks/id1" {
		t.Fatalf("自定义前缀应归一为 /v2/videos/tasks/id1，实际 %q", got)
	}
}

// 查询响应解析（与 seedance 同形，共用 parseDuomiVideoTask）。
func TestDuomiMusePollParsesVideoEnvelope(t *testing.T) {
	ok := `{"id":"t1","state":"succeeded","progress":100,"message":"",` +
		`"data":{"videos":[{"url":"https://cdn3.dmiapi.com/a.mp4?token=x"}]},"action":"generate"}`
	st, err := parseDuomiVideoTask([]byte(ok))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if st.Status != taskstate.Succeeded || len(st.ResultURLs) != 1 ||
		st.ResultURLs[0] != "https://cdn3.dmiapi.com/a.mp4?token=x" {
		t.Fatalf("成功响应解析错误：%+v", st)
	}

	running, err := parseDuomiVideoTask([]byte(`{"id":"t1","state":"running","progress":40}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if running.Status != taskstate.Running || len(running.ResultURLs) != 0 {
		t.Fatalf("running 响应解析错误：%+v", running)
	}

	// 成功但无产物 → 必须判失败退费，不能"结算成功但用户拿不到文件"。
	empty, err := parseDuomiVideoTask([]byte(`{"id":"t1","state":"succeeded","data":{"videos":[]}}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if empty.Status != taskstate.Failed || !strings.Contains(empty.Error, "缺少视频产物") {
		t.Fatalf("成功无产物应判失败，实际 %+v", empty)
	}

	// 业务码失败（HTTP 200 + code 非 0）：直接判失败，别让它烂在 running。
	bad, err := parseDuomiVideoTask([]byte(`{"code":400,"msg":"image_urls too many","data":null}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if bad.Status != taskstate.Failed || !strings.Contains(bad.Error, "image_urls too many") {
		t.Fatalf("业务码失败应带上游原话，实际 %+v", bad)
	}
}

// 提交期业务码分档：400 类归调用方参数错，401 归钥匙级（HTTPStatusError）。
func TestDuomiMuseSubmitBusinessCode(t *testing.T) {
	cases := []struct {
		body  string
		check func(t *testing.T, err error)
	}{
		{`{"code":400,"msg":"duration must be 10/20/30"}`, func(t *testing.T, err error) {
			if !errors.Is(err, relay.ErrCallerParam) {
				t.Fatalf("业务码 400 应为 CallerParamError，实际 %v", err)
			}
			if !strings.Contains(err.Error(), "duration must be") {
				t.Fatalf("应带上游原话，实际 %q", err.Error())
			}
		}},
		{`{"code":401,"msg":"invalid token"}`, func(t *testing.T, err error) {
			var hs *relay.HTTPStatusError
			if !errors.As(err, &hs) {
				t.Fatalf("业务码 401 应为 HTTPStatusError（否则坏 key 永不冷却），实际 %v", err)
			}
			if hs.Code != http.StatusUnauthorized {
				t.Fatalf("状态码应为 401，实际 %d", hs.Code)
			}
		}},
	}
	for _, c := range cases {
		srv, _ := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
			duomiJSON(w, 200, c.body)
		})
		_, _, err := submitDuomi(t, duomiCfg(srv.URL, nil), "muse-video", museBody)
		if err == nil {
			t.Fatalf("应返回错误：%s", c.body)
		}
		c.check(t, err)
	}
}

// 内置档案与族推断必须都认 muse-video（前者给上游名兜底，后者让同族新型号零配置）。
func TestDuomiMuseProfileAndFamily(t *testing.T) {
	p, ok := duomiModels["muse-video"]
	if !ok {
		t.Fatal("muse-video 未登记到 duomiModels：绑定未填 upstream_model 时会直接报缺上游名")
	}
	if p.Protocol != duomiProtoMuse || p.Upstream != "muse-video" {
		t.Fatalf("内置档案不符：%+v", p)
	}
	// 序列化一次，防"字段加了但忘了导出到前端常量"（真正的门禁是 gen 比对）。
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("档案序列化失败：%v", err)
	}
}
