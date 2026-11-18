package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// TestMetasoErrorEnvelopeTolerance 锁死错误信封的宽容度（P0-16）。
//
// 上游把 `error` 写成字符串 / `{code,message}` / `{message}` 都出现过，
// `http_code` 也既可能是数字也可能是数字字符串。这里用 string/int 接的后果
// 不是「这条字段读不到」，而是**整个结构体解析失败** → 信封识别静默失效 →
// 一条 401 被当成「HTTP 200 但缺少 task_id」→ 归因成上游提交失败并 failover，
// 把所有渠道挨个撞一遍（每次都是一次真实出站）。
func TestMetasoErrorEnvelopeTolerance(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantErr  bool
		wantCode int
		contains string
	}{
		{
			name: "type=error + 数字 http_code",
			body: `{"type":"error","http_code":401,"message":"invalid api key"}`,
			// 401/402/429 绝不能被降级成参数错：鉴权失败对调用方毫无指导意义，
			// 还会让整条渠道继续留在候选里（每次请求先撞一次）。
			wantErr: true, wantCode: 401, contains: "invalid api key",
		},
		{
			name:     "error 是字符串",
			body:     `{"error":"quota exceeded","http_code":402}`,
			wantErr:  true,
			wantCode: 402,
			contains: "quota exceeded",
		},
		{
			name:     "error 是对象（code+message）",
			body:     `{"error":{"code":"rate_limited","message":"too many requests"},"http_code":"429"}`,
			wantErr:  true,
			wantCode: 429,
			contains: "too many requests",
		},
		{
			name:     "http_code 是对象",
			body:     `{"type":"error","error":{"message":"boom"},"http_code":{"code":503}}`,
			wantErr:  true,
			wantCode: 503,
			contains: "boom",
		},
		{
			name:     "error 是数组",
			body:     `{"error":[{"message":"first"},{"message":"second"}],"http_code":400}`,
			wantErr:  true,
			wantCode: 400,
			contains: "first; second",
		},
		{
			name:     "base_resp 非零",
			body:     `{"base_resp":{"status_code":1001,"status_msg":"内容审核未通过"}}`,
			wantErr:  true,
			wantCode: 0, // 1001 不是 HTTP 状态码 → 由调用方按 400 处理
			contains: "内容审核未通过",
		},
		{
			name:     "只有 http_code 没有别的说明",
			body:     `{"type":"error","http_code":500}`,
			wantErr:  true,
			wantCode: 500,
			contains: "业务错误",
		},
		{
			name:    "正常成功响应不算信封",
			body:    `{"task_id":"abc","status":"running"}`,
			wantErr: false,
		},
		{
			name:    "空 error 与空 base_resp 不算信封",
			body:    `{"task":{"id":"t1"},"error":null,"base_resp":{"status_code":0}}`,
			wantErr: false,
		},
		{
			name:    "error 是空串不算信封",
			body:    `{"error":""}`,
			wantErr: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg, isErr := metasoErrorEnvelope([]byte(c.body))
			if isErr != c.wantErr {
				t.Fatalf("isErr=%v，期望 %v（判错会让 401 变成「缺少 task_id」并触发无意义的 failover）", isErr, c.wantErr)
			}
			if !c.wantErr {
				return
			}
			if code != c.wantCode {
				t.Fatalf("code=%d，期望 %d", code, c.wantCode)
			}
			if c.contains != "" && !strings.Contains(msg, c.contains) {
				t.Fatalf("文案应包含 %q，实际 %q", c.contains, msg)
			}
		})
	}
}

// TestMetasoEnvelopeMapsToHTTPClass 验证信封里的状态码真的会变成对应的归因分档。
func TestMetasoEnvelopeMapsToHTTPClass(t *testing.T) {
	// 401 必须保持 401（不是 400）：它决定「只冷却密钥」还是「调用方参数错」。
	err := metasoEnvelopeError(401, "invalid api key")
	if got := relay.StatusOf(err); got != 401 {
		t.Fatalf("401 信封应保留状态码，实际 %d（错误 %v）", got, err)
	}
	// 缺状态码按 400（参数错）处理：宁可让调用方改参数，也不要误熔断渠道。
	// 400 在 metasoHTTPError 里映射成 CallerParamError（不带状态码），
	// 所以这里断言的是「归因类别」而不是数字码。
	err = metasoEnvelopeError(0, "bad prompt")
	if !errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("无状态码应回落参数错，实际 %v", err)
	}
	// 小于 400 的怪码同样回落参数错。
	err = metasoEnvelopeError(200, "weird")
	if !errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("非错误码应回落参数错，实际 %v", err)
	}
}

