package relay

import (
	"encoding/json"

	"lingdong-api/internal/service/billing"
)

// 本文件是 Responses 形状 usage 的唯一归一处（非流式与流式共用，否则同模型两条口径）。
//
// 字段差异（判错的后果是**系统性多收**）：
//   - Responses：`input_tokens` **已包含** cached 部分（与 OpenAI 的 prompt_tokens 同义）；
//   - Anthropic：`input_tokens` **不含** cache_read，缓存部分要另加。
//
// 所以 Responses 的 usage 必须归一到 OpenAI 语义：缓存命中按 cached 部分折价，
// 而不是按全价再收一遍。归到 Anthropic 的后果是 billing/formula.go 的 Anthropic 分支
// 不再扣 cache —— 每一笔带缓存的上游调用都多收，而账面上一切正常。

// responsesUsage 是 Responses usage 的结构化视图（指针用于区分「字段缺失」与「值为 0」）。
type responsesUsage struct {
	InputTokens   *int64 `json:"input_tokens"`
	OutputTokens  *int64 `json:"output_tokens"`
	TotalTokens   *int64 `json:"total_tokens"`
	InputDetails  *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// looksLikeResponsesUsage 是**窄判别**：只有出现 Responses 独有的键才算。
//
// 判别必须窄 —— Anthropic 永远没有 `*_tokens_details`，也没有 `total_tokens`。
// 放宽成"有 input_tokens 就算"会把 Anthropic 的真实用量按 OpenAI 语义算，
// 那是反方向的错账（缓存被重复折价，少收）。
func looksLikeResponsesUsage(u responsesUsage) bool {
	return u.InputDetails != nil || u.OutputDetails != nil || u.TotalTokens != nil
}

// usageFromResponses 把 Responses 形状的 usage 归一成 OpenAI 语义。
//
// 返回 ok=false 表示这段 JSON 不是 Responses usage（调用方继续按 Anthropic 处理）。
func usageFromResponses(raw json.RawMessage) (UsageInfo, bool) {
	if len(raw) == 0 {
		return UsageInfo{}, false
	}
	var u responsesUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		return UsageInfo{}, false
	}
	if u.InputTokens == nil && u.OutputTokens == nil {
		return UsageInfo{}, false
	}
	if !looksLikeResponsesUsage(u) {
		return UsageInfo{}, false
	}
	out := UsageInfo{}
	out.Semantic = billing.SemanticOpenAI
	if u.InputTokens != nil {
		out.PromptTokens = *u.InputTokens
	}
	if u.OutputTokens != nil {
		// output_tokens 已含推理 token（ReasoningTokens 只用于解释，不单列计费通道）。
		out.CompletionTokens = *u.OutputTokens
	}
	if d := u.InputDetails; d != nil && d.CachedTokens != nil {
		// 缓存命中量：OpenAI 语义下 prompt 已含它，计费时按 cached 单价折出来。
		out.CacheTokens = *d.CachedTokens
	}
	return out, true
}
