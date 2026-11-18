package relay

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/channelhealth"
	"lingdong-api/internal/service/keyhealth"
)

// outcome 是一次上游尝试的**归属分类**（审计项 E2）。
//
// 为什么需要它：S5 的实现只有「成功/失败」两态，于是
//
//	401（这枚 key 失效）→ 记为渠道失败 → 攒够样本后整条渠道被熔断；
//	400（用户参数错）  → 同样记为渠道失败 → 用户打错字也能把渠道打挂。
//
// 分类之后：key 级问题只冷却那枚 key，调用方问题完全不记渠道，只有上游自身问题才影响渠道健康。
type outcome int

const (
	outcomeSuccess     outcome = iota // 2xx
	outcomeAuth                       // 401/403：这枚密钥失效
	outcomeRateLimit                  // 429：上游限流
	outcomeUpstream                   // 5xx / 连接失败 / 超时：上游自身问题
	outcomeCaller                     // 4xx（参数/路径/体积）：调用方问题，与渠道无关
	outcomeClientAbort                // 客户端主动断开
	// outcomeInternal 是「我方适配层自身的问题」：插件实现 bug、协议错、
	// 执行服务不可用/繁忙、插件超时。它既不是上游的错，也不是调用方的错，
	// 因此**不扣渠道健康、不冷却密钥**——否则一个插件 bug 就能把整条渠道熔断。
	outcomeInternal
)

