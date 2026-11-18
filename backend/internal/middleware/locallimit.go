package middleware

// Redis 不可用时的**进程内兜底限流**（KEY 维度）。
//
// 背景（D2）：`rpmAllowed` / `acquireConcurrency` 原本在 `!RedisReady()` 或
// `Incr` 出错时直接 `return true`。压测实测：停掉 Redis 60s，某把
// `rpm_limit=60` 的 KEY 从「60 次 200 + 40 次 429」变成「100 次全 200」——
// 也就是说，**把 Redis 打挂就是关掉限流**，攻击者不需要打穿它，只需要让它出错。
//
// 为什么不改成 fail-closed（503）：`/v1` 的调用方是第三方脚本，普遍没有退避，
// 503 会把「Redis 抖动几十秒」放大成「全站推理不可用 + 重试风暴」。
//
// 所以用第三档：**fail-local** —— 仍然放行，但把额度改由本进程计数器执行。
// 额度在多副本部署下会变成「副本数 × 配置」，这与渠道 `max_concurrency`
// （进程内信号量）是同一个已知取舍，写进文档即可。
//
// 与 `rateLimitWith` 的 strict 分支是同一套 localRateAllow（自然分钟窗口），
// 这里只补两件事：KEY 维度的键名、以及**并发**闸的进程内计数。

import (
	"fmt"
	"sync"
	"time"
)

const (
	// localCCIdleTTL 是空闲计数的回收期限。
	//
	// 与 Redis 侧的 ccLeakTTL 同一个理由：进程被 SIGKILL 时 DECR 不会执行，
	// 计数会卡在正值。Redis 侧靠键的 2h TTL 自愈，进程内没有 TTL 机制，
	// 于是按「条目最后一次活动时间」回收 —— 空闲超过 2h 的计数直接丢掉。
	localCCIdleTTL = 2 * time.Hour
	// localCCMaxEntries 是 map 的软上限：超过就先做一次陈旧清理，防止
	// 「很多把 KEY 各来一次」把 map 撑大（条目只有 24B 级，纯防御）。
	localCCMaxEntries = 4096
)

type localCCEntry struct {
	n    int
	last time.Time
}

var (
	localCCMu sync.Mutex
	localCC   = make(map[uint64]*localCCEntry, 64)
)

// localRateKey 是 KEY 维度 RPM 兜底的键名（与全站/公开面的 bucket 区分开）。
func localRateKey(keyID uint64) string { return "key:" + fmt.Sprint(keyID) }

// localKeyRateAllow 在 Redis 不可用时执行 KEY 维度的进程内 RPM。
func localKeyRateAllow(keyID uint64, limit int) bool {
	if limit <= 0 {
		return true
	}
	return localRateAllow(localRateKey(keyID), int(limit))
}

// localConcurrency 在 Redis 不可用时执行 KEY 维度的进程内并发闸。
//
// 语义与 Redis 版一致：拿不到立即返回 false（不阻塞 —— 阻塞会把网关 goroutine
// 挂在自己的额度上，等于把限流转成排队）。
// 返回的释放函数是**幂等**的：调用两次不会把计数减成负数。
func localConcurrency(keyID uint64, limit int) (func(), bool) {
	if limit <= 0 {
		return func() {}, true
	}
	now := time.Now()
	localCCMu.Lock()
	if len(localCC) > localCCMaxEntries {
		for id, e := range localCC {
			if now.Sub(e.last) > localCCIdleTTL {
				delete(localCC, id)
			}
		}
	}
	e := localCC[keyID]
	if e == nil {
		e = &localCCEntry{}
		localCC[keyID] = e
	}
	if e.n >= limit {
		localCCMu.Unlock()
		return func() {}, false
	}
	e.n++
	e.last = now
	localCCMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			localCCMu.Lock()
			defer localCCMu.Unlock()
			if x := localCC[keyID]; x != nil {
				x.n--
				if x.n < 0 {
					// 迟到的释放（TTL 回收后再 DECR）不许把计数变成负数，
					// 否则下一次 INCR 从 0 开始，等于白送一个额度。
					x.n = 0
				}
				x.last = time.Now()
			}
		})
	}, true
}

// resetLocalLimitsForTest 清空进程内计数（仅供测试）。
func resetLocalLimitsForTest() {
	localCCMu.Lock()
	localCC = make(map[uint64]*localCCEntry, 64)
	localCCMu.Unlock()
	localRateMu.Lock()
	localRateWin = make(map[string]localRateWindow, 64)
	localRateMu.Unlock()
}
