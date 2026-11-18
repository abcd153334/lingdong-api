package adapters

// 提示词弱校验参数扫描器的用例（volcengine_ark_weakparams.go）。
//
// 这一层只做只读识别：认错的代价是「假的计价冲突告警」（告警一假就没人看），
// 漏认的代价是「按低档收费、按高档交付」没人知道。所以边界用例比正常用例更重要。

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

func TestArkScanWeakParams(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		want  map[string]string
		empty bool
	}{
		{name: "官方示例整串", text: "小猫对着镜头打哈欠 --rs 720p --rt 16:9 --dur 5 --seed 11 --cf false --wm true",
			want: map[string]string{"resolution": "720p", "ratio": "16:9", "duration": "5", "seed": "11", "camera_fixed": "false", "watermark": "true"}},
		{name: "中文句号是左边界", text: "小猫打哈欠。--rs 4k", want: map[string]string{"resolution": "4k"}},
		{name: "行首也是边界", text: "--dur 5 一只猫", want: map[string]string{"duration": "5"}},
		{name: "全名同样认", text: "cat --resolution 1080p", want: map[string]string{"resolution": "1080p"}},

		// —— 以下都必须「不识别」，否则正常文案会被误报成计价冲突 ——
		{name: "破折号后是空格", text: "作者：-- 注：这是草稿", empty: true},
		{name: "URL 里的双横线", text: "https://cdn.x/a--seed", empty: true},
		{name: "标识符里的双横线", text: "doubao-seedream-5-0--preview 风格", empty: true},
		{name: "名字后没有空白", text: "小猫 --rs4k", empty: true},
		{name: "名字后没有空白（后缀数字）", text: "小猫 --seed11", empty: true},
		{name: "值形状不符（档位）", text: "小猫 --rs 很高", empty: true},
		{name: "值形状不符（数字）", text: "小猫 --dur abc", empty: true},
		{name: "结尾没有值", text: "小猫 --rs", empty: true},
		{name: "未登记的键", text: "小猫 --unknown 1", empty: true},
		{name: "em dash 不是双横线", text: "—— 作者注 ——", empty: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := arkScanWeakParams(c.text)
			if c.empty {
				if len(got) != 0 {
					t.Fatalf("不应识别出参数，实际 %v", got)
				}
				return
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("%s = %q，期望 %q（全部：%v）", k, got[k], v, got)
				}
			}
			for k := range got {
				if _, ok := c.want[k]; !ok {
					t.Fatalf("多识别出 %s=%q", k, got[k])
				}
			}
		})
	}
}

// TestArkWeakNotesFromBody 守「与 body 比对」这一步：只有计价类的三参数、且确实不一致才出条。
func TestArkWeakNotesFromBody(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantSubs []string // 空 = 不应产生任何 note
	}{
		{
			name:     "body 与提示词不一致",
			body:     `{"prompt":"猫 --rs 4k","resolution":"720p"}`,
			wantSubs: []string{"weak.resolution=4k", "body=720p"},
		},
		{
			name:     "body 没给分辨率",
			body:     `{"prompt":"猫 --rs 4k"}`,
			wantSubs: []string{"weak.resolution=4k", "body 未传"},
		},
		{
			name:     "一致就不报",
			body:     `{"prompt":"猫 --rs 720p","resolution":"720p"}`,
			wantSubs: nil,
		},
		{
			name:     "size 折叠后与 resolution 同义",
			body:     `{"prompt":"猫 --rs 720p","size":"720P"}`,
			wantSubs: nil,
		},
		{
			name:     "时长与秒数等价",
			body:     `{"prompt":"猫 --dur 5","seconds":5}`,
			wantSubs: nil,
		},
		{
			name:     "时长不一致",
			body:     `{"prompt":"猫 --dur 10","duration":5}`,
			wantSubs: []string{"weak.duration=10", "body=5"},
		},
		{
			name:     "frames 一律记（内置模型都不支持）",
			body:     `{"prompt":"猫 --frames 121","duration":5}`,
			wantSubs: []string{"weak.frames=121"},
		},
		{
			name:     "不影响计价的参数不产生 note",
			body:     `{"prompt":"猫 --rt 16:9 --seed 11 --cf false --wm true","ratio":"9:16"}`,
			wantSubs: nil,
		},
		{
			// 原生通道：参数藏在 content[].text 里，比规范体更容易漏。
			name:     "原生 content[] 里的提示词",
			body:     `{"model":"m","content":[{"type":"text","text":"猫 --rs 4k"}],"resolution":"720p"}`,
			wantSubs: []string{"weak.resolution=4k"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := arkWeakNotesFromBody([]byte(c.body))
			if len(c.wantSubs) == 0 {
				if len(got) != 0 {
					t.Fatalf("不应产生 note，实际 %v", got)
				}
				return
			}
			joined := strings.Join(got, " | ")
			for _, sub := range c.wantSubs {
				if !strings.Contains(joined, sub) {
					t.Fatalf("note 缺 %q，实际 %q", sub, joined)
				}
			}
		})
	}
}

// TestArkSubmitTaskReportsWeakParams 端到端：适配器必须把 note 挂到 req.Notes 上，
// 且**不得修改出站报文**（用户决定不拦不剥，改写提示词等于违约）。
func TestArkSubmitTaskReportsWeakParams(t *testing.T) {
	var gotBody string
	cfg := arkTestChannel(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cgt-1"}`))
	})

	req := &relay.TaskSubmitRequest{
		UpstreamModel: "doubao-seedance-2-0-260128",
		Body:          []byte(`{"model":"m","prompt":"猫 --rs 4k --rt 16:9","resolution":"720p","seconds":5}`),
	}
	if _, err := (VolcengineARK{}).SubmitTask(context.Background(), cfg, req); err != nil {
		t.Fatalf("提交失败：%v", err)
	}
	if len(req.Notes) == 0 {
		t.Fatal("提示词里的计价参数必须上报成 note（否则运营永远发现不了套利）")
	}
	if !strings.Contains(gotBody, "--rs 4k") {
		t.Fatalf("出站报文不得被改写（本轮只记异常），实际 %s", gotBody)
	}
	if !strings.Contains(gotBody, `"resolution":"720p"`) {
		t.Fatalf("body 参数必须原样出站，实际 %s", gotBody)
	}
}
