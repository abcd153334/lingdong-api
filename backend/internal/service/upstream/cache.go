package upstream

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	// cacheTTL 是快照有效期：价目表是"日更"级数据，10 分钟足够新鲜。
	cacheTTL = 10 * time.Minute
	// retryCooldown 是失败后的冷却：上游挂着时，每个击键窗口都发一次 30s 请求，
	// 用户每次都要白等半分钟、上游也会被无意义地打。冷却期内直接复用旧快照。
	retryCooldown = 45 * time.Second
	// hardStale 是快照的硬上限：24 小时前的价格没有参考价值，宁可报错。
	hardStale = 24 * time.Hour
)

// loadCall 是一次共享拉取（单飞）。
//
// 结构照抄 service/pricing 的 buildCall：读路径只拿 RLock，网络请求期间**不持锁**，
// 等待者靠 channel 收同一份结果。不要改成"持写锁做 HTTP" —— 那会把缓存命中一起串行化。
type loadCall struct {
	done  chan struct{}
	snap  *snapshot
	stale bool
	err   error
}

var (
	cacheMu     sync.RWMutex
	cached      *snapshot
	inflight    *loadCall
	nextRetryAt time.Time
)

// index 返回可用快照；stale=true 表示"刷新失败，这是上一次的数据"。
//
// 为什么不直接返回 error：运营点开面板是为了看一个数字，旧数字 + 明确的"已过期"
// 比一句"服务不可用"有用得多。但硬过期（24h）之后不再兜底 —— 那时旧价会误导决策。
func index(ctx context.Context) (*snapshot, bool, error) {
	cacheMu.RLock()
	if fresh(cached) {
		s := cached
		cacheMu.RUnlock()
		return s, false, nil
	}
	cacheMu.RUnlock()

	cacheMu.Lock()
	if fresh(cached) {
		s := cached
		cacheMu.Unlock()
		return s, false, nil
	}
	if c := inflight; c != nil {
		cacheMu.Unlock()
		<-c.done
		return c.snap, c.stale, c.err
	}
	if time.Now().Before(nextRetryAt) {
		s := cached
		cacheMu.Unlock()
		if s == nil || time.Since(s.fetchedAt) >= hardStale {
			return nil, false, ErrSourceUnavailable
		}
		return s, true, nil
	}
	c := &loadCall{done: make(chan struct{})}
	inflight = c
	cacheMu.Unlock()

	entries, err := loadUpstream(ctx)

	cacheMu.Lock()
	if err == nil {
		cached = &snapshot{entries: entries, fetchedAt: time.Now()}
		nextRetryAt = time.Time{}
		c.snap = cached
	} else {
		nextRetryAt = time.Now().Add(retryCooldown)
		if cached != nil && time.Since(cached.fetchedAt) < hardStale {
			c.snap, c.stale = cached, true
		} else {
			c.err = err
		}
	}
	inflight = nil
	close(c.done)
	cacheMu.Unlock()

	if c.err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrSourceUnavailable, c.err)
	}
	return c.snap, c.stale, nil
}

func fresh(s *snapshot) bool {
	return s != nil && time.Since(s.fetchedAt) < cacheTTL
}

// loadUpstream 执行一次拉取，生命周期与调用方请求**解耦**。
//
// context.WithoutCancel 是必须的：快照是进程级资源，由第一个触发它的管理员请求"代为"拉取。
// 如果沿用该请求的 ctx，他关掉面板/切页面就会取消 HTTP，所有等待者一起失败、快照永远填不上。
func loadUpstream(ctx context.Context) ([]entry, error) {
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
	defer cancel()
	return fetchIndex(fetchCtx)
}

// resetCacheForTest 只给单测用：清空快照、单飞与冷却状态。
func resetCacheForTest() {
	cacheMu.Lock()
	cached = nil
	inflight = nil
	nextRetryAt = time.Time{}
	cacheMu.Unlock()
}
