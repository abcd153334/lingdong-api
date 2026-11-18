package spec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/relay"
)

// 本文件守「规格 DSL 的表达力」这一组能力：非标字段、路径占位符、产物过滤、时长口径。
//
// 它们共同回答一个问题：**换一家非标上游，规格还写不写得出来**。
// 每一条断言都对应一次真实故障（见各自的注释）。

func TestRequestFieldRequiredIsLocalError(t *testing.T) {
	a := mustAdapter(t, `{"slug":"rf-1","label":"rf","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"request_fields":[{"name":"reference_audio","type":"url","required":true,"desc":"参考音色的音频地址"}],
			"body":{"copy":{"prompt_simple":"$req.reference_audio"}},
			"extract":{"result":"$.url"}}}}`)
	ep := a.byKey["audio/speech"]

	err := checkRequestFields(ep, map[string]any{"input": "hi"})
	var ce *relay.CallerParamError
	if !errors.As(err, &ce) {
		t.Fatalf("缺必填应返回 CallerParamError，实际 %T：%v", err, err)
	}
	if !strings.Contains(ce.Msg, "reference_audio") || !strings.Contains(ce.Msg, "参考音色的音频地址") {
		t.Fatalf("文案应带字段名与说明，实际 %q", ce.Msg)
	}
}

func TestRequestFieldTypeAndEnum(t *testing.T) {
	a := mustAdapter(t, `{"slug":"rf-2","label":"rf","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"request_fields":[
				{"name":"emo_random","type":"boolean"},
				{"name":"emo_control_method","type":"string","enum":["A","B"]}],
			"body":{"copy":{"a":"$req.emo_random","b":"$req.emo_control_method"}},
			"extract":{"result":"$.url"}}}}`)
	ep := a.byKey["audio/speech"]

	if err := checkRequestFields(ep, map[string]any{"emo_random": "true"}); err == nil {
		t.Fatal("字符串 \"true\" 不应被当成 boolean（隐式转换会让上游 400）")
	}
	if err := checkRequestFields(ep, map[string]any{"emo_control_method": "C"}); err == nil {
		t.Fatal("枚举外取值应被拒绝")
	}
	if err := checkRequestFields(ep, map[string]any{"emo_random": true, "emo_control_method": "A"}); err != nil {
		t.Fatalf("合法取值不应报错: %v", err)
	}
}

func TestRequestFieldDefaultsAndCopy(t *testing.T) {
	a := mustAdapter(t, `{"slug":"rf-3","label":"rf","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"request_fields":[{"name":"emo_random","type":"boolean","default":false}],
			"body":{"copy":{"emo_random":"$req.emo_random"}},
			"extract":{"result":"$.url"}}}}`)
	ep := a.byKey["audio/speech"]
	rq := &relay.RelayRequest{Body: json.RawMessage(`{"input":"hi"}`)}

	src, err := a.prepareRequest(ep, rq)
	if err != nil {
		t.Fatalf("预处理失败: %v", err)
	}
	body, err := a.buildBody(nil, ep, rq, src)
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if got["emo_random"] != false {
		t.Fatalf("默认值应被注入并发给上游，实际 %+v", got)
	}
}

func TestSpecRejectsRequestFieldNotConsumed(t *testing.T) {
	_, err := decodeJSON(t, `{"slug":"rf-4","label":"rf","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"request_fields":[{"name":"detail","type":"string"}],
			"body":{"const":{"a":1}},
			"extract":{"result":"$.url"}}}}`)
	if err == nil {
		t.Fatal("声明了却不会被发给上游，应当被拒绝")
	}
	if !strings.Contains(err.Error(), "不会发给上游") {
		t.Fatalf("文案应指明未被消费，实际: %v", err)
	}
}

func TestSpecRejectsRequestFieldCollidingWithContract(t *testing.T) {
	// input 是 /v1/audio/speech 的平台契约字段，扩展字段不能同名。
	_, err := decodeJSON(t, `{"slug":"rf-5","label":"rf","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"request_fields":[{"name":"input","type":"string"}],
			"body":{"copy":{"a":"$req.input"}},
			"extract":{"result":"$.url"}}}}`)
	if err == nil {
		t.Fatal("与平台契约字段同名应被拒绝")
	}
	if !strings.Contains(err.Error(), "契约字段同名") {
		t.Fatalf("文案应指明冲突，实际: %v", err)
	}
}

