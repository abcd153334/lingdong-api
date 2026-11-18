package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/channelhealth"
)

// HTTPStatusError 是「上游明确回了一个非 2xx 状态码」的结构化错误。
//
// 为什么需要类型而不是继续用 fmt.Errorf：异步任务提交的失败处理取决于
// **上游到底有没有把任务建出来**，而这只能从「有没有拿到响应」判断。
// 只把状态码拼进字符串，调用方就只能靠前缀匹配猜，改一次文案就静默失效
// （历史教训：anomaly 里 charge_capped 被写丢，正是靠文案判定埋的坑）。
//
// 错误文案与改造前逐字一致（`上游返回 %d：%s`），因此既有日志/断言不受影响。
type HTTPStatusError struct {
	Code int
	// Body 是**已截断脱敏**的响应体片段（调用方用 textx.Snippet 处理过），不是原文。
	//
	// 它同时承担一个语义作用：5xx 且 Body 为空 = 「上游回了错但没有任何说明」，
	// 视为提交结果歧义（见 IsAmbiguousSubmit）。因此**不要**为了少写文案而把
	// 响应体塞进 Note、让 Body 空着——那会把「上游明确拒绝」误判成「可能已建单」。
	Body string
	// Note 是适配器自定义的可读文案（含渠道专属的排障指引，例如「请检查 Base URL 是否带 /api 前缀」）。
	//
	// 非空时 Error() 直接返回它，用于**逐字保留**改造前的错误文案：这些文案会出现在
	// 管理台渠道测试结果与用户可见的 502 文案里，改一次就等于把运维熟悉的信息换掉。
	Note string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.Note != "" {
		return e.Note
	}
	return fmt.Sprintf("上游返回 %d：%s", e.Code, e.Body)
}

// Class 实现 outcome.go 的 errKind，让「没传状态码」的调用方也能被合理归因。
//
// 401/403 刻意返回空串：classifyError 没有 auth 分支（auth 只能由 HTTP 状态判定，
// 因为 keyhealth 需要知道是哪枚 key）。返回空串走 internal → 不扣渠道健康也不冷却密钥，
// 是这里唯一不会误伤的选择。需要正确行为请用 RecordChannelAttempt（它会带上状态码）。
func (e *HTTPStatusError) Class() string {
	if e == nil {
		return ""
	}
	switch {
	case e.Code == 401 || e.Code == 403:
		return ""
	case e.Code == 429:
		return "rate_limited"
	case e.Code >= 500:
		return "channel"
	case e.Code >= 400:
		return "caller"
	}
	return ""
}

// StatusError 构造一个带状态码的上游错误（body 必须是已截断脱敏的片段）。
func StatusError(code int, body string) *HTTPStatusError {
	return &HTTPStatusError{Code: code, Body: body}
}

// StatusOf 提取错误里的上游 HTTP 状态码（非 HTTPStatusError 返回 0）。
func StatusOf(err error) int {
	var he *HTTPStatusError
	if errors.As(err, &he) && he != nil {
		return he.Code
	}
	return 0
}

// IsAmbiguousSubmit 判断一次**任务提交**失败的「上游是否可能已建单」。
//
// 这个判定决定要不要换下一个渠道重投，是资金安全与可用性之间的那道闸：
//
//   - 返回 true → 上游可能已经受理，换渠道重投会**建出两个上游任务**（平台双倍成本，
//     且多出来的那个产物没有任何记录可对账）→ 必须直接失败并全额退款；
//   - 返回 false → 明确没建单（参数错、鉴权错、限流、连接没建立），换渠道是安全且必要的。
//
// 归类依据：
//   - 拿到响应且状态码表明上游拒绝（4xx、429）→ 不歧义；
//   - 拿到 5xx 且带响应体 → 上游明确报了错，按不歧义处理（可换渠道）；
//   - 拿到 5xx 但响应体为空 → **歧义**（典型是网关 502/504 在上游提交后才返回，
//     或上游写库成功但序列化响应失败）；
//   - 超时 / EOF / 连接被重置（ECONNRESET/EPIPE）→ 歧义（请求可能已送达）；
//   - 拨号（dial）阶段失败 / 连接被拒绝 → **不歧义**：TCP 都没建起来，请求不可能送达。
//     这条必须放在超时判断之前，否则「DNS 慢导致的 dial timeout」会被误判成歧义，
//     让本该换渠道成功的任务直接失败退款。
func IsAmbiguousSubmit(err error) bool {
	if err == nil {
		return false
	}
	// 拨号阶段失败：请求体一个字节都没发出去。
	var oe *net.OpError
	if errors.As(err, &oe) && oe != nil && (oe.Op == "dial" || oe.Op == "dialer") {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return false
	}
	var he *HTTPStatusError
	if errors.As(err, &he) && he != nil {
		return he.Code >= 500 && strings.TrimSpace(he.Body) == ""
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// context.Canceled 也要算歧义（P3-7）。
	//
	// 触发场景：客户端提交视频任务后中途断开（关页面/切网络），网关的 ctx 被取消，
	// 而出站请求**可能已经完整送达并建单**。此时判「明确失败」→ 全额退款 + 不记任务，
	// 上游那次生成就是纯亏损，且产物无人认领可查。
	//
	// 代价只是「任务被判失败并要求用户用同一幂等键重试」，与真建单时的处理完全一致。
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	return false
}

// RecordChannelAttempt 是**异步任务链路**记录一次上游尝试归属的公开入口。
//
// 与同步转发链路（relay.go 的 recordAttempt）同口径，不允许各写一份：
//   - 有 HTTP 状态码 → 只按状态归因（5xx 记渠道失败，4xx 记调用方，401/403/429 只动密钥）；
//   - 没有状态码（连接/超时）→ 按错误自述类别归因，普通错误记上游故障。
//
// ref.ID 为空表示「调用方不知道是哪枚 key」（例如轮询阶段只有渠道、没有当次选中的 key）：
// 此时**只记渠道级失败，绝不做密钥级动作**——随便挑一枚去冷却/解冻会让好 key 被无端封禁
// 或坏 key 被提前解封，两种都是线上事故。池空/全禁用/全坏也是同一条路径：配置问题
// 不该把某把 key 冷却掉。
func RecordChannelAttempt(ch *model.Channel, ref KeyRef, err error) {
	if ch == nil {
		return
	}
	status := StatusOf(err)
	var o outcome
	if status > 0 {
		o = classify(status, nil, false)
	} else {
		o = classify(0, err, false)
	}
	if ref.ID == "" {
		if o == outcomeUpstream {
			channelhealth.Record(context.Background(), ch.ID, false)
		}
		return
	}
	recordAttempt(Candidate{Channel: ch}, ref, o, status)
}

// RecordChannelSuccess 记录一次渠道成功（异步轮询「从连续失败中恢复」时用）。
//
// 为什么异步链路也要记成功：如果只记失败，一条长期正常、偶发一次 5 连败的渠道
// 在 10 分钟窗口里会攒出「10 个失败样本 + 0 个成功样本」→ 失败率 100% → 被熔断。
// 配对记录（一次连败记 1 条失败，恢复时记 1 条成功）才能让失败率回到它本来的含义。
func RecordChannelSuccess(ch *model.Channel) {
	if ch == nil {
		return
	}
	channelhealth.Record(context.Background(), ch.ID, true)
}
