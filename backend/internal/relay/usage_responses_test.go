package relay

import (
	"encoding/json"
	"testing"

	"lingdong-api/internal/service/billing"
)

// TestUsageFromResponses 守住「Responses 的 input_tokens 已含缓存」这条语义。
//
// 判错（当成 Anthropic）的后果是缓存部分按全价再收一遍：billing 的 Anthropic 分支
// 认为 prompt 不含 cache，于是 CacheTokens 只被折价一次、PromptTokens 也照全额收 ——
// 每一笔带缓存的上游调用都多收，而账面一切正常。
func TestUsageFromResponses(t *testing.T) {
	raw := json.RawMessage(`{"input_tokens":120,"output_tokens":30,"total_tokens":150,
		"input_tokens_details":{"cached_tokens":40},"output_tokens_details":{"reasoning_tokens":8}}`)
	u, ok := usageFromResponses(raw)
	if !ok {
		t.Fatal("应识别为 Responses usage")
	}
	if u.Semantic != billing.SemanticOpenAI {
		t.Fatalf("语义应为 OpenAI，实际 %v", u.Semantic)
	}
	if u.PromptTokens != 120 || u.CompletionTokens != 30 || u.CacheTokens != 40 {
		t.Fatalf("字段映射不符: %+v", u)
	}
}

// TestParseUsageDistinguishesResponsesFromAnthropic 守住「窄判别」：
// 只有出现 Responses 独有的键（*_tokens_details / total_tokens）才按 OpenAI 语义。
// 放宽成"有 input_tokens 就算"会把 Anthropic 的真实用量算错，那是反方向的错账。
func TestParseUsageDistinguishesResponsesFromAnthropic(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		semantic billing.Semantic
		cache    int64
	}{
		{
			name:     "responses（带 details）",
			body:     `{"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":25}}}`,
			semantic: billing.SemanticOpenAI,
			cache:    25,
		},
		{
			name:     "responses（只有 total_tokens）",
			body:     `{"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110}}`,
			semantic: billing.SemanticOpenAI,
		},
		{
			name:     "anthropic（无 details）",
			body:     `{"usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":25}}`,
			semantic: billing.SemanticAnthropic,
			cache:    25,
		},
		{
			name:     "openai chat（有 prompt_tokens）",
			body:     `{"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":25}}}`,
			semantic: billing.SemanticOpenAI,
			cache:    25,
		},
	}
	for _, c := range cases {
		u := ParseUsage([]byte(c.body))
		if u.Semantic != c.semantic {
			t.Errorf("%s: 语义 %v，期望 %v", c.name, u.Semantic, c.semantic)
		}
		if u.CacheTokens != c.cache {
			t.Errorf("%s: 缓存 %d，期望 %d", c.name, u.CacheTokens, c.cache)
		}
		if u.PromptTokens != 100 || u.CompletionTokens != 10 {
			t.Errorf("%s: prompt/completion 不符: %+v", c.name, u)
		}
	}
}