// TestMetasoRatioAlias 验证 ratio 与 aspect_ratio 两种写法都能生效（P1-21）。
func TestMetasoRatioAlias(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"ok-1"}`)
	})
	cfg := metasoCfg(srv.URL, nil)

	// ratio 优先
	if _, err := submitMetaso(t, cfg, `{"prompt":"p","resolution":"720p","seconds":5,"ratio":"9:16","aspect_ratio":"1:1"}`); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if got := rec.body["ratio"]; got != "9:16" {
		t.Fatalf("ratio 应优先于 aspect_ratio，实际 %v", got)
	}
	// 只给 aspect_ratio
	if _, err := submitMetaso(t, cfg, `{"prompt":"p","resolution":"720p","seconds":5,"aspect_ratio":"4:3"}`); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if got := rec.body["ratio"]; got != "4:3" {
		t.Fatalf("aspect_ratio 应作为 ratio 的别名生效，实际 %v", got)
	}
	// 都不给 → 16:9（**文生视频不能 adaptive**：上游直接 400「ratio 不能为空或 adaptive」）
	if _, err := submitMetaso(t, cfg, `{"prompt":"p","resolution":"720p","seconds":5}`); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if got := rec.body["ratio"]; got != metasoDefaultTextRatio {
		t.Fatalf("文生视频缺省应为 %s，实际 %v", metasoDefaultTextRatio, got)
	}
	// 非法值拒绝
	wantMetasoCaller(t, mustMetasoErr(t, cfg, `{"prompt":"p","resolution":"720p","seconds":5,"ratio":"5:4"}`), "画幅比例")
}

func mustMetasoErr(t *testing.T, cfg *relay.ChannelConfig, body string) error {
	t.Helper()
	_, err := submitMetaso(t, cfg, body)
	return err
}

// TestMetasoUsageParsing 验证上游用量被解析进 TaskStatus.Usage（P1-22）。
//
// 这是结算准确性的入口：解析不到就只能按请求参数计费，
// 而「上游实际跑了多少秒」与「用户请求多少秒」在按档位取整时会差一整个档。
func TestMetasoUsageParsing(t *testing.T) {
	cases := []struct {
		name string
		body string
		// want 只比对平台认识的四个数值字段（Raw 是 []byte，结构体不可比较）。
		want   relay.TaskUsage
		status string
		// hasUsage=false 的用例必须一个字节都不留原文：
		// 空 usage 落成 `{}` 会让核账的人以为上游报了空对象。
		hasUsage bool
	}{
		{
			// total_seconds 是「输出 + 输入」的合计：这里 8 = 输出 6 + 参考视频 2。
			// 真机实测（4 秒成片 + 4 秒参考视频）上游返回 total=8/input=4，
			// 若把 total 当输出，会连同 ref_video_second_price 一起收，参考视频被收两次。
			name:     "标准三字段（数字，total 含输入）",
			body:     `{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"},"usage":{"total_seconds":8,"input_seconds":2,"input_image_count":3}}}`,
			status:   "succeeded",
			want:     relay.TaskUsage{OutputSeconds: 6, InputSeconds: 2, ImageCount: 3},
			hasUsage: true,
		},
		{
			// 真机原始回执（参考视频 4 秒 + 成片 4 秒）：上游给的 total_seconds 是 8，
			// 而其中 4 秒是参考视频。平台按 total-input 得到纯输出 4 秒，
			// 参考视频那 4 秒由 ref_video_second_price 单独计——两条口径不能重叠。
			name:     "参考视频真机回执",
			body:     `{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"},"usage":{"total_seconds":8,"input_seconds":4,"output_seconds":4,"raw":{"total_seconds":8,"input_seconds":4,"output_seconds":4}}}}`,
			status:   "succeeded",
			want:     relay.TaskUsage{OutputSeconds: 4, InputSeconds: 4},
			hasUsage: true,
		},
		{
			name:     "数字字符串",
			body:     `{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"},"usage":{"total_seconds":"12","input_seconds":"0","input_image_count":"1"}}}`,
			status:   "succeeded",
			want:     relay.TaskUsage{OutputSeconds: 12, ImageCount: 1},
			hasUsage: true,
		},
		{
			name:     "小数秒",
			body:     `{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"},"usage":{"total_seconds":7.5}}}`,
			status:   "succeeded",
			want:     relay.TaskUsage{OutputSeconds: 7.5},
			hasUsage: true,
		},
		{
			name:     "没有 usage",
			body:     `{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"}}}`,
			status:   "succeeded",
			want:     relay.TaskUsage{},
			hasUsage: false,
		},
		{
			name:     "失败态也带用量（上游跑过就可能计过费）",
			body:     `{"task":{"status":"failed","error":{"message":"x"},"usage":{"total_seconds":4}}}`,
			status:   "failed",
			want:     relay.TaskUsage{OutputSeconds: 4},
			hasUsage: true,
		},
		{
			name: "上游多给的字段必须原样带走（B7-3）",
			body: `{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"},` +
				`"usage":{"total_seconds":6,"billing_unit":"second","vendor_ext":{"k":1}}}}`,
			status: "succeeded",
			want:   relay.TaskUsage{OutputSeconds: 6},
			// Raw 里含未被解析的 billing_unit / vendor_ext —— 它们只可能靠原文留住。
			hasUsage: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, err := parseMetasoTask([]byte(c.body))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if st.Status != c.status {
				t.Fatalf("status=%q，期望 %q", st.Status, c.status)
			}
			got := st.Usage
			if got.OutputSeconds != c.want.OutputSeconds ||
				got.InputSeconds != c.want.InputSeconds ||
				got.ImageCount != c.want.ImageCount ||
				got.AudioSeconds != c.want.AudioSeconds {
				t.Fatalf("usage=%+v，期望 %+v", got, c.want)
			}
			if !c.hasUsage {
				if len(got.Raw) != 0 {
					t.Fatalf("没有 usage 时必须留空，实际 %s", got.Raw)
				}
				return
			}
			if !json.Valid(got.Raw) || len(got.Raw) == 0 {
				t.Fatalf("usage 原文必须完整留存，实际 %q", string(got.Raw))
			}
			if !strings.Contains(string(got.Raw), "total_seconds") {
				t.Fatalf("原文丢字段：%s", got.Raw)
			}
		})
	}
	// 额外锁死：上游多给的键必须原样在原文里（这是本改造的全部意义）。
	st, err := parseMetasoTask([]byte(`{"task":{"status":"succeeded","content":{"url":"https://cdn/v.mp4"},` +
		`"usage":{"total_seconds":6,"billing_unit":"second"}}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(string(st.Usage.Raw), "billing_unit") {
		t.Fatalf("未被解析的字段必须留在原文里，实际 %s", st.Usage.Raw)
	}
}

// TestMetasoRegenKeepsFrames 验证再生成走 base_video 时**不丢**首尾帧与参考素材（P1-23/24）。
//
// 改造前这里手写「text + base_video」两条，于是请求里带的 first_frame / reference_image
// 全部被静默丢弃：用户按「带素材」的价目付钱，上游却只收到一段文本 + 一条底片。
func TestMetasoRegenKeepsFrames(t *testing.T) {
	srv, rec, _ := startMetaso(t, func(w http.ResponseWriter, _ *http.Request) {
		metasoJSON(w, 200, `{"task_id":"regen-2"}`)
	})
	cfg := metasoCfg(srv.URL, nil)
	body := `{"resolution":"2k","prompt":"加点雨","input_reference":[
		{"type":"base_video","video_url":"https://cdn/base.mp4"},
		{"type":"first_frame","image_url":"https://cdn/f.png"},
		{"type":"last_frame","image_url":"https://cdn/l.png"},
		{"type":"reference_image","image_url":"https://cdn/r1.png"}]}`
	if _, err := (Metaso{}).SubmitTask(context.Background(), cfg, &relay.TaskSubmitRequest{
		Kind: relay.TaskKindRegeneration, Body: []byte(body),
	}); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	roles := metasoContentRoles(t, rec.body)
	for _, want := range []string{"first_frame", "last_frame", "base_video", "reference_image"} {
		if !roles[want] {
			t.Fatalf("再生成 content 丢了 %s：%v", want, rec.body["content"])
		}
	}
}

// metasoContentRoles 提取 content 数组里出现过的 role 集合（text 记为 "text"）。
func metasoContentRoles(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	raw, _ := body["content"]
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("content 不是数组：%v", raw)
	}
	for _, it := range arr {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := obj["role"].(string); ok && r != "" {
			out[r] = true
			continue
		}
		if tp, ok := obj["type"].(string); ok {
			out[tp] = true
		}
	}
	return out
}

// TestMetasoContentOmitsEmptyText 验证空 prompt 不下发 text 项。
//
// `{"type":"text","text":""}` 在部分上游实现里会被当成「有一条空指令」而拒绝，
// 而再生成本来就不需要 prompt —— 下发空文本等于给再生成加了一道隐形门槛。
func TestMetasoContentOmitsEmptyText(t *testing.T) {
	refs := &metasoRefs{baseVideo: "https://cdn/base.mp4"}
	content, err := metasoContentOf("   ", refs)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	for _, c := range content {
		if c.Type == "text" {
			t.Fatalf("空 prompt 不应下发 text 项：%+v", content)
		}
	}
	if len(content) != 1 || content[0].Role != "base_video" {
		t.Fatalf("应只有 base_video 一项，实际 %+v", content)
	}
}

// TestMetasoParseFloatJSON 是宽容解析的边界（上游字段类型不稳定）。
func TestMetasoParseFloatJSON(t *testing.T) {
	cases := map[string]float64{
		`8`:     8,
		`"8"`:   8,
		`7.5`:   7.5,
		`"7.5"`: 7.5,
		`null`:  0,
		`""`:    0,
		`"abc"`: 0,
		``:      0,
		`true`:  0,
	}
	for in, want := range cases {
		if got := parseFloatJSON(json.RawMessage(in)); got != want {
			t.Fatalf("parseFloatJSON(%s)=%v，期望 %v", in, got, want)
		}
	}
}
