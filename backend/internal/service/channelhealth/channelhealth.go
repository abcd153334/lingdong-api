// Package channelhealth 是渠道熔断状态机（v2.0 §4.6.5 / G16 / B6）。
// 语义：Redis 滑窗内 fail_rate > fail_threshold 且样本数 ≥ fail_window → 自动禁用（status=3）；
// 探测 Worker 每 probe_interval_min 分钟探测一次，连续 2 次成功且 auto_enable → 恢复（status=1）。
// 人工禁用（status=2）永不自动恢复；Redis 不可用时静默降级，不阻断调用链。
package channelhealth

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// probeThreshold 是恢复所需的连续探测成功次数（§4.6.5）。
const probeThreshold = 2

// Policy 是熔断参数（monitor.*；S10 已冻结键与校验）。
type Policy struct {
	AutoDisable      bool    `json:"auto_disable"`
	FailThreshold    float64 `json:"fail_threshold"`
	FailWindow       float64 `json:"fail_window"`
	AutoEnable       bool    `json:"auto_enable"`
	ProbeIntervalMin float64 `json:"probe_interval_min"`
}

// CurrentPolicy 读取运行时熔断参数（配置热更新即时生效），缺项按 seed 默认兜底。
func CurrentPolicy() Policy {
	p := Policy{AutoDisable: true, FailThreshold: 0.5, FailWindow: 10, AutoEnable: true, ProbeIntervalMin: 5}
	_ = conf.System().Unmarshal("monitor", &p)
	return p
}

// Window 是熔断滑窗内的**实时**样本（来自 Redis ZSET），与 channels.health 里
// 「连通测试累计的成功/失败数」不是同一口径：前者反映真实流量，后者反映最近一次测试。
//
// 为什么要单独暴露（P2-22）：管理台只显示测试计数时，运营看到「失败率 2%」却看到渠道
// 已被自动禁用，或者反过来看到「失败率 60%」却仍启用——因为熔断看的是**流量**滑窗，
// 而界面上根本没有这个数据，于是熔断行为无法解释、也无法验证。
type Window struct {
	Total    int64   `json:"total"`
	Fails    int64   `json:"fails"`
	FailRate float64 `json:"fail_rate"`
}

// WindowStats 批量读取多枚渠道的滑窗样本（单次 pipeline，按页取用）。
//
// 只返回窗口内**有样本**的渠道：无流量与「0 失败」在语义上不同，前者不该显示成 0%。
// Redis 不可用时返回空 map（熔断本身在无 Redis 时也不工作，界面显示「无数据」是实情）。
func WindowStats(ctx context.Context, channelIDs []uint64) map[uint64]Window {
	out := make(map[uint64]Window, len(channelIDs))
	if len(channelIDs) == 0 || !db.RedisReady() {
		return out
	}
	pipe := db.RDB.Pipeline()
	type pair struct {
		id     uint64
		req    *redis.IntCmd
		failed *redis.IntCmd
	}
	pairs := make([]pair, 0, len(channelIDs))
	for _, id := range channelIDs {
		reqKey, failKey := windowKeys(id)
		pairs = append(pairs, pair{id: id, req: pipe.ZCard(ctx, reqKey), failed: pipe.ZCard(ctx, failKey)})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return out
	}
	for _, p := range pairs {
		total, fails := p.req.Val(), p.failed.Val()
		if total <= 0 {
			continue
		}
		w := Window{Total: total, Fails: fails, FailRate: float64(fails) / float64(total)}
		out[p.id] = w
	}
	return out
}

// Evaluate 是纯函数：判定是否应自动禁用（total 为窗口内样本数，fails 为其中失败数）。
// fail_window 语义冻结为「窗口分钟数」且同为「最小样本数」（§4.6.5）。
func Evaluate(total, fails int64, pol Policy) bool {
	if !pol.AutoDisable || pol.FailThreshold <= 0 || pol.FailWindow <= 0 {
		return false
	}
	if float64(total) < pol.FailWindow {
		return false
	}
	return float64(fails)/float64(total) > pol.FailThreshold
}

