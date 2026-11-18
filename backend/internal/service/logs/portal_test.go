package logs

import (
	"lingdong-api/internal/pkg/taskstate"

	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/service/taskview"
)

// TestExtractOutputTextCoversEveryShape 锁死「响应文本视图」的六种真实形态。
//
// 为什么必须逐个覆盖：门户详情的「输出」就靠它。少一种形态的表现不是报错，
// 而是**那条记录的输出区永远空白**（流式对话曾经如此，占全部调用的大头）。
func TestExtractOutputTextCoversEveryShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "OpenAI 非流式字符串",
			in:   `{"choices":[{"message":{"content":"你好"}}]}`,
			want: "你好",
		},
		{
			name: "OpenAI 内容数组",
			in:   `{"choices":[{"message":{"content":[{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]}}]}`,
			want: "第一段第二段",
		},
		{
			name: "completion 文本",
			in:   `{"choices":[{"text":"续写内容"}]}`,
			want: "续写内容",
		},
		{
			name: "responses 的 output_text",
			in:   `{"output_text":"直接给的文本"}`,
			want: "直接给的文本",
		},
		{
			name: "Anthropic 顶层 content 数组",
			in:   `{"content":[{"type":"text","text":"克劳德回答"}]}`,
			want: "克劳德回答",
		},
		{
			name: "流式聚合体（relay.streamAggregateJSON）",
			in:   `{"_note":"流式响应","content":"拼接后的回答","streamed":true}`,
			want: "拼接后的回答",
		},
		{
			name: "原始 SSE（OpenAI delta）",
			in:   "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\ndata: [DONE]\n\n",
			want: "你好",
		},
		{
			name: "原始 SSE（Anthropic content_block_delta）",
			in:   "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"克劳德\"}}\n\n",
			want: "克劳德",
		},
		{
			name: "图片响应无文本",
			in:   `{"data":[{"url":"https://example.com/a.png"}]}`,
			want: "",
		},
		{
			name: "二进制占位说明",
			in:   `{"_note":"上游返回空响应体（0 字节）"}`,
			want: "",
		},
	}
	for _, c := range cases {
		if got := extractOutputText(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestExtractOutputTextCapsAt8KiB 保证文本视图不会把整段对话再下发一遍。
func TestExtractOutputTextCapsAt8KiB(t *testing.T) {
	long := strings.Repeat("字", 10000)
	got := extractOutputText(`{"choices":[{"message":{"content":"` + long + `"}}]}`)
	if len(got) > portalOutputTextLimit {
		t.Fatalf("文本视图超过上限: %d > %d", len(got), portalOutputTextLimit)
	}
	if got == "" {
		t.Fatal("截断不应把内容切空")
	}
}

// TestPortalIOOfPrefersDeliveredBody 锁死选体规则：交付体优先，空则回落上游原文。
func TestPortalIOOfPrefersDeliveredBody(t *testing.T) {
	// 普通透传：两份相同 → 落库时只存 resp_raw，投影直接用 resp_raw。
	same := portalIOOf(&IOView{
		RespRaw:   `{"choices":[{"message":{"content":"你好"}}]}`,
		RespBytes: 40,
	})
	if same == nil || same.RespBody != `{"choices":[{"message":{"content":"你好"}}]}` {
		t.Fatalf("普通透传应回落到 resp_raw，实际 %+v", same)
	}
	if same.OutputText != "你好" {
		t.Fatalf("文本视图丢失: %q", same.OutputText)
	}

	// 改写/转码/失败：交付体优先。
	delivered := portalIOOf(&IOView{
		RespRaw:      `{"data":[{"url":"https://cdn.vendor.example/a.png"}]}`,
		DeliveredRaw: `{"data":[{"url":"https://api.example.com/v1/media/tok"}]}`,
		RespBytes:    0,
	})
	if delivered == nil || !strings.Contains(delivered.RespBody, "/v1/media/") {
		t.Fatalf("应优先用交付体，实际 %+v", delivered)
	}
	if strings.Contains(delivered.RespBody, "vendor.example") {
		t.Fatal("上游地址泄漏到用户端")
	}
	// 失败体没有写出字节数，必须按正文长度补齐，否则详情显示「响应 0 B」。
	if delivered.RespBytes != int64(len(delivered.RespBody)) {
		t.Fatalf("失败体字节数未补齐: %d", delivered.RespBytes)
	}
}

// TestPortalIOOfNil 报文被保留策略清掉时整块缺省（不是空对象）。
func TestPortalIOOfNil(t *testing.T) {
	if got := portalIOOf(nil); got != nil {
		t.Fatalf("call_io 不存在时应为 nil，实际 %+v", got)
	}
}

// TestPortalDetailOmitsChannelKeyID 门户详情不得带出「走了哪把上游密钥」。
//
// 为什么值得单独一条：`PortalItem` 是**白名单投影**，`ChannelKeyID` 天然不在里面，
// 所以这条规则现在「碰巧」成立。真正的风险是以后有人图省事把 `PortalItemOf`
// 改成直接内嵌 `Item`（少写十几个字段），那一刻泄漏是静默的 —— 页面照常显示，
// 只是每个用户都能看出平台在哪几家上游之间切、哪把 key 被限流。
//
// 断言方式是行为式而非结构式：把内部字段填上哨兵值，再序列化对外视图。
func TestPortalDetailOmitsChannelKeyID(t *testing.T) {
	const keyID = "ffffffffffffffffffffffffffffffff"

	d := &Detail{
		Item: Item{
			ID: 1, Model: "gpt-4o", Status: "settled", HTTPStatus: 200,
			ChannelID: 3, ChannelKeyID: keyID,
		},
	}
	raw, err := json.Marshal(PortalDetail(d))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), keyID) {
		t.Fatalf("门户详情泄漏了上游密钥标识: %s", raw)
	}
	if strings.Contains(string(raw), "channel_key_id") {
		t.Fatalf("门户详情出现了 channel_key_id 键: %s", raw)
	}

	// 列表投影同一口径（两处共用一个白名单，漏一处就是两个出口不一致）。
	itemRaw, err := json.Marshal(PortalItemOf(d.Item, "", 0, 0, 0))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(itemRaw), keyID) || strings.Contains(string(itemRaw), "channel_key_id") {
		t.Fatalf("门户列表泄漏了上游密钥标识: %s", itemRaw)
	}
}

