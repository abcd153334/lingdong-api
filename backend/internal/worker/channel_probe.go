package worker

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/channel"
	"lingdong-api/internal/service/channelhealth"
)

// channelProbeTick 是探测任务节拍；实际探测间隔由 monitor.probe_interval_min 运行时自节流控制。
const channelProbeTick = time.Minute

// probeConcurrency 是一轮探测的并发度（配单个 5s 超时，最坏 200 个目标约 2 轮预算内跑完）。
const probeConcurrency = 8

// ChannelProbeJob 探测自动禁用渠道，连续成功达阈值后按 auto_enable 决定是否恢复。
type ChannelProbeJob struct {
	mu      sync.Mutex
	lastRun time.Time
}

// NewChannelProbeJob 构造渠道熔断探测任务。
func NewChannelProbeJob() *ChannelProbeJob { return &ChannelProbeJob{} }

// Name 返回任务标识。
func (*ChannelProbeJob) Name() string { return "channel_probe" }

// Interval 返回固定探测节拍（间隔由配置运行时控制）。
func (*ChannelProbeJob) Interval() time.Duration { return channelProbeTick }

// Run 执行一轮探测：间隔未到则跳过；探测成功连击达阈值且开启 auto_enable 时恢复渠道。
//
// P2-18 的三处修正：
//  1. **单 key 探测**（`channel.ProbeOne`）+ 单次 5s 超时：原来会把整个密钥池跑一遍；
//  2. **并发 8 + 单轮总预算 2min**：原来是串行，慢渠道会把一轮拖到下一轮；
//  3. **lastRun 在整轮跑完后才写**：原来在开始前写，一次失败/中断就要再等一个完整间隔。
func (j *ChannelProbeJob) Run(ctx context.Context) error {
	pol := channelhealth.CurrentPolicy()
	interval := time.Duration(pol.ProbeIntervalMin * float64(time.Minute))
	if interval <= 0 {
		interval = channelProbeTick
	}
	j.mu.Lock()
	if !j.lastRun.IsZero() && time.Since(j.lastRun) < interval {
		j.mu.Unlock()
		return nil
	}
	j.mu.Unlock()

	targets, err := channelhealth.ProbeTargets(ctx)
	if err != nil {
		logger.L().Warn("熔断渠道探测目标读取失败", zap.Error(err))
		return nil
	}
	if len(targets) == 0 {
		j.markRun()
		return nil
	}

	roundCtx, cancel := context.WithTimeout(ctx, channel.ProbeRoundBudget)
	defer cancel()

	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	skipped := 0
	for _, ch := range targets {
		select {
		case <-roundCtx.Done():
			skipped++
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(ch model.Channel) {
			defer wg.Done()
			defer func() { <-sem }()
			j.probeOne(roundCtx, ch, pol)
		}(ch)
	}
	wg.Wait()
	if skipped > 0 {
		// 预算耗尽：本轮未跑完，不写 lastRun，下个节拍立刻继续（避免「失败要等一个完整间隔」）。
		logger.L().Warn("熔断渠道探测超出单轮预算，剩余目标顺延", zap.Int("skipped", skipped), zap.Int("total", len(targets)))
		return nil
	}
	j.markRun()
	return nil
}

// probeOne 探测单条渠道并在满足条件时恢复。
func (j *ChannelProbeJob) probeOne(ctx context.Context, ch model.Channel, pol channelhealth.Policy) {
	res, err := channel.ProbeOne(ctx, ch.ID)
	ok := err == nil && res != nil && res.OK
	if !ok {
		logger.L().Info("熔断渠道探测失败", zap.Uint64("channel_id", ch.ID), zap.Error(err))
	}
	if channelhealth.MarkProbe(ctx, ch.ID, ok) && pol.AutoEnable {
		if err := channelhealth.Restore(ctx, ch.ID, ch.Name); err != nil {
			logger.L().Warn("熔断渠道恢复失败", zap.Uint64("channel_id", ch.ID), zap.Error(err))
		}
	}
}

// markRun 记录本轮完成时间（仅在一轮真正跑完后调用）。
func (j *ChannelProbeJob) markRun() {
	j.mu.Lock()
	j.lastRun = time.Now()
	j.mu.Unlock()
}
