package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// 本文件锁定「平台与上游的字段口径」：
//   - **平台映射写过的键以映射为准；没写的顶层键原样下发**（relay.ForwardUnmapped）。
//     「上游支不支持」不归平台判断 —— 平台替上游判能力，等于把上游新增的参数永久挡在门外；
//   - 仍保留的两类 400：结构前提（edits 必须给参考图、`data:` 内联无法直传）与
//     跨协议不可等价表达（`content`）；
//   - 自动时长不猜秒数（平台的归一值优先）。

// TestDuomiNanoForwardsQuality nano 的官方 schema 没有 quality（那是 gpt-image 的字段），
// 但「上游认不认」由上游回答：平台不再本地判死，原值下发。
//
// 这条是从「nano 收到 quality 就 400」改过来的：静默丢弃会让用户以为参数生效，
// 本地拒绝又等于平台替上游判能力 —— 两者都错，正确做法是把原话交给上游。
func TestDuomiNanoForwardsQuality(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"n1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	if _, _, err := submitDuomi(t, cfg, "nano-banana-2",
		`{"model":"nano-banana-2","prompt":"a cat","quality":"high"}`); err != nil {
		t.Fatalf("quality 应透传而不是 400：%v", err)
	}
	if got := rec.body["quality"]; got != "high" {
		t.Fatalf("quality 应原样下发，实际 %#v", got)
	}
}

// TestDuomiGPTImageQualityPassthrough quality 的官方 enum 是 low/medium/high（思考深度），
// 而 OpenAI 习惯传 hd/standard。平台不再就地拒绝：合法取值不掌握在我们手里，
// 原值下发、由上游回原话。
func TestDuomiGPTImageQualityPassthrough(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"g1"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)

	for _, v := range []string{"hd", "high"} {
		if _, _, err := submitDuomi(t, cfg, "gpt-image-2",
			`{"model":"gpt-image-2","prompt":"a cat","size":"1024x1024","quality":"`+v+`"}`); err != nil {
			t.Fatalf("quality=%s 应原样下发：%v", v, err)
		}
		if got := rec.body["quality"]; got != v {
			t.Fatalf("quality 应原样下发，实际 %#v", got)
		}
	}
}

// TestDuomiKlingImage2VideoIgnoresAspectRatio 图生视频的 schema 没有 aspect_ratio
// （画幅由参考图决定）。但重放体恒写它，所以**不能 400** —— 否则每次重试都失败。
//
// 忽略后**不落 Notes/anomaly**：Notes 在平台侧被硬编码成
// weak_param_pricing_conflict（「提示词里的计价参数与请求参数不一致」），
// 用它记录「画幅被忽略」会在账单上显示一条与事实无关的解释。
// 留痕由出站报文承担（入站带 aspect_ratio、出站不带，详情页两栏可直接对比）。
func TestDuomiKlingImage2VideoIgnoresAspectRatio(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"k-i2v"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	req := &relay.TaskSubmitRequest{
		ModelID: "kling-v3-std",
		Body: []byte(`{"model":"kling-v3-std","prompt":"镜头推进","aspect_ratio":"9:16",
			"input_reference":[{"type":"image","image_url":"https://cdn.example.com/1.png"}]}`),
	}
	if _, err := (DuoMi{}).SubmitTask(context.Background(), cfg, req); err != nil {
		t.Fatalf("图生视频带 aspect_ratio 不应失败：%v", err)
	}
	if _, has := rec.body["aspect_ratio"]; has {
		t.Fatalf("image2video 不该下发 aspect_ratio：%+v", rec.body)
	}
	if len(req.Notes) != 0 {
		t.Fatalf("不能借用 Notes 通道（会落 weak_param_pricing_conflict 误导账单）：%v", req.Notes)
	}
}

// TestDuomiKlingCameraOnlyOnText2Video 运镜只在文生视频的 schema 里。
//
// 文档（多米可灵 API）：text2video 有 camera_control 六轴对象；
// image2video 没有该字段 —— 静默下发会被上游当未知键丢掉，用户以为运镜生效。
func TestDuomiKlingCameraOnlyOnText2Video(t *testing.T) {
	srv, rec := startDuomi(t, func(w http.ResponseWriter, _ *http.Request) {
		duomiJSON(w, 200, `{"code":0,"data":{"task_id":"k-t2v"}}`)
	})
	cfg := duomiCfg(srv.URL, nil)
	// 入站是**平铺六轴**（对外契约），出站才包成 {type, config}
	camera := `"camera_control":{"horizontal":0,"vertical":0,"pan":0,"tilt":0,"roll":0,"zoom":3}`

	if _, _, err := submitDuomi(t, cfg, "kling-v3-std",
		`{"model":"kling-v3-std","prompt":"hi",`+camera+`}`); err != nil {
		t.Fatalf("文生视频应允许运镜：%v", err)
	}
	if rec.body["camera_control"] == nil {
		t.Fatal("文生视频必须下发 camera_control")
	}

	wantDuomiCaller(t, mustDuomiErr(t, cfg, "kling-v3-std",
		`{"model":"kling-v3-std","prompt":"hi",`+camera+`,
		 "input_reference":[{"type":"image","image_url":"https://cdn.example.com/1.png"}]}`),
		"文生视频")
}

