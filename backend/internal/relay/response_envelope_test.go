package relay

import "testing"

// TestJudgeErrorEnvelope 守「2xx 但响应体是错误」的判据边界。
//
// 这条判据的代价不对称：**误判失败**会让一次成功的调用变成 502（用户白跑、还可能要重试），
// **漏判**会让用户什么都没拿到却被扣费。所以判据刻意做窄 ——
// 只有「顶层有 error」且「没有任何成功形状」才算失败。
func TestJudgeErrorEnvelope(t *testing.T) {
	failed := []struct {
		name string
		body string
		want string
	}{
		{
			name: "视觉模型取图失败（本次故障的形状）",
			body: `{"error":{"message":"messages[1].image[0]: You have uploaded an unsupported image","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`,
			want: "invalid_request_error: messages[1].image[0]: You have uploaded an unsupported image",
		},
		{
			name: "error 是字符串",
			body: `{"error":"upstream boom"}`,
			want: "upstream boom",
		},
		{
			name: "只有 type 没有 code",
			body: `{"error":{"message":"rate limited","type":"rate_limit"}}`,
			want: "rate_limit: rate limited",
		},
		{
			name: "有 error 但没有任何成功形状",
			body: `{"error":{"message":"moderation"},"choices":[]}`,
			want: "moderation",
		},
	}

	for _, c := range failed {
		t.Run("失败/"+c.name, func(t *testing.T) {
			msg, isErr := judgeErrorEnvelope([]byte(c.body))
			if !isErr {
				t.Fatalf("应判为失败：%s", c.body)
			}
			if msg != c.want {
				t.Fatalf("文案应为 %q，实际 %q", c.want, msg)
			}
		})
	}

	ok := []struct {
		name string
		body string
	}{
		{"正常 chat 响应", `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`},
		{"部分成功 + 顶层 error", `{"error":{"message":"one image failed"},"choices":[{"index":0,"message":{"content":"ok"}}]}`},
		{"embeddings", `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":3}}`},
		{"rerank", `{"results":[{"index":0,"relevance_score":0.9}]}`},
		{"responses 成功形状", `{"object":"response","output":[],"error":null}`},
		{"error 为 null", `{"error":null,"choices":[{"index":0}]}`},
		{"没有 error 字段", `{"choices":[{"index":0}]}`},
		{"空 body", ``},
		{"非 JSON（二进制/HTML）", `<html>502 Bad Gateway</html>`},
	}

	for _, c := range ok {
		t.Run("非失败/"+c.name, func(t *testing.T) {
			if msg, isErr := judgeErrorEnvelope([]byte(c.body)); isErr {
				t.Fatalf("不应判为失败（会误伤正常响应）：%s → %q", c.body, msg)
			}
		})
	}
}
