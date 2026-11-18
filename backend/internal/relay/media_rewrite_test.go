package relay

import (
	"bytes"
	"strings"
	"testing"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/mediaurl"
)

const testBase = "https://api.example.com"

func TestShapeResponseRewritesImageURLsOnly(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	conf.Load()
	body := []byte(`{"data":[{"url":"https://cdn.example.com/a.png"}]}`)

	out := shapeResponse(kindImagesGenerations, "m", "r1", testBase, body)
	if out == nil {
		t.Fatal("图像端点应改写")
	}
	if strings.Contains(string(out), "cdn.example.com") {
		t.Fatalf("上游域名仍然出现在响应里: %s", out)
	}
	if !strings.Contains(string(out), "/v1/media/") {
		t.Fatalf("没有换成本站能力 URL: %s", out)
	}
	// 对话响应里的图片地址是模型输出的一部分，改写它等于篡改回答内容。
	for _, k := range []endpointKind{kindChat, kindCompletions, kindEmbeddings, kindRerank} {
		chat := []byte(`{"id":"x","choices":[{"message":{"content":"see https://cdn.example.com/a.png"}}]}`)
		if got := shapeResponse(k, "m", "r1", testBase, chat); got != nil && strings.Contains(string(got), "/v1/media/") {
			t.Fatalf("端点 %d 不该改写正文里的地址: %s", int(k), got)
		}
	}
}

// TestShapeResponseSkipsWithoutBase 守住「推不出对外地址就不改写」。
//
// 拼成相对地址会让 SDK 拿到一个无法直接访问的 url，比保留上游地址更糟。
func TestShapeResponseSkipsWithoutBase(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	conf.Load()
	body := []byte(`{"data":[{"url":"https://cdn.example.com/a.png"}]}`)
	if out := shapeResponse(kindImagesGenerations, "m", "r1", "", body); out != nil {
		t.Fatalf("没有基址时不应改写: %s", out)
	}
}

// TestShapeResponseKeepsB64 守住 b64_json 分支不被碰。
func TestShapeResponseKeepsB64(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	conf.Load()
	body := []byte(`{"data":[{"b64_json":"AAAA"}]}`)
	if out := shapeResponse(kindImagesGenerations, "m", "r1", testBase, body); out != nil {
		t.Fatal("b64_json 响应不应改写")
	}
}

// TestShapeResponseNormalizesIDAndModel 守 id/model 归一。
//
// 三件事各自是一个真实故障：
//   - id 透传 = 把上游请求号交给终端用户（对方后台可凭它查单）；
//   - model 不回填 = 渠道配了别名时客户端看到的模型名与它请求的不是同一个；
//   - 顺手补 created = 往规范里没有这个键的端点（/v1/responses）塞字段。
func TestShapeResponseNormalizesIDAndModel(t *testing.T) {
	body := []byte(`{"id":"up-123","object":"chat.completion","created":1700000000,` +
		`"model":"doubao-seed-x","usage":{"prompt_tokens":3},"choices":[]}`)

	out := shapeResponse(kindChat, "gpt-4o", "abc123", "", body)
	if out == nil {
		t.Fatal("应改写")
	}
	s := string(out)
	for _, want := range []string{`"id":"chatcmpl-abc123"`, `"model":"gpt-4o"`, `"created":1700000000`} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %s：%s", want, s)
		}
	}
	// usage 一个字节都不能动：上游没报与报了 0 是两种事实，对账结论相反。
	if !strings.Contains(s, `"usage":{"prompt_tokens":3}`) {
		t.Fatalf("usage 被改动了：%s", s)
	}
	// choices 里的嵌套 id 不是响应 id，不能改。
	nested := []byte(`{"id":"a","choices":[{"id":"nested-id"}]}`)
	if got := string(shapeResponse(kindChat, "m", "x", "", nested)); !strings.Contains(got, `"nested-id"`) {
		t.Fatalf("嵌套 id 被误改：%s", got)
	}
}

// TestShapeResponseResponsesKeepsCreatedAt 守「不伪造 created」。
func TestShapeResponseResponsesKeepsCreatedAt(t *testing.T) {
	body := []byte(`{"id":"resp_up","object":"response","created_at":1700000000,"model":"up-m"}`)
	out := shapeResponse(kindResponses, "gpt-4o", "rid", "", body)
	if out == nil {
		t.Fatal("应改写 id/model")
	}
	s := string(out)
	if strings.Contains(s, `"created"`) {
		t.Fatalf("Responses 响应不该出现 created：%s", s)
	}
	if !strings.Contains(s, `"created_at":1700000000`) || !strings.Contains(s, `"id":"resp_rid"`) {
		t.Fatalf("created_at 应原样保留、id 应带 resp_ 前缀：%s", s)
	}
}

// TestShapeResponseLeavesClaudeAlone 守「编码器负责的端点不归一」。
func TestShapeResponseLeavesClaudeAlone(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","model":"up","content":[]}`)
	if out := shapeResponse(kindMessages, "m", "r", "", body); out != nil {
		t.Fatalf("messages 由编码器出形状，不该在这里改：%s", out)
	}
}

// TestShapeResponseLeavesNonJSON 守「改不动就原样交付」。
func TestShapeResponseLeavesNonJSON(t *testing.T) {
	body := []byte("not json")
	if out := shapeResponse(kindChat, "m", "r", "", body); out != nil {
		t.Fatalf("非 JSON 不该被改写: %s", out)
	}
}

// TestShapeFrameKeepsUnknownKeys 流式帧：只改 id/model，其它键逐字节保留。
func TestShapeFrameKeepsUnknownKeys(t *testing.T) {
	frame := []byte(`{"id":"up","object":"chat.completion.chunk","model":"up-m","choices":[{"delta":{"content":"hi"}}]}`)
	out := shapeFrame(kindChat, "gpt-4o", "rid", frame)
	if !bytes.Contains(out, []byte(`"id":"chatcmpl-rid"`)) || !bytes.Contains(out, []byte(`"model":"gpt-4o"`)) {
		t.Fatalf("id/model 未归一: %s", out)
	}
	if !bytes.Contains(out, []byte(`"delta":{"content":"hi"}`)) {
		t.Fatalf("未知字段被吞: %s", out)
	}
	// [DONE] 由调用方拦掉，这里只保证非 JSON 原样返回。
	if got := shapeFrame(kindChat, "m", "r", []byte("[DONE]")); string(got) != "[DONE]" {
		t.Fatalf("非 JSON 帧应原样返回: %s", got)
	}
	// Claude 帧不走这里（kind 不在表里）。
	if got := shapeFrame(kindMessages, "m", "r", frame); !bytes.Equal(got, frame) {
		t.Fatal("messages 帧不该被改")
	}
}

// TestShapeRespIDStableFormat 守 id 主体是 24 位十六进制且每次不同。
func TestShapeRespIDStableFormat(t *testing.T) {
	a, b := shapeRespID(), shapeRespID()
	if len(a) != 24 {
		t.Fatalf("id 主体长度 %d，期望 24: %q", len(a), a)
	}
	if a == b {
		t.Fatal("两次生成的 id 不应相同")
	}
	if _, err := mediaurl.Verify(a); err == nil {
		t.Fatal("响应 id 不该能被当作媒体能力 token 解出来")
	}
}
