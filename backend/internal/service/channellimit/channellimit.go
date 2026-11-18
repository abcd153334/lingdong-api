// Package channellimit 执行渠道级限流（channels.rpm_limit / channels.max_concurrency）。
//
// 背景（P2-4）：这两个字段自建库起就存在，管理台可编辑、渠道列表也展示，
// 但**从来没有任何执行点**——运营以为「这个渠道最多 60 rpm」，实际可以无限打，
// 直到上游 429 才发现。这是典型的假开关，比不提供更危险。
//
// 两层语义：
//   - rpm_limit：Redis 固定窗口计数（跨实例共享，窗口 = 自然分钟）；Redis 不可用时**放行**
//     （与全站降级口径一致：宁可少限流，不能因为缓存不可用把所有请求打成 503）。
//   - max_concurrency：见下。
//
// max_concurrency 有两种实现，由 `LD_CLUSTER_MODE` 选择：
//
//   - 单副本（默认）：进程内信号量。零额外往返，语义 = 配置值。
//   - 多副本：Redis 共享计数（一次 Lua 同时做并发 INCR 与 RPM INCR，往返次数与单副本
//     持平）。**必须共享**：进程内信号量在多副本下的真实上限是「配置 × 副本数」，
//     运营按 20 配置、3 个副本就是 60 —— 上游配额被打爆时，管理台看起来一切正常。
//     Redis 不可用时回落到进程内并打 channel_limit_degraded 指标（同样的 ×副本 放大，
//     但至少有曲线可查）。
//
// 两道限制都是**非阻塞**的：获取不到立即返回 LimitError，由选路层换下一个候选渠道或
// 直接回 429。阻塞等待会把网关的 goroutine 挂在上游配额上，等于把限流转成排队。
package channellimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/metrics"
)

// ErrRateLimited 是渠道限流的哨兵错误（调用方用 IsRateLimited 判定）。
var ErrRateLimited = errors.New("channel: 渠道限流")

// LimitError 携带建议重试秒数，用于 429 的 Retry-After。
type LimitError struct {
	Scope      string // "rpm" | "concurrency"
	RetryAfter int    // 秒，≥1
}

func (e *LimitError) Error() string {
	if e.Scope == "concurrency" {
		return fmt.Sprintf("%v：并发已达上限", ErrRateLimited)
	}
	return fmt.Sprintf("%v：每分钟请求数已达上限", ErrRateLimited)
}

func (e *LimitError) Unwrap() error { return ErrRateLimited }

// IsRateLimited 判断错误是否为渠道限流（含包装）。
func IsRateLimited(err error) bool { return errors.Is(err, ErrRateLimited) }

// rpmKeyPrefix 是固定窗口计数键前缀；窗口按自然分钟切分。
const rpmKeyPrefix = "ld:rl:ch:"

// Acquire 尝试获取渠道的调用额度，成功返回**必须调用一次**的释放函数。
//
// 释放函数只负责并发槽位（rpm 计数是窗口内的累计量，不随请求结束回退）。
func Acquire(ctx context.Context, ch *model.Channel) (func(), error) {
	if ch == nil {
		return func() {}, nil
	}
	// 两个上限都没配：什么都不用做，连 Redis 都不去。
	if ch.MaxConcurrency <= 0 && ch.RPMLimit <= 0 {
		return func() {}, nil
	}
	if clusterCounting() {
		return acquireShared(ctx, ch)
	}
	// 先占并发槽：本进程资源最紧张，拿不到就没必要再去打 Redis。
	release, err := acquireSlot(ch.ID, ch.MaxConcurrency)
	if err != nil {
		return func() {}, err
	}
	if err := takeRPM(ctx, ch.ID, ch.RPMLimit); err != nil {
		release()
		return func() {}, err
	}
	return release, nil
}

// clusterCounting 报告是否使用跨实例共享的计数。
//
// 只有「多节点共用一个数据库」时才需要；单副本用它只会多一次 Redis 依赖，
// 而单副本下进程内信号量本来就更准（没有网络延迟与 TTL 自愈的窗口）。
//
// 写成变量而不是函数，是为了让用例能直接切模式：LD_CLUSTER_MODE 是进程级配置，
// 在单测里改它需要重建整个 conf（会把别的用例一起带偏）。
var clusterCounting = func() bool {
	return conf.EnvConfig().ClusterMode
}