// Record 记录一次渠道调用结果；命中熔断条件时自动禁用该渠道。
func Record(ctx context.Context, channelID uint64, ok bool) {
	if channelID == 0 || !db.RedisReady() {
		return
	}
	pol := CurrentPolicy()
	if pol.FailWindow <= 0 {
		return
	}
	window := time.Duration(pol.FailWindow * float64(time.Minute))
	now := time.Now()
	cutoff := strconv.FormatInt(now.Add(-window).UnixMilli(), 10)
	reqKey, failKey := windowKeys(channelID)
	score := float64(now.UnixMilli())
	member := strconv.FormatInt(now.UnixNano(), 10)

	pipe := db.RDB.Pipeline()
	pipe.ZRemRangeByScore(ctx, reqKey, "-inf", cutoff)
	pipe.ZRemRangeByScore(ctx, failKey, "-inf", cutoff)
	pipe.ZAdd(ctx, reqKey, redis.Z{Score: score, Member: member})
	if !ok {
		pipe.ZAdd(ctx, failKey, redis.Z{Score: score, Member: "f" + member})
	}
	pipe.Expire(ctx, reqKey, window*2)
	pipe.Expire(ctx, failKey, window*2)
	reqCmd := pipe.ZCard(ctx, reqKey)
	failCmd := pipe.ZCard(ctx, failKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return
	}
	if !Evaluate(reqCmd.Val(), failCmd.Val(), pol) {
		return
	}
	disable(ctx, channelID, pol, reqCmd.Val(), failCmd.Val())
}

// disable 将命中熔断的渠道置为自动禁用（仅限 status=1 且开启 auto_ban 的渠道）。
func disable(ctx context.Context, channelID uint64, pol Policy, total, fails int64) {
	var ch model.Channel
	if err := db.DB.WithContext(ctx).Where("id = ?", channelID).First(&ch).Error; err != nil {
		return
	}
	if ch.Status != int16(model.ChannelStatusEnabled) || !ch.AutoBan {
		return
	}
	if err := db.DB.WithContext(ctx).Model(&model.Channel{}).
		Where("id = ? AND status = ?", channelID, model.ChannelStatusEnabled).
		Update("status", model.ChannelStatusAutoBan).Error; err != nil {
		logger.L().Warn("channel auto ban failed", zap.Uint64("channel_id", channelID), zap.Error(err))
		return
	}
	reqKey, failKey := windowKeys(channelID)
	db.RDB.Del(ctx, reqKey, failKey)
	logger.L().Warn("channel auto disabled by circuit breaker",
		zap.Uint64("channel_id", channelID), zap.String("name", ch.Name),
		zap.Int64("samples", total), zap.Int64("fails", fails),
		zap.Float64("fail_threshold", pol.FailThreshold))
}

// probeBatchSize 是单轮探测的目标上限（P2-18：避免上千条熔断渠道把一轮探测拖成几十分钟）。
// 已恢复的渠道会离开 status=3 集合，窗口因此自然前移，不会长期饿死后面的渠道。
const probeBatchSize = 200

// ProbeTargets 返回待探测的自动禁用渠道（status=3，最多 probeBatchSize 条）。
func ProbeTargets(ctx context.Context) ([]model.Channel, error) {
	out := []model.Channel{}
	err := db.DB.WithContext(ctx).
		Where("status = ? AND deleted_at IS NULL", model.ChannelStatusAutoBan).
		Order("id").Limit(probeBatchSize).Find(&out).Error
	return out, err
}

// MarkProbe 记录一次探测结果；连续 probeThreshold 次成功返回 true（可恢复）。
func MarkProbe(ctx context.Context, channelID uint64, ok bool) bool {
	if !db.RedisReady() {
		return false
	}
	key := probeKey(channelID)
	if !ok {
		db.RDB.Del(ctx, key)
		return false
	}
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		return false
	}
	db.RDB.Expire(ctx, key, 30*time.Minute)
	return n >= probeThreshold
}

// Restore 将自动禁用渠道恢复为启用（仅 status=3 生效；人工禁用不受影响）。
func Restore(ctx context.Context, channelID uint64, name string) error {
	res := db.DB.WithContext(ctx).Model(&model.Channel{}).
		Where("id = ? AND status = ?", channelID, model.ChannelStatusAutoBan).
		Update("status", model.ChannelStatusEnabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		if db.RedisReady() {
			pk := probeKey(channelID)
			reqKey, failKey := windowKeys(channelID)
			db.RDB.Del(ctx, pk, reqKey, failKey)
		}
		logger.L().Info("channel restored by probe",
			zap.Uint64("channel_id", channelID), zap.String("name", name))
	}
	return nil
}

func windowKeys(channelID uint64) (string, string) {
	return fmt.Sprintf("ld:ch:hreq:%d", channelID), fmt.Sprintf("ld:ch:hfail:%d", channelID)
}

func probeKey(channelID uint64) string { return fmt.Sprintf("ld:ch:probe:%d", channelID) }
