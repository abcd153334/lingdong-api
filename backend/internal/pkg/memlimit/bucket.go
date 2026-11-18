// Package memlimit 提供「Redis 不可用时的进程内固定窗口限流」。
//
// 存在理由：整站的限流都建在 Redis 计数器上（多副本共享、原子 INCR）。Redis 一挂，
// 所有依赖它的限流点只能二选一 —— 全放行（等于对公网敞开）或全拒绝（整站 503）。
// 本包给匿名高成本端点第三条路：退化成**单副本**计数，阈值更严，进程重启即清零。
//
// 边界（改之前先读）：
//   - 只在降级期用。Redis 正常时调用方**不要**走这里：单副本计数在多副本下
//     实际阈值是 limit × 副本数，看起来"没生效"。
//   - 窗口是自然固定窗口（对齐 Unix 时间），与 Redis 那套 `now.Unix()/60` 同口径，
//     避免降级前后阈值表现不一致。
//   - 内存有硬上限：条目数超过 maxEntries 时强制清理；清理后仍超限则**放行**并
//     由调用方打指标。宁可降级期少拦，也不能让限流器自己 OOM。
package memlimit

import (
	"sync"
	"time"
)

const (
	// maxEntries 是条目上限。单条约 80 字节（key + 结构体），10 万条约 8 MB；
	// 再高的量级已经不是"少量 IP 的兜底"，而是被扫了 —— 那种量级交给 Redis 或网关。
	maxEntries = 100_000
	// sweepInterval 是清理间隔。清理是 O(n) 遍历，不能每次 Allow 都做。
	sweepInterval = time.Minute
)

// now 是时钟钩子（单测注入，生产恒为 time.Now）。
var now = time.Now

// entry 是一次固定窗口内的计数。
type entry struct {
	slot   int64 // 窗口编号（Unix 时间 / 窗口秒数），变化即视为新窗口
	count  int
	expire time.Time
}

var (
	mu        sync.Mutex
	items     = make(map[string]*entry, 256)
	lastSweep = now()
)

// Allow 判断 key 在当前窗口内是否还有配额。limit<=0 视为不限制。
//
// 返回 false 表示已超限（调用方应回 429）。并发安全。
func Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	if window <= 0 {
		window = time.Minute
	}
	t := now()
	slot := t.UnixNano() / int64(window)
	exp := t.Add(2 * window) // 留一个窗口的余量再清，避免边界抖动

	mu.Lock()
	defer mu.Unlock()

	if t.Sub(lastSweep) >= sweepInterval {
		sweepLocked(t)
	}

	e, ok := items[key]
	if !ok || e.slot != slot {
		if !ok && len(items) >= maxEntries {
			// 满了：先强制清一轮；仍然满就放行（不让限流器成为故障源）。
			sweepLocked(t)
			if len(items) >= maxEntries {
				return true
			}
		}
		items[key] = &entry{slot: slot, count: 1, expire: exp}
		return true
	}
	e.count++
	if e.expire.Before(exp) {
		e.expire = exp
	}
	return e.count <= limit
}

// sweepLocked 删除已过期条目；调用方必须持有 mu。
func sweepLocked(t time.Time) {
	lastSweep = t
	for k, e := range items {
		if e.expire.Before(t) {
			delete(items, k)
		}
	}
}

// Reset 清空全部计数（仅单测使用：用例之间不能互相污染）。
func Reset() {
	mu.Lock()
	items = make(map[string]*entry, 256)
	lastSweep = now()
	mu.Unlock()

	// 信号量也要清：用例之间留着别人的占位会表现成「莫名达到上限」。
	semMu.Lock()
	defer semMu.Unlock()
	sems = map[string]*semEntry{}
}
