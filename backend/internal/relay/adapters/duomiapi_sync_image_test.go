package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// 本文件覆盖同步图像桥接（duomiapi_sync_image.go）。
//
// 为什么这批用例必须有：改造前 DuoMi.SupportsEndpoint 全 false，任何走
// /v1/images/generations 的请求都在选路阶段被拒（"渠道 N 不支持端点 images/generations"），
// 而异步链路的测试全绿 —— 缺陷被「只测异步」的测试集完整遮住了。

// syncRec 记录桥接的两跳：提交（POST）与轮询（GET）。
//
// 不复用 duomiRec：那个只保留**最后一次**请求的现场，
// 而桥接的断言需要同时看到「提交报文」与「响应产物」，两者的请求不同。
type syncRec struct {
	submitPath  string
	submitQuery string
	submitBody  map[string]any
	polls       int32
}

// startDuomiSync 起一个「提交即回任务号、轮询若干次后到终态」的上游桩。
//
// polls 是第几次查询开始返回终态（1 = 立刻成功）。
type startDuomiSync struct {
	polls          int32
	failMsg        string
	emptyOnSuccess bool
	// url 是成功时返回的产物链接（空则用默认值）。
	url string
}

func startSyncSrv(t *testing.T, s *startDuomiSync) (*httptest.Server, *syncRec) {
	t.Helper()
	rec := &syncRec{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			rec.submitPath = r.URL.Path
			rec.submitQuery = r.URL.RawQuery
			_ = json.Unmarshal(buf, &rec.submitBody)
			duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"t-1"},"id":"t-1"}`)
			return
		}
		rec.polls++
		if rec.polls < s.polls {
			duomiJSON(w, 200, `{"status":"processing","progress":30}`)
			return
		}
		if s.failMsg != "" {
			duomiJSON(w, 200, `{"status":"failed","error":{"message":"`+s.failMsg+`"}}`)
			return
		}
		if s.emptyOnSuccess {
			duomiJSON(w, 200, `{"status":"succeeded","data":[]}`)
			return
		}
		u := s.url
		if u == "" {
			u = "https://cdn.example.com/a.png"
		}
		duomiJSON(w, 200, `{"status":"succeeded","data":[{"url":"`+u+`"}]}`)
	}))
	t.Cleanup(ts.Close)
	return ts, rec
}

// forwardDuomi 走 DuoMi.Forward（同步链路入口）。
// forwardDuomi 模拟网关调用：`RelayRequest.ImageSize` 由网关解析后注入，
// 这里用同一套 billing 解析从 body 里造出来（适配器不再自己解析请求体）。
func forwardDuomi(t *testing.T, cfg *relay.ChannelConfig, modelID, path, body string) (*relay.RawResponse, error) {
	t.Helper()
	var h struct {
		Size        string `json:"size"`
		ImageSize   string `json:"image_size"`
		Resolution  string `json:"resolution"`
		AspectRatio string `json:"aspect_ratio"`
	}
	_ = json.Unmarshal([]byte(body), &h)
	size := billing.ParseImageSize(billing.ImageDecl{
		Size: h.Size, ImageSize: h.ImageSize, Resolution: h.Resolution, AspectRatio: h.AspectRatio,
	})
	return DuoMi{}.Forward(context.Background(), cfg, &relay.RelayRequest{
		Model: modelID, ModelID: modelID, Body: []byte(body), ContentType: "application/json",
		ImageSize: &size,
	}, path)
}

// ---------------------------------------------------------------- 端点声明

// TestDuomiSupportsOnlyImageSyncEndpoints 锁死「只放行图像两个端点」。
//
// 放开更多端点会让同步网关把 chat / 音频请求派给本适配器，而它们的实现全是
// errDuoMiNoEndpoint —— 那种错会被归因成渠道故障并熔断一条健康渠道。
func TestDuomiSupportsOnlyImageSyncEndpoints(t *testing.T) {
	a := DuoMi{}
	for _, k := range []relay.EndpointKind{relay.KindImagesGenerations, relay.KindImagesEdits} {
		if !a.SupportsEndpoint(k) {
			t.Fatalf("%s 应被支持", relay.EndpointPathOf(k))
		}
	}
	for _, k := range []relay.EndpointKind{
		relay.KindChat, relay.KindCompletions, relay.KindEmbeddings,
		relay.KindImagesVariations, relay.KindAudioSpeech,
		relay.KindAudioTranscriptions, relay.KindAudioTranslations,
		relay.KindModerations, relay.KindRerank, relay.KindResponses,
	} {
		if a.SupportsEndpoint(k) {
			t.Fatalf("%s 不应被支持（本适配器没有对应实现）", relay.EndpointPathOf(k))
		}
	}
}

// TestDuomiForwardRejectsUnknownPath 确认非图像路径仍然报「不支持端点」。
func TestDuomiForwardRejectsUnknownPath(t *testing.T) {
	for _, p := range []string{"/v1/chat/completions", "/v1/audio/speech", "/v1/images/variations"} {
		_, err := forwardDuomi(t, duomiCfg("http://127.0.0.1:1", nil), "gpt-image-2", p,
			`{"model":"gpt-image-2","prompt":"x"}`)
		if !errors.Is(err, errDuoMiNoEndpoint) {
			t.Fatalf("%s 应返回 errDuoMiNoEndpoint，实际 %v", p, err)
		}
	}
}

// TestDuomiNeedsPublicURLOnlyForEdits 确认只有 edits 触发 multipart 直链化。
//
// generations 是纯 JSON（image 直接是链接），转成 JSON 反而会破坏原始报文。
func TestDuomiNeedsPublicURLOnlyForEdits(t *testing.T) {
	a := DuoMi{}
	if !a.NeedsPublicURL(relay.KindImagesEdits) {
		t.Fatal("edits 必须声明需要直链化")
	}
	if a.NeedsPublicURL(relay.KindImagesGenerations) {
		t.Fatal("generations 是 JSON 入参，不该触发 multipart 转换")
	}
}

// ---------------------------------------------------------------- 成功路径

// TestDuomiSyncImageGenerationsGPTImage 覆盖 gpt-image 协议的完整桥接：
// 提交报文 → 轮询 → OpenAI 标准响应。
func TestDuomiSyncImageGenerationsGPTImage(t *testing.T) {
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})

	resp, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2", "/v1/images/generations",
		`{"model":"gpt-image-2","prompt":"a cat","size":"1024x1024","quality":"high","n":1}`)
	if err != nil {
		t.Fatalf("桥接失败：%v", err)
	}
	// 提交报文：路径带 async=true，字段与异步链路同口径。
	if rec.submitBody["model"] != "gpt-image-2" || rec.submitBody["prompt"] != "a cat" {
		t.Fatalf("提交报文字段不对：%+v", rec.submitBody)
	}
	if rec.submitBody["size"] != "1024x1024" || rec.submitBody["quality"] != "high" {
		t.Fatalf("size/quality 应原样下发：%+v", rec.submitBody)
	}
	if !strings.Contains(rec.submitQuery, "async=true") {
		t.Fatalf("提交必须带 async=true，实际 query=%q", rec.submitQuery)
	}

	// 响应必须是 OpenAI 标准形状
	var out struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", resp.StatusCode)
	}
	if len(out.Data) != 1 || out.Data[0].URL != "https://cdn.example.com/a.png" {
		t.Fatalf("响应 data 不对：%s", resp.Body)
	}
	if out.Created == 0 || out.Model != "gpt-image-2" {
		t.Fatalf("created/model 缺失：%s", resp.Body)
	}
}

// TestDuomiSyncImageNanoMapsSizeToAspect 覆盖 nano 协议的入参映射：size → aspect_ratio。
func TestDuomiSyncImageNanoMapsSizeToAspect(t *testing.T) {
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})

	if _, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "nano-banana-2", "/v1/images/generations",
		`{"model":"nano-banana-2","prompt":"sea","size":"1536x1024"}`); err != nil {
		t.Fatalf("桥接失败：%v", err)
	}
	if rec.submitBody["aspect_ratio"] != "3:2" {
		t.Fatalf("1536x1024 应映射为 3:2，实际 %+v", rec.submitBody)
	}
	if _, ok := rec.submitBody["size"]; ok {
		t.Fatalf("nano 协议不该下发 size：%+v", rec.submitBody)
	}
	if rec.submitPath != duomiNanoT2IPath {
		t.Fatalf("提交路径应为 %s，实际 %s", duomiNanoT2IPath, rec.submitPath)
	}

	// 档位键：转成上游的 image_size，且 K 必须大写（官方文档明写）
	if _, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "nano-banana-2", "/v1/images/generations",
		`{"model":"nano-banana-2","prompt":"sea","size":"2k"}`); err != nil {
		t.Fatalf("nano-banana-2 应接受 2K 档位：%v", err)
	}
	if rec.submitBody["image_size"] != "2K" {
		t.Fatalf("size=2k 应归一成 image_size=2K，实际 %+v", rec.submitBody)
	}

	// 另一个型号同样透传（不再有「本地拒绝」这一层）
	if _, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "nano-banana", "/v1/images/generations",
		`{"model":"nano-banana","prompt":"sea","size":"2K"}`); err != nil {
		t.Fatalf("参数应一律透传（不再按型号白名单拒绝）：%v", err)
	}
}

// TestDuomiSyncImageEditsNanoSendsImageURLs 覆盖图生图：image 转成 image_urls 数组。
func TestDuomiSyncImageEditsNanoSendsImageURLs(t *testing.T) {
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})

	if _, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "nano-banana-2", "/v1/images/edits",
		`{"model":"nano-banana-2","prompt":"make it blue","image":"https://cdn.example.com/src.png"}`); err != nil {
		t.Fatalf("桥接失败：%v", err)
	}
	urls, ok := rec.submitBody["image_urls"].([]any)
	if !ok || len(urls) != 1 || urls[0] != "https://cdn.example.com/src.png" {
		t.Fatalf("image 应转成 image_urls：[%+v]", rec.submitBody)
	}
	if rec.submitPath != duomiNanoEditPath {
		t.Fatalf("提交路径应为 %s，实际 %s", duomiNanoEditPath, rec.submitPath)
	}

	// 缺参考图必须本地拒绝（上游会白跑一次再 400）
	_, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "nano-banana-2", "/v1/images/edits",
		`{"model":"nano-banana-2","prompt":"make it blue"}`)
	wantDuomiCaller(t, err, "必须提供参考图")
}

// TestDuomiSyncImageGenerationsWithImageGoesEdit 文生图端点带了参考图 → 自动走编辑端点。
//
// 这是「端点由有没有参考图决定」的反向证明：客户端不必知道官方有两个端点。
func TestDuomiSyncImageGenerationsWithImageGoesEdit(t *testing.T) {
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})
	if _, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "nano-banana-2", "/v1/images/generations",
		`{"model":"nano-banana-2","prompt":"x","image":"https://c/a.png"}`); err != nil {
		t.Fatalf("桥接失败：%v", err)
	}
	if rec.submitPath != duomiNanoEditPath {
		t.Fatalf("带图请求应走编辑端点 %s，实际 %s", duomiNanoEditPath, rec.submitPath)
	}
}

// TestDuomiNanoRatioNeverGoesIntoImageSize 画幅只能进 aspect_ratio。
//
// nano 的 image_size 只收档位词：把 16:9 塞进去就是一个必然 400 的非法值，
// 而用户只是想要一个宽幅。
func TestDuomiNanoRatioNeverGoesIntoImageSize(t *testing.T) {
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})
	if _, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "nano-banana-2", "/v1/images/generations",
		`{"model":"nano-banana-2","prompt":"x","size":"16:9"}`); err != nil {
		t.Fatalf("桥接失败：%v", err)
	}
	if got := rec.submitBody["aspect_ratio"]; got != "16:9" {
		t.Fatalf("画幅应进 aspect_ratio，实际 %v", got)
	}
	if _, ok := rec.submitBody["image_size"]; ok {
		t.Fatalf("画幅不该进 image_size：%+v", rec.submitBody)
	}
}

// ---------------------------------------------------------------- 拒绝路径

// TestDuomiSyncImageProtocolMismatch 覆盖仍然保留的「结构前提」拒绝。
//
// n≠1 / nano 的 quality / mask 已从这张表移除：它们属于「上游支不支持」，
// 由上游回答（见 TestDuomiSyncImageForwardsUnmapped）。
func TestDuomiSyncImageProtocolMismatch(t *testing.T) {
	cfg := duomiCfg("http://127.0.0.1:1", nil)
	cases := []struct {
		name    string
		model   string
		path    string
		body    string
		wantMsg string
	}{
		{"视频模型走图像端点", "kling-v3-std", "/v1/images/generations",
			`{"model":"kling-v3-std","prompt":"x"}`, "不是图像模型"},
		{"edits 没有参考图", "nano-banana-2", "/v1/images/edits",
			`{"model":"nano-banana-2","prompt":"x"}`, "必须提供参考图"},
	}
	for _, c := range cases {
		_, err := forwardDuomi(t, cfg, c.model, c.path, c.body)
		if err == nil {
			t.Fatalf("%s：应报错，实际成功", c.name)
		}
		wantDuomiCaller(t, err, c.wantMsg)
	}
}

// TestDuomiSyncImageUpstreamFailed 覆盖上游任务失败。
func TestDuomiSyncImageUpstreamFailed(t *testing.T) {
	ts, _ := startSyncSrv(t, &startDuomiSync{polls: 1, failMsg: "internal error"})

	_, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2", "/v1/images/generations",
		`{"model":"gpt-image-2","prompt":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatalf("应带回上游失败原因，实际 %v", err)
	}
	// 普通上游失败不是调用方问题：必须是普通错误（dispatch 会记渠道健康度）
	if errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("普通上游失败不该归因为调用方参数错")
	}
}

