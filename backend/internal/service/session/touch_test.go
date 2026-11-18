package session

import (
	"sync"
	"testing"
	"time"
)

// 本文件测 Touch 的节流判定（纯内存，不需要 DB）。

const touchWindow = 5 * time.Minute

func resetTouch() {
	touchMu.Lock()
	touchSeen = make(map[uint64]time.Time)
	touchSwept = time.Time{}
	touchMu.Unlock()
}

// TestClaimTouchThrottles 断言同一会话在窗口内只放行一次。
func TestClaimTouchThrottles(t *testing.T) {
	resetTouch()
	base := time.Now()

	if !claimTouch(1, base, touchWindow) {
		t.Fatal("首次续期必须放行")
	}
	if claimTouch(1, base.Add(time.Minute), touchWindow) {
		t.Fatal("窗口内必须拦下（否则每个请求都会 UPDATE，节流等于没有）")
	}
	if !claimTouch(1, base.Add(touchWindow+time.Second), touchWindow) {
		t.Fatal("超出窗口后必须放行")
	}
	// 不同会话互不影响
	if !claimTouch(2, base.Add(time.Minute), touchWindow) {
		t.Fatal("会话 2 的首续期被会话 1 的登记挡住")
	}
}

// TestClaimTouchConcurrentSingleWinner 断言并发下同一会话只有一个 goroutine 拿到写权。
//
// 这正是原来 sync.Map（Load 再 Store）漏掉的场景：高并发时几十个请求同时判成
// 「该写」，节流在流量越大的时候越失效。
func TestClaimTouchConcurrentSingleWinner(t *testing.T) {
	resetTouch()
	now := time.Now()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if claimTouch(7, now, touchWindow) {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("同一会话同一时刻应恰好 1 个赢家，实际 %d", winners)
	}
}

// TestReleaseTouchAllowsRetry 断言写库失败后能立刻重试（否则失败会静默吞掉一个窗口）。
func TestReleaseTouchAllowsRetry(t *testing.T) {
	resetTouch()
	now := time.Now()

	if !claimTouch(3, now, touchWindow) {
		t.Fatal("首次续期必须放行")
	}
	releaseTouch(3)
	if !claimTouch(3, now.Add(time.Second), touchWindow) {
		t.Fatal("释放后必须能立刻重试")
	}
}

// TestClaimTouchSweepsStaleEntries 断言过期登记会被清掉（否则 map 随历史会话无界增长）。
func TestClaimTouchSweepsStaleEntries(t *testing.T) {
	resetTouch()
	base := time.Now()

	claimTouch(10, base, touchWindow)
	// 推进两个窗口：这次调用既登记新会话，也触发一次清扫
	claimTouch(11, base.Add(2*touchWindow), touchWindow)

	touchMu.Lock()
	_, kept10 := touchSeen[10]
	_, kept11 := touchSeen[11]
	size := len(touchSeen)
	touchMu.Unlock()

	if kept10 {
		t.Error("超过窗口的登记必须被清掉")
	}
	if !kept11 {
		t.Error("本次登记的会话不能被自己触发的清扫删掉")
	}
	if size != 1 {
		t.Errorf("清扫后只应剩 1 条，实际 %d", size)
	}
}
