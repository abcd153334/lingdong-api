package relay

import (
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

func TestParseUsageOpenAI(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":500,
		"prompt_tokens_details":{"cached_tokens":400,"image_tokens":20,"audio_tokens":10},
		"completion_tokens_details":{"audio_tokens":5}}}`)
	u := ParseUsage(body)
	if u.Semantic != billing.SemanticOpenAI {
		t.Fatalf("semantic=%s", u.Semantic)
	}
	if u.PromptTokens != 1000 || u.CompletionTokens != 500 || u.CacheTokens != 400 ||
		u.ImageTokens != 20 || u.AudioInputTokens != 10 || u.AudioOutputTokens != 5 {
		t.Fatalf("字段解析不符: %+v", u)
	}
	if u.Raw == "" {
		t.Fatal("Raw 不应为空")
	}
	if !u.HasTokenUsage() {
		t.Fatal("HasTokenUsage 应为 true")
	}
}

func TestParseUsageAnthropic(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,
		"cache_read_input_tokens":800,"cache_creation_input_tokens":300,
		"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":100}}}`)
	u := ParseUsage(body)
	if u.Semantic != billing.SemanticAnthropic {
		t.Fatalf("semantic=%s", u.Semantic)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.CacheTokens != 800 {
		t.Fatalf("字段解析不符: %+v", u)
	}
	if u.CacheWrite5mTokens != 200 || u.CacheWrite1hTokens != 100 {
		t.Fatalf("缓存写入分档不符: %+v", u)
	}
}

func TestParseUsageMissing(t *testing.T) {
	u := ParseUsage([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	if u.HasTokenUsage() || u.Semantic != "" {
		t.Fatalf("无 usage 应返回零值: %+v", u)
	}
}

func TestEstimatePromptTokensCJK(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("中文测试", 25) + `"}]}`)
	got := estimatePromptTokens(raw)
	if got < 100 {
		t.Fatalf("100 汉字估算=%d，应 ≥100（不得低估中文）", got)
	}
}

func TestObservedOutputTokens(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"` + strings.Repeat("字", 40) + `"}}]}`)
	got := observedOutputTokens(body)
	if got < 40 {
		t.Fatalf("输出估算=%d，应 ≥40", got)
	}
	// 元数据不得计入
	if observedOutputTokens([]byte(`{"id":"x","model":"gpt-4o","object":"chat"}`)) != 0 {
		t.Fatal("元数据不应计入输出")
	}
}

// 计数已并入 judgeImageResponse（判据与计数同源，见 image_verdict.go）：
// 这里守的是「空值不算交付」这条不变的语义。
func TestImageDeliveredCount(t *testing.T) {
	body := []byte(`{"data":[{"url":"a"},{"b64_json":"b"},{"url":""},{"b64_json":""}]}`)
	if got := judgeImageResponse(body).Count; got != 2 {
		t.Fatalf("返回张数=%d，期望 2", got)
	}
	if got := judgeImageResponse([]byte(`{"data":[]}`)).Count; got != 0 {
		t.Fatalf("空数组应为 0，实际 %d", got)
	}
}

func TestMergeUsageMax(t *testing.T) {
	dst := UsageInfo{Usage: billing.Usage{PromptTokens: 100, CompletionTokens: 0, Semantic: billing.SemanticOpenAI}}
	src := UsageInfo{Usage: billing.Usage{PromptTokens: 0, CompletionTokens: 50}}
	got := MergeUsageMax(dst, src)
	if got.PromptTokens != 100 || got.CompletionTokens != 50 {
		t.Fatalf("合并结果不符: %+v", got)
	}
	if got.Semantic != billing.SemanticOpenAI {
		t.Fatalf("语义丢失: %+v", got)
	}
}

func TestEnsureUsageMatrix(t *testing.T) {
	est := promptCompletionEst{Prompt: 300, Completion: 1024}
	cases := []struct {
		name     string
		up       UsageInfo
		mode     string
		observed int64
		aborted  bool
		source   string
		minQuota int64 // Charge 后的最小额度（模型倍率 1 时等于 token 总数）
		flag     string
	}{
		{name: "完整上游用量", up: UsageInfo{Usage: billing.Usage{PromptTokens: 100, CompletionTokens: 50}}, mode: model.BillingModeTokens, source: "upstream", minQuota: 150},
		{name: "有prompt缺completion", up: UsageInfo{Usage: billing.Usage{PromptTokens: 100}}, mode: model.BillingModeTokens, observed: 40, source: "mixed", minQuota: 140, flag: "usage_estimated"},
		{name: "完全无usage有输出", up: UsageInfo{}, mode: model.BillingModeTokens, observed: 80, source: "estimated", minQuota: 380, flag: "usage_estimated"},
		{name: "无usage无输出", up: UsageInfo{}, mode: model.BillingModeTokens, source: "estimated", minQuota: 300, flag: "output_unmeasured"},
		{name: "客户端断开且无输出", up: UsageInfo{}, mode: model.BillingModeTokens, aborted: true, source: "estimated", minQuota: 301, flag: "client_abort"},
		{name: "按规格不按token", up: UsageInfo{}, mode: model.BillingModeSpec, source: "none", minQuota: 0},
	}
	// $1 / 1M tokens：charge 的微数恰好等于 token 数，断言可读。
	snap := billing.Snapshot{BillingMode: model.BillingModeTokens,
		Price: model.Prices{Input: iptrR(1_000_000), Output: iptrR(1_000_000)}}
	for _, c := range cases {
		u, source, anomaly := ensureUsage(c.up, est, c.mode, c.observed, false, c.aborted)
		if source != c.source {
			t.Fatalf("%s: source=%s，期望 %s", c.name, source, c.source)
		}
		if c.flag != "" && !strings.Contains(anomaly, c.flag) {
			t.Fatalf("%s: 缺少标记 %s（实际 %q）", c.name, c.flag, anomaly)
		}
		if c.minQuota > 0 {
			got, err := billing.Charge(snap, u)
			if err != nil {
				t.Fatalf("%s: Charge 失败 %v", c.name, err)
			}
			if got < c.minQuota {
				t.Fatalf("%s: charge=%d，期望 ≥%d", c.name, got, c.minQuota)
			}
		}
	}
}

func iptrR(v int64) *int64 { return &v }