// AcquireRPM 只计 RPM，**不占并发槽**（轮询专用）。
//
// 为什么轮询不能占并发槽：轮询是后台行为，占槽就会与用户请求抢同一份额度，
// 表现成「有任务在跑时新请求 429」——这是最难解释的一种 429。
//
// 为什么轮询必须计 RPM：100 个在途任务 × 5s 轮询 ≈ 1200 真实 RPM 打到上游，
// 不计数的话网关以为「一条都没超」，直到上游 429 才发现配额早被打满。
func AcquireRPM(ctx context.Context, ch *model.Channel) error {
	if ch == nil {
		return nil
	}
	return takeRPM(ctx, ch.ID, ch.RPMLimit)
}

/* ------------------------------ 并发槽位 ------------------------------ */

// slots 保存每渠道的信号量；容量随配置变化时整体替换（在飞请求归还到旧信号量，
// 不会因替换而泄漏——释放闭包捕获的是它自己拿到的那只 chan）。
var (
	slotMu sync.Mutex
	slots  = map[uint64]chan struct{}{}
)

func acquireSlot(channelID uint64, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	sem := slotChan(channelID, limit)
	select {
	case sem <- struct{}{}:
		// 释放必须幂等：重复 `<-sem` 会把别的持有者的额度偷走，若信号量为空还会**永久阻塞**。
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	default:
		return func() {}, &LimitError{Scope: "concurrency", RetryAfter: 1}
	}
}

// Inflight 返回某渠道的实时并发占用（cur）与上限（limit；0 = 见调用方兜底）。
//
// 单副本读本机信号量；多副本读 Redis 共享计数（真值）。上限在多副本下由配置决定、
// 不在这个键里，所以返回 0 交给调用方用 channels.max_concurrency 补。
func Inflight(channelID uint64) (cur, limit int) {
	if clusterCounting() {
		if !db.RedisReady() {
			return 0, 0
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		n, err := db.RDB.Get(ctx, ccKeyPrefix+strconv.FormatUint(channelID, 10)).Int()
		if err != nil || n < 0 {
			return 0, 0
		}
		return n, 0
	}
	slotMu.Lock()
	defer slotMu.Unlock()
	sem, ok := slots[channelID]
	if !ok {
		return 0, 0
	}
	return len(sem), cap(sem)
}

// slotChan 返回（必要时重建）指定渠道的信号量。
func slotChan(channelID uint64, limit int) chan struct{} {
	slotMu.Lock()
	defer slotMu.Unlock()
	if sem, ok := slots[channelID]; ok && cap(sem) == limit {
		return sem
	}
	sem := make(chan struct{}, limit)
	slots[channelID] = sem
	return sem
}

// resetSlots 清空信号量缓存（仅测试使用：避免用例间互相污染）。
func resetSlots() {
	slotMu.Lock()
	slots = map[uint64]chan struct{}{}
	slotMu.Unlock()
}

/* ------------------------------ RPM 窗口 ------------------------------ */

// takeRPM 在固定窗口内累加计数；超限返回 LimitError。
func takeRPM(ctx context.Context, channelID uint64, limit int) error {
	if limit <= 0 || !db.RedisReady() {
		return nil
	}
	now := time.Now()
	key := rpmKeyPrefix + strconv.FormatUint(channelID, 10) + ":" + strconv.FormatInt(now.Unix()/60, 10)
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		// 限流器故障不阻断业务（与「Redis 不可用=降级放行」同一口径）。
		return nil
	}
	if n == 1 {
		// 窗口 60s；即便 EXPIRE 失败也只是留下一个下次同分钟会复用的键，不会无限堆积。
		_ = db.RDB.Expire(ctx, key, 60*time.Second).Err()
	}
	if int(n) > limit {
		retry := 60 - now.Second()
		if retry < 1 {
			retry = 1
		}
		return &LimitError{Scope: "rpm", RetryAfter: retry}
	}
	return nil
}

/* --------------------------- 跨实例共享计数（多副本） --------------------------- */

// ccKeyPrefix 是并发占用的键前缀。
const ccKeyPrefix = "ld:cc:ch:"

// ccLeakTTL 是并发计数键的存活时间。
//
// 为什么需要它：持有者进程被 kill -9 时不会走释放路径，计数会永久 +1；
// 没有 TTL 的话这条渠道的并发额度会越跑越少，最后彻底不可用（而且没人能解释）。
// 2 小时与 KEY 维度的在途计数同一口径：比任何一条真实上游调用都长得多，
// 又能在几次发布周期内自愈。
const ccLeakTTL = 2 * time.Hour