func (o outcome) String() string {
	switch o {
	case outcomeSuccess:
		return "success"
	case outcomeAuth:
		return "auth"
	case outcomeRateLimit:
		return "ratelimit"
	case outcomeUpstream:
		return "upstream"
	case outcomeCaller:
		return "caller"
	case outcomeClientAbort:
		return "client_abort"
	case outcomeInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// callerStatuses 是「明确属于调用方」的状态码白名单（其余 4xx 仍按调用方处理，但会打 Warn 便于发现漏分类）。
var callerStatuses = map[int]bool{400: true, 404: true, 413: true, 422: true}

// classify 判定一次尝试的归属。纯函数，便于穷举测试。
//
//	status>0              → 按上游 HTTP 状态判定（它比适配器自述更权威）
//	status=0 且 err!=nil  → 没拿到响应（连接/超时/适配器内部错）→ 看 err 的 Class()
//	aborted=true          → 客户端断开（无论有没有响应）→ clientAbort
func classify(status int, err error, aborted bool) outcome {
	if aborted {
		return outcomeClientAbort
	}
	// 状态码优先于适配器的自述类别（Class()）。
	//
	// 为什么必须这样：`HTTPStatusError.Class()` 对 401/403 **刻意**返回空串
	// （插件路径与 submit_ambiguous 依赖这个行为），空串在 classifyError 里
	// 落到 default → internal 或 upstream。于是同步路径上一枚坏 key 会把整条
	// 渠道判成上游故障：多 key 连坐熔断，而且 401 不冷却 key（坏 key 一直被选中）。
	//
	// 拿到状态码时它是**上游的原话**，比任何适配器自述都权威。
	// 本地判定的调用方错误（CallerParamError）没有状态码，仍走 classifyError。
	if err != nil && status <= 0 {
		// 状态码也可能只存在于错误里（适配器把「HTTP 200 + 业务码 401」这类
		// 信封折成 HTTPStatusError）：不取它就会把鉴权失败记成上游故障。
		// 这一条让「业务码」与「HTTP 状态码」走完全相同的归因，不需要第二套规则。
		if s := StatusOf(err); s > 0 {
			return classify(s, nil, aborted)
		}
		return classifyError(err)
	}
	switch {
	case status >= 200 && status < 300:
		return outcomeSuccess
	case status == 401 || status == 403:
		return outcomeAuth
	case status == 429:
		return outcomeRateLimit
	case status >= 500:
		return outcomeUpstream
	case status >= 400:
		return outcomeCaller
	}
	// 1xx/3xx 等异常态：不归因给调用方，也不惩罚渠道（记 upstream 会误熔断，记 caller 会掩盖）。
	return outcomeUpstream
}

// errKind 是适配器错误的「自述类别」接口（插件适配器的 CallError 实现它）。
//
// 用接口而不是具体类型：relay 不能 import internal/plugin（会成环），
// 也不该为某个适配器实现开特例。
type errKind interface {
	Class() string
}

// classifyError 把「适配器返回的错误」归因。
//
// 没有 Class() 的错误一律按 upstream 处理——这是内置适配器的既有语义（连接失败/超时），
// 不能因为引入了插件就改变它们的行为。
func classifyError(err error) outcome {
	var k errKind
	if !errors.As(err, &k) {
		return outcomeUpstream
	}
	switch k.Class() {
	case "channel":
		return outcomeUpstream
	case "rate_limited":
		return outcomeRateLimit
	case "sensitive_input":
		return outcomeCaller
	case "caller":
		// 适配器在本地就判定「调用方参数越界」（见 caller_error.go）。
		// 不走 default：default 会把已知的调用方错误记成 internal，只留一条 Warn，
		// 排查时看不出这是用户传错，而不是我们自己的 bug。
		return outcomeCaller
	case "config":
		// 渠道声明式配置有误：既不是上游故障也不是调用方问题，
		// 记 internal 才不会被渠道熔断连坐（ConfigError.Class 见 declarative.go）。
		return outcomeInternal
	case "slow":
		// 同步桥接等满了我们自己设的上限（见 SyncWaitTimeoutError）。
		// 上游没错、渠道也没坏，它只是还没跑完 —— 记 upstream 会导致
		// 「模型越慢越容易熔断自己的渠道」，正好打在最该保留的那条上。
		return outcomeInternal
	case "timeout", "protocol", "busy", "":
		// 空 kind 是插件抛出的普通异常（最常见）：最可能是插件自身 bug，
		// 归 internal 而不是 upstream，避免"调试一个插件把渠道熔断"。
		return outcomeInternal
	default:
		// 插件自造的未知 kind：不认，也不让它影响渠道健康。
		return outcomeInternal
	}
}

// failoverDecision 是「拿到一次尝试结果后怎么走」的判定。
type failoverDecision int

const (
	decideReturn failoverDecision = iota // 终结本次请求（成功或不可重试）
	decideRetry                          // 换下一个候选
	decideAbort                          // 客户端已断开，立即终止
)

// decideAfterResponse 冻结 S5 的 failover 规则（B1 只在此处加归因，不改规则）：
//   - 客户端断开 → abort；
//   - 2xx → return；
//   - 可重试状态（429/5xx）且还有尝试额度 → retry；
//   - 其余（含可重试但额度用尽、以及全部 4xx）→ return（原样透传状态码）。
func decideAfterResponse(status int, attempt, maxAttempts int, aborted bool) failoverDecision {
	if aborted {
		return decideAbort
	}
	if status >= 200 && status < 300 {
		return decideReturn
	}
	if retryableStatus(status) && attempt+1 < maxAttempts {
		return decideRetry
	}
	return decideReturn
}

// recordChannel 记录一次渠道级结果（成功/失败）。
//
// 与 recordAttempt 的分工：recordAttempt 按 outcome 细分归因（key 冷却 / 渠道健康 / 不记），
// 而结算成功时我们只知道"这条渠道把这单做成了"，没有 outcome 可传 —— 直接记成功即可。
// 空候选（全员失败路径传的是 Candidate{}）必须跳过，否则会用 channel_id=0 污染健康表。
func recordChannel(cand Candidate, ok bool) {
	if cand.Channel == nil {
		return
	}
	channelhealth.Record(context.Background(), cand.Channel.ID, ok)
}

// recordAttempt 把一次尝试的归属落到 key 级冷却 / 渠道健康。
//
// 归属不复用：key 级只影响那枚密钥（keyhealth），渠道级只在 outcomeUpstream 时记失败。
// 用后台 Context：客户端断开不应中断归因本身（与 channelhealth.Record 的既有约定一致）。
//
// ref.ID 为空表示"本次没有可用密钥"（池空/全禁用/全坏）：此时只做渠道级动作，
// 绝不做 key 级动作——否则所有 key 会被一起冷却，把配置事故伪装成限流。
func recordAttempt(cand Candidate, ref KeyRef, o outcome, status int) {
	if cand.Channel == nil {
		return
	}
	ctx := context.Background()
	switch o {
	case outcomeSuccess:
		// 成功即解除该密钥的冷却与失败计数：一次抖动不该让一枚好 key 长期不可用。
		keyhealth.Clear(ctx, cand.Channel.ID, ref.ID)
	case outcomeAuth:
		keyhealth.Cool(ctx, cand.Channel.ID, ref.ID, keyhealth.ReasonAuth, errState(status, keyhealth.ReasonAuth))
		logger.L().Warn("渠道密钥鉴权失败，已冷却该密钥",
			zap.Uint64("channel_id", cand.Channel.ID), zap.String("key_id", ref.ID),
			zap.Int("key_index", ref.Index), zap.Int("http_status", status))
	case outcomeRateLimit:
		keyhealth.Cool(ctx, cand.Channel.ID, ref.ID, keyhealth.ReasonRateLimit, errState(status, keyhealth.ReasonRateLimit))
	case outcomeUpstream:
		channelhealth.Record(ctx, cand.Channel.ID, false)
		// 指标只在「真的计入渠道健康」时 +1，口径必须与熔断判据完全一致：
		// 否则看板上的失败率与熔断器状态会对不上，排障时先怀疑指标再怀疑代码。
		metrics.Inc(metrics.NameChannelFailures, "channel", strconv.FormatUint(cand.Channel.ID, 10))
	case outcomeCaller:
		// 调用方问题不影响渠道健康；未在白名单里的 4xx 打 Warn，便于发现新的漏分类状态码。
		if !callerStatuses[status] && status >= 400 {
			logger.L().Warn("未归类的上游 4xx，按调用方错误处理",
				zap.Int("http_status", status), zap.Uint64("channel_id", cand.Channel.ID))
		}
	case outcomeClientAbort:
		// 客户端断开不是上游的错，什么都不记。
	case outcomeInternal:
		// 适配层自身问题：只留痕，不动渠道健康与密钥冷却。
		logger.L().Warn("适配层内部错误（不计渠道健康）",
			zap.Uint64("channel_id", cand.Channel.ID), zap.String("adapter", cand.Channel.AdapterID),
			zap.Int("http_status", status))
	}
}

// CoolKeyIfAuth 只在「上游明确说这枚 key 没有权限」时冷却它。
//
// 用于后台链路（任务轮询 / 终态复查 / 取产物）与熔断探测：它们的失败原因很杂
// （网络、上游 5xx、路径写错），而无条件调 RecordChannelAttempt 会把渠道健康的
// 连败口径搅乱（轮询本身有"连败 N 次才算一次失败"的阈值，被绕过后会误熔断）。
//
// 判据复用 classify（401/403 → outcomeAuth），不新增第二套状态码规则：
// 这里一旦自己判 401，将来 classify 调整（比如把 403 归到 caller）就会分叉。
func CoolKeyIfAuth(ch *model.Channel, ref KeyRef, err error) {
	if ch == nil || ref.ID == "" {
		return
	}
	status := StatusOf(err)
	if classify(status, nil, false) != outcomeAuth {
		return
	}
	keyhealth.Cool(context.Background(), ch.ID, ref.ID, keyhealth.ReasonAuth,
		errState(status, keyhealth.ReasonAuth))
}

// PollFailureClass 是后台轮询失败的**处置档位**（不是新的状态码规则）。
//
// 为什么需要分档：改造前所有错误都走同一条路 —— 计连败、20 次判超时退款。
// 但 404（上游任务已被清理）、429（我方被上游限流）、401（这一把 key 失效）三者的
// 正确处置完全不同：立即失败 / 等一会儿 / 换一把 key。一律当"连续失败"的结果是
// 上游明明还在生成，任务已经被判超时退款 —— 钱付了、东西丢了。
type PollFailureClass int

const (
	// PollFailureTransient 是「上游暂时不行」：5xx、网络错误、超时。唯一该计连败的一档。
	PollFailureTransient PollFailureClass = iota
	// PollFailureNotFound 是「上游说这个任务不存在」：404/410，确定性结论，不必再等。
	PollFailureNotFound
	// PollFailureAuth 是「这枚密钥没权限」：401/403，换 key 即可恢复。
	PollFailureAuth
	// PollFailureRateLimited 是「上游限流」：429，等一会儿即可恢复。
	PollFailureRateLimited
	// PollFailureOther 是「其它 4xx」：多为查询路径/参数写错，必须快速暴露。
	PollFailureOther
)

// ClassifyPoll 判定一次轮询失败该走哪条处置。
//
// 判据复用 classify（401/403、429、5xx 的唯一真相），这里只多认 404/410 这一档：
// 它们对 classify 是「调用方问题」，对轮询却是「任务没了」——语义由调用方决定，
// 状态码到归因的映射仍然只有一份。
func ClassifyPoll(err error) PollFailureClass {
	status := StatusOf(err)
	switch status {
	case 404, 410:
		return PollFailureNotFound
	}
	switch classify(status, err, false) {
	case outcomeAuth:
		return PollFailureAuth
	case outcomeRateLimit:
		return PollFailureRateLimited
	case outcomeUpstream:
		return PollFailureTransient
	case outcomeCaller:
		if status > 0 {
			return PollFailureOther
		}
		return PollFailureTransient
	}
	return PollFailureTransient
}

// errState 生成「最近一次失败」的现场串（面板展示用，不参与任何判定）。
func errState(status int, reason string) string {
	return fmt.Sprintf("%d|%s|%d", status, reason, time.Now().Unix())
}
