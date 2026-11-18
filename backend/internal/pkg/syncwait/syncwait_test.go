package syncwait

import (
	"lingdong-api/internal/pkg/taskstate"

	"context"
	"errors"
	"testing"
	"time"
)

// hooks 是测试用的错误构造器（真实调用方注入 relay 的错误类型）。
func hooks() (Hooks, *int, *int) {
	timeouts, abandons := 0, 0
	return Hooks{
		Timeout: func(time.Duration, string) error {
			timeouts++
			return errors.New("timeout")
		},
		Abandoned: func(_, _ time.Duration, _ string, _ error) error {
			abandons++
			return errors.New("abandoned")
		},
	}, &timeouts, &abandons
}

// TestWaitReturnsSucceededImmediately 锁死正常路径：终态立即返回，不睡任何间隔。
func TestWaitReturnsSucceededImmediately(t *testing.T) {
	h, _, _ := hooks()
	calls := 0
	start := time.Now()
	st, err := Wait(context.Background(), Options{Wait: time.Second, Interval: 50 * time.Millisecond},
		h, "t1", func(context.Context) (State, error) {
			calls++
			return State{Status: taskstate.Succeeded}, nil
		})
	if err != nil {
		t.Fatalf("不应出错：%v", err)
	}
	if st.Status != taskstate.Succeeded {
		t.Fatalf("应为成功终态，实际 %q", st.Status)
	}
	if calls != 1 {
		t.Fatalf("应只查询一次，实际 %d", calls)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("不应等待，实际耗时 %v", elapsed)
	}
}

// TestWaitDeadlineComputedOnce 锁死「截止时刻只算一次」。
//
// 若在循环体里每轮重算，运行中的任务永远等不到超时 —— 请求会一直挂着，
// 用户的连接与冻结额度都被占死。
func TestWaitDeadlineComputedOnce(t *testing.T) {
	h, timeouts, _ := hooks()
	polls := 0
	start := time.Now()
	_, err := Wait(context.Background(), Options{Wait: 200 * time.Millisecond, Interval: 40 * time.Millisecond},
		h, "t2", func(context.Context) (State, error) {
			polls++
			return State{}, nil // 永远运行中
		})
	if err == nil {
		t.Fatal("等待上限耗尽必须返回错误")
	}
	if *timeouts != 1 {
		t.Fatalf("超时构造器应恰好调用一次，实际 %d", *timeouts)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("超出上限后必须放弃，实际耗时 %v", elapsed)
	}
	if polls < 2 {
		t.Fatalf("应按间隔多次查询，实际 %d 次", polls)
	}
}

// TestWaitAbandonsOnCancel 锁死「客户端断开立即放弃」，不等上限。
func TestWaitAbandonsOnCancel(t *testing.T) {
	h, _, abandons := hooks()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := Wait(ctx, Options{Wait: 10 * time.Second, Interval: 20 * time.Millisecond},
		h, "t3", func(context.Context) (State, error) {
			return State{}, nil
		})
	if err == nil {
		t.Fatal("取消后必须返回错误")
	}
	if *abandons != 1 {
		t.Fatalf("放弃构造器应调用一次，实际 %d", *abandons)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("取消应立即返回，实际耗时 %v", elapsed)
	}
}

// TestWaitStreakError 锁死「查询连续失败不判死、但要封顶」。
//
// 单次查询失败不能判死（上游任务可能还在跑，判死等于丢掉已付费的产物）；
// 连续失败到上限必须停，否则一条坏掉的查询端点会让请求耗满整个等待上限。
func TestWaitStreakError(t *testing.T) {
	h, timeouts, _ := hooks()
	calls := 0
	_, err := Wait(context.Background(), Options{Wait: 5 * time.Second, Interval: 5 * time.Millisecond, ErrStreakMax: 3},
		h, "t4", func(context.Context) (State, error) {
			calls++
			return State{}, errors.New("boom")
		})
	var streak *StreakError
	if !errors.As(err, &streak) {
		t.Fatalf("应为 StreakError，实际 %v", err)
	}
	if streak.Count != 3 || streak.TaskID != "t4" {
		t.Fatalf("StreakError 内容不符：%+v", streak)
	}
	if calls != 3 {
		t.Fatalf("应在第 3 次失败时停止，实际 %d 次", calls)
	}
	if *timeouts != 0 {
		t.Fatalf("连续失败不是超时，超时构造器不应被调用")
	}
}

// TestWaitStreakResetsAfterSuccess 锁死「成功一次就清零」：
// 偶发抖动不该累积成故障，否则一个 10 分钟的长任务必被误杀。
func TestWaitStreakResetsAfterSuccess(t *testing.T) {
	h, _, _ := hooks()
	calls := 0
	_, err := Wait(context.Background(), Options{Wait: 2 * time.Second, Interval: 5 * time.Millisecond, ErrStreakMax: 2},
		h, "t5", func(context.Context) (State, error) {
			calls++
			switch {
			case calls == 1 || calls == 3:
				return State{}, errors.New("flaky")
			case calls >= 5:
				return State{Status: taskstate.Succeeded}, nil
			default:
				// 查询成功但任务还在跑：这才是「失败—成功—失败—成功」的中间态，
				// 返回终态就直接结束了，测不到 streak 归零。
				return State{}, nil
			}
		})
	if err != nil {
		t.Fatalf("偶发失败不应判死：%v", err)
	}
	if calls != 5 {
		t.Fatalf("应重试到成功（第 5 次），实际 %d 次", calls)
	}
}

// TestWaitRequiresHooks 锁死「构造器缺失立刻报错」：
// 缺了它运行期只能 panic 或吞错，两者都会让故障不可诊断。
func TestWaitRequiresHooks(t *testing.T) {
	if _, err := Wait(context.Background(), Options{Wait: time.Second, Interval: time.Millisecond},
		Hooks{}, "t6", func(context.Context) (State, error) { return State{}, nil }); !errors.Is(err, ErrNoHook) {
		t.Fatalf("应为 ErrNoHook，实际 %v", err)
	}
}
