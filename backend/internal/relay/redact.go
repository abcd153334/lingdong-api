package relay

// 上游错误文案脱敏（P2-6）。
//
// 为什么必须做：usages.error_msg 会被门户用户、CSV 导出、管理台列表看到。
// 上游的 4xx 报文里经常原样回显请求头（`Authorization: Bearer sk-...`）
// 与自己 API 主机名（`https://api.internal-proxy.example.com`）——
// 前者等于把用户密钥写进他自己的记录页，后者等于把平台供应链暴露给终端用户。
//
// 铁律：**先脱敏后截断**。反过来会在 500 字符处把 token 切成两半，
// 使正则失配，泄漏的恰好是"看起来不像密钥"的那一半。upstreamSnippet 因此
// 先调用 redactUpstream，再按字符数截断。

import (
	"regexp"
	"strings"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
	"lingdong-api/internal/pkg/textx"
)

// 脱敏占位符（与前端展示约定一致）。
//
// 导出是为了让「截断后裁残片」的调用方（task.sanitizeReason）用同一份字面量：
// 各写一份的后果曾经是 `task` 里写着 `<upstream-host>`（早已不存在的占位符），
// 于是末尾被截断的 `<redacted-host` 残片永远裁不掉。
const (
	RedactedKeyPlaceholder  = "<redacted-key>"
	RedactedHostPlaceholder = "<redacted-host>"
)

const (
	redactedKeyPlaceholder  = RedactedKeyPlaceholder
	redactedHostPlaceholder = RedactedHostPlaceholder
)

var (
	// Bearer 凭据：`Bearer xxx` 整段替换。
	reRedactBearer = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-._~+/]{6,}=*`)
	// 常见密钥前缀形态（sk-/ak-/rk-/pk-/ld-），至少 6 位避免误伤 `sk-1` 这类短词。
	// 不加前导边界：`prefixsk-...` 这类拼接形态用 \b 匹配不到（RE2 无 lookbehind），
	// 而少抹一次就等于把密钥写进 error_msg（门户用户可见）。代价是英文单词里
	// 恰好含 `sk-xxxx` 的片段会被打码，属可接受的可读性损失。
	reRedactPrefix = regexp.MustCompile(`(?:sk|ak|rk|pk|ld)-[A-Za-z0-9_\-]{6,}`)
	// 查询串 / 表单项里的密钥。
	reRedactQuery = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|key|token|access_token|secret|signature)=)[^&\s"']+`)
)

// redactUpstream 抹掉上游文案里的密钥与渠道主机名。
//
// ch 可以为 nil、key 可以为空：异步任务/worker 拿不到渠道上下文，
// 此时只做通用形态脱敏 —— 不能因为"没有上下文"就原样返回，
// 那等于上层以为脱敏了、实际没有。
//
// **ch 非 nil 时按整个密钥池脱敏，而不是只抹当前这把**：
// 上游的错误报文经常把"同账号其它 key"也带出来（"key sk-xxx is rate limited,
// try sk-yyy"），只抹当前这把等于把池子里其它凭据写进门户可见的 error_msg。
func redactUpstream(text string, ch *model.Channel, key string) string {
	if text == "" {
		return ""
	}
	base := ""
	var keys []string
	if ch != nil {
		base = ch.BaseURL
		keys = poolKeys(ch)
	}
	if key != "" {
		keys = append(keys, key)
	}
	return redactWithKeys(text, base, keys)
}

// poolKeys 取渠道密钥池的明文（解密失败的忽略），仅供脱敏使用。
//
// 只在**失败路径**调用：成功路径不做全池解密，那属于给每条请求加固定开销。
func poolKeys(ch *model.Channel) []string {
	dec := channelkeys.Decrypt(ch.Keys)
	out := make([]string, 0, len(dec.Entries))
	for _, e := range dec.Entries {
		if e.OK() {
			out = append(out, e.Plain)
		}
	}
	return out
}

// redactWith 是脱敏规则的**唯一实现**：给定原文、上游基址与密钥，返回脱敏后的文本。
//
// 两个导出入口（RedactUpstream / RedactUpstreamWith）都是它的薄包装：
// relay 内部拿得到 *model.Channel，而规格适配器只有 ChannelConfig，
// 但两边必须用同一套规则 —— 各写一份的话，新增一种密钥形态时必然漏掉其中一边。
func redactWith(text, baseURL, key string) string {
	if key == "" {
		return redactWithKeys(text, baseURL, nil)
	}
	return redactWithKeys(text, baseURL, []string{key})
}

// redactWithKeys 是脱敏的真正实现：抹掉给定的**全部**密钥 + 通用形态 + 主机名。
//
// 顺序：先逐字替换已知密钥（精确），再走正则兜底（覆盖未知形态），最后抹主机名。
// 逐字替换必须在前：正则 `sk-xxx{6,}` 匹配不到自建前缀（`duomi_...`、`xhz-...`），
// 而那些恰恰是最容易被上游原样回显的。
func redactWithKeys(text, baseURL string, keys []string) string {
	if text == "" {
		return ""
	}
	out := text
	for _, key := range keys {
		if key == "" {
			continue
		}
		out = strings.ReplaceAll(out, key, redactedKeyPlaceholder)
	}
	out = reRedactBearer.ReplaceAllString(out, "Bearer "+redactedKeyPlaceholder)
	out = reRedactPrefix.ReplaceAllString(out, redactedKeyPlaceholder)
	out = reRedactQuery.ReplaceAllString(out, "${1}"+redactedKeyPlaceholder)
	if host := hostOf(baseURL); host != "" {
		out = strings.ReplaceAll(out, host, redactedHostPlaceholder)
	}
	return out
}

// RedactUpstream 是脱敏的导出入口（异步任务与 worker 复用）。
// 与 redactWith 完全同一实现：导出只是为了让 relay 外部不必重复一遍规则。
func RedactUpstream(text string, ch *model.Channel, key string) string {
	return redactUpstream(text, ch, key)
}

// RedactUpstreamWith 是给「拿不到 *model.Channel」的调用方（规格适配器）用的入口。
func RedactUpstreamWith(text, baseURL, key string) string {
	return redactWith(text, baseURL, key)
}

// CallerMessage 返回「调用方可见」的文案：先脱敏，再按字符截断。
//
// 这是**所有**把上游/适配器错误文案交给终端用户的出口的唯一入口：
// 适配器会把上游 4xx 响应体片段拼进 CallerParamError.Msg（便于排查），
// 而那段原文里经常带着 `Authorization: Bearer sk-...` 与上游主机名。
// 直接 WithMessage 透出等于把用户密钥和自己的供应链写给对方看。
//
// 顺序不可交换（见本文件顶部铁律）；上限 500 字符与 usages.error_msg 一致，
// 保证「响应里看到的」与「账单里记的」是同一句话。
func CallerMessage(msg string, ch *model.Channel, key string) string {
	return textx.TruncateRunes(redactUpstream(msg, ch, key), callerMessageMaxRunes)
}

// CallerMessageMaxRunes 是调用方可见文案的字符上限（usages.error_msg 同长）。
const callerMessageMaxRunes = 500

// hostOf 从 BaseURL 取主机名（含端口），解析失败返回空串。
// 只抹主机名不抹整段 URL：保留路径有助于判断是哪个上游端点出的错。
func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
