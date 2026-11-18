package snapshot

// 进程内整表快照：TTL + 双检锁 + 失败沿用旧值 + 失败退避。
//
// 为什么需要这个包：应用域有两处一模一样的快照（公开面按 slug 的应用行、
// 维护态按 appID 的文案），都是「只读小表 + 热路径每个请求都要读 + 人工改动
// 按周计频」。这份逻辑里只有一处容易写错 —— 失败之后要不要继续尝试 ——
// 抄两遍就一定会有一处写得不一样。
//
// 为什么不含「失败策略」：两处对"加载失败且没有任何快照"的取向**刻意相反**：
//   - 公开面：返回错误 → 500。编造一个空应用会把「服务挂了」显示成
//     「这个应用还没配置品牌」，是最难排查的一类故障；
//   - 维护态：放行 → 宁可维护晚 10 秒生效，也不要把一次 DB 抖动放大成
//     全应用 503。
// 把这两种取向塞进一个 `failOpen bool` 会立刻产生第二个布尔（为第三处缓存
// 准备），所以本包只负责「拿不到就告诉你拿不到」，由调用方决定后果。
//
// 返回值约定：Get 成功时返回的 map 与元素**只读**，调用方不得改写。
// 元素是共享指针（如 *model.App），就地赋值会污染整个实例的快照。

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
)

// ErrUnavailable 表示"从未成功加载过，且本次加载也失败了"。
//
// 有旧快照时不会返回它（调用方拿到的是旧值 + nil）。
var ErrUnavailable = errors.New("snapshot: 快照不可用")

// loadTimeout 是单次加载的上限。加载用独立的 Background ctx：
// 共享加载不能因为"恰好触发它的那个客户端断连"而整体失败。
const loadTimeout = 5 * time.Second

// Snapshot 是一个带 TTL 的整表快照。
type Snapshot[K comparable, V any] struct {
	name string
	ttl  time.Duration
	load func(context.Context) (map[K]V, error)

	mu       sync.RWMutex
	data     map[K]V
	loadedAt time.Time // 最近一次**成功**加载（或失败后确认沿用旧值的时刻）
	nextTry  time.Time // 无旧快照时，失败后的退避截止
}

// New 构造快照。name 只用于日志，便于分辨是哪一处快照在报错。
//
// load 在调用时求值，因此调用方可以继续用「可替换的函数变量」做测试注入。
func New[K comparable, V any](name string, ttl time.Duration, load func(context.Context) (map[K]V, error)) *Snapshot[K, V] {
	return &Snapshot[K, V]{name: name, ttl: ttl, load: load}
}

// Get 取快照。
//
// 失败退避：无旧快照且加载失败时，ttl 内不再重试。不这么做的话，DB 抖动期间
// 每个请求都会在写锁后面排一次队、再打一次全表查询 —— 把故障放大成雪崩。
func (s *Snapshot[K, V]) Get(ctx context.Context) (map[K]V, error) {
	s.mu.RLock()
	data, loadedAt, nextTry := s.data, s.loadedAt, s.nextTry
	s.mu.RUnlock()

	if data != nil && time.Since(loadedAt) < s.ttl {
		return data, nil
	}
	if data == nil && time.Now().Before(nextTry) {
		return nil, ErrUnavailable
	}
	return s.refresh(ctx)
}

// refresh 重建快照。同一时刻只有一个协程真正查库。
func (s *Snapshot[K, V]) refresh(ctx context.Context) (map[K]V, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 双重检查：等锁期间别的协程可能已经刷新过（或刚失败退避）。
	if s.data != nil && time.Since(s.loadedAt) < s.ttl {
		return s.data, nil
	}
	if s.data == nil && time.Now().Before(s.nextTry) {
		return nil, ErrUnavailable
	}

	// 用独立的 ctx：调用方的请求取消不该让共享加载整体失败。
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
	defer cancel()

	next, err := s.load(loadCtx)
	if err != nil {
		logger.L().Error("加载快照失败",
			zap.String("snapshot", s.name),
			zap.Error(err),
			zap.Bool("snapshot_cache_empty", s.data == nil))
		s.nextTry = time.Now().Add(s.ttl)
		if s.data == nil {
			return nil, ErrUnavailable
		}
		// 有旧值：沿用，并把 loadedAt 推到现在 —— 否则每个请求都会重试一次。
		s.loadedAt = time.Now()
		return s.data, nil
	}
	s.data, s.loadedAt, s.nextTry = next, time.Now(), time.Time{}
	return next, nil
}

// Invalidate 让下一次 Get 重新加载（管理端写成功后调用）。
//
// 多实例不广播：其余实例最多 ttl 后自然收敛，这个传播窗口已写进运营文案。
func (s *Snapshot[K, V]) Invalidate() {
	s.mu.Lock()
	s.loadedAt = time.Time{}
	s.mu.Unlock()
}

// ResetForTest 清空快照（仅供测试）。
func (s *Snapshot[K, V]) ResetForTest() {
	s.mu.Lock()
	s.data, s.loadedAt, s.nextTry = nil, time.Time{}, time.Time{}
	s.mu.Unlock()
}

// ExpireForTest 把快照做旧到 TTL 之外（仅供测试，替代直接改包级变量）。
func (s *Snapshot[K, V]) ExpireForTest() {
	s.mu.Lock()
	s.loadedAt = time.Now().Add(-s.ttl - time.Second)
	s.mu.Unlock()
}