// TestPortalDetailKeepsTaskObjectIntact 门户与内部详情下发**同一个**任务对象。
//
// 这条替换了旧的 portalTaskRef 用例：以前门户会把 tasks.error_msg 原文换成分类文案
// （否则调用详情能读到外部域名与真实模型名）。现在投影时就只有分类文案了 ——
// 任务对象里根本没有原文字段，所以门户侧不需要再裁剪一遍，也**不允许**再裁剪
// （裁一次就会出现"门户少了 contents"这类静默缺失）。
func TestPortalDetailKeepsTaskObjectIntact(t *testing.T) {
	in := &Detail{}
	in.Task = &taskview.View{
		ID: "9", Object: "task", Status: taskstate.Failed, Kind: "video", Progress: 100,
		Error: &taskview.ViewError{Message: taskstate.PortalError(taskstate.Failed)},
		Contents: []taskview.ViewArtifact{
			{Index: 0, URL: "/api/portal/tasks/9/contents/0", ContentType: "image/png", Bytes: 1, Kind: "image"},
		},
	}
	got := PortalDetail(in).Task
	if got == nil {
		t.Fatal("门户详情丢了任务对象")
	}
	if got.Error == nil || got.Error.Message != taskstate.PortalError(taskstate.Failed) {
		t.Fatalf("应带分类文案，实际 %+v", got.Error)
	}
	if strings.Contains(got.Error.Message, "vendor.example") {
		t.Fatal("门户文案里出现了外部域名")
	}
	if got.Progress != 100 || len(got.Contents) != 1 || got.Contents[0].Kind != "image" {
		t.Fatalf("非敏感字段被改动: %+v", got)
	}

	if PortalDetail(&Detail{}).Task != nil {
		t.Fatal("没有关联任务时应为 nil")
	}
}