func TestSpecRejectsRequiredWithDefault(t *testing.T) {
	_, err := decodeJSON(t, `{"slug":"rf-6","label":"rf","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"request_fields":[{"name":"ref","type":"string","required":true,"default":"x"}],
			"body":{"copy":{"a":"$req.ref"}},
			"extract":{"result":"$.url"}}}}`)
	if err == nil {
		t.Fatal("必填 + 默认值应被拒绝（否则必填永不触发）")
	}
}

func TestSpecPathPlaceholderUsesUpstreamModel(t *testing.T) {
	a := mustAdapter(t, `{"slug":"ph-1","label":"ph","endpoints":{
		"audio/speech":{"path":"/wf/{upstream_model}","omit_model":true,
			"extract":{"result":"$.url"}}}}`)
	path, err := buildPath(a.byKey["audio/speech"].Path,
		map[string]string{"upstream_model": "indextts2-v1"},
		map[string]pathVarScope{"upstream_model": pathVarFromConfig})
	if err != nil {
		t.Fatalf("替换失败: %v", err)
	}
	if path != "/wf/indextts2-v1" {
		t.Fatalf("占位符未替换: %s", path)
	}
}

func TestSpecRejectsUndeclaredPlaceholder(t *testing.T) {
	_, err := decodeJSON(t, `{"slug":"ph-2","label":"ph","endpoints":{
		"audio/speech":{"path":"/wf/{workflow}","omit_model":true,
			"extract":{"result":"$.url"}}}}`)
	if err == nil {
		t.Fatal("未声明的占位符应被拒绝（运行期取值来源不明）")
	}
	if !strings.Contains(err.Error(), "未声明") {
		t.Fatalf("文案应指明未声明，实际: %v", err)
	}
}

func TestBuildPathRejectsIllegalValue(t *testing.T) {
	_, err := buildPath("/wf/{upstream_model}",
		map[string]string{"upstream_model": "../etc/passwd"},
		map[string]pathVarScope{"upstream_model": pathVarFromConfig})
	var ce *relay.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("来自绑定的非法值应是配置错（不记渠道健康），实际 %T", err)
	}

	_, err = buildPath("/wf/{wf}",
		map[string]string{"wf": "a/b"},
		map[string]pathVarScope{"wf": pathVarFromCaller})
	var pe *relay.CallerParamError
	if !errors.As(err, &pe) {
		t.Fatalf("来自调用方的非法值是用户错（400），实际 %T", err)
	}
}

func TestSelectURLsFiltersByType(t *testing.T) {
	raw := map[string]any{"data": map[string]any{"results": []any{
		map[string]any{"url": "https://x/cover.png", "type": "image"},
		map[string]any{"url": "https://x/a.wav", "type": "audio"},
	}}}
	got := selectURLs(raw, URLSelector{Path: "$.data.results[*].url", Where: map[string]string{"type": "audio"}})
	if len(got) != 1 || got[0] != "https://x/a.wav" {
		t.Fatalf("应只取音频产物，实际 %v", got)
	}
}

func TestSpecRejectsSelectorWhereWithoutWildcard(t *testing.T) {
	_, err := decodeJSON(t, `{"slug":"sel-1","label":"sel","endpoints":{
		"audio/speech":{"path":"/s","extract":{"task_id":"$.id"},
			"async":{"mode":"bridge","poll":"audio/speech@poll"}},
		"audio/speech@poll":{"path":"/r/{task_id}",
			"extract":{"status":"$.s","result_urls":{"path":"$.data.url","where":{"type":"audio"}}},
			"status_map":{"succeeded":["ok"],"failed":["bad"]}}}}`)
	if err == nil {
		t.Fatal("where 只能用于数组路径")
	}
}

func TestSecondsOfUnit(t *testing.T) {
	if got := secondsOf(float64(196), "secs"); got != 196 {
		t.Fatalf("秒单位不应换算，实际 %v", got)
	}
	if got := secondsOf(float64(196000), "ms"); got != 196 {
		t.Fatalf("毫秒应换算成秒，实际 %v", got)
	}
}

func TestSpecRejectsDurationUnitWithoutDuration(t *testing.T) {
	_, err := decodeJSON(t, `{"slug":"du-1","label":"du","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"extract":{"result":"$.url","duration_unit":"ms"}}}}`)
	if err == nil {
		t.Fatal("没有 duration 却写 duration_unit 是死开关，应被拒绝")
	}
}