// acquireScript 一次往返完成「并发槽 + RPM 窗口」。
//
// 返回值：1 = 通过；-1 = 并发超限；-2 = RPM 超限。
// ARGV[1] = 并发上限（0 = 不限），ARGV[2] = RPM 上限（0 = 不限），
// ARGV[3] = 并发键 TTL(ms)，ARGV[4] = RPM 键 TTL(ms)。
//
// 为什么用一个脚本而不是两次命令：并发槽与 RPM 必须**原子地**一起成功/失败，
// 否则「并发拿到了、RPM 超了」需要在应用层回滚并发（多一次往返且有回滚失败窗口）。
var acquireScript = redis.NewScript(`
local lim = tonumber(ARGV[1])
if lim > 0 then
  local cc = redis.call("incr", KEYS[1])
  if redis.call("pttl", KEYS[1]) < 0 then
    redis.call("pexpire", KEYS[1], ARGV[3])
  end
  if cc > lim then
    redis.call("decr", KEYS[1])
    return -1
  end
end
local rpm = tonumber(ARGV[2])
if rpm > 0 then
  local n = redis.call("incr", KEYS[2])
  if n == 1 or redis.call("pttl", KEYS[2]) < 0 then
    redis.call("pexpire", KEYS[2], ARGV[4])
  end
  if n > rpm then
    if lim > 0 then redis.call("decr", KEYS[1]) end
    return -2
  end
end
return 1`)

// releaseScript 归还并发额度，减到负数时钳回 0。
//
// 钳零与 KEY 闸同理：TTL 到期后键消失，迟到的 DECR 会把它重建成 -1，
// 下一次 INCR 从 0 开始 —— 等于白送一个并发额度。
var releaseScript = redis.NewScript(`
local v = redis.call("decr", KEYS[1])
if v < 0 then redis.call("set", KEYS[1], 0) return 0 end
return v`)

// acquireShared 是多副本模式下的获取路径。
func acquireShared(ctx context.Context, ch *model.Channel) (func(), error) {
	if !db.RedisReady() {
		// 降级：回落到进程内信号量。真实上限会变成「配置 × 副本数」，
		// 所以必须打指标 —— 否则上游被打爆时没人知道是我们自己放开的。
		noteDegraded("redis_down")
		return acquireSlot(ch.ID, ch.MaxConcurrency)
	}
	now := time.Now()
	key := ccKeyPrefix + strconv.FormatUint(ch.ID, 10)
	rpmKey := rpmKeyPrefix + strconv.FormatUint(ch.ID, 10) + ":" + strconv.FormatInt(now.Unix()/60, 10)

	runCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	n, err := acquireScript.Run(runCtx, db.RDB, []string{key, rpmKey},
		ch.MaxConcurrency, ch.RPMLimit,
		int64(ccLeakTTL/time.Millisecond), int64(60*time.Second/time.Millisecond)).Int64()
	if err != nil {
		noteDegraded("script_error")
		return acquireSlot(ch.ID, ch.MaxConcurrency)
	}
	switch n {
	case -1:
		return func() {}, &LimitError{Scope: "concurrency", RetryAfter: 1}
	case -2:
		retry := 60 - now.Second()
		if retry < 1 {
			retry = 1
		}
		return func() {}, &LimitError{Scope: "rpm", RetryAfter: retry}
	}
	if ch.MaxConcurrency <= 0 {
		return func() {}, nil // 没占并发槽，释放无事可做
	}
	var once sync.Once
	return func() { once.Do(func() { releaseAsync(key) }) }, nil
}

// releaseCh 是释放请求的缓冲队列。
//
// 为什么异步：释放发生在请求返回路径上，为它多等一次 Redis 往返会直接抬高 p99；
// 而失败不影响正确性 —— TTL 会在 2h 内自愈。用**单个** worker 串行消费，
// 而不是每请求起一个 goroutine：后者在高并发下就是连接数炸弹。
var releaseCh = make(chan string, 1024)

var releaseOnce sync.Once

func releaseAsync(key string) {
	releaseOnce.Do(func() {
		go func() {
			for k := range releaseCh {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, _ = releaseScript.Run(ctx, db.RDB, []string{k}).Result()
				cancel()
			}
		}()
	})
	select {
	case releaseCh <- key:
	default:
		// 队列满：丢弃这次释放，靠 TTL 自愈。丢的是「额度晚还」，不是「多收钱」。
		noteDegraded("release_queue_full")
	}
}

func noteDegraded(reason string) {
	metrics.Inc(metrics.NameChannelLimitDegraded, "reason", reason)
}
