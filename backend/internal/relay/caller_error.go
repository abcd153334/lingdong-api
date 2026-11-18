package relay

import (
	"errors"

	"lingdong-api/internal/model"
)

// ErrCallerParam 表示「上游明确拒绝了本次请求的参数」，属于调用方问题。
//
// 为什么需要它：任务提交链路（service/task.Submit / Retry）出错时只会记 lastErr 并换
// 下一个候选。若参数错也走这条路，10 个候选就会打 10 次上游、最后返回 502
// 「上游提交失败」，用户看不出是自己传错了；同时每一次都是真实出站请求。
//
// 归因规则（与 outcome.go 的 classify 一致）：
//   - 只有上游以 400/422 明确拒绝、或适配器在本地就判定参数越界时才用它；
//   - 401/403（鉴权）、404/405（配置/路径）、429/5xx（上游故障）一律**不能**用，
//     否则运营把 workflow id 填错会让用户收到「参数不合法」。
var ErrCallerParam = errors.New("relay: 调用方参数错误")

// CallerParamError 是带可读原因的调用方参数错误。
//
// Msg 会原样透出给调用方（控制器把它映射成 400 invalid_param），因此必须是
// **能指导修正**的中文短句（例如「时长需为 1–15 的整数」「本模型不支持 1080p」），
// 不要塞上游原始响应体。
type CallerParamError struct {
	Msg string
}

func (e *CallerParamError) Error() string {
	if e == nil || e.Msg == "" {
		return "参数不合法"
	}
	return e.Msg
}

// Unwrap 让 errors.Is(err, ErrCallerParam) 成立（调用方只认 sentinel，不认具体类型）。
func (e *CallerParamError) Unwrap() error { return ErrCallerParam }

// Class 实现 outcome.go 的 errKind：归因为调用方问题，不记渠道健康、不冷却密钥。
func (e *CallerParamError) Class() string { return "caller" }

// CallerMessageOf 把「调用方参数错」的 error 收敛成可下发的文案（脱敏 + 截断）。
//
// 同步出口与异步出口都必须用它，而不是各自 `err.Error()`：
//   - 取 Msg 而不是 Error()，是因为 err 可能被 `fmt.Errorf("%w：%s", ...)` 再包一层，
//     Error() 会带上包装前缀，用户看到的是「提交失败：参数不合法」这种没信息量的话；
//   - 脱敏与截断的理由见 redact.go（适配器会把上游响应体片段拼进 Msg）。
func CallerMessageOf(err error, ch *model.Channel, key string) string {
	msg := ""
	var ce *CallerParamError
	if errors.As(err, &ce) && ce != nil {
		msg = ce.Msg
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return CallerMessage(msg, ch, key)
}

// AmbiguousSubmitError 表示「提交阶段失败，但上游可能已经受理」。
//
// 为什么要单独成型：桥接与异步提交都是「先让上游干活，再拿任务号」，
// 若在拿任务号的这一步遇到连接中断/5xx，上游很可能已经创建了一个计费任务。
// 此时按普通错误处理会**换渠道重试** —— 一次请求付两次上游钱，而台账上
// 只是两条普通的 502。
//
// 处置：502 但**不换渠道**，并打 submit_ambiguous 标记，供对账筛出这批记录。
type AmbiguousSubmitError struct {
	Err error
}

func (e *AmbiguousSubmitError) Error() string {
	if e == nil || e.Err == nil {
		return "上游提交结果未知"
	}
	return e.Err.Error()
}

// Unwrap 让上层仍能 errors.As 到具体原因（超时/连接重置）。
func (e *AmbiguousSubmitError) Unwrap() error { return e.Err }

// Class 实现 outcome.go 的 errKind：没有拿到响应，按上游故障处理（会记渠道健康）。
func (e *AmbiguousSubmitError) Class() string { return "upstream" }
