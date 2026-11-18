package gateway

import "lingdong-api/internal/pkg/errs"

// Wire 是协议面的一次失败投影：状态码 + 业务码 + error.type。
type Wire struct {
	Status int
	Code   string
	// Type 是 OpenAI 错误体里的 error.type。默认按状态码派生（见 openAIType），
	// 只有鉴权家族显式覆盖成 `new_api_error`（见 typeOverrides 的理由）。
	Type string
	// RequestID 表示 message 末尾要带 ` (request id: xxx)`。
	//
	// 只给鉴权家族开：用户报障时贴的就是这句话，而其余错误的文案里已经有
	// 明确的处置动作，再挂一串 id 反而把「该做什么」挤到看不见。
	RequestID bool
}

// wireOverrides 只放**与内部 Def 有意不同**的极少数几条，每条都要写理由。
//
// 不把 200 多个 Def 抄成一张表：那会变成第二真相源 —— 新增 Def 时静默落到
// 默认分支，而文档、预检文案、前端错误码表都还按旧值走。
var wireOverrides = map[string]Wire{
	// 额度不足：内部 402（控制面用它区分「余额」与「权限」），协议面必须是 403
	// —— 兼容协议从不使用 402，而主流客户端只对 403 做「去充值」引导；
	// 402 会掉进它们的 default 分支显示「未知错误」。
	errs.CodeInsufficientBalance: {Status: 403, Code: "insufficient_user_quota"},
	// 单笔预估超上限：兼容协议把它归到同一个「额度不足」（403 insufficient_user_quota）。
	// 曾经是 400 request_quota_exceeded —— 客户端会去改参数，而正确动作是充值。
	errs.CodeRequestQuotaExceeded: {Status: 403, Code: "insufficient_user_quota"},
}

// typeOverrides 把鉴权家族的 error.type 换成 `new_api_error`。
//
// 兼容协议在鉴权中间件里就只写这一个 type（不细分 authentication/permission），
// 而按 type 分流的客户端认的就是它。其余错误仍按状态码派生 —— 那是 OpenAI 的
// 词表，改它会让 OpenAI SDK 的错误分支全部走空。
var typeOverrides = map[string]bool{
	errs.CodeInvalidAPIKey: true,
	errs.CodeUnauthorized:  true,
	errs.CodeKeyExpired:    true,
	errs.CodeKeyDisabled:   true,
	errs.CodeKeyRetired:    true,
	errs.CodeKeyIPDenied:   true,
}

// noRetryCodes 是「调用方**绝不能**自动重试」的码。
//
// 这些失败的共同点是：上游可能已经受理并计费，重试等于再付一次成本、用户也可能
// 拿到两份产物。状态码本身已经挑过一轮（409/502 都不在主流 SDK 的重试白名单里），
// 但仍有客户端按自己的规则重试，所以再显式下发 `X-Should-Retry: false` ——
// OpenAI 与 Anthropic 的官方 SDK 都认这个头。
var noRetryCodes = map[string]bool{
	errs.CodeUpstreamResultUnknown:   true,
	errs.CodeUpstreamSubmitAmbiguous: true,
}

// WireOf 是 errs.Def → 协议面三元组（状态码/业务码/type）的**唯一投影**。
//
// 为什么要有它：状态码与业务码的对应关系此前散在 5 个 switch 里（controller 的
// failDefOf、relay 的 failover 判定、logs 的事件分类、文档的码表、前端的标签），
// 改一处必然漏四处。现在只有这里能改，「额度不足是 403 还是 402」只有一个答案。
//
// 日志侧也用它：事件分类按 WireOf 的结果判，才不会出现「对外 403、后台仍按
// 402 筛选」这种只有用户能看出来的分裂。
func WireOf(def errs.Def) Wire {
	w := Wire{Status: def.HTTP, Code: def.Code, Type: openAIType(def.HTTP)}
	if o, ok := wireOverrides[def.Code]; ok {
		if o.Status != 0 {
			w.Status = o.Status
			// 状态码变了，type 要跟着重新派生（否则 403 还挂着 400 的 type）。
			w.Type = openAIType(o.Status)
		}
		if o.Code != "" {
			w.Code = o.Code
		}
	}
	if typeOverrides[def.Code] {
		w.Type = "new_api_error"
		w.RequestID = true
	}
	return w
}

// WireCodes 返回「协议面可能出现、但**不在** errs 里」的业务码集合。
//
// 只服务门禁：文档码表允许出现这些码（它们由 WireOf 覆盖出来），
// 除此之外的每一个码都必须在 errs 里有定义。没有这个出口，门禁只能
// 硬编码白名单 —— 新增一条 override 就要同步改测试，而漏改的表现是
// "文档里那个码被判为不存在"，于是有人把文档改错去迁就测试。
func WireCodes() []string {
	out := make([]string, 0, len(wireOverrides))
	seen := map[string]bool{}
	for _, w := range wireOverrides {
		if w.Code == "" || seen[w.Code] {
			continue
		}
		seen[w.Code] = true
		out = append(out, w.Code)
	}
	return out
}
