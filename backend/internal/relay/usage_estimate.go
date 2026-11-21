package relay

import (
	"encoding/json"
	"lingdong-api/internal/model"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"lingdong-api/internal/service/billing"
)

// MaxUpstreamBodyBytes 是读取**JSON** 上游响应体的上限（超过则视为截断，usage 可能不可解析）。
const MaxUpstreamBodyBytes = 8 << 20

// MaxBinaryBodyBytes 是读取**二进制**产物的上限（音频/图片）。
//
// 与 JSON 上限分开：一段 10 分钟 WAV 就有十几 MB，按 8 MiB 读会把正常音频
// 判成"上游体过大"（用户看到 502，实际只是音频长了）。
// 单一来源：适配器的 TTS 流、规格的同步/桥接下载都引用它。
const MaxBinaryBodyBytes = 32 << 20

// MaxImageBodyBytes 是图片端点**同步 JSON** 响应体的上限。
//
// 与 chat 的 8 MiB 分开：出图接口默认回 b64_json，且 base64 有 4/3 放大 ——
// 一张 1792×1024 的 PNG 编码后就可能越过 8 MiB，4K 组图可达数十 MiB。
// 按 8 MiB 读会被判成"上游体过大"，用户看到 502，实际只是图大了。
//
// 取值与 Ark 适配器既有的 arkMaxUpstreamBody 一致（64 MiB），不制造第二套口径。
const MaxImageBodyBytes = 64 << 20

// BodyCapFor 按端点标识（path 或 spec 的端点 key，如 "images/generations"）
// 返回该端点应有的响应体上限。
//
// 唯一判据：端点族带 "images/"，其余一律按 JSON 的 8 MiB。
// 注意**不要改 MaxUpstreamBodyBytes 本身** —— spec/bridge_test.go 明确锁住
// 「chat/completions 端点超 8 MiB 必须报 ErrUpstreamBodyTooLarge」。
func BodyCapFor(endpoint string) int64 {
	if strings.Contains(endpoint, "images/") {
		return MaxImageBodyBytes
	}
	return MaxUpstreamBodyBytes
}

// promptCompletionEst 是预扣时使用的估算 token（Prompt 为请求侧估算，Completion 为 max_tokens 最坏值）。
type promptCompletionEst struct {
	Prompt     int64
	Completion int64
}

// estimateTextTokens 估算一段文本的 token 数（系数见 estimate_consts.go）。
// 相比 len(bytes)/4，中文不再被系统性低估。
func estimateTextTokens(s string) int64 {
	if s == "" {
		return 0
	}
	var cjk, other int64
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	v := float64(cjk)*tokensPerCJKChar + float64(other)/charsPerTokenASCII
	return int64(math.Ceil(v))
}

// isCJK 判断是否为中日韩字符（含全角标点）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

// estimatePromptTokens 估算请求侧 prompt token：遍历 JSON 字符串叶子，
// 图片/音频按各自的折算口径计，另按 messages 数组元素数补 tokensPerMessage/条。
//
// 关键：图片与音频**不能**当成普通字符串下钻。此前 `image_url`/`url` 被整个跳过，
// 带图请求的视觉 token 完全不计；`input_audio.data`（base64）同样跳过，
// 于是「带图/带音频」的请求冻结额远低于实际输入，结算被 3 倍截顶 → 确定性少收。
func estimatePromptTokens(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return estimateTextTokens(string(raw))
	}
	var total int64
	messages := 0
	var walk func(node any, key string)
	walk = func(node any, key string) {
		switch t := node.(type) {
		case string:
			if skipEstimateKey(key) || strings.HasPrefix(t, "data:") {
				return
			}
			total += estimateTextTokens(t)
		case []any:
			// 根节点即 messages 数组（relayHead.Messages 直接传数组）时按消息条数计开销。
			if key == "messages" || key == "input" || key == "" {
				messages += len(t)
			}
			for _, it := range t {
				walk(it, key)
			}
		case map[string]any:
			// 多模态块整体折算，不下钻内部字段：
			// 下钻只会得到 base64/URL 这类被跳过或长度失真的字符串。
			if n, ok := mediaPartTokens(t); ok {
				total += n
				return
			}
			for k, it := range t {
				walk(it, k)
			}
		}
	}
	walk(v, "")
	return total + int64(messages)*tokensPerMessage
}

