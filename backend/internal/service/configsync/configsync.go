// Package configsync 让多实例的 system_config 内存镜像与定价缓存保持一致（P1-8）。
//
// 背景：`conf.System()` 是每进程一份的内存镜像，原实现只有「保存的那个实例」会更新它。
// 多副本部署下，A 实例改了 `monitor.fail_threshold`，B/C 实例仍在用旧值——
// 表现是「同一份配置，不同副本行为不同」，且**没有任何报错**，极难排查。
//
// 机制（双通道，任一失效都不至于长期不一致）：
//  1. pub/sub：保存方 Publish 变更键集，其它实例收到后整表重载（实时，毫秒级）；
//  2. 30s 轮询：比对 `max(system_config.updated_at)`，发现变化就重载（兜底；
//     pub/sub 在 Redis 重启、订阅断线重连期间会丢消息）。
//
// 幂等性：重载是整表覆盖，重复执行没有副作用；因此不怕两条通道同时触发。
package configsync

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
)

const (
	// systemChannel 是 system_config 变更广播频道。
	systemChannel = "ld:cfg:system"
	// pricingChannel 是定价缓存失效广播频道（模型/分组/汇率变更）。
	pricingChannel = "ld:cfg:pricing"
	// specChannel 是 L2 规格变更广播频道（adapter_specs 增删改）。
	//
	// 为什么规格也需要广播：relay 的适配器注册表把**成功解析的实例**长期留在表里
	// （否则每个请求都要查库 + 反序列化 64KB JSON）。A 实例改了规格后，
	// 只有 A 自己的表被 ForgetAdapter 清掉，B/C 会一直用旧规格——
	// 表现是「同一份渠道配置，不同副本行为不同」，与 P1-8 是同一类故障。
	specChannel = "ld:cfg:spec"
)

// pollInterval 是轮询兜底周期（变量以便用例注入更短的周期）。
var pollInterval = 30 * time.Second

var (
	hookMu          sync.RWMutex
	pricingHook     func()
	specHook        func(slug string)
	lastSeen        time.Time
	lastSeenMu      sync.Mutex
	started         bool
	startedMu       sync.Mutex
	publishTimeout  = time.Second
	reloadTotal     int64 // 仅供日志统计（原子性不敏感）
	reloadTotalLock sync.Mutex
)

// SetPricingInvalidator 注入「仅清本机定价缓存」的函数（由 main 装配，避免 pricing ↔ configsync 循环导入）。
func SetPricingInvalidator(fn func()) {
	hookMu.Lock()
	pricingHook = fn
	hookMu.Unlock()
}

// NotifySystem 广播一次 system_config 变更（保存方调用）。
func NotifySystem(changed []string) {
	publish(systemChannel, map[string]any{"keys": changed, "ts": time.Now().UnixMilli()})
	// 发布方已经把新值写进本机镜像，同步 lastSeen 避免 30s 轮询再整表重载一次。
	rememberNow()
}

// NotifyPricing 广播一次定价缓存失效（模型/供应商/分组/汇率变更后调用）。
func NotifyPricing() {
	publish(pricingChannel, map[string]any{"ts": time.Now().UnixMilli()})
}

// SetSpecInvalidator 注入「仅清本机规格缓存」的函数（由 service/spec 装配）。
//
// 注入的必须是**只清本机**的版本：若注入会再次广播的版本，两个实例会互相触发死循环。
func SetSpecInvalidator(fn func(slug string)) {
	hookMu.Lock()
	specHook = fn
	hookMu.Unlock()
}

// NotifySpec 广播一次规格变更（slug 为空表示全清）。
func NotifySpec(slug string) {
	publish(specChannel, map[string]any{"slug": slug, "ts": time.Now().UnixMilli()})
}

