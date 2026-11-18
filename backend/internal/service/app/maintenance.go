package app

// 维护模式的热路径缓存。
//
// 为什么不能每次请求查库：这段判断在 TokenAuth 里，是**每一个** /v1 请求的
// 必经之路。一次额外的 SELECT 会被放大到全部推理流量上，而维护开关是
// 按周计频的运营动作 —— 用一次查询换 10 秒的新鲜度不值得。
//
// 为什么不用 Redis：维护开关必须在该应用全部实例上一致，但只读一份小表；
// 引入 Redis 的代价是"Redis 挂掉时维护模式失效或全站 503"，两者都不可接受。
// 10 秒的进程内快照在"误开维护"这个场景下是足够的响应速度。
//
// 机制（TTL / 双检锁 / 失败沿用 / 失败退避）在 pkg/snapshot。

import (
	"context"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/snapshot"
)

const maintTTL = 10 * time.Second

// maintLimit 防止把整张表读进内存：应用数量是平台拓扑，正常情况下是个位数。
const maintLimit = 1000

// maintRow 是加载查询的行。
type maintRow struct {
	ID      uint64
	Message string
}

// loadMaintenance 是可替换的加载器（单测用它注入失败与调用计数）。
var loadMaintenance = func(ctx context.Context) (map[uint64]string, error) {
	var rows []maintRow
	if err := db.DB.WithContext(ctx).
		Table("apps").
		Select("id, maintenance_message AS message").
		Where("maintenance_mode = ?", true).
		Order("id").
		Limit(maintLimit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint64]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Message
	}
	return out, nil
}

var maintSnap = snapshot.New("app_maintenance", maintTTL, func(ctx context.Context) (map[uint64]string, error) {
	return loadMaintenance(ctx)
})

// MaintenanceOf 返回该应用是否处于维护模式，以及对外提示文案。
//
// 失败策略（与公开面**相反**，刻意）：无快照且加载失败 ⇒ **放行**。
// 把 DB 抖动放大成"全平台 503"的代价，远大于"维护晚 10 秒生效"。
// 有旧快照时沿用旧值 —— 宁可多拦 10 秒，也不要因为一次抖动把维护中的应用放开。
func MaintenanceOf(ctx context.Context, appID uint64) (string, bool) {
	if appID == 0 {
		// 平台直连密钥不属于任何应用，连缓存都不读。
		return "", false
	}
	cache, err := maintSnap.Get(ctx)
	if err != nil {
		return "", false
	}
	msg, blocked := cache[appID]
	return msg, blocked
}

// InvalidateMaintenance 让本实例立刻重新加载（管理端写成功后调用）。
//
// 多实例不需要广播：其余实例最多 maintTTL 后自然收敛，而"开启维护"本就
// 允许几秒的传播窗口 —— 写进确认文案即可，不要为此引入分布式通知。
func InvalidateMaintenance() { maintSnap.Invalidate() }

// ResetMaintenanceForTest 清空缓存（仅供测试）。
func ResetMaintenanceForTest() { maintSnap.ResetForTest() }