// mediaPartTokens 识别多模态块并返回其折算 token；不是媒体块时 ok=false。
//
// 覆盖三种上游形态：
//   - OpenAI chat：`{"type":"image_url","image_url":{"url":...}}`
//   - Claude 转换后 / 部分兼容网关：`{"type":"image","source":{...}}`
//   - 音频输入：`{"type":"input_audio","input_audio":{"data":"<base64>"}}`
func mediaPartTokens(m map[string]any) (int64, bool) {
	typ, _ := m["type"].(string)
	switch typ {
	case "image_url", "image":
		return tokensPerImagePart, true
	case "input_audio":
		// 音频：按 base64 长度反推字节数，再按每秒 token 上限折算。
		if inner, ok := m["input_audio"].(map[string]any); ok {
			if data, ok := inner["data"].(string); ok && data != "" {
				// base64 每 4 字符 3 字节；不精确解码，估算不需要。
				bytes := int64(len(data)) / 4 * 3
				secs := bytes / audioBytesPerSecond
				if secs < 1 {
					secs = 1
				}
				return secs * tokensPerAudioSecond, true
			}
		}
		// 只给了 url 的音频：拿长度换不到字节数，按 1 秒下限计（偏保守但非 0）。
		return tokensPerAudioSecond, true
	}
	return 0, false
}

// skipEstimateKey 跳过不应计入 token 的字段（二进制/URL/元数据）。
func skipEstimateKey(key string) bool {
	switch key {
	case "image_url", "url", "b64_json", "file_data", "input_audio", "data", "id", "type", "role", "model":
		return true
	}
	return false
}

// observedOutputTokens 从响应体中提取可观测输出并估算 token（content/text/arguments 等）。
// 用于上游未返回 usage 时的兜底；无任何可观测输出返回 0。
func observedOutputTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	var total int64
	var walk func(node any, key string)
	walk = func(node any, key string) {
		switch t := node.(type) {
		case string:
			if countOutputKey(key) {
				total += estimateTextTokens(t)
			}
		case []any:
			for _, it := range t {
				walk(it, key)
			}
		case map[string]any:
			for k, it := range t {
				walk(it, k)
			}
		}
	}
	walk(v, "")
	return total
}

// countOutputKey 仅统计真正的模型输出字段，避免把元数据算成输出。
func countOutputKey(key string) bool {
	switch key {
	case "content", "text", "reasoning_content", "arguments", "output_text":
		return true
	}
	return false
}

// CountStreamOutputTokens 统计流式帧中的输出 token（供适配器累计观测用量）。
func CountStreamOutputTokens(payload []byte) int64 { return streamOutputTokens(payload) }

