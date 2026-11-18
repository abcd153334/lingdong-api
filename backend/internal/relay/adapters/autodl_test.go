package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingdong-api/internal/relay"
)

// autodlRecord 记录一次上游请求，供断言 URL / 鉴权头 / 请求体。
type autodlRecord struct {
	path  string
	auth  string
	ctype string
	body  map[string]any
	raw   string
}

// newAutoDLServer 起一个假上游：每次请求都记入 rec，响应由 reply 决定。
func newAutoDLServer(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *autodlRecord, *atomic.Int32) {
	t.Helper()
	rec := &autodlRecord{}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		rec.path = r.URL.Path
		rec.auth = r.Header.Get("Authorization")
		rec.ctype = r.Header.Get("Content-Type")
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec.raw = string(buf)
		_ = json.Unmarshal(buf, &rec.body)
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec, &hits
}

// replyOK 是「提交/查询都成功」的通用响应。
func replyOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"code":"Success","data":{"task_id":"up-1","status":"QUEUED"},"msg":""}`))
}

func autoDLCfg(base string, opts []byte) *relay.ChannelConfig {
	return &relay.ChannelConfig{BaseURL: base, APIKey: "tok-123", Timeout: 5 * time.Second, Options: opts}
}

func submitAutoDL(t *testing.T, base, body string) (string, error) {
	t.Helper()
	return AutoDL{}.SubmitTask(context.Background(), autoDLCfg(base, nil), &relay.TaskSubmitRequest{Body: []byte(body)})
}

func wantCaller(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望参数错误，实际成功")
	}
	if !errors.Is(err, relay.ErrCallerParam) {
		t.Fatalf("期望 ErrCallerParam，实际 %v", err)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("错误文案应包含 %q，实际 %q", contains, err.Error())
	}
}

// ---------------------------------------------------------------- 模式识别

func TestAutoDLModeRouting(t *testing.T) {
	const (
		wfText  = "minimax_h3_lightx2v_no_pic"
		wfImage = "minimax_h3_lightx2v_v5_15s"
		wfMulti = "minimax_h3_zm_u24"
		wfFL    = "minimax_h3_lightx2v"
	)
	cases := []struct {
		name     string
		body     string
		workflow string
		check    func(t *testing.T, b map[string]any)
	}{
		{
			name:     "无素材走文生",
			body:     `{"prompt":"猫","seconds":5,"resolution":"480p"}`,
			workflow: wfText,
			check: func(t *testing.T, b map[string]any) {
				if b["duration"].(float64) != 5 || b["resolution"].(string) != "480p横" {
					t.Fatalf("文生体不对：%v", b)
				}
				if _, ok := b["ref_image_0"]; ok {
					t.Fatalf("文生不应带 ref_image")
				}
			},
		},
		{
			name:     "单图走图片参考",
			body:     `{"prompt":"猫","seconds":5,"resolution":"480p","input_reference":[{"type":"image","image_url":"https://cdn/1.png"}]}`,
			workflow: wfImage,
			check: func(t *testing.T, b map[string]any) {
				if b["ref_image_0"] != "https://cdn/1.png" {
					t.Fatalf("ref_image_0 不对：%v", b)
				}
			},
		},
		{
			name:     "九图顺序保持",
			body:     nineImagesBody(),
			workflow: wfImage,
			check: func(t *testing.T, b map[string]any) {
				for i := 0; i < 9; i++ {
					key := "ref_image_" + string(rune('0'+i))
					want := "https://cdn/" + string(rune('0'+i)) + ".png"
					if b[key] != want {
						t.Fatalf("%s 期望 %s，实际 %v", key, want, b[key])
					}
				}
			},
		},
		{
			name:     "图+音走多模态",
			body:     `{"prompt":"猫","seconds":5,"resolution":"480p","input_reference":[{"type":"image","image_url":"https://cdn/1.png"},{"type":"audio","audio_url":"https://cdn/a.mp3"}]}`,
			workflow: wfMulti,
			check: func(t *testing.T, b map[string]any) {
				if b["ref_image_0"] != "https://cdn/1.png" || b["ref_audio_0"] != "https://cdn/a.mp3" {
					t.Fatalf("多模态体不对：%v", b)
				}
			},
		},
		{
			name:     "首尾帧走首尾帧工作流",
			body:     `{"prompt":"猫","seconds":5,"resolution":"480p","input_reference":[{"type":"first_frame","image_url":"https://cdn/f.png"},{"type":"last_frame","image_url":"https://cdn/l.png"}]}`,
			workflow: wfFL,
			check: func(t *testing.T, b map[string]any) {
				if b["first_frame"] != "https://cdn/f.png" || b["last_frame"] != "https://cdn/l.png" {
					t.Fatalf("首尾帧体不对：%v", b)
				}
			},
		},
		{
			name:     "first-frame 连字符别名归一",
			body:     `{"prompt":"猫","seconds":5,"resolution":"480p","input_reference":[{"type":"first-frame","image_url":"https://cdn/f.png"},{"type":"last-frame","image_url":"https://cdn/l.png"}]}`,
			workflow: wfFL,
		},
		{
			name:     "type 缺省按链接推断",
			body:     `{"prompt":"猫","seconds":5,"resolution":"480p","input_reference":[{"image_url":"https://cdn/1.png"}]}`,
			workflow: wfImage,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, rec, _ := newAutoDLServer(t, replyOK)
			id, err := submitAutoDL(t, srv.URL, c.body)
			if err != nil {
				t.Fatalf("提交失败：%v", err)
			}
			if id != "up-1" {
				t.Fatalf("task_id 期望 up-1，实际 %q", id)
			}
			if !strings.HasSuffix(rec.path, "/"+c.workflow) {
				t.Fatalf("workflow 路径期望以 /%s 结尾，实际 %s", c.workflow, rec.path)
			}
			if c.check != nil {
				c.check(t, rec.body)
			}
		})
	}
}

func nineImagesBody() string {
	var sb strings.Builder
	sb.WriteString(`{"prompt":"猫","seconds":5,"resolution":"480p","input_reference":[`)
	for i := 0; i < 9; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"type":"image","image_url":"https://cdn/` + string(rune('0'+i)) + `.png"}`)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// TestAutoDLModeRejects 覆盖所有「不报错就会跑错模式」的素材组合，并断言零出站请求。
func TestAutoDLModeRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"十张图", tenImagesBody(), "参考图过多"},
		{"只有音频", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"audio","audio_url":"https://cdn/a.mp3"}]}`, "至少需要 1 张参考图"},
		{"四个音频", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"image","image_url":"https://cdn/1.png"},{"type":"audio","audio_url":"https://cdn/a.mp3"},{"type":"audio","audio_url":"https://cdn/b.mp3"},{"type":"audio","audio_url":"https://cdn/c.mp3"},{"type":"audio","audio_url":"https://cdn/d.mp3"}]}`, "音频过多"},
		{"仅首帧", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"first_frame","image_url":"https://cdn/f.png"}]}`, "必须同时提供"},
		{"首尾帧加参考图", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"first_frame","image_url":"https://cdn/f.png"},{"type":"last_frame","image_url":"https://cdn/l.png"},{"type":"image","image_url":"https://cdn/x.png"}]}`, "不能再附加参考图"},
		{"首尾帧加音频", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"first_frame","image_url":"https://cdn/f.png"},{"type":"last_frame","image_url":"https://cdn/l.png"},{"type":"audio","audio_url":"https://cdn/a.mp3"}]}`, "不支持音频"},
		{"视频素材", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"video","video_url":"https://cdn/v.mp4"}]}`, "不支持视频参考素材"},
		{"拼错类型", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"firstframe","image_url":"https://cdn/f.png"}]}`, "不支持的素材类型"},
		{"重复首帧", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"first_frame","image_url":"https://cdn/f1.png"},{"type":"first_frame","image_url":"https://cdn/f2.png"},{"type":"last_frame","image_url":"https://cdn/l.png"}]}`, "首帧只能有 1 个"},
		{"内联素材", `{"prompt":"p","seconds":5,"resolution":"480p","input_reference":[{"type":"image","image_url":"data:image/png;base64,AAAA"}]}`, "/v1/files"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, hits := newAutoDLServer(t, replyOK)
			_, err := submitAutoDL(t, srv.URL, c.body)
			wantCaller(t, err, c.want)
			if n := hits.Load(); n != 0 {
				t.Fatalf("参数错不应发出任何上游请求，实际 %d 次", n)
			}
		})
	}
}

