package memlimit

import (
	"sync"
	"time"

	"lingdong-api/internal/pkg/metrics"
)

// DegradedAllow 是「Redis 不可用期间」的统一降级判定。
//
// 三选一里为什么选「降额放行」：
//   - 全放行：Redis 一挂，直链、图标、媒体代理、上传并发就都没有上限了 ——
//     恰恰在最脆弱的时候对公网敞开；
//   - 全拒绝：Redis 一挂，全站图片 404、上传全 500，把一次缓存故障放大成业务事故；
//   - 降额放行：按 limit/6（下限 5）执行，多副本时真实额度 ≈ limit/6 × 副本数。
//
// 这个偏差是**已知且可观测**的（ratelimit_degraded 的 allow/deny 两档），
// 也是「宁可少拦，不要全拒」这条产品口径的实现。
//
// key 的调用方约定：把「主体的稳定标识」传进来（IP、用户 id、密钥 id），
// 不要传整条 URL —— 那会让每个路径各占一个桶，等于没有上限。
func DegradedAllow(scope, key string, limit int) bool {
	lim := limit / 6
	if lim < 5 {
		lim = 5
	}
	ok := Allow("ld:rl:degraded:"+scope+":"+key, lim, time.Minute)
	result := "deny"
	if ok {
		result = "allow"
	}
	metrics.Inc(metrics.NameRateLimitDegraded, "scope", scope, "result", result)
	return ok
}

// Concurrent 是「并发数」类限制的降级实现：进程内信号量。
//
// 为什么不能复用 DegradedAllow（它按分钟计数）：并发数与速率是两种语义。
// 一个 limit=3 的用户连着传 10 个文件（任何时刻只有 1 个在传）是合法的，
// 用固定窗口计数会把他拦在第 6 个 —— 降级后行为与配置完全对不上，
// 表现为「Redis 一挂，上传就莫名失败」。
//
// 语义与 Redis 版一致：拿不到立刻返回 false（不阻塞排队），release 幂等。
// 多副本时真实上限 = limit × 副本数，这是降级期的已知偏差。
func Concurrent(key string, limit int) (func(), bool) {
	if limit <= 0 {
		return func() {}, true
	}
	semMu.Lock()
	e, ok := sems[key]
	if !ok || cap(e.ch) != limit {
		e = &semEntry{ch: make(chan struct{}, limit)}
		sems[key] = e
	}
	e.used = now()
	sweepSemsLocked()
	semMu.Unlock()

	select {
	case e.ch <- struct{}{}:
		// 幂等释放：重复 `<-ch` 会偷走别人的额度，信号量为空时还会永久阻塞。
		var once sync.Once
		return func() { once.Do(func() { <-e.ch }) }, true
	default:
		return func() {}, false
	}
}

type semEntry struct {
	ch   chan struct{}
	used time.Time
}

var (
	semMu sync.Mutex
	sems  = map[string]*semEntry{}
)

// maxSems 是信号量条数上限：每个「用户/渠道」一条，正常量级远低于它。
const maxSems = 8192

// sweepSemsLocked 清掉空闲信号量，防止内存随「来过的用户数」无限增长。
// 只在超过上限时做，且只删当前没人持有的（len==0）。
func sweepSemsLocked() {
	if len(sems) <= maxSems {
		return
	}
	for k, e := range sems {
		if len(e.ch) == 0 {
			delete(sems, k)
		}
	}
}
