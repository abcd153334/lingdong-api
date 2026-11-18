package relay

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// ───────────────────── B1.1 用量单源化 ─────────────────────

// TestUsageInfoIsBillingUsage 守住「token 字段只有一份定义」。
//
// 这条用例的存在理由：改造前 UsageInfo 手写 13 个字段，结算前再由
// toBillingUsage 逐字段搬一次，结果 CacheWrite5m/1h、ImageTokens、Semantic
// 只加在了 billing.Usage 上 —— 缓存写入与图片 token 完全免费且无任何告警。
// 只要有人再往 UsageInfo 上"顺手加一个字段"，这里必须红。
func TestUsageInfoIsBillingUsage(t *testing.T) {
	bt := reflect.TypeOf(billing.Usage{})
	ut := reflect.TypeOf(UsageInfo{})
	if ut.NumField() != 3 {
		t.Fatalf("UsageInfo 必须恰好是「嵌入 billing.Usage + Source + Raw」三个字段，实际 %d 个（多了说明有人又在抄字段）", ut.NumField())
	}
	emb := ut.Field(0)
	if !emb.Anonymous || emb.Type != bt {
		t.Fatalf("第一个字段必须是匿名嵌入 %s，实际 %s（匿名=%v）", bt, emb.Type, emb.Anonymous)
	}
	if ut.Field(1).Name != "Source" || ut.Field(2).Name != "Raw" {
		t.Fatalf("后两个字段必须固定为 Source/Raw，实际 %s/%s", ut.Field(1).Name, ut.Field(2).Name)
	}
}

// TestEnsureUsageKeepsCacheWriteLanes 验证缓存写入通道能穿过兜底逻辑。
//
// 这是「免费缓存写入」的直接回归点：字段在 ParseUsage 里解析出来了，
// 但兜底函数换了一份结构体就丢了。
func TestEnsureUsageKeepsCacheWriteLanes(t *testing.T) {
	up := UsageInfo{}
	up.PromptTokens = 1000
	up.CacheWrite5mTokens = 700
	up.CacheWrite1hTokens = 300
	up.Semantic = billing.SemanticAnthropic

	u, source, anomaly := ensureUsage(up, promptCompletionEst{}, model.BillingModeTokens, 0, false, false)
	if source != "upstream" {
		t.Fatalf("source=%s，期望 upstream", source)
	}
	if u.CacheWrite5mTokens != 700 || u.CacheWrite1hTokens != 300 {
		t.Fatalf("缓存写入通道丢失: %+v", u)
	}
	if u.Semantic != billing.SemanticAnthropic {
		t.Fatalf("语义丢失: %q", u.Semantic)
	}
	if anomaly != "" {
		t.Fatalf("语义明确时不该有异常标记，实际 %q", anomaly)
	}
}

// TestEnsureUsageFlagsUnknownSemantic 验证「有分通道用量但无语义」会被标记。
//
// 不标记的后果：某条渠道一直用 Anthropic 形状上报、语义识别失败，
// 计费按 OpenAI 解释（prompt 再减一次缓存并钳 0）→ 长期少收而无人发现。
func TestEnsureUsageFlagsUnknownSemantic(t *testing.T) {
	up := UsageInfo{}
	up.PromptTokens = 1000
	up.CacheTokens = 400
	// 刻意不设 Semantic

	_, _, anomaly := ensureUsage(up, promptCompletionEst{}, model.BillingModeTokens, 0, false, false)
	if !model.HasAnomaly(anomaly, model.AnomalyUsageSemanticUnknown) {
		t.Fatalf("应标记 %s，实际 %q", model.AnomalyUsageSemanticUnknown, anomaly)
	}

	// 纯 prompt/completion 的用量对两种语义等价，不该打标（否则全是噪音）。
	plain := UsageInfo{}
	plain.PromptTokens = 1000
	if _, _, a := ensureUsage(plain, promptCompletionEst{}, model.BillingModeTokens, 0, false, false); a != "" {
		t.Fatalf("无分通道用量不该打标，实际 %q", a)
	}
}