func tenImagesBody() string {
	var sb strings.Builder
	sb.WriteString(`{"prompt":"猫","seconds":5,"resolution":"480p","input_reference":[`)
	for i := 0; i < 10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"type":"image","image_url":"https://cdn/x.png"}`)
	}
	sb.WriteString(`]}`)
	return sb.String()
}

// ---------------------------------------------------------------- 参数翻译

func TestAutoDLResolutionAndOrientation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// 缺省横向：与文档承诺的 16:9 一致（旧行为是竖屏，真机 D1 出过 480x864）。
		{"480p 缺省横", `{"prompt":"p","seconds":5,"resolution":"480p"}`, "480p横"},
		{"720p 映射到 768p 横", `{"prompt":"p","seconds":5,"resolution":"720p"}`, "768p横"},
		// 上游档位名 768p 必须吸附到平台 720p 档（真机 C1：原来直接 400）。
		{"768p 吸附到 720p", `{"prompt":"p","seconds":5,"resolution":"768p"}`, "768p横"},
		{"768P 大写同样吸附", `{"prompt":"p","seconds":5,"resolution":"768P"}`, "768p横"},
		{"resolution 宽高判横", `{"prompt":"p","seconds":5,"resolution":"1280x720"}`, "768p横"},
		{"size 宽高判竖", `{"prompt":"p","seconds":5,"size":"720x1280"}`, "768p竖"},
		{"方形", `{"prompt":"p","seconds":5,"resolution":"720p","aspect_ratio":"1:1"}`, "768p(1:1)"},
		{"orientation 兜底", `{"prompt":"p","seconds":5,"resolution":"720p","metadata":{"orientation":"landscape"}}`, "768p横"},
		{"显式宽高优先于 orientation", `{"prompt":"p","seconds":5,"resolution":"720x1280","metadata":{"orientation":"landscape"}}`, "768p竖"},
		{"aspect_ratio 兜底", `{"prompt":"p","seconds":5,"resolution":"720p","aspect_ratio":"16:9"}`, "768p横"},
		{"480p 横", `{"prompt":"p","seconds":5,"resolution":"854x480"}`, "480p横"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, rec, _ := newAutoDLServer(t, replyOK)
			if _, err := submitAutoDL(t, srv.URL, c.body); err != nil {
				t.Fatalf("提交失败：%v", err)
			}
			if got := rec.body["resolution"]; got != c.want {
				t.Fatalf("resolution 期望 %q，实际 %v", c.want, got)
			}
		})
	}
}

func TestAutoDLRejects1030AndBadParams(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"1080p 不支持", `{"prompt":"p","seconds":5,"resolution":"1080p"}`, "不支持 1080p"},
		{"1080x1920 归 1080p 后拒绝", `{"prompt":"p","seconds":5,"resolution":"1080x1920"}`, "不支持 1080p"},
		{"未知档位", `{"prompt":"p","seconds":5,"resolution":"4k"}`, "不支持的分辨率"},
		{"时长为 0", `{"prompt":"p","seconds":0,"resolution":"480p"}`, "时长需为"},
		{"时长超限", `{"prompt":"p","seconds":16,"resolution":"480p"}`, "时长需为"},
		{"时长非整数", `{"prompt":"p","seconds":5.5,"resolution":"480p"}`, "整数秒"},
		{"缺少时长", `{"prompt":"p","resolution":"480p"}`, "缺少 seconds"},
		{"prompt 为空", `{"prompt":"  ","seconds":5,"resolution":"480p"}`, "prompt 不能为空"},
		{"prompt 超长", `{"prompt":"` + strings.Repeat("字", 20001) + `","seconds":5,"resolution":"480p"}`, "prompt 过长"},
		{"朝向非法", `{"prompt":"p","seconds":5,"resolution":"480p","metadata":{"orientation":"wide"}}`, "orientation"},
		{"aspect_ratio 非法", `{"prompt":"p","seconds":5,"resolution":"480p","aspect_ratio":"wide"}`, "aspect_ratio"},
		{"seed 非整数", `{"prompt":"p","seconds":5,"resolution":"480p","seed":1.5}`, "seed 必须是整数"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, hits := newAutoDLServer(t, replyOK)
			_, err := submitAutoDL(t, srv.URL, c.body)
			wantCaller(t, err, c.want)
			if n := hits.Load(); n != 0 {
				t.Fatalf("参数错不应发出上游请求，实际 %d 次", n)
			}
		})
	}
}

func TestAutoDLDurationBoundaries(t *testing.T) {
	for _, secs := range []string{"1", "15", `"5"`, "5.0"} {
		srv, rec, _ := newAutoDLServer(t, replyOK)
		body := `{"prompt":"p","seconds":` + secs + `,"resolution":"480p"}`
		if _, err := submitAutoDL(t, srv.URL, body); err != nil {
			t.Fatalf("seconds=%s 应通过，实际 %v", secs, err)
		}
		if _, ok := rec.body["duration"].(float64); !ok {
			t.Fatalf("duration 应为数字，实际 %T", rec.body["duration"])
		}
	}
}

// TestAutoDLMetadataCannotOverride 锁定「metadata 是白名单、不是透传」（P2-04）。
//
// AutoDL 是 workflow 网关：请求体里的任意键都可能是该 workflow 的参数名（模型、步数…）。
// 改造前这里会把未知键原样塞进上游请求体，等于让调用方靠 metadata 直接改上游行为与成本。
// 现在只有 seed / orientation 放行，其余一律 400 且零出站。
func TestAutoDLMetadataCannotOverride(t *testing.T) {
	srv, _, hits := newAutoDLServer(t, replyOK)
	body := `{"prompt":"真的","seconds":5,"resolution":"480p","metadata":{"prompt":"假的","duration":99,"ref_image_0":"https://evil/x.png","first_frame":"https://evil/f.png","negative_prompt":"模糊"}}`
	_, err := submitAutoDL(t, srv.URL, body)
	wantCaller(t, err, "metadata 只支持")
	// 文案必须把被拒的键列清楚（且稳定排序），便于运营一次改完。
	for _, k := range []string{"prompt", "duration", "ref_image_0", "first_frame", "negative_prompt"} {
		if !strings.Contains(err.Error(), k) {
			t.Fatalf("错误文案应列出被拒的键 %s，实际 %v", k, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("参数错必须零出站，实际 %d 次", n)
	}

	// 允许的键：seed 与 orientation 都放行，且 orientation 不下发上游。
	srv2, rec2, _ := newAutoDLServer(t, replyOK)
	if _, err := submitAutoDL(t, srv2.URL,
		`{"prompt":"p","seconds":5,"resolution":"480p","metadata":{"seed":42,"orientation":"landscape"}}`); err != nil {
		t.Fatalf("seed/orientation 应放行：%v", err)
	}
	if rec2.body["seed"] != float64(42) {
		t.Fatalf("seed 应下发，实际 %v", rec2.body["seed"])
	}
	if _, ok := rec2.body["orientation"]; ok {
		t.Fatalf("orientation 是平台侧提示，不下发上游：%v", rec2.body)
	}
}

func TestAutoDLSeedPrecedence(t *testing.T) {
	srv, rec, _ := newAutoDLServer(t, replyOK)
	if _, err := submitAutoDL(t, srv.URL, `{"prompt":"p","seconds":5,"resolution":"480p","seed":42,"metadata":{"seed":7}}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.body["seed"].(float64) != 42 {
		t.Fatalf("顶层 seed 应优先，实际 %v", rec.body["seed"])
	}
	srv2, rec2, _ := newAutoDLServer(t, replyOK)
	if _, err := submitAutoDL(t, srv2.URL, `{"prompt":"p","seconds":5,"resolution":"480p","metadata":{"seed":7}}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec2.body["seed"].(float64) != 7 {
		t.Fatalf("metadata.seed 应生效，实际 %v", rec2.body["seed"])
	}
}

// ---------------------------------------------------------------- 协议与错误分档

func TestAutoDLBareTokenAndProtocol(t *testing.T) {
	srv, rec, _ := newAutoDLServer(t, replyOK)
	if _, err := submitAutoDL(t, srv.URL, `{"prompt":"p","seconds":5,"resolution":"480p"}`); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if rec.auth != "tok-123" {
		t.Fatalf("鉴权头必须是裸 token，实际 %q", rec.auth)
	}
	if !strings.HasPrefix(rec.ctype, "application/json") {
		t.Fatalf("Content-Type 不对：%q", rec.ctype)
	}
	if rec.path != "/api/v1/comfyui/comfyui_workflow/minimax_h3_lightx2v_no_pic" {
		t.Fatalf("提交路径不对：%s", rec.path)
	}
	if _, ok := rec.body["model"]; ok {
		t.Fatalf("不应下发 model 字段")
	}
}

func TestAutoDLSubmitErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		resp       string
		wantCaller bool
		wantSub    string
	}{
		{"400 归调用方", 400, `{"code":"Fail","msg":"bad prompt"}`, true, "拒绝了本次参数"},
		{"422 归调用方", 422, `{"msg":"invalid"}`, true, ""},
		{"401 归鉴权", 401, `{"msg":"unauthorized"}`, false, "鉴权失败"},
		{"403 归鉴权", 403, `{"msg":"forbidden"}`, false, "鉴权失败"},
		{"404 归配置", 404, `{"msg":"workflow not found"}`, false, "workflow 或接口路径不存在"},
		{"429 归上游", 429, `{}`, false, "限流"},
		{"500 归上游", 500, `{}`, false, "上游返回 500"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := newAutoDLServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.resp))
			})
			_, err := submitAutoDL(t, srv.URL, `{"prompt":"p","seconds":5,"resolution":"480p"}`)
			if err == nil {
				t.Fatalf("期望失败")
			}
			if got := errors.Is(err, relay.ErrCallerParam); got != c.wantCaller {
				t.Fatalf("caller 归因期望 %v，实际 %v（err=%v）", c.wantCaller, got, err)
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("错误文案应包含 %q，实际 %q", c.wantSub, err.Error())
			}
		})
	}
}

func TestAutoDLSubmitBusinessFailure(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want string
	}{
		{"code 非 Success", `{"code":"Fail","msg":"quota exhausted"}`, "上游拒绝提交"},
		{"缺 task_id", `{"code":"Success","data":{"status":"QUEUED"}}`, "缺少 task_id"},
		{"缺 data", `{"code":"Success"}`, "缺少 task_id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := newAutoDLServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(c.resp))
			})
			_, err := submitAutoDL(t, srv.URL, `{"prompt":"p","seconds":5,"resolution":"480p"}`)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望错误含 %q，实际 %v", c.want, err)
			}
			if errors.Is(err, relay.ErrCallerParam) {
				t.Fatalf("业务拒绝不能归给调用方（会把配置问题甩给用户）：%v", err)
			}
		})
	}
}

// ---------------------------------------------------------------- 轮询

func TestAutoDLPoll(t *testing.T) {
	cases := []struct {
		name       string
		resp       string
		wantStatus string
		wantURL    string
		wantErr    bool
	}{
		{"排队", `{"code":"Success","data":{"status":"QUEUED"}}`, "queued", "", false},
		{"运行中", `{"code":"Success","data":{"status":"RUNNING"}}`, "running", "", false},
		{"成功取视频", `{"code":"Success","data":{"status":"SUCCESS","results":[{"url":"https://cdn/v.mp4","type":"video","file_type":"mp4"}]}}`, "succeeded", "https://cdn/v.mp4", false},
		{"成功优先取 video 而非首项", `{"code":"Success","data":{"status":"SUCCESS","results":[{"url":"https://cdn/cover.jpg","type":"image","file_type":"jpg"},{"url":"https://cdn/v.mp4","type":"video","file_type":"mp4"}]}}`, "succeeded", "https://cdn/v.mp4", false},
		{"file_type 兜底", `{"code":"Success","data":{"status":"SUCCESS","results":[{"url":"https://cdn/v.mp4","file_type":"mp4"}]}}`, "succeeded", "https://cdn/v.mp4", false},
		{"成功无产物判失败", `{"code":"Success","data":{"status":"SUCCESS","results":[]}}`, "failed", "", false},
		{"上游失败带原因", `{"code":"Success","data":{"status":"FAILED","message":"OOM"}}`, "failed", "", false},
		{"code 非 Success", `{"code":"Fail","msg":"no such task"}`, "", "", true},
		{"缺 data", `{"code":"Success"}`, "", "", true},
		{"非 2xx", ``, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status := 200
			if c.name == "非 2xx" {
				status = 502
			}
			srv, rec, _ := newAutoDLServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(c.resp))
			})
			st, err := AutoDL{}.PollTask(context.Background(), autoDLCfg(srv.URL, nil), "up-1")
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望失败，实际 %+v", st)
				}
				return
			}
			if err != nil {
				t.Fatalf("查询失败：%v", err)
			}
			if st.Status != c.wantStatus {
				t.Fatalf("状态期望 %q，实际 %q", c.wantStatus, st.Status)
			}
			if st.ResultURL != c.wantURL {
				t.Fatalf("产物期望 %q，实际 %q", c.wantURL, st.ResultURL)
			}
			if !strings.HasSuffix(rec.path, "/api/v1/comfyui/comfyui_workflow/result/up-1") {
				t.Fatalf("查询路径不对：%s", rec.path)
			}
		})
	}
}

func TestAutoDLPollRejectsEmptyID(t *testing.T) {
	if _, err := (AutoDL{}).PollTask(context.Background(), autoDLCfg("https://x", nil), "  "); err == nil {
		t.Fatalf("空任务 ID 应报错")
	}
}

// ---------------------------------------------------------------- 配置校验

func TestAutoDLValidateOptions(t *testing.T) {
	good := `{"workflows":{"multimodal":"a_1","image":"b-2","text":"c3","first_last":"d4"},"resolution_map":{"720p":"768p","1080p":"1080p"}}`
	if err := (AutoDL{}).ValidateAdapterOptions([]byte(good)); err != nil {
		t.Fatalf("合法配置被拒：%v", err)
	}
	for _, raw := range []string{"", "null", "{}"} {
		if err := (AutoDL{}).ValidateAdapterOptions([]byte(raw)); err != nil {
			t.Fatalf("%q 应视为默认配置：%v", raw, err)
		}
	}

	bad := []struct{ name, raw, want string }{
		{"缺项", `{"workflows":{"multimodal":"a","image":"b","text":"c"}}`, "必须全部填写"},
		{"未知模式键", `{"workflows":{"multimodal":"a","image":"b","text":"c","first_last":"d","video":"e"}}`, "未知的模式键"},
		{"未知顶层字段", `{"workflow":{"text":"a"}}`, "unknown field"},
		{"非法 ID", `{"workflows":{"multimodal":"a/b","image":"b","text":"c","first_last":"d"}}`, "1-64 位"},
		{"重复 ID", `{"workflows":{"multimodal":"a","image":"a","text":"c","first_last":"d"}}`, "不能共用"},
		{"档位键非法", `{"resolution_map":{"768p":"768p"}}`, "只支持 480p/720p/1080p"},
		{"档位值非法", `{"resolution_map":{"720p":"540p"}}`, "取值只能是"},
		{"尾部垃圾", `{"resolution_map":{}} {}`, "只能包含一个 JSON 对象"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := (AutoDL{}).ValidateAdapterOptions([]byte(c.raw))
			if err == nil {
				t.Fatalf("应被拒绝")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误文案应含 %q，实际 %q", c.want, err.Error())
			}
			var cfgErr *relay.ConfigError
			if !errors.As(err, &cfgErr) {
				t.Fatalf("应为 ConfigError（管理台要按字段定位），实际 %T", err)
			}
			if cfgErr.Scope != "渠道适配器配置错误" {
				t.Fatalf("Scope 应为「渠道适配器配置错误」，实际 %q", cfgErr.Scope)
			}
		})
	}
}

func TestAutoDLConfiguredWorkflowUsed(t *testing.T) {
	opts := []byte(`{"workflows":{"multimodal":"m1","image":"i1","text":"t1","first_last":"f1"}}`)
	srv, rec, _ := newAutoDLServer(t, replyOK)
	if _, err := (AutoDL{}).SubmitTask(context.Background(), autoDLCfg(srv.URL, opts),
		&relay.TaskSubmitRequest{Body: []byte(`{"prompt":"p","seconds":5,"resolution":"480p"}`)}); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if !strings.HasSuffix(rec.path, "/t1") {
		t.Fatalf("应使用配置的 workflow，实际 %s", rec.path)
	}
}

func TestAutoDLFailsClosedOnBadConfig(t *testing.T) {
	// adapter_config 脏（DeclErr）：必须在发请求前返回。
	srv, _, hits := newAutoDLServer(t, replyOK)
	cfg := autoDLCfg(srv.URL, nil)
	cfg.DeclErr = errors.New("adapter_config 非法")
	if _, err := (AutoDL{}).SubmitTask(context.Background(), cfg,
		&relay.TaskSubmitRequest{Body: []byte(`{"prompt":"p","seconds":5,"resolution":"480p"}`)}); err == nil {
		t.Fatalf("DeclErr 应 fail-closed")
	}
	// adapter_options 语法非法（OptionsErr）：同样 fail-closed，不能静默用默认 workflow。
	cfg2 := autoDLCfg(srv.URL, nil)
	cfg2.OptionsErr = errors.New("adapter_options 非法")
	if _, err := (AutoDL{}).SubmitTask(context.Background(), cfg2,
		&relay.TaskSubmitRequest{Body: []byte(`{"prompt":"p","seconds":5,"resolution":"480p"}`)}); err == nil {
		t.Fatalf("OptionsErr 应 fail-closed")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("配置错不应发出请求，实际 %d 次", n)
	}
}

func TestAutoDLMissingKey(t *testing.T) {
	srv, _, hits := newAutoDLServer(t, replyOK)
	cfg := &relay.ChannelConfig{BaseURL: srv.URL, Timeout: time.Second}
	if _, err := (AutoDL{}).SubmitTask(context.Background(), cfg,
		&relay.TaskSubmitRequest{Body: []byte(`{"prompt":"p","seconds":5,"resolution":"480p"}`)}); err == nil {
		t.Fatalf("空密钥应报错")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("空密钥不应发出请求")
	}
}

// ---------------------------------------------------------------- Test()

func TestAutoDLTestProbe(t *testing.T) {
	cases := []struct {
		status  int
		wantErr bool
		wantSub string
	}{
		{200, false, ""},
		{400, false, ""},
		{429, false, ""},
		{401, true, "鉴权失败"},
		{403, true, "鉴权失败"},
		{404, true, "接口路径不存在"},
		{500, true, "上游返回"},
	}
	for _, c := range cases {
		srv, rec, _ := newAutoDLServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{}`))
		})
		err := (AutoDL{}).Test(context.Background(), autoDLCfg(srv.URL, nil))
		if c.wantErr {
			if err == nil {
				t.Fatalf("HTTP %d 应报错（否则熔断渠道会被自动恢复）", c.status)
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("HTTP %d 文案应含 %q，实际 %q", c.status, c.wantSub, err.Error())
			}
			continue
		}
		if err != nil {
			t.Fatalf("HTTP %d 应通过，实际 %v", c.status, err)
		}
		if !strings.HasSuffix(rec.path, "/result/__probe__") {
			t.Fatalf("探测路径不对：%s", rec.path)
		}
	}
}

