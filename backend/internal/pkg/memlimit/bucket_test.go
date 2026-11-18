package memlimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withClock 把 now 固定成可推进的假时钟，用例结束后还原。
func withClock(t *testing.T, start time.Time) func(time.Duration) {
	t.Helper()
	cur := start
	var mu sync.Mutex
	now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	t.Cleanup(func() { now = time.Now })
	Reset()
	return func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(d)
	}
}

func TestAllowWithinWindow(t *testing.T) {
	_ = withClock(t, time.Unix(1_700_000_000, 0))
	if !Allow("k", 2, time.Minute) {
		t.Fatal("第 1 次应放行")
	}
	if !Allow("k", 2, time.Minute) {
		t.Fatal("第 2 次应放行")
	}
	if Allow("k", 2, time.Minute) {
		t.Fatal("第 3 次应被拒")
	}
	// 不同 key 各自独立配额
	if !Allow("other", 2, time.Minute) {
		t.Fatal("不同 key 不应互相影响")
	}
}

func TestAllowNewWindowResetsCounter(t *testing.T) {
	advance := withClock(t, time.Unix(1_700_000_000, 0))
	if !Allow("k", 1, time.Minute) || Allow("k", 1, time.Minute) {
		t.Fatal("同窗口内第 2 次应被拒")
	}
	advance(61 * time.Second)
	if !Allow("k", 1, time.Minute) {
		t.Fatal("跨窗口后应重新放行")
	}
}

func TestAllowNonPositiveLimitIsUnlimited(t *testing.T) {
	_ = withClock(t, time.Unix(1_700_000_000, 0))
	for i := 0; i < 100; i++ {
		if !Allow("k", 0, time.Minute) {
			t.Fatal("limit<=0 应视为不限制")
		}
	}
}

func TestSweepDropsStaleEntries(t *testing.T) {
	advance := withClock(t, time.Unix(1_700_000_000, 0))
	for i := 0; i < 5; i++ {
		if !Allow(string(rune('a'+i)), 10, time.Minute) {
			t.Fatalf("第 %d 个 key 应放行", i)
		}
	}
	advance(5 * time.Minute) // 超过 expire(2×window) 与 sweepInterval
	Allow("trigger", 10, time.Minute)

	mu.Lock()
	n := len(items)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("过期条目应被清理，期望只剩 1 条，实际 %d", n)
	}
}

func TestConcurrentAllowCapsAtLimit(t *testing.T) {
	_ = withClock(t, time.Unix(1_700_000_000, 0))
	const limit = 50
	var allowed int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if Allow("hot", limit, time.Minute) {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()
	if allowed != limit {
		t.Fatalf("并发下应恰好放行 %d 次，实际 %d", limit, allowed)
	}
}
