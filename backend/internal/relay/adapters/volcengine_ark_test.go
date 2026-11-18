package adapters

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/relay"
)

// 本文件测的是**请求体构造与本地校验**，不出网、不依赖数据库。
//
// 为什么这批用例值钱：适配器最容易错的地方不是 HTTP，而是「平台字段 → 上游字段」的映射。
// 映射错一个键名，上游不会报错（未知字段常常被忽略），只会安静地按默认值生成，
// 而用户看到的是「我说的时长/画幅没生效」。

// decodeObj 把构造结果解成 map 便于按键断言（键序无关）。
func decodeObj(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("构造结果不是 JSON 对象：%v（原文 %s）", err, raw)
	}
	return m
}

// contentOf 取出 content 数组。
func contentOf(t *testing.T, m map[string]any) []any {
	t.Helper()
	arr, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("content 不是数组：%v", m["content"])
	}
	return arr
}

// ① 规范通道：文本排第一、素材带 role、档位转小写、秒数落到 duration。
func TestArkBuildSpecBodyTextAndFirstFrame(t *testing.T) {
	in := `{"model":"seedance-x","prompt":"镜头推近","seconds":5,"resolution":"720P",
		"input_reference":[{"type":"first_frame","image_url":"https://cdn.example.com/a.png"}]}`
	out, _, err := arkBuildSpecBody([]byte(in), "doubao-seedance-2-0-260128", false, false)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	m := decodeObj(t, out)
	if m["model"] != "doubao-seedance-2-0-260128" {
		t.Fatalf("model 应替换为上游名，实际 %v", m["model"])
	}
	if m["resolution"] != "720p" {
		t.Fatalf("档位必须小写，实际 %v", m["resolution"])
	}
	if m["duration"] != float64(5) {
		t.Fatalf("duration 应为 5，实际 %v", m["duration"])
	}
	c := contentOf(t, m)
	if len(c) != 2 {
		t.Fatalf("content 应有 2 项（文本 + 图），实际 %d 项：%v", len(c), c)
	}
	first := c[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "镜头推近" {
		t.Fatalf("文本必须排第一且内容原样，实际 %v", first)
	}
	second := c[1].(map[string]any)
	if second["type"] != "image_url" || second["role"] != "first_frame" {
		t.Fatalf("首帧必须显式带 role=first_frame，实际 %v", second)
	}
	img := second["image_url"].(map[string]any)
	if img["url"] != "https://cdn.example.com/a.png" {
		t.Fatalf("图片地址被改写了：%v", img["url"])
	}
}

// ② 智能时长：autoDuration 时发 -1，且**不**发秒数。
func TestArkBuildSpecBodyAutoDuration(t *testing.T) {
	out, _, err := arkBuildSpecBody([]byte(`{"model":"x","prompt":"a","seconds":30,"resolution":"720p"}`), "up", true, false)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	m := decodeObj(t, out)
	if m["duration"] != float64(-1) {
		t.Fatalf("智能时长必须发 -1，实际 %v", m["duration"])
	}
	if _, ok := m["seconds"]; ok {
		t.Fatal("上游不认 seconds，不该发出去")
	}
}

// ③ 无提示词无素材：本地 400（而不是带着空 content 出站）。
func TestArkBuildSpecBodyRejectsEmptyContent(t *testing.T) {
	if _, _, err := arkBuildSpecBody([]byte(`{"model":"x","prompt":"  "}`), "up", false, false); err == nil {
		t.Fatal("没有文本也没有素材时必须报错")
	}
}

// ④ 顶层厂商参数必须透传：契约里它们是顶层字段，漏掉就是静默丢参。
func TestArkBuildSpecBodyForwardsTopLevelVendorFields(t *testing.T) {
	in := `{"model":"x","prompt":"a","seconds":5,"resolution":"720p","frames":121,
		"output_format":"mov","generate_audio":false,"ratio":"16:9","seed":7}`
	out, _, err := arkBuildSpecBody([]byte(in), "up", false, false)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	m := decodeObj(t, out)
	if m["frames"] != float64(121) {
		t.Fatalf("frames 丢了：%v", m["frames"])
	}
	if m["output_format"] != "mov" {
		t.Fatalf("output_format 丢了：%v", m["output_format"])
	}
	if m["generate_audio"] != false {
		t.Fatalf("generate_audio 必须透传 false，实际 %v", m["generate_audio"])
	}
	if m["ratio"] != "16:9" || m["seed"] != float64(7) {
		t.Fatalf("ratio / seed 丢了：%v / %v", m["ratio"], m["seed"])
	}
}

// ⑤ metadata 里的白名单键提升到顶层，非白名单键必须被丢弃（不能污染上游请求体）。
func TestArkBuildSpecBodyMetadataWhitelist(t *testing.T) {
	in := `{"model":"x","prompt":"a","seconds":5,"resolution":"720p",
		"metadata":{"draft":true,"unknown_key":"v"}}`
	out, _, err := arkBuildSpecBody([]byte(in), "up", false, false)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	m := decodeObj(t, out)
	if m["draft"] != true {
		t.Fatalf("draft 应提升到顶层，实际 %v", m["draft"])
	}
	if _, ok := m["unknown_key"]; ok {
		t.Fatal("非白名单键必须丢弃：上游对未知字段的处理不可控")
	}
	if _, ok := m["metadata"]; ok {
		t.Fatal("metadata 容器本身不该出站")
	}
}

// ⑩ 原生通道判定：只有非空 content[] 才算原生。
func TestArkIsNative(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"原生 content", `{"content":[{"type":"text","text":"a"}]}`, true},
		{"空 content 数组", `{"content":[]}`, false},
		{"content 不是数组", `{"content":"x"}`, false},
		{"规范体没有 content", `{"prompt":"a","seconds":5}`, false},
		{"非法 JSON", `not-json`, false},
	}
	for _, c := range cases {
		if got := arkIsNative([]byte(c.body)); got != c.want {
			t.Fatalf("%s：期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}

// ⑪ 任务号解析：四种写法都认，缺失时报错（上层据此判「提交歧义」）。
func TestArkDecodeTaskID(t *testing.T) {
	ok := map[string]string{
		`{"id":"cgt-1"}`:                       "cgt-1",
		`{"task_id":"cgt-2"}`:                  "cgt-2",
		`{"data":{"id":"cgt-3"}}`:              "cgt-3",
		`{"data":{"task_id":"cgt-4"}}`:         "cgt-4",
		`{"id":"  cgt-5  ","task_id":"other"}`: "cgt-5",
	}
	for body, want := range ok {
		got, err := arkDecodeTaskID([]byte(body))
		if err != nil || got != want {
			t.Fatalf("%s：期望 %q，实际 %q（err=%v）", body, want, got, err)
		}
	}
	for _, body := range []string{`{}`, `{"data":{}}`, `[]`} {
		if _, err := arkDecodeTaskID([]byte(body)); err == nil {
			t.Fatalf("%s 应解析失败", body)
		}
	}
}

// ⑫ 原生体替换 model：只动 model 一个键，其余值**字节级**保留。
//
// 这条是「1:1 转发」的核心保障：精度敏感的数字（seed / duration / frames）
// 一旦走 map[string]any 就会变成 float64，9007199254740993 这类值会被悄悄改写。
func TestArkReplaceModelKeepsOtherValues(t *testing.T) {
	in := `{"model":"old","seed":9007199254740993,"content":[{"type":"text","text":"a"}],"duration":-1}`
	out, err := arkReplaceModel([]byte(in), "new-model")
	if err != nil {
		t.Fatalf("替换失败：%v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"model":"new-model"`) {
		t.Fatalf("model 未替换：%s", s)
	}
	if !strings.Contains(s, "9007199254740993") {
		t.Fatalf("大整数被改写了（精度丢失）：%s", s)
	}
	if !strings.Contains(s, `"duration":-1`) {
		t.Fatalf("duration 被改写了：%s", s)
	}
}

// ⑬ 非对象请求体必须 400，而不是被当成空对象发出去。
func TestArkReplaceModelRejectsNonObject(t *testing.T) {
	for _, body := range []string{``, `[]`, `"x"`, `null`} {
		if _, err := arkReplaceModel([]byte(body), "m"); err == nil {
			t.Fatalf("%q 应被拒绝", body)
		}
	}
}

// ⑭ 认不出的顶层键必须被**报告出来**（不静默丢），而平台别名不能被误报。
//
// 这两边都会出真问题：
//   - 漏报 → 用户拼错 `frame`（应为 `frames`）提交成功、生成时长却是默认值，
//     事后查不到是哪一环吃掉的；
//   - 误报 → 用户正常传 `image`（task 层会折叠成 input_reference 但保留原键）
//     每次都进告警，日志随即失去意义，真正的问题被淹没。
func TestArkSpecBodyReportsUnknownTopKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "平台别名不算未知",
			body: `{"model":"m","prompt":"a","image":"https://cdn.example.com/a.png","width":1280,"height":720}`,
			want: nil,
		},
		{
			name: "白名单键不算未知",
			body: `{"model":"m","prompt":"a","frames":121,"draft":true,"camera_fixed":false}`,
			want: nil,
		},
		{
			name: "别家协议字段被报告",
			body: `{"model":"m","prompt":"a","quality":"hd","n":2}`,
			want: []string{"n", "quality"},
		},
		{
			name: "拼错的键被报告",
			body: `{"model":"m","prompt":"a","frame":121}`,
			want: []string{"frame"},
		},
	}
	for _, c := range cases {
		_, ignored, err := arkBuildSpecBody([]byte(c.body), "up", false, false)
		if err != nil {
			t.Fatalf("%s：构造失败 %v", c.name, err)
		}
		if len(ignored) != len(c.want) {
			t.Fatalf("%s：期望忽略 %v，实际 %v", c.name, c.want, ignored)
		}
		for i := range c.want {
			if ignored[i] != c.want[i] {
				t.Fatalf("%s：期望忽略 %v，实际 %v", c.name, c.want, ignored)
			}
		}
	}
}

// TestArkDeclaresReplayPolicy：适配器必须声明「哪些子协议不可重放」「哪些顶层键是厂商参数」。
//
// 这两份声明是任务层唯一的事实来源：
//   - 不可重放：原生体 content[] 的请求体完全由用户拼装，平台字段拼不出来，
//     重放会变成"按素材计费、只发一段文本"的错账。任务层拿字符串硬比 "ark.native"
//     就是把这句知识复制一份，改适配器时必然漏改。
//   - 厂商参数：这批键要落进 request_params 快照，否则重试后 watermark / frames
//     这类参数会静默消失（首批生效、重试丢失，两处代码单看都对）。
func TestArkDeclaresReplayPolicy(t *testing.T) {
	if relay.TaskReplayable("volcengine-ark", arkSubKindNative) {
		t.Fatalf("原生体（%s）必须被判为不可重放", arkSubKindNative)
	}
	if !relay.TaskReplayable("volcengine-ark", arkSubKindSpec) {
		t.Fatalf("规范通道（%s）必须可重放", arkSubKindSpec)
	}
	// 未注册的适配器/空子协议一律可重放（单路径适配器的既有行为不变）。
	if !relay.TaskReplayable("volcengine-ark", "") || !relay.TaskReplayable("not-registered", "x") {
		t.Fatalf("默认必须是可重放")
	}

}

// TestArkVendorParamsFromMetadataAndTopLevel：顶层与 metadata 两个入口走同一条提升路径。
//
// 分开实现过一次的后果：顶层传 watermark=false 生效、metadata 传同一字段丢失，
// 而两处代码各自都"看起来对"。
func TestArkVendorParamsFromMetadataAndTopLevel(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"p","seconds":5,"resolution":"720p",
		"watermark":false,"generate_audio":true,
		"metadata":{"output_format":"mov","camera_fixed":true,"unknown_key":"x"}}`)
	raw, ignored, err := arkBuildSpecBody(body, "up-model", false, false)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	m := decodeObj(t, raw)
	if m["watermark"] != false || m["generate_audio"] != true {
		t.Fatalf("顶层厂商参数丢失：%v", m)
	}
	if m["output_format"] != "mov" || m["camera_fixed"] != true {
		t.Fatalf("metadata 白名单未提升：%v", m)
	}
	if _, ok := m["unknown_key"]; ok {
		t.Fatalf("非白名单键不该出站：%v", m)
	}
	// metadata 里的未知键必须留痕（P2-6）：出站仍然丢掉，但要在调用记录里报出来。
	// 这里断言的是**恰好这一条**：多了说明白名单键被误判成未知，少了就是又回到静默丢弃。
	if len(ignored) != 1 || ignored[0] != "metadata.unknown_key" {
		t.Fatalf("只应报出 metadata.unknown_key 一条未识别，实际 %v", ignored)
	}
}