// ---------------------------------------------------------------- 契约

func TestAutoDLContracts(t *testing.T) {
	var a relay.Adapter = AutoDL{}
	var _ relay.TaskAdapter = AutoDL{}
	var _ relay.EndpointSupporter = AutoDL{}
	var _ relay.OptionsSupporter = AutoDL{}
	var _ relay.DeclarativeSupporter = AutoDL{}

	if a.Name() != "autodl" {
		t.Fatalf("名称不对：%s", a.Name())
	}
	for _, k := range []relay.EndpointKind{
		relay.KindChat, relay.KindCompletions, relay.KindEmbeddings,
		relay.KindImagesGenerations, relay.KindImagesEdits, relay.KindImagesVariations,
		relay.KindAudioSpeech, relay.KindAudioTranscriptions, relay.KindAudioTranslations,
		relay.KindModerations, relay.KindRerank, relay.KindResponses,
	} {
		if a.(relay.EndpointSupporter).SupportsEndpoint(k) {
			t.Fatalf("同步端点 %v 必须声明不支持，否则会接走 chat", k)
		}
	}
	if !relay.TaskEndpointSupported("autodl", "video/generations") {
		t.Fatalf("必须承接 video/generations")
	}
	if relay.TaskEndpointSupported("autodl", "audio/music/generations") {
		t.Fatalf("不得声明承接音乐端点：上游没有这个接口，声明了会让音乐模型误绑到本渠道")
	}
	if a.(relay.DeclarativeSupporter).SupportsDeclarativeConfig() {
		t.Fatalf("不应支持声明式配置")
	}
	if !relay.SupportsAdapterOptions("autodl") {
		t.Fatalf("必须声明支持渠道私有配置")
	}
}

