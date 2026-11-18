package adapters

// 金标准（golden）用例：把「官方文档示例请求体」完整跑一遍构造链路，断言
// **出站体与入站体语义等价** —— 唯一的差异是 `model` 被替换成绑定里的上游模型名。
//
// 为什么按语义比较而不按字节比较：构造链路里有 map 中转（键序会被 Go 重排），
// 字节相等永远不成立。而「语义相等」恰好就是「1:1 端口转发、不遗漏字段参数」的定义：
//   - 少一个键 = 静默丢参（上游按默认值生成，用户以为参数没生效）；
//   - 多一个键 = 我们擅自替用户做了决定（上游对未知字段是 400 还是忽略不可控）。
//
// 反向保护同样重要：`arkImageKnownKeys` 与 SDK typed 结构必须**双向一致**，
// 少一个键 → 该字段被塞进 extra_body，出站体里同一字段出现两次（JSON 允许重复键，
// 上游取哪个由解析器决定）；多一个键 → 该字段永远进不了 typed，用户传了就是丢。

import (
	"encoding/json"
	"reflect"
	"testing"
)

const arkGoldenUpstream = "doubao-seedream-5-0-260128"

// assertSemanticEqual 比较两个 JSON 对象的语义（键序无关、数字按值）。
func assertSemanticEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("期望值不是合法 JSON：%v（%s）", err, want)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("实际值不是合法 JSON：%v（%s）", err, got)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("出站体与入站体不等价（1:1 转发被破坏）\n入站/期望: %s\n出站/实际: %s", want, got)
	}
}

// TestArkVideoGolden 覆盖 Seedance 规范通道的 7 类官方示例请求体。
//
// 这里用**完全相等**而不是「包含」：视频出站体是我们逐字段重建的，
// 多一个键（例如把平台的 seconds 也发出去）就意味着上游会看到不认识的字段。
func TestArkVideoGolden(t *testing.T) {
	const up = "doubao-seedance-2-0-260128"
	cases := []struct {
		name         string
		body         string
		autoDuration bool
		want         string
	}{
		{
			name: "文生视频（档位大写必须归一）",
			body: `{"model":"m","prompt":"海边日落，镜头缓慢推进","seconds":5,"resolution":"720P","ratio":"16:9"}`,
			want: `{"model":"` + up + `","content":[{"type":"text","text":"海边日落，镜头缓慢推进"}],` +
				`"duration":5,"resolution":"720p","ratio":"16:9"}`,
		},
		{
			name: "首尾帧（素材顺序与 role 都要原样）",
			body: `{"model":"m","prompt":"从白天过渡到夜晚","seconds":5,"resolution":"1080p",` +
				`"input_reference":[` +
				`{"type":"first_frame","image_url":"https://cdn.example.com/a.png"},` +
				`{"type":"last_frame","image_url":"https://cdn.example.com/b.png"}]}`,
			want: `{"model":"` + up + `","content":[` +
				`{"type":"text","text":"从白天过渡到夜晚"},` +
				`{"type":"image_url","image_url":{"url":"https://cdn.example.com/a.png"},"role":"first_frame"},` +
				`{"type":"image_url","image_url":{"url":"https://cdn.example.com/b.png"},"role":"last_frame"}],` +
				`"duration":5,"resolution":"1080p"}`,
		},
		{
			// 不传 type 的素材必须按链接推断，否则「文档承诺按链接推断」就是空话。
			name: "参考图 / 参考视频 / 参考音频（不传 type）",
			body: `{"model":"m","prompt":"用这段音乐给这张图配动作","seconds":8,"resolution":"720p",` +
				`"input_reference":[{"image_url":"https://cdn.example.com/a.png"},` +
				`{"video_url":"https://cdn.example.com/v.mp4"},` +
				`{"audio_url":"https://cdn.example.com/a.mp3"}]}`,
			want: `{"model":"` + up + `","content":[` +
				`{"type":"text","text":"用这段音乐给这张图配动作"},` +
				`{"type":"image_url","image_url":{"url":"https://cdn.example.com/a.png"},"role":"reference_image"},` +
				`{"type":"video_url","video_url":{"url":"https://cdn.example.com/v.mp4"},"role":"reference_video"},` +
				`{"type":"audio_url","audio_url":{"url":"https://cdn.example.com/a.mp3"},"role":"reference_audio"}],` +
				`"duration":8,"resolution":"720p"}`,
		},
		{
			// 智能时长：请求里的 seconds 只是**计费基准**，出站必须发 -1 且不发 seconds。
			name:         "智能时长（seconds 不得出站）",
			body:         `{"model":"m","prompt":"a","seconds":10,"resolution":"720p"}`,
			autoDuration: true,
			want:         `{"model":"` + up + `","content":[{"type":"text","text":"a"}],"duration":-1,"resolution":"720p"}`,
		},
		{
			name: "厂商顶层参数全量透传",
			body: `{"model":"m","prompt":"a","seconds":5,"resolution":"720p","frames":121,` +
				`"output_format":"mov","draft":true,"priority":2,"camera_fixed":false,` +
				`"safety_identifier":"uid-1","generate_audio":true,"seed":42,"watermark":false,` +
				`"tools":[{"type":"web_search"}]}`,
			want: `{"model":"` + up + `","content":[{"type":"text","text":"a"}],"duration":5,"resolution":"720p",` +
				`"frames":121,"output_format":"mov","draft":true,"priority":2,"camera_fixed":false,` +
				`"safety_identifier":"uid-1","generate_audio":true,"seed":42,"watermark":false,` +
				`"tools":[{"type":"web_search"}]}`,
		},
		{
			// metadata 是历史入口：白名单键提升到顶层，容器本身与非白名单键都不出站。
			name: "metadata 白名单提升",
			body: `{"model":"m","prompt":"a","duration":5,"metadata":{"return_last_frame":true,"unknown":"x"}}`,
			want: `{"model":"` + up + `","content":[{"type":"text","text":"a"}],"duration":5,"return_last_frame":true}`,
		},
		{
			// size 是平台的写法，Ark 只认 resolution。
			name: "size 映射到 resolution",
			body: `{"model":"m","prompt":"a","duration":5,"size":"1080P"}`,
			want: `{"model":"` + up + `","content":[{"type":"text","text":"a"}],"duration":5,"resolution":"1080p"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _, err := arkBuildSpecBody([]byte(c.body), up, c.autoDuration, false)
			if err != nil {
				t.Fatalf("构造失败：%v", err)
			}
			assertSemanticEqual(t, []byte(c.want), out)
		})
	}
}