// TestDuomiSecondsUsesPlatformValue 时长以平台归一值为准，body 只作兜底。
//
// 这条守的是「少收/多收」：duomi 不回报 usage，出站时长与请求时长不一致时，
// 账单完全看不出来。
func TestDuomiSecondsUsesPlatformValue(t *testing.T) {
	body := &duomiBody{
		Seconds:  json.RawMessage(`5`),
		Duration: json.RawMessage(`6`),
	}
	cases := []struct {
		name string
		req  *relay.TaskSubmitRequest
		want int64
	}{
		{"无 req 时读 body.seconds", nil, 5},
		{"秒数优先于 duration", &relay.TaskSubmitRequest{}, 5},
		{"平台归一值优先", &relay.TaskSubmitRequest{Seconds: 8}, 8},
		{"智能时长不下发", &relay.TaskSubmitRequest{AutoDuration: true, Seconds: 15}, 0},
		{"省略时长不下发", &relay.TaskSubmitRequest{OmitDuration: true, Seconds: 15}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := duomiSeconds(body, c.req); got != c.want {
				t.Fatalf("want %d, got %d", c.want, got)
			}
		})
	}

	// 只有 duration 的客户端（重放体的形态）：必须能读出来。
	onlyDuration := &duomiBody{Duration: json.RawMessage(`"7"`)}
	if got := duomiSeconds(onlyDuration, &relay.TaskSubmitRequest{}); got != 7 {
		t.Fatalf("duration 字符串应被识别，got %d", got)
	}
	// -1 是「自动」的标记，不能下发给不认它的上游。
	autoMark := &duomiBody{Duration: json.RawMessage(`-1`)}
	if got := duomiSeconds(autoMark, &relay.TaskSubmitRequest{}); got != 0 {
		t.Fatalf("duration=-1 应归零（不下发），got %d", got)
	}
}

// TestDuomiGPTImageSizeTranslatesTiers 锁定「平台档位词必须按官方表翻译成上游像素」。
//
// 线上实测（2026-11-04 10:00）：客户端传 size=2K，原样发给 gpt-image 换回
// `size must be widthxheight ... or one of: 1:1, 3:2, 2:3, auto`。
// 平台卖的就是 1K/2K/4K（tier_price 的键），不翻译等于这三个档位全不可用。
//
// **档位 × 画幅**两个维度都要对：只按档位取一个固定像素，会让「1:1 + 4K」拿到横图。
// 表值逐格来自官方《生成图片》的「比例参考（1K、2K、4K）」。
func TestDuomiGPTImageSizeTranslatesTiers(t *testing.T) {
	cases := []struct {
		name    string
		in      billing.ImageSize
		want    string
		wantErr bool
	}{
		{"1K 方图", billing.ImageSize{DeclaredTier: "1K", Raw: "1K"}, "1024x1024", false},
		{"2K 方图", billing.ImageSize{DeclaredTier: "2K", Raw: "2K"}, "2048x2048", false},
		// 4K 方图是 2880²（恰等于像素预算上限 8,294,400），不是 4096²、也不是 3840×2160。
		{"4K 方图", billing.ImageSize{DeclaredTier: "4K", Raw: "4K"}, "2880x2880", false},
		{"4K 横图", billing.ImageSize{DeclaredTier: "4K", Raw: "4K", Ratio: "16:9"}, "3840x2160", false},
		{"2K 竖图", billing.ImageSize{DeclaredTier: "2K", Raw: "2K", Ratio: "3:4"}, "1728x2304", false},
		{"1K 宽幅", billing.ImageSize{DeclaredTier: "1K", Raw: "1K", Ratio: "21:9"}, "1456x624", false},
		{"小写档位归一后同样翻译", billing.ImageSize{DeclaredTier: "2K", Raw: "2k"}, "2048x2048", false},
		// 1:3 / 3:1 官方只给两列：2K 与 4K 能翻译，1K 必须明确拒绝。
		{"1:3 的 2K", billing.ImageSize{DeclaredTier: "2K", Raw: "2K", Ratio: "1:3"}, "688x2048", false},
		{"3:1 的 4K", billing.ImageSize{DeclaredTier: "4K", Raw: "4K", Ratio: "3:1"}, "3840x1280", false},
		{"1:3 没有 1K 档", billing.ImageSize{DeclaredTier: "1K", Raw: "1K", Ratio: "1:3"}, "", true},
		// 未收录的档位词（运营自定义 1.5K/3K）上游没有对应像素：拒绝并说清可用档位，
		// 不把 "1.5K" 原样发出去（那必然换回一句上游 size 报错，用户看不到真实原因）。
		{"未收录档位拒绝", billing.ImageSize{DeclaredTier: "1.5K", Raw: "1.5K"}, "", true},
		// 画幅不在表里：拒绝并列出可用画幅。
		{"未收录画幅拒绝", billing.ImageSize{DeclaredTier: "2K", Raw: "2K", Ratio: "7:3"}, "", true},
		{"像素写法原样透传", billing.ImageSize{Raw: "2048x1152"}, "2048x1152", false},
		{"比例写法原样透传", billing.ImageSize{Ratio: "16:9", Raw: "16:9"}, "16:9", false},
		{"auto 原样透传", billing.ImageSize{Auto: true, Raw: "auto"}, "auto", false},
		{"未声明尺寸", billing.ImageSize{}, "", false},
	}
	for _, c := range cases {
		got, err := duomiGPTImageSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s: 期望拒绝，实际返回 %q", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: 意外报错 %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