// TestAutoDLTierOf 锁定与 service/task.normalizeVideoResolution 的同一套档位规则。
//
// 两处必须逐字一致：不一致会出现「按 480p 冻结、按 720p 生成」的错账。
// service/task 侧有同名表（normalize_test.go），共用这份期望值实现交叉锁定。
func TestAutoDLTierOf(t *testing.T) {
	cases := map[string]string{
		"480p":      "480p",
		"720P":      "720p",
		"1080p":     "1080p",
		"1280x720":  "720p",
		"720*1280":  "720p",
		"720×1280":  "720p",
		"854x480":   "480p",
		"1024x1024": "1080p", // 短边 1024 > 720：按平台规则归 1080p（本模型随后拒绝）
		"768x1366":  "1080p",
		"1080x1920": "1080p",
		"4k":        "4k",
		"768p":      "720p", // 上游档位名吸附到平台 720p 档
		"768P":      "720p",
		"":          "",
	}
	for in, want := range cases {
		if got := tierOf(in, ""); got != want {
			t.Fatalf("tierOf(%q) 期望 %q，实际 %q", in, want, got)
		}
	}
	// resolution 为空时回落 size（与 ParseSubmit 的取值顺序一致）。
	if got := tierOf("", "854x480"); got != "480p" {
		t.Fatalf("应回落 size，实际 %q", got)
	}
}