// TestDuomiSyncImageModerationIsCallerFault 确认审核拒绝归因到调用方（400，不污染渠道健康度）。
func TestDuomiSyncImageModerationIsCallerFault(t *testing.T) {
	ts, _ := startSyncSrv(t, &startDuomiSync{polls: 1, failMsg: "内容审核未通过：图片包含违规内容"})

	_, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2", "/v1/images/generations",
		`{"model":"gpt-image-2","prompt":"x"}`)
	if !errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("审核拒绝应归因为 ErrCallerParam，实际 %v", err)
	}
}

// TestDuomiSyncImageEmptyArtifactsIsFailure 确认「上游说成功但没有产物」判失败。
//
// 判成功会让用户拿不到图却照常扣费，这比失败更糟（失败至少全额释放）。
func TestDuomiSyncImageEmptyArtifactsIsFailure(t *testing.T) {
	ts, _ := startSyncSrv(t, &startDuomiSync{polls: 1, emptyOnSuccess: true})

	_, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2", "/v1/images/generations",
		`{"model":"gpt-image-2","prompt":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "没有返回任何产物") {
		t.Fatalf("空产物应判失败，实际 %v", err)
	}
}

// TestDuomiSyncImageTimeoutIsTyped 确认超时返回 relay.SyncWaitTimeoutError（dispatch 据此给 504）。
func TestDuomiSyncImageTimeoutIsTyped(t *testing.T) {
	// 等待上限压到 2 秒：轮询间隔 1s，够跑两轮。
	t.Setenv("LD_DUOMI_SYNC_IMAGE_WAIT_SEC", "2")
	ts, _ := startSyncSrv(t, &startDuomiSync{polls: 1 << 20}) // 永远 running

	_, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2", "/v1/images/generations",
		`{"model":"gpt-image-2","prompt":"x"}`)
	var to *relay.SyncWaitTimeoutError
	if !errors.As(err, &to) {
		t.Fatalf("应返回 SyncWaitTimeoutError，实际 %T %v", err, err)
	}
	if to.Waited.Seconds() != 2 {
		t.Fatalf("Waited 应为 2s，实际 %v", to.Waited)
	}
	// 文案必须告诉调用方出路（异步端点）
	if !strings.Contains(to.Error(), "异步端点") {
		t.Fatalf("超时文案应指向异步端点，实际 %q", to.Error())
	}
}

// TestDuomiSyncImageModelOverrideProtocol 确认 model_overrides 登记的模型也能走桥接。
//
// 线上新模型大多只写渠道配置（不发版），桥接必须认这条路径，
// 否则「档案里没写的新模型」一律报「未知的多米模型」。
func TestDuomiSyncImageModelOverrideProtocol(t *testing.T) {
	ts, rec := startSyncSrv(t, &startDuomiSync{polls: 1})
	opts := []byte(`{"model_overrides":{"gemini-3.1-flash-image-preview":{"protocol":"nano","upstream_model":"gemini-3.1-flash-image-preview"}}}`)

	if _, err := forwardDuomi(t, duomiCfg(ts.URL, opts), "gemini-3.1-flash-image-preview",
		"/v1/images/generations", `{"model":"gemini-3.1-flash-image-preview","prompt":"sea","size":"1024x1024"}`); err != nil {
		t.Fatalf("桥接失败：%v", err)
	}
	if rec.submitBody["aspect_ratio"] != "1:1" {
		t.Fatalf("1024x1024 应映射为 1:1，实际 %+v", rec.submitBody)
	}
	if rec.submitBody["model"] != "gemini-3.1-flash-image-preview" {
		t.Fatalf("上游模型名不对：%+v", rec.submitBody)
	}
}

// ---------------------------------------------------------------- 尺寸映射

// TestDuomiAspectFromSize 覆盖「像素尺寸 → 上游画幅」的折算。
//
// 折算本身只保留一份实现（billing.NormalizeRatio：约分 + 吸附标准画幅），
// 适配器只做白名单校验 —— 各适配器自己算比例，就是第二份比例表。
func TestDuomiAspectFromSize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1024x1024", "1:1"},
		{"1536x1024", "3:2"},
		{"1024x1536", "2:3"},
		// 1792x1024 是 DALL-E 3 的 7:4 横版尺寸：nano 系没有这个画幅，
		// 折算结果是 7:4 而不是"吸附到 16:9"（吸附表已收录 7:4，不会再把它算歪）。
		{"1792x1024", "7:4"},
		{"1024x1792", "4:7"},
		{"1408x1120", "5:4"},
		{"auto", ""},
		{"7:3", "7:3"},
		{"", ""},
		{"abc", ""},
	}
	for _, c := range cases {
		if got := duomiAspectFromSizeForTest(c.in); got != c.want {
			t.Fatalf("duomiAspectFromSize(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// duomiAspectFromSizeForTest 复刻「平台解析 → 字段映射」这两步。
//
// 不再有画幅白名单：适配器只做字段改名（size→image_size、画幅→aspect_ratio），
// 上游认不认由它决定 —— 平台替上游判能力只会在上游扩容时误杀可用组合。
func duomiAspectFromSizeForTest(raw string) string {
	size := billing.ParseImageSize(billing.ImageDecl{Size: raw})
	return size.Ratio
}

// TestDuomiSyncImageWaitEnv 确认等待上限可被环境变量覆盖（压测/应急入口）。
func TestDuomiSyncImageWaitEnv(t *testing.T) {
	t.Setenv("LD_DUOMI_SYNC_IMAGE_WAIT_SEC", "42")
	if got := duomiSyncImageWait().Seconds(); got != 42 {
		t.Fatalf("环境变量未生效，实际 %v", got)
	}
	t.Setenv("LD_DUOMI_SYNC_IMAGE_WAIT_SEC", "not-a-number")
	if got := duomiSyncImageWait(); got != duomiSyncImageDefaultWait {
		t.Fatalf("非法值应回落默认，实际 %v", got)
	}
	// 越界值必须夹到上限，不能"原样生效"：写成 86400 会让一个请求占住连接与
	// 冻结额度一整天，而反向代理早在 960s 就断了，表现为"额度不释放"的假故障。
	t.Setenv("LD_DUOMI_SYNC_IMAGE_WAIT_SEC", "86400")
	if got := duomiSyncImageWait(); got != duomiSyncImageMaxWait {
		t.Fatalf("超过上限应夹到 %v，实际 %v", duomiSyncImageMaxWait, got)
	}
	// 小值不夹：压测与单测都要靠它把等待压到秒级（TestDuomiSyncImageTimeoutIsTyped 就是 2s）。
	t.Setenv("LD_DUOMI_SYNC_IMAGE_WAIT_SEC", "2")
	if got := duomiSyncImageWait(); got != 2*time.Second {
		t.Fatalf("小值应原样生效，实际 %v", got)
	}
	t.Setenv("LD_DUOMI_SYNC_IMAGE_WAIT_SEC", "0")
	if got := duomiSyncImageWait(); got != duomiSyncImageDefaultWait {
		t.Fatalf("0 应回落默认，实际 %v", got)
	}
}

// TestDuomiSyncImageWaitBelowProxy 守住「应用层必须先于反向代理返回」这条约束。
//
// 反向代理（入口层模板）的 proxy_read_timeout 是 960s，应用上限一旦 ≥ 它，
// 应用算出的结果就写不出去：客户端只会拿到 nginx 的 504 HTML，而调用记录里可能是
// 200 settled —— 这正是"上游成功、系统成功、用户 504"的成因。
func TestDuomiSyncImageWaitBelowProxy(t *testing.T) {
	const proxyReadTimeout = 960 * time.Second
	if duomiSyncImageDefaultWait >= proxyReadTimeout {
		t.Fatalf("默认等待上限 %v 必须小于反向代理读超时 %v（且要留出余量）",
			duomiSyncImageDefaultWait, proxyReadTimeout)
	}
	if duomiSyncImageMaxWait >= proxyReadTimeout {
		t.Fatalf("上限 %v 也必须小于反向代理读超时 %v：环境变量调大后仍然要能被应用自己先超时",
			duomiSyncImageMaxWait, proxyReadTimeout)
	}
	// 覆盖 gpt-image 的实测 540s，否则每次都在真实耗时附近掷骰子。
	if duomiSyncImageDefaultWait < 600*time.Second {
		t.Fatalf("默认等待上限 %v 覆盖不了 gpt-image 的实测 540s", duomiSyncImageDefaultWait)
	}
}

// ---------------------------------------------------------------- P6：image 形态校验

// TestDuomiSyncImageGPTImageRejectsInlineDataURL 覆盖 P6：同步 gpt-image 链路的
// image 字段与异步链路同口径——data: 内联必须拒绝并引导先 /v1/files 上传。
// 修复前原样下发，上游返回的错误不可归因（nano 系早已拒绝，两条链路曾不一致）。
func TestDuomiSyncImageGPTImageRejectsInlineDataURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string // image 字段的 JSON 原文
		want  string // 错误文案必须包含的关键词
	}{
		{"字符串 data:", `"data:image/png;base64,iVBORw0KGgo="`, "/v1/files"},
		{"数组内 data:", `["https://cdn.example.com/a.png","data:image/png;base64,iVBORw0KGgo="]`, "/v1/files"},
		{"相对路径", `"/f/abc/ref.png"`, "http(s) 公网直链"},
	} {
		_, err := forwardDuomi(t, duomiCfg("http://127.0.0.1:1", nil), "gpt-image-2",
			"/v1/images/generations",
			`{"model":"gpt-image-2","prompt":"x","image":`+tc.image+`}`)
		pe, ok := err.(*relay.CallerParamError)
		if !ok {
			t.Fatalf("%s：应返回 CallerParamError，实际 %v", tc.name, err)
		}
		if !strings.Contains(pe.Msg, tc.want) {
			t.Errorf("%s：文案应含 %q，实际 %q", tc.name, tc.want, pe.Msg)
		}
	}
}

// TestDuomiSyncImageGPTImageAcceptsPublicURL 守住不误伤：http(s) 直链原样放行。
func TestDuomiSyncImageGPTImageAcceptsPublicURL(t *testing.T) {
	ts, _ := startSyncSrv(t, &startDuomiSync{polls: 1})
	_, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2",
		"/v1/images/generations",
		`{"model":"gpt-image-2","prompt":"x","image":"https://cdn.example.com/ref.png"}`)
	if err != nil {
		t.Fatalf("公网直链不应被拒：%v", err)
	}
}

// TestDuomiSyncImageForwardsUnmapped 是本次整改的核心回归：用户原始报文的每一个字段
// 都必须有明确归宿 —— 被映射的（mask_url）按同名下发，平台本地消费的
// （response_format）不下发，其余（output_format）原样透传。
//
// 改造前的症状：duomiSyncImageRequest 只声明 8 个字段，mask_url 与 output_format
// 连读都读不到，上游收到的是一次**没有蒙版**的生成请求，而账单上看不出任何异常。
func TestDuomiSyncImageForwardsUnmapped(t *testing.T) {
	var submit map[string]any
	var srvURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = json.Unmarshal(buf, &submit)
			duomiJSON(w, 200, `{"code":0,"message":"SUCCEED","data":{"task_id":"t-1"},"id":"t-1"}`)
			return
		}
		if r.URL.Path == "/artifact.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("PNG-BYTES"))
			return
		}
		duomiJSON(w, 200, `{"status":"succeeded","data":[{"url":"`+srvURL+`/artifact.png"}]}`)
	}))
	t.Cleanup(ts.Close)
	srvURL = ts.URL

	const (
		imageURL = "https://ling-ai.oss-cn-shanghai.aliyuncs.com/files/244/ref.webp"
		maskURL  = "https://ling-ai.oss-cn-shanghai.aliyuncs.com/files/244/mask.png"
	)
	body := `{"model":"gpt-image-2.5-flare","prompt":"仅将透明蒙版覆盖的区域改为：2026你好",` +
		`"image":"` + imageURL + `","mask_url":"` + maskURL + `",` +
		`"size":"1024x1024","response_format":"b64_json","output_format":"png"}`

	resp, err := forwardDuomi(t, duomiCfg(ts.URL, nil), "gpt-image-2.5-flare",
		"/v1/images/generations", body)
	if err != nil {
		t.Fatalf("同步桥应成功：%v", err)
	}

	// ① 蒙版：平台名就是 mask_url，出站同名，绝不改成 mask。
	if got := submit["mask_url"]; got != maskURL {
		t.Fatalf("mask_url 应原样下发，实际 %#v", got)
	}
	if _, ok := submit["mask"]; ok {
		t.Fatal("出站不该出现 mask：平台字段名只有 mask_url")
	}
	// ② 未知键原样透传（改造前就死在这里）。
	if got := submit["output_format"]; got != "png" {
		t.Fatalf("output_format 应原样透传，实际 %#v", got)
	}
	// ③ 尺寸仍由平台解析结果决定（映射优先，不被请求体原值覆盖）。
	if got := submit["size"]; got != "1024x1024" {
		t.Fatalf("size 应原样下发，实际 %#v", got)
	}
	// ④ response_format 是平台本地消费：不下发，但要真的生效（响应变 b64）。
	if _, ok := submit["response_format"]; ok {
		t.Fatal("response_format 由平台消费，不该下发给上游")
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%s", resp.Body)
	}
	if len(out.Data) != 1 || out.Data[0].B64 == "" {
		t.Fatalf("response_format=b64_json 应返回 b64，实际 %s", resp.Body)
	}
}
