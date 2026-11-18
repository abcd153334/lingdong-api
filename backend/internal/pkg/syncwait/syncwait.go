// Package syncwait 提供「提交后在本请求内等结果」的唯一等待循环实现。
//
// 为什么要有这个包：平台有两条同步桥接链路 —— 内置适配器（duomi 同步生图）
// 与声明式规格（spec 的 async.mode=bridge）。两边原本各写一份等待循环，
// 于是同一种故障在两条链路上的处置不同：一边判超时、一边还在傻等；
// 一边把「查询连续失败」当上游故障记渠道健康度、一边当成任务失败退款。
// 等待语义只有一份才不会分叉。
//
// 本包**不依赖 relay 包**：超时/放弃的错误类型由调用方通过 Hooks 注入，
// 否则 adapters → syncwait → relay → adapters 会形成循环依赖。
package syncwait

import (
	"lingdong-api/internal/pkg/taskstate"

	"context"
	"errors"
	"fmt"
	"time"
)

// Options 是等待循环的节奏参数。
type Options struct {
	// Wait 是等待上限：到点必须放弃（返回 Hooks.Timeout 构造的错误）。
	//
	// 调用方必须保证它小于反向代理的读超时（deploy/nginx.conf 的 960s）：
	// 代理先断的话，应用这边即使等到结果也写不出去，客户端只拿到 504 HTML。
	Wait time.Duration
	// Interval 是首次轮询间隔。
	Interval time.Duration
	// MaxInterval 大于 Interval 时启用递增退避（每次 +Interval，封顶 MaxInterval）。
	//
	// 退避的意义：秒回的小任务要快抓住，10 分钟的长任务不该打出上百次查询
	// （上游按 QPS 限流的账号会被自己的轮询打限）。
	MaxInterval time.Duration
	// ErrStreakMax 是「查询本身连续失败」的容忍次数（<=0 时取 5）。
	//
	// 单次查询超时/5xx 不能立刻判死：上游任务可能还在跑，判死等于丢掉已付费的产物。
	ErrStreakMax int
}

// Hooks 让调用方注入自己的错误类型（本包不认任何具体错误类型）。
type Hooks struct {
	// Timeout 在等待上限耗尽时调用。
	Timeout func(waited time.Duration, taskID string) error
	// Abandoned 在请求上下文被取消（客户端断开）时调用。
	Abandoned func(waited, limit time.Duration, taskID string, cause error) error
}

// State 是一次轮询的结论。
type State struct {
	// Status 只在终态时有值：succeeded / failed；运行中返回空串。
	Status string
	// Detail 是失败原因（上游原文），成功时为空。
	Detail string
}

// StreakError 表示查询链路连续失败（不是任务失败）。
//
// 单独成型是为了让调用方能把它翻成自己的错误措辞（配置错误 / 上游故障），
// 同时保证文案里**一定带任务号**：没有任务号，运维既补不了产物也查不出上游发生了什么。
type StreakError struct {
	TaskID string
	Count  int
	Last   error
}

func (e *StreakError) Error() string {
	return fmt.Sprintf("查询上游任务 %s 状态连续失败 %d 次：%v", e.TaskID, e.Count, e.Last)
}

func (e *StreakError) Unwrap() error { return e.Last }

// ErrTerminal 表示「这次失败不该重试，立刻结束等待」。
//
// 为什么需要它：等待循环里的失败默认按「可能是抖动」处理（连续 5 次才放弃），
// 而有些失败是确定性的 —— 比如上游状态词不在 status_map 里（配置错）、
// 或者任务号为空。这类失败重试 5 次只是把同一句话再看 5 遍，还把用户
// 多挂了十几秒。poll 用 fmt.Errorf("%w: ...", ErrTerminal) 包一层即可喊停。
var ErrTerminal = errors.New("syncwait：终止轮询")

// ErrNoHook 表示调用方没提供错误构造器（配置期就错，不该在运行期才发现）。
var ErrNoHook = errors.New("syncwait：未提供错误构造器（Hooks 不能为空）")

// Wait 反复调用 poll 直到终态、超时或上下文取消。
//
// poll 的返回值：State{Status: ""} 表示仍在运行；只有终态才返回非空 Status。
// poll 返回 error 时按「查询失败」计数，连续失败达到上限后返回 *StreakError。
func Wait(ctx context.Context, opt Options, h Hooks, taskID string, poll func(context.Context) (State, error)) (State, error) {
	if h.Timeout == nil || h.Abandoned == nil {
		return State{}, ErrNoHook
	}
	if opt.Wait <= 0 || opt.Interval <= 0 {
		return State{}, errors.New("syncwait：等待上限与轮询间隔必须为正")
	}
	streakMax := opt.ErrStreakMax
	if streakMax <= 0 {
		streakMax = 5
	}

	startedAt := time.Now()
	// 截止时刻在**进入循环之前**算一次：放在循环体里每轮重算就等于永不超时。
	deadline := startedAt.Add(opt.Wait)
	interval := opt.Interval
	abandoned := func(cause error) error {
		return h.Abandoned(time.Since(startedAt), opt.Wait, taskID, cause)
	}

	var lastErr error
	streak := 0
	for {
		st, err := poll(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return State{}, abandoned(ctx.Err())
		case err != nil && errors.Is(err, ErrTerminal):
			// 确定性失败：不计数、不重试，原样上抛（调用方用 errors.As 取自己的类型）。
			return State{}, err
		case err != nil:
			lastErr = err
			streak++
			if streak >= streakMax {
				return State{}, &StreakError{TaskID: taskID, Count: streak, Last: lastErr}
			}
		default:
			streak = 0
			if st.Status == taskstate.Succeeded || st.Status == taskstate.Failed {
				return st, nil
			}
		}

		// 先判截止再睡：剩 0.5s 时睡 5s 等于多等一个周期，用户看到的就是「超时了还在挂」。
		if time.Now().Add(interval).After(deadline) {
			if remain := time.Until(deadline); remain > 0 {
				interval = remain
			} else {
				return State{}, h.Timeout(opt.Wait, taskID)
			}
		}
		select {
		case <-ctx.Done():
			return State{}, abandoned(ctx.Err())
		case <-time.After(interval):
		}
		if opt.MaxInterval > interval {
			interval += opt.Interval
			if interval > opt.MaxInterval {
				interval = opt.MaxInterval
			}
		}
	}
}