// TestBridgeUnknownStatusFailsFast 锁死「状态词不认识不要空等」。
//
// 上游每 200ms 回一个没见过的状态；3 次之后必须报配置错，
// 而不是等 timeout_secs（真实配置里是 120–900 秒，上游成本已经付了）。
func TestBridgeUnknownStatusFailsFast(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_, _ = w.Write([]byte(`{"data":{"task_id":"t1"}}`))
		case strings.HasPrefix(r.URL.Path, "/r/"):
			_, _ = w.Write([]byte(`{"data":{"status":"WEIRD"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, `{"slug":"us-1","label":"us","endpoints":{
		"audio/speech":{"path":"/submit","omit_model":true,
			"extract":{"task_id":"$.data.task_id"},
			"async":{"mode":"bridge","poll":"audio/speech@poll","interval_ms":200,"timeout_secs":30}},
		"audio/speech@poll":{"path":"/r/{task_id}",
			"extract":{"status":"$.data.status","result":"$.data.url"},
			"status_map":{"succeeded":["ok"],"failed":["bad"]}}}}`)

	start := time.Now()
	_, err := a.Forward(context.Background(),
		&relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"},
		&relay.RelayRequest{Body: json.RawMessage(`{}`)}, "/v1/audio/speech")
	if err == nil {
		t.Fatal("不认识的状态应报配置错")
	}
	var ce *relay.ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("应是配置错（不记渠道健康），实际 %T：%v", err, err)
	}
	if !strings.Contains(err.Error(), "WEIRD") {
		t.Fatalf("报错要带上游原文，实际: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("应在几秒内失败，实际等了 %v", d)
	}
}

// TestBridgeCanceledIsFailure 锁死「取消不是成功」。
//
// canceled 若被当成"仍在运行"，桥接会一直等到超时；当成 succeeded 则会
// 给用户返回空产物并照常扣费。
func TestBridgeCanceledIsFailure(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/submit"):
			_, _ = w.Write([]byte(`{"data":{"task_id":"t1"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"status":"cancelled"}}`))
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, `{"slug":"cn-1","label":"cn","endpoints":{
		"audio/speech":{"path":"/submit","omit_model":true,
			"extract":{"task_id":"$.data.task_id"},
			"async":{"mode":"bridge","poll":"audio/speech@poll","interval_ms":200,"timeout_secs":30}},
		"audio/speech@poll":{"path":"/r/{task_id}",
			"extract":{"status":"$.data.status","result":"$.data.url"},
			"status_map":{"succeeded":["ok"],"failed":["bad"],"canceled":["CANCELLED"]}}}}`)

	_, err := a.Forward(context.Background(),
		&relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"},
		&relay.RelayRequest{Body: json.RawMessage(`{}`)}, "/v1/audio/speech")
	if err == nil {
		t.Fatal("取消应判失败")
	}
	if errors.As(err, new(*relay.CallerParamError)) {
		t.Fatalf("取消不是用户参数错: %v", err)
	}
}

// TestOkCodesAreCaseInsensitive 锁死「业务码比较只有一种语义」。
func TestOkCodesAreCaseInsensitive(t *testing.T) {
	a := mustAdapter(t, `{"slug":"oc-1","label":"oc","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"errors":{"success":"body_code","code_path":"$.code","ok_codes":["Success"]},
			"extract":{"result":"$.url"}}}}`)
	if err := a.checkSuccess(nil, a.byKey["audio/speech"], 200, []byte(`{"code":"success"}`)); err != nil {
		t.Fatalf("大小写不同不应判失败: %v", err)
	}
}

// TestUpstream4xxIsCallerError 锁死「用户参数写错要能看到上游原话」。
func TestUpstream4xxIsCallerError(t *testing.T) {
	a := mustAdapter(t, `{"slug":"u4-1","label":"u4","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,"extract":{"result":"$.url"}}}}`)
	cfg := &relay.ChannelConfig{BaseURL: "https://api.internal.example.com", APIKey: "sk-secret-123456"}
	err := a.checkSuccess(cfg, a.byKey["audio/speech"], 400,
		[]byte(`{"msg":"prompt_simple 不是有效音频地址","host":"api.internal.example.com","key":"sk-secret-123456"}`))
	var ce *relay.CallerParamError
	if !errors.As(err, &ce) {
		t.Fatalf("400 应是调用方参数错，实际 %T：%v", err, err)
	}
	if strings.Contains(ce.Msg, "sk-secret-123456") || strings.Contains(ce.Msg, "api.internal.example.com") {
		t.Fatalf("透传文案必须脱敏，实际 %q", ce.Msg)
	}
	if !strings.Contains(ce.Msg, "不是有效音频地址") {
		t.Fatalf("应保留上游原话，实际 %q", ce.Msg)
	}
}

// TestUpstream200BizFailureIsCallerError 锁死「HTTP 200 + 业务码失败」的归因。
//
// 线上症状（IndexTTS）：参考音频 URL 返回的 Content-Type 是 `audio/wave`，
// 上游白名单只收 `audio/mpeg`/`audio/wav`，于是 HTTP 200 + `code != Success` +
// msg 里写明「文件类型不正确」。旧实现把它当上游故障 → 对外 503
// 「服务暂时不可用，本次未扣费」——模型配置完全正确、上游也活着，用户却被
// 引导去"稍后重试"，永远查不出真实原因。
//
// 判据：HTTP 通了、业务说不行 = 上游拒绝了本次请求内容（调用方侧，400 且带原话）。
// 上游真故障会伴随 5xx/401，不会伪装成 200。
func TestUpstream200BizFailureIsCallerError(t *testing.T) {
	a := mustAdapter(t, `{"slug":"bc-200","label":"bc","endpoints":{
		"audio/speech":{"path":"/s","omit_model":true,
			"errors":{"success":"body_code","code_path":"$.code","ok_codes":["Success"],"message_path":"$.msg"},
			"extract":{"result":"$.url"}}}}`)
	cfg := &relay.ChannelConfig{BaseURL: "https://api.internal.example.com", APIKey: "sk-secret-123456"}
	err := a.checkSuccess(cfg, a.byKey["audio/speech"], 200, []byte(
		`{"code":"Error","msg":"URL地址文件类型不正确，当前类型为 audio/wave，允许的类型为 [audio/mpeg audio/wav]",`+
			`"host":"api.internal.example.com","key":"sk-secret-123456"}`))
	var ce *relay.CallerParamError
	if !errors.As(err, &ce) {
		t.Fatalf("200 + 业务码失败应是调用方参数错，实际 %T：%v", err, err)
	}
	if !strings.Contains(ce.Msg, "audio/wave") {
		t.Fatalf("必须带出上游原话（用户据此才能换文件），实际 %q", ce.Msg)
	}
	if strings.Contains(ce.Msg, "sk-secret-123456") || strings.Contains(ce.Msg, "api.internal.example.com") {
		t.Fatalf("透传文案必须脱敏，实际 %q", ce.Msg)
	}
	// 不能再说成「服务不可用」：那是运营侧的故障语义，会误导用户重试。
	if strings.Contains(ce.Msg, "服务") && strings.Contains(ce.Msg, "不可用") {
		t.Fatalf("文案不该暗示服务故障，实际 %q", ce.Msg)
	}
}

// TestForward200BizFailureIsCallerError 是上一条的端到端版本：不只看 checkSuccess
// 的返回值，而是走完整 Forward（桥接提交阶段），确认错误类型能穿到 relay 层
// ——那里才决定「400 + 原话」还是「503 服务不可用」。
func TestForward200BizFailureIsCallerError(t *testing.T) {
	t.Setenv("LD_UPSTREAM_ALLOW_PRIVATE", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"Error","msg":"URL地址文件类型不正确，当前类型为 audio/wave，` +
			`允许的类型为 [audio/mpeg audio/wav]"}`))
	}))
	defer srv.Close()

	a := mustAdapter(t, `{"slug":"biz-e2e","label":"biz","endpoints":{
		"audio/speech":{"path":"/submit","omit_model":true,
			"errors":{"success":"body_code","code_path":"$.code","ok_codes":["Success"],"message_path":"$.msg"},
			"extract":{"task_id":"$.data.task_id"},
			"async":{"mode":"bridge","poll":"audio/speech@poll","interval_ms":200,"timeout_secs":30}},
		"audio/speech@poll":{"path":"/r/{task_id}",
			"extract":{"status":"$.data.status","result":"$.data.url"},
			"status_map":{"succeeded":["SUCCESS"],"failed":["FAILED"]}}}}`)

	_, err := a.Forward(context.Background(),
		&relay.ChannelConfig{BaseURL: srv.URL, APIKey: "k"},
		&relay.RelayRequest{Body: json.RawMessage(`{}`)}, "/v1/audio/speech")
	var ce *relay.CallerParamError
	if !errors.As(err, &ce) {
		t.Fatalf("桥接提交阶段业务失败应是调用方参数错（不能被包成上游故障），实际 %T：%v", err, err)
	}
	if !strings.Contains(ce.Msg, "audio/wave") {
		t.Fatalf("应带出上游原话，实际 %q", ce.Msg)
	}
}