// streamOutputTokens 统计流式帧中的输出 token。
//
// 必须覆盖工具调用：`tool_calls[].function.arguments` 是模型真实生成的文本，
// 纯工具调用场景（Agent / MCP 客户端）里它常占输出的绝大部分。
// 只统计 content 会让这类请求的 completion 记 0 —— 工具参数完全免费。
func streamOutputTokens(payload []byte) int64 {
	var p struct {
		Choices []struct {
			Delta struct {
				Content          any `json:"content"`
				ReasoningContent any `json:"reasoning_content"`
				Refusal          any `json:"refusal"`
				ToolCalls        []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				// 旧的 function_call 形态（部分兼容网关仍在用）。
				FunctionCall *struct {
					Arguments string `json:"arguments"`
				} `json:"function_call"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return 0
	}
	var total int64
	for _, c := range p.Choices {
		total += estimateTextTokens(anyText(c.Delta.Content))
		total += estimateTextTokens(anyText(c.Delta.ReasoningContent))
		total += estimateTextTokens(anyText(c.Delta.Refusal))
		for _, tc := range c.Delta.ToolCalls {
			total += estimateTextTokens(tc.Function.Name)
			total += estimateTextTokens(tc.Function.Arguments)
		}
		if c.Delta.FunctionCall != nil {
			total += estimateTextTokens(c.Delta.FunctionCall.Arguments)
		}
	}
	return total
}

// anyText 把 content 的两种形态（string / [{text}]）统一为字符串。
func anyText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, it := range t {
			if m, ok := it.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// ensureUsage 是「上游用量缺失/残缺」的唯一兜底入口，返回计费用量、来源与异常标记。
//
// 口径（与方案 §4.4 判定表一致）：
//   - 按次/按规格（mode≠tokens）：token 不参与计费，来源 none；
//   - 上游给出任一 token 通道：source=upstream；completion 缺失但有可观测输出时补估算（source=mixed）；
//   - 上游完全无 token：source=estimated，prompt 用请求估算、completion 只用可观测输出（不设人为下限）；
//   - 客户端断开且无任何输出：completion 记 1（标记 client_abort）。
//
// observedTokens 由调用方提供：非流式取响应体文本估算，流式取帧累计估算。
func ensureUsage(up UsageInfo, est promptCompletionEst, mode string, observedTokens int64, bodyTruncated, clientAborted bool) (billing.Usage, string, string) {
	u := up.Billing()
	var flags []string
	if bodyTruncated {
		flags = append(flags, "body_truncated")
	}

	if mode != model.BillingModeTokens {
		// 按次/按规格：金额由请求侧参数决定（与 usage 无关），来源标记 none 便于区分。
		u.PromptTokens, u.CompletionTokens = 0, 0
		u.CacheTokens, u.CacheWrite5mTokens, u.CacheWrite1hTokens = 0, 0, 0
		u.ImageTokens, u.AudioInputTokens, u.AudioOutputTokens = 0, 0, 0
		return u, "none", joinFlags(flags)
	}

	if up.HasTokenUsage() {
		source := "upstream"
		if u.CompletionTokens == 0 && observedTokens > 0 {
			u.CompletionTokens = observedTokens
			source = "mixed"
			flags = append(flags, "usage_estimated")
		}
		if u.PromptTokens == 0 && est.Prompt > 0 {
			u.PromptTokens = est.Prompt
			source = "mixed"
			flags = append(flags, "usage_estimated")
		}
		// 语义缺失 + 分通道用量：计费按 OpenAI 解释，但必须留痕。
		// 判据是「有分通道用量」，因为只有 OpenAI 语义把这些通道算在 prompt 里；
		// 纯 prompt/completion 的用量对两种语义等价，打标只会制造噪音。
		if up.Semantic == "" && (u.CacheTokens > 0 || u.CacheWrite5mTokens > 0 ||
			u.CacheWrite1hTokens > 0 || u.ImageTokens > 0 ||
			u.AudioInputTokens > 0 || u.AudioOutputTokens > 0) {
			flags = append(flags, model.AnomalyUsageSemanticUnknown)
		}
		return u, source, joinFlags(flags)
	}

	// 上游完全未给 usage：按请求侧 prompt + 可观测输出兜底。
	u.PromptTokens = est.Prompt
	if observedTokens > 0 {
		u.CompletionTokens = observedTokens
	} else {
		flags = append(flags, "output_unmeasured")
	}
	if clientAborted && u.CompletionTokens == 0 {
		u.CompletionTokens = 1
		flags = append(flags, "client_abort")
	}
	flags = append(flags, "usage_estimated")
	if u.PromptTokens == 0 && u.CompletionTokens == 0 {
		flags = append(flags, "zero_usage")
	}
	return u, "estimated", joinFlags(flags)
}

// joinFlags 去重并拼接异常标记（落 usages.anomaly，逗号分隔）。
func joinFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(flags))
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return strings.Join(out, ",")
}

// runeLen 返回字符数（供测试与日志）。
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// isImageKind 判断是否为图片生成端点（按规格计费，需要按实际返回张数校正）。
func isImageKind(kind endpointKind) bool {
	return kind == kindImagesGenerations || kind == kindImagesEdits || kind == kindImagesVariations
}

// buildBillingUsage 组装落库/计费用量：兜底判定 + 请求侧规格参数 + 图片实际张数校正。
// observedOverride>0 时优先使用（流式帧累计值）；否则从 body 提取可观测输出。
// declaredN 是请求里**显式写下**的张数（0 = 没写），用于把收费收敛到用户下单的张数。
func buildBillingUsage(kind endpointKind, up UsageInfo, est promptCompletionEst, mode string,
	spec billing.Usage, body []byte, observedOverride int64, truncated, aborted bool,
	declaredN int64) (billing.Usage, string, string) {
	observed := observedOverride
	if observed == 0 {
		observed = observedOutputTokens(body)
	}
	u, source, anomaly := ensureUsage(up, est, mode, observed, truncated, aborted)
	// 请求侧的规格参数必须**整组**搬过来：漏一个就少一维留痕。
	// 曾经这里只搬 ImageSpec/Quality/N，于是 ImageSpecExplicit 恒 false →
	// billing.go 的 tier_mismatch 条件恒假、image_spec_source 恒空。
	carrySpecFields(&u, spec)
	u.ImageN = spec.ImageN
	u.AudioChars, u.RerankDocs = spec.AudioChars, spec.RerankDocs
	if u.AudioDurationSecs == 0 && spec.AudioDurationSecs > 0 {
		// 上游没回报时长（ASR 的 json 格式默认就没有 duration），回落到请求侧估算。
		//
		// 这条回落**必须留痕**：估算按 8KB/s（64kbps 下界）算，而 WAV 16k/16bit
		// 是 32KB/s —— 实际时长只有估算的 1/4，等于按 4 倍收费；44.1k 立体声更差。
		// 打 audio_seconds_estimated 让这批单能被对账筛出、可人工退差；
		// 根治要靠解析上传文件的容器头（mediaprobe 目前只处理 URL，不支持上传体）。
		u.AudioDurationSecs = spec.AudioDurationSecs
		anomaly = joinFlags([]string{anomaly, model.AnomalyAudioSecondsEstimated})
	}
	if mode != model.BillingModeTokens && isImageKind(kind) {
		// 一次解析同时给出「交付张数」与「响应语义」：两者必须来自同一份判据，
		// 否则会出现「数出 0 张、却认为上游成功」这种自相矛盾的账单
		// （见 image_verdict.go 的判定表）。
		out := judgeImageResponse(body)
		observed := out.Count
		// 张数规则单源（billing.ResolveImageCount）：同步路径的「产出」与「交付」
		// 是同一份 body，两个入参传同一个值。分支结构保持原样，只把 n 的来源收敛掉。
		n, _ := billing.ResolveImageCount(observed, observed, u.ImageN)
		// 上游多给的图不向用户收费：用户只下单了 declaredN 张。
		// 只在**显式写了 n** 时收敛 —— edits/variations 不写 n 也常一次回多张，
		// 无条件收敛会把合法交付压成 1 张，变成系统性少收。
		if capped, over := capImageCountToDeclared(n, declaredN); over {
			n = capped
			anomaly = joinFlags([]string{anomaly, model.AnomalyImageNOverDelivered})
		}
		switch {
		case observed > 0:
			if u.ImageN > 0 && observed != u.ImageN {
				anomaly = joinFlags([]string{anomaly, model.AnomalyImageNMismatch})
			}
			if out.PartialError {
				anomaly = joinFlags([]string{anomaly, model.AnomalyImagePartialError})
			}
			u.ImageN = n
		default:
			// 0 张：无论用户写没写 n，都必须留痕。这里以前分两支，
			// 而 "没写 n" 那一支连标记都没有 —— 用户什么都没拿到、
			// 被收 1 张，调用记录却看起来完全正常。
			switch {
			case u.ImageN > 0:
				anomaly = joinFlags([]string{anomaly, model.AnomalyImageCountZero})
			case out.PerImageFailed:
				// 组图逐项全失败（部分模型会这么回）：原因是明确的，不是我们解析漏了。
				anomaly = joinFlags([]string{anomaly, model.AnomalyImagePartialAllFailed})
			default:
				anomaly = joinFlags([]string{anomaly, model.AnomalyImageNUnknown})
			}
			u.ImageN = n
		}
	}
	// 按秒计费但拿不到真实时长：分两种，标记也必须分开。
	//   - 有文本长度 → 计费层会按字符估秒并打 audio_seconds_estimated（有金额、秒数不精确）；
	//   - 连文本长度都没有 → 只能按最低消费兜底，这类必须在这里标记出来，
	//     否则"某条渠道的音频永远按最低价收"这件事没人会发现，直到月底对账整体少收。
	if mode == model.BillingModeSpec && kind == kindAudioSpeech &&
		u.AudioDurationSecs == 0 && u.AudioChars == 0 {
		anomaly = joinFlags([]string{anomaly, model.AnomalyAudioSecondsUnknown})
	}
	return u, source, anomaly
}

// capImageCountToDeclared 把计费张数收敛到用户**显式下单**的张数。
//
// 只在请求真的写了 n（declaredN > 0）且上游多给时才收敛：
//   - 常态下多给的图是上游行为（重试、批量返回），用户没有为它下单；
//     按它收费会变成「我只点了 1 张，却被收了 2 张」；
//   - 但绝不能在「没写 n」时收敛：edits / variations 不写 n 也会一次返回多张，
//     压成 1 张就是系统性少收（这类请求的 n 是"没说"，不是"要 1 张"）。
func capImageCountToDeclared(n, declaredN int64) (int64, bool) {
	if declaredN <= 0 || n <= declaredN {
		return n, false
	}
	return declaredN, true
}

// upstreamImageSpec 从同步图片响应里取**上游实际交付**的尺寸原文。
//
// 只认响应 data[].size 这一个字段：不同厂商写法不一（有的回像素、有的回档位），
// 拿不到就返回空 —— 猜一个值会让 tier_mismatch 变成一堆假告警，
// 比没有这个字段更糟（告警一假就没人看）。
func upstreamImageSpec(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var p struct {
		Data []struct {
			Size string `json:"size"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &p) != nil {
		return ""
	}
	for _, d := range p.Data {
		if s := strings.TrimSpace(d.Size); s != "" {
			return s
		}
	}
	return ""
}