// publish 发布一条消息；Redis 不可用时静默跳过（轮询兜底仍然生效）。
func publish(channel string, payload map[string]any) {
	if !db.RedisReady() {
		return
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := db.RDB.Publish(ctx, channel, buf).Err(); err != nil {
		logger.L().Warn("配置变更广播失败（依赖轮询兜底）", zap.String("channel", channel), zap.Error(err))
	}
}

// Start 启动订阅与轮询兜底；ctx 取消即停止。重复调用只生效一次。
func Start(ctx context.Context) {
	startedMu.Lock()
	if started {
		startedMu.Unlock()
		return
	}
	started = true
	startedMu.Unlock()

	// 初始基线：记录当前 max(updated_at)，避免启动后第一次轮询就把整表重载一遍
	//（main 在 Start 之前已经 LoadSystemConfig + Reload 过了）。
	if t, err := maxUpdated(); err == nil {
		setLastSeen(t)
	}
	go subscribe(ctx)
	go poll(ctx)
}

// subscribe 订阅两个频道并处理消息；断线后自动重连（Redis 客户端负责重连，这里只保证循环不退出）。
func subscribe(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if !db.RedisReady() {
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		sub := db.RDB.Subscribe(ctx, systemChannel, pricingChannel, specChannel)
		if _, err := sub.Receive(ctx); err != nil {
			_ = sub.Close()
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}
		logger.L().Info("配置订阅已建立",
			zap.Strings("channels", []string{systemChannel, pricingChannel, specChannel}))

		ch := sub.Channel()
		closed := false
		for !closed {
			select {
			case <-ctx.Done():
				_ = sub.Close()
				return
			case msg, ok := <-ch:
				if !ok {
					closed = true
					break
				}
				handleMessage(ctx, msg)
			}
		}
		_ = sub.Close()
		if !sleep(ctx, time.Second) {
			return
		}
	}
}

// handleMessage 处理一条广播。
func handleMessage(ctx context.Context, msg *redis.Message) {
	switch msg.Channel {
	case systemChannel:
		var payload struct {
			Keys []string `json:"keys"`
		}
		_ = json.Unmarshal([]byte(msg.Payload), &payload)
		reloadSystemConfig(ctx, "pubsub", payload.Keys)
	case pricingChannel:
		callPricingHook()
	case specChannel:
		var payload struct {
			Slug string `json:"slug"`
		}
		_ = json.Unmarshal([]byte(msg.Payload), &payload)
		callSpecHook(payload.Slug)
	}
}

// poll 每 pollInterval 检查一次 max(updated_at)；发现变化（或有记录但未见过）就重载。
func poll(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		t, err := maxUpdated()
		if err != nil {
			logger.L().Warn("配置轮询失败", zap.Error(err))
			continue
		}
		if t.IsZero() {
			continue
		}
		if !t.After(getLastSeen()) {
			continue
		}
		reloadSystemConfig(ctx, "poll", nil)
	}
}

// reloadSystemConfig 整表重载内存镜像并刷新定价缓存（幂等）。
func reloadSystemConfig(ctx context.Context, reason string, changed []string) {
	cfg, err := db.LoadSystemConfig(db.DB)
	if err != nil {
		logger.L().Warn("配置重载失败", zap.String("reason", reason), zap.Error(err))
		return
	}
	if len(cfg) == 0 {
		// 空结果只可能是「读错库/表还没初始化」。这时两件事都不能做：
		//   - 不能 Reload：会把正在用的镜像抹成空，写路径立刻全部 503；
		//   - 不能推进 lastSeen：推进了 30s 轮询就再也不会重试，永久停在空状态。
		logger.L().Warn("配置重载得到空结果，保留现有镜像并等待下一轮",
			zap.String("reason", reason))
		return
	}
	conf.System().Reload(cfg)
	if t, err := maxUpdated(); err == nil {
		setLastSeen(t)
	} else {
		rememberNow()
	}
	// 配置变了，定价派生结果（分组倍率/汇率/默认模型）必然可能变化：清本机缓存即可，
	// **不要再广播**（否则两个实例会互相触发无限循环）。
	callPricingHook()

	reloadTotalLock.Lock()
	reloadTotal++
	n := reloadTotal
	reloadTotalLock.Unlock()
	logger.L().Info("配置已同步", zap.String("reason", reason), zap.Int("keys", len(cfg)),
		zap.Strings("changed", changed), zap.Int64("reload_total", n))
}

func callPricingHook() {
	hookMu.RLock()
	fn := pricingHook
	hookMu.RUnlock()
	if fn != nil {
		fn()
	}
}

func callSpecHook(slug string) {
	hookMu.RLock()
	fn := specHook
	hookMu.RUnlock()
	if fn != nil {
		fn(slug)
	}
}

// maxUpdated 返回 system_config 最近一次更新时间（空表返回零值）。
func maxUpdated() (time.Time, error) {
	var row struct{ T *time.Time }
	if err := db.DB.Raw("SELECT max(updated_at) AS t FROM system_config").Scan(&row).Error; err != nil {
		return time.Time{}, err
	}
	if row.T == nil {
		return time.Time{}, nil
	}
	return *row.T, nil
}

func getLastSeen() time.Time {
	lastSeenMu.Lock()
	defer lastSeenMu.Unlock()
	return lastSeen
}

func setLastSeen(t time.Time) {
	lastSeenMu.Lock()
	lastSeen = t
	lastSeenMu.Unlock()
}

func rememberNow() { setLastSeen(time.Now().UTC()) }

// sleep 等待 d 或 ctx 结束；ctx 结束返回 false。
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
