package callio

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSummarizeBodyDropsUserText 是本轮新增的隐私边界锁：
// meta 口径下正文**绝不进库**，只留结构（模型、条数、尺寸、图片数）。
//
// 断言的是"用户内容不出现"，而不是"摘要长什么样" —— 后者会随字段增删频繁变，
// 前者是真正的约束。
func TestSummarizeBodyDropsUserText(t *testing.T) {
	req := []byte(`{
		"model":"gpt-image-1",
		"stream":true,
		"n":2,
		"size":"1024x1024",
		"quality":"high",
		"prompt":"我的身份证号 110101199001011234 和公司机密方案",
		"messages":[
			{"role":"user","content":"这是我的私密问题：如何转移资产"},
			{"role":"user","content":[{"type":"text","text":"第二段私密内容"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}
		],
		"tools":[{"type":"function"},{"type":"function"}]
	}`)

	out := summarizeBody(req, "request")

	for _, leaked := range []string{"身份证号", "转移资产", "第二段私密内容", "公司机密"} {
		if strings.Contains(string(out), leaked) {
			t.Fatalf("meta 摘要泄漏了用户内容 %q：%s", leaked, out)
		}
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("摘要不是合法 JSON: %v (%s)", err, out)
	}
	if got["_meta"] != "request" {
		t.Fatalf("缺少 _meta 标记：%s", out)
	}
	if got["model"] != "gpt-image-1" || got["size"] != "1024x1024" || got["quality"] != "high" {
		t.Fatalf("枚举型标量应保留：%s", out)
	}
	if got["stream"] != true || got["n"] != float64(2) {
		t.Fatalf("布尔/数值标量应保留：%s", out)
	}
	if got["messages_count"] != float64(2) || got["tools_count"] != float64(2) {
		t.Fatalf("数组应只留长度：%s", out)
	}
	if got["image_count"] != float64(1) {
		t.Fatalf("应数出 1 个图片片段：%s", out)
	}
}

// TestSummarizeBodyLongStringDropped：长字符串即便是枚举键也不留
// （上游/客户端可能把 prompt 塞进任意键）。
func TestSummarizeBodyLongStringDropped(t *testing.T) {
	long := strings.Repeat("秘密", 40)
	out := string(summarizeBody([]byte(`{"object":"`+long+`"}`), "response"))
	if strings.Contains(out, "秘密") {
		t.Fatalf("超过 64 字符的字符串应按正文丢弃：%s", out)
	}
}

// TestSummarizeBodyNonJSON：非 JSON 正文只记大小，不猜内容。
func TestSummarizeBodyNonJSON(t *testing.T) {
	out := string(summarizeBody([]byte("------boundary\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\n私密文本\r\n"), "request"))
	if strings.Contains(out, "私密文本") || strings.Contains(out, "boundary") {
		t.Fatalf("非 JSON 正文不应留存任何片段：%s", out)
	}
	if !strings.Contains(out, `"format":"non-json"`) {
		t.Fatalf("应标记为非 JSON：%s", out)
	}
}

// TestSummarizeBodyKeepsUsage：响应里的 usage 是计费核对的关键，必须保留
// （只保留数值，不保留任何文本）。
func TestSummarizeBodyKeepsUsage(t *testing.T) {
	resp := []byte(`{"object":"chat.completion","model":"m1","choices":[{"message":{"content":"你好，这是回答"}}],
		"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`)
	out := summarizeBody(resp, "response")
	if strings.Contains(string(out), "这是回答") {
		t.Fatalf("响应正文不应留存：%s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("摘要不是合法 JSON: %v", err)
	}
	u, ok := got["usage"].(map[string]any)
	if !ok || u["total_tokens"] != float64(46) {
		t.Fatalf("usage 数值应保留：%s", out)
	}
	if got["choices_count"] != float64(1) {
		t.Fatalf("choices 应只留长度：%s", out)
	}
}

// 口径归一（拼错 → meta）的用例在 internal/conf：那是解析处，
// 而 bodyMeta() 读的是已加载的 Env 快照，t.Setenv 改不动它。