// TestBillingUsageChargesCacheWrite 是纯公式回归：缓存写入必须真的收钱。
//
// 价格：input 1/百万、cache_create 2/百万（微），用量 prompt=1000、cw5m=1000。
// Anthropic 语义下 base=prompt=1000（不减），金额 = 1000*1 + 1000*2 = 3000 微。
func TestBillingUsageChargesCacheWrite(t *testing.T) {
	snap := billing.Snapshot{
		BillingMode: model.BillingModeTokens,
		Price: model.Prices{
			Input:       iptrR(1_000_000),
			CacheCreate: iptrR(2_000_000),
		},
	}
	u := billing.Usage{
		PromptTokens:       1000,
		CacheWrite5mTokens: 1000,
		Semantic:           billing.SemanticAnthropic,
	}
	got, err := billing.Charge(snap, u)
	if err != nil {
		t.Fatalf("Charge 失败: %v", err)
	}
	if got != 3000 {
		t.Fatalf("charge=%d，期望 3000（1000 输入 + 1000 缓存写入×2）", got)
	}
}

// ───────────────────── B1.2 冻结估算补全 ─────────────────────

// TestEstimateCoversToolsAndImages 验证 tools 与图片进入 prompt 估算。
//
// 改造前：tools 完全没进估算、图片块被整块跳过 —— 带工具/带图的请求
// 冻结额可低一个数量级，结算被 3 倍截顶后确定性少收。
func TestEstimateCoversToolsAndImages(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"read_file","description":"` +
		strings.Repeat("x", 20000) + `","parameters":{"type":"object","properties":{}}}}]`)
	msgs := json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"看图"},` +
		`{"type":"image_url","image_url":{"url":"https://cdn.example.com/a.png"}},` +
		`{"type":"image_url","image_url":{"url":"https://cdn.example.com/b.png"}}]}]`)

	p, _ := estimateTokens(relayHead{Messages: msgs, Tools: tools}, kindChat, 0)
	// 20000 ASCII ≈ 5715 token；2 张图 = 2048；文本 2 字 CJK ≈ 4；1 条消息 +4。
	const floor = int64(5715 + 2048)
	if p < floor {
		t.Fatalf("prompt 估算=%d，应 ≥ %d（tools 与图片必须计入）", p, floor)
	}

	// 图片不能下钻成 URL 字符串：否则一张图按 URL 长度计几个 token。
	onlyImg := json.RawMessage(`[{"role":"user","content":[{"type":"image_url",` +
		`"image_url":{"url":"https://cdn.example.com/` + strings.Repeat("y", 4000) + `.png"}}]}]`)
	pi, _ := estimateTokens(relayHead{Messages: onlyImg}, kindChat, 0)
	if pi > tokensPerImagePart+tokensPerMessage+8 {
		t.Fatalf("图片应按 %d token 计（且不计 URL 本体），实际 %d", tokensPerImagePart, pi)
	}
}

// TestEstimateUsesMaxCompletionTokens 验证三个输出上限字段的处理。
//
// max_completion_tokens 曾被解析却完全没用：按对外文档传参的用户
// 仍按 1024 冻结，与文档承诺相反。
func TestEstimateUsesMaxCompletionTokens(t *testing.T) {
	cases := []struct {
		name string
		h    relayHead
		want int64
	}{
		{"只传 max_completion_tokens", relayHead{MaxCompletionTokens: 8192}, 8192},
		{"三源取最大", relayHead{MaxTokens: 100, MaxCompletionTokens: 200, MaxOutputTokens: 300}, 300},
		{"超上限钳制", relayHead{MaxCompletionTokens: 99999}, maxCompletionCap},
		{"全部缺省", relayHead{}, defaultMaxOutputTokens},
	}
	for _, c := range cases {
		if _, got := estimateTokens(c.h, kindChat, 0); got != c.want {
			t.Fatalf("%s: completion=%d，期望 %d", c.name, got, c.want)
		}
	}
}

// TestEstimateFallsBackToModelMaxOutput 验证「模型声明的输出上限」被采用。
//
// 请求没写上限时按模型能力冻结，而不是写死的缺省值：
// 多冻的钱结算时原路退回，少冻的钱会被截顶吃掉（确定性少收）。
func TestEstimateFallsBackToModelMaxOutput(t *testing.T) {
	if _, got := estimateTokens(relayHead{}, kindChat, 16384); got != 16384 {
		t.Fatalf("completion=%d，期望采用模型声明的 16384", got)
	}
	// 请求显式声明时优先用它，不被模型上限覆盖。
	if _, got := estimateTokens(relayHead{MaxTokens: 512}, kindChat, 16384); got != 512 {
		t.Fatalf("显式 max_tokens 应优先，实际 %d", got)
	}
	// 模型声明超过封顶时仍受 maxCompletionCap 约束。
	if _, got := estimateTokens(relayHead{}, kindChat, 999999); got != maxCompletionCap {
		t.Fatalf("应钳到 %d，实际 %d", maxCompletionCap, got)
	}
}

// TestEstimateCoversChargeMatrix 是性质测试：只要实际用量不超过估算，
// 结算额就必须不超过冻结额（否则每条请求都会被截顶少收）。
//
// 只在 OpenAI 语义下成立：OpenAI 的 prompt_tokens 已含缓存/图片/音频通道，
// 而 Anthropic 语义的 input 不含缓存，缓存是**额外**通道 —— 那种形态由
// 上游明确上报，冻结侧无法预知，属于已知边界（见方案 §2.2 的说明）。
func TestEstimateCoversChargeMatrix(t *testing.T) {
	snap := billing.Snapshot{
		BillingMode: model.BillingModeTokens,
		Price: model.Prices{
			Input:         iptrR(2_000_000),
			Output:        iptrR(8_000_000),
			Cache:         iptrR(1_000_000),
			CacheCreate:   iptrR(4_000_000),
			CacheCreate1h: iptrR(5_000_000),
			Image:         iptrR(3_000_000),
			Audio:         iptrR(6_000_000),
			AudioOutput:   iptrR(7_000_000),
		},
	}
	const promptEst = int64(40_000)
	const compEst = int64(8_000)
	est, err := billing.Estimate(snap, promptEst, compEst, billing.Usage{})
	if err != nil {
		t.Fatalf("Estimate 失败: %v", err)
	}

	cases := []struct {
		name string
		u    billing.Usage
	}{
		{"纯输入输出", billing.Usage{PromptTokens: promptEst, CompletionTokens: compEst}},
		{"缓存命中占大头", billing.Usage{PromptTokens: promptEst, CacheTokens: promptEst, CompletionTokens: 100}},
		{"缓存写入 5m", billing.Usage{PromptTokens: promptEst, CacheWrite5mTokens: promptEst / 2}},
		{"缓存写入 1h", billing.Usage{PromptTokens: promptEst, CacheWrite1hTokens: promptEst / 2}},
		{"图片 token", billing.Usage{PromptTokens: promptEst, ImageTokens: promptEst}},
		{"音频输入输出", billing.Usage{PromptTokens: promptEst, AudioInputTokens: promptEst, AudioOutputTokens: compEst}},
		{"全通道混合", billing.Usage{
			PromptTokens: promptEst, CompletionTokens: compEst,
			CacheTokens: 1000, CacheWrite5mTokens: 1000, CacheWrite1hTokens: 1000,
			ImageTokens: 1000, AudioInputTokens: 1000, AudioOutputTokens: 1000,
		}},
		{"零用量", billing.Usage{}},
	}
	for _, c := range cases {
		charge, err := billing.Charge(snap, c.u)
		if err != nil {
			t.Fatalf("%s: Charge 失败: %v", c.name, err)
		}
		if charge > est {
			t.Fatalf("%s: charge=%d > 冻结额 %d（估算覆盖不足，会被截顶少收）", c.name, charge, est)
		}
	}
}

// ───────────────────── B1.3 流式输出口径 ─────────────────────

// TestStreamOutputTokensCountsToolArguments 验证工具调用分片被计入输出。
//
// 纯工具调用（Agent 场景）里 arguments 常占输出绝大部分；
// 只统计 delta.content 会让 completion 记 0 —— 工具参数完全免费。
func TestStreamOutputTokensCountsToolArguments(t *testing.T) {
	f1 := []byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`)
	f2 := []byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`)
	// 旧形态 function_call
	f3 := []byte(`{"choices":[{"delta":{"function_call":{"arguments":"{\"q\":1}"}}}]}`)

	sum := streamOutputTokens(f1) + streamOutputTokens(f2) + streamOutputTokens(f3)
	if sum <= 0 {
		t.Fatal("工具调用的 arguments 必须计入输出 token（实际 0）")
	}
	// 两帧拼接后的完整参数 ≈ 13 字符，加上函数名 9 字符，量级必须对得上。
	if sum < 5 {
		t.Fatalf("输出估算=%d，明显偏低（工具参数与函数名都应计入）", sum)
	}

	// 普通文本帧仍然正常统计。
	plain := []byte(`{"choices":[{"delta":{"content":"` + strings.Repeat("字", 40) + `"}}]}`)
	if got := streamOutputTokens(plain); got < 40 {
		t.Fatalf("文本输出估算=%d，应 ≥40", got)
	}
}

// ───────────────────── messages 与 chat 必须同口径 ─────────────────────

// TestEstimateMessagesNonZero 守住「Claude 入站不会冻结 0」。
//
// 这是最容易漏的一处：estimateTokens 的 switch 按端点枚举取值，
// 新增 kindMessages 后若忘了加 case，会直接返回 (0,0) —— 冻结 0 元，
// 而结算侧 chargeCapFor(0) 只允许多收一点点，等于长回答几乎免费。
func TestEstimateMessagesNonZero(t *testing.T) {
	msgs := json.RawMessage(`[{"role":"user","content":"你好"}]`)
	pChat, cChat := estimateTokens(relayHead{Messages: msgs, MaxTokens: 1000}, kindChat, 0)
	pMsg, cMsg := estimateTokens(relayHead{Messages: msgs, MaxTokens: 1000}, kindMessages, 0)
	if pMsg == 0 || cMsg == 0 {
		t.Fatalf("messages 的估算不能为 0：prompt=%d completion=%d", pMsg, cMsg)
	}
	if pMsg != pChat || cMsg != cChat {
		t.Fatalf("messages 与 chat 的估算必须一致：(%d,%d) vs (%d,%d)", pMsg, cMsg, pChat, cChat)
	}
}

// TestMessagesBillsLikeChat 守住「协议变体不改变计费」。
//
// 两个端点的请求体在解码后完全相同，因此计费结果必须逐字段相等。
// 一旦有人给 messages 单独写计费分支，这条会立刻红。
func TestMessagesBillsLikeChat(t *testing.T) {
	up := UsageInfo{}
	up.PromptTokens = 1200
	up.CompletionTokens = 300
	up.CacheTokens = 200
	up.Semantic = billing.SemanticOpenAI

	snap := billing.Snapshot{
		BillingMode: model.BillingModeTokens,
		Price:       model.Prices{Input: iptrR(1_000_000), Output: iptrR(2_000_000), Cache: iptrR(500_000)},
	}
	est := promptCompletionEst{Prompt: 5000, Completion: 2000}

	chatUsage, chatSrc, chatAnom := buildBillingUsage(kindChat, up, est, model.BillingModeTokens,
		billing.Usage{}, nil, 0, false, false, 0)
	msgUsage, msgSrc, msgAnom := buildBillingUsage(kindMessages, up, est, model.BillingModeTokens,
		billing.Usage{}, nil, 0, false, false, 0)

	if chatSrc != msgSrc || chatAnom != msgAnom {
		t.Fatalf("来源/异常标记必须一致：%q/%q vs %q/%q", chatSrc, chatAnom, msgSrc, msgAnom)
	}
	chatCharge, err := billing.Charge(snap, chatUsage)
	if err != nil {
		t.Fatalf("chat 计费失败: %v", err)
	}
	msgCharge, err := billing.Charge(snap, msgUsage)
	if err != nil {
		t.Fatalf("messages 计费失败: %v", err)
	}
	if chatCharge != msgCharge {
		t.Fatalf("同一用量在 chat 与 messages 上的金额必须相同：%d vs %d", chatCharge, msgCharge)
	}
	if msgCharge == 0 {
		t.Fatal("计费金额不能为 0（说明用量没进公式）")
	}
}
