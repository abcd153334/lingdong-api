package spec

import (
	"errors"
	"sync"
	"time"

	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/pricing"
)

// Loader 从持久层读一份规格（slug → spec）。由 main 注入，避免 relay 依赖 service。
//
// 为什么不让 spec 包自己查库：relay 是被 service 与 controller 共同依赖的底层包，
// 一旦它 import db/service 就会形成环，且单元测试必须连库。
// Loader 返回一份规格。
//
// 失败时必须给出**原因**（不存在 / 已停用 / 内容非法）：运行期把三者压成
// 「适配器未注册」的话，运营面对一条停用的规格会去查渠道配置、查注册表，
// 而真正该做的是把规格重新启用。
type Loader func(slug string) (*Spec, error)

var (
	loaderMu sync.RWMutex
	loader   Loader
)

// SetLoader 注入规格来源（传 nil 关闭惰性解析）。
func SetLoader(l Loader) { loaderMu.Lock(); loader = l; loaderMu.Unlock() }

// Resolve 是注入给 relay.SetAdapterResolver 的函数：把 `spec:<slug>` 解析成适配器。
//
// 返回 false 时 relay 会把该名字记进 5s 负缓存；规格写库后必须调
// relay.ForgetAdapter 清掉（见 service/spec 的写入路径）。
func Resolve(name string) (relay.Adapter, error) {
	const prefix = "spec:"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveNotFound, Name: name}
	}
	slug := name[len(prefix):]
	if !slugRe.MatchString(slug) {
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveNotFound, Name: name}
	}
	loaderMu.RLock()
	l := loader
	loaderMu.RUnlock()
	if l == nil {
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveNotFound, Name: name}
	}
	s, err := l(slug)
	if err != nil {
		// loader 已经把原因分好（不存在 / 停用 / 内容非法），原样上抛。
		var re *relay.AdapterResolveError
		if errors.As(err, &re) {
			return nil, re
		}
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveNotFound, Name: name}
	}
	if s == nil {
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveNotFound, Name: name}
	}
	// 从库里读出来的规格同样要过一遍校验：手工改库/迁移残留可能让它不合法，
	// 静默接受等于把「配置脏了」表现成「上游莫名其妙 400」。
	if err := s.Validate(); err != nil {
		field, msg := AsFieldError(err)
		detail := msg
		if field != "" {
			detail = field + "：" + msg
		}
		return nil, &relay.AdapterResolveError{Kind: relay.ResolveInvalid, Name: name, Detail: detail}
	}
	return New(s), nil
}

// modalityOf 查平台模型目录里的模态（用于同规格多异步端点时选提交端点）。
//
// 取不到（目录未加载 / 模型不在目录）返回空串，调用方按"无法判定"处理，
// 不用任何默认值猜——猜错就是拿视频请求去打音乐端点。
func modalityOf(modelID string) string {
	if modelID == "" {
		return ""
	}
	cat, err := pricing.Get()
	if err != nil || cat == nil {
		return ""
	}
	item := relay.FindModel(cat, modelID)
	if item == nil {
		return ""
	}
	return item.Modality
}

// cacheEntry 是进程内规格缓存项（避免每次选路都查库 + 反序列化 64KB JSON）。
type cacheEntry struct {
	spec *Spec
	at   time.Time
}

// Cache 是最小 LRU 语义的规格缓存（容量固定、TTL 固定）。
//
// 为什么不用第三方 LRU：容量只有几十条，map + 时间戳足够；引入依赖反而要维护
// 驱逐策略与并发语义。失效由 service/spec 在写库后显式 Purge 保证（多副本靠
// configsync 广播 + TTL 兜底），不依赖缓存自身的一致性。
type Cache struct {
	mu      sync.RWMutex
	items   map[string]cacheEntry
	ttl     time.Duration
	maxSize int
}

// NewCache 构造缓存（ttl<=0 用 30s，maxSize<=0 用 64）。
func NewCache(ttl time.Duration, maxSize int) *Cache {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if maxSize <= 0 {
		maxSize = 64
	}
	return &Cache{items: make(map[string]cacheEntry, maxSize), ttl: ttl, maxSize: maxSize}
}

// Get 命中未过期的缓存项。
func (c *Cache) Get(slug string) (*Spec, bool) {
	c.mu.RLock()
	it, ok := c.items[slug]
	c.mu.RUnlock()
	if !ok || time.Since(it.at) > c.ttl {
		return nil, false
	}
	return it.spec, true
}

// Put 写入缓存；超出容量时清空（规格数量极少，全清比实现 LRU 更不容易出错）。
func (c *Cache) Put(slug string, s *Spec) {
	if s == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.maxSize {
		c.items = make(map[string]cacheEntry, c.maxSize)
	}
	c.items[slug] = cacheEntry{spec: s, at: time.Now()}
}

// Purge 删除一条（写库/停用/删除后调用）。slug 为空表示全清。
func (c *Cache) Purge(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slug == "" {
		c.items = make(map[string]cacheEntry, c.maxSize)
		return
	}
	delete(c.items, slug)
}
