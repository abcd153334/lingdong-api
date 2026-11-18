package worker

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/disk"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/task"
)

// capacityInterval 是容量统计节拍（每小时：磁盘水位的变化以小时计，更密只是徒增日志）。
const capacityInterval = time.Hour

// CapacitySnapshot 是一次容量体检的结果（同时供指标、日志与管理台读取）。
type CapacitySnapshot struct {
	// Files / Bytes 是 files 表登记的产物规模（任务产物 + 上传件）。
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
	// DiskTotal / DiskAvail 是 STORAGE_DIR 所在盘的容量与可用字节。
	DiskTotal uint64 `json:"disk_total"`
	DiskAvail uint64 `json:"disk_avail"`
	// FreeRatio 是可用占比（0-1；-1 表示磁盘信息不可用）。
	FreeRatio float64 `json:"free_ratio"`
	// Warn 表示本次体检触发了阈值。
	Warn bool `json:"warn"`
	// Warning 是人类可读的告警原因（为空表示正常）。
	Warning string `json:"warning"`
	// At 是本次体检时间。
	At time.Time `json:"at"`
	// Error 是**磁盘读取**失败的原因（非空时 FreeRatio 为 -1）。
	Error string `json:"error,omitempty"`
	// StatsError 是产物统计失败的原因（DB 抖动），与磁盘无关：
	// 两者混在一个字段里会让「DB 慢」被报成「磁盘监控瞎了」，处置方向完全相反。
	StatsError string `json:"stats_error,omitempty"`
}

// capacityLatest 保存最近一次体检结果（管理台读取，避免每次请求都去 stat 磁盘）。
var capacityLatest atomic.Value

// Capacity 返回最近一次容量体检快照；尚未跑过时返回零值（At 为零时间）。
func Capacity() CapacitySnapshot {
	if v, ok := capacityLatest.Load().(CapacitySnapshot); ok {
		return v
	}
	return CapacitySnapshot{FreeRatio: -1}
}

// CapacityJob 每小时统计产物总量与磁盘水位（B7 §2.10）。
type CapacityJob struct{}

// NewCapacityJob 构造容量监控任务。
func NewCapacityJob() *CapacityJob { return &CapacityJob{} }

// Name 返回任务标识。
func (*CapacityJob) Name() string { return "capacity" }

// Interval 返回统计节拍。
func (*CapacityJob) Interval() time.Duration { return capacityInterval }

// Run 执行一轮体检。
func (*CapacityJob) Run(ctx context.Context) error {
	snap := CollectCapacity(ctx, time.Now())
	capacityLatest.Store(snap)
	if snap.Error != "" {
		// 读不到磁盘不等于磁盘满了，但「监控瞎了」本身必须告警：
		// STORAGE_DIR 配错（例如容器里挂载点没进去）就是这种表现。
		logger.L().Error("容量监控：磁盘水位读取失败",
			zap.Bool("capacity_probe_failed", true),
			zap.String("storage_dir", conf.EnvConfig().StorageDir),
			zap.String("error", snap.Error))
		return nil
	}
	if snap.StatsError != "" {
		// DB 抖动只影响「产物总量」这一条曲线，磁盘水位仍然有效，所以是 Warn 不是 Error。
		logger.L().Warn("容量监控：产物总量统计失败",
			zap.Bool("capacity_stats_failed", true), zap.String("error", snap.StatsError))
	}
	if snap.Warn {
		logger.L().Error("容量水位超过阈值",
			zap.Bool("capacity_warn", true),
			zap.String("reason", snap.Warning),
			zap.Int64("files", snap.Files), zap.Int64("bytes", snap.Bytes),
			zap.Float64("free_ratio", snap.FreeRatio),
			zap.Uint64("disk_avail", snap.DiskAvail), zap.Uint64("disk_total", snap.DiskTotal))
		return nil
	}
	logger.L().Info("容量体检完成",
		zap.Int64("files", snap.Files), zap.Int64("bytes", snap.Bytes),
		zap.Float64("free_ratio", snap.FreeRatio))
	return nil
}

// CollectCapacity 采集一次容量快照（导出给单测与管理台按需采集）。
func CollectCapacity(ctx context.Context, now time.Time) CapacitySnapshot {
	return collectCapacityWith(ctx, now, conf.EnvConfig())
}

// collectCapacityWith 是带显式配置的实体（单测用固定阈值，不改全局 env）。
func collectCapacityWith(ctx context.Context, now time.Time, cfg conf.Env) CapacitySnapshot {
	snap := CapacitySnapshot{FreeRatio: -1, At: now}

	if bytes, files, err := task.StorageUsage(ctx); err == nil {
		snap.Bytes, snap.Files = bytes, files
		metrics.Set(metrics.NameStorageBytes, float64(bytes))
	} else {
		snap.StatsError = "统计产物总量失败: " + err.Error()
	}

	dir := cfg.StorageDir
	if dir == "" {
		dir = "."
	}
	// 目录可能还没建（首次启动、storage.active=s3 时本地目录根本不用）：
	// 取最近的存在祖先，否则 stat 恒失败、监控永远处于「瞎」的状态。
	if _, err := os.Stat(dir); err != nil {
		if abs, aerr := os.Getwd(); aerr == nil {
			dir = abs
		}
	}
	total, avail, derr := disk.Usage(dir)
	if derr != nil {
		snap.Error = "读取磁盘容量失败: " + derr.Error()
		return snap
	}
	snap.DiskTotal, snap.DiskAvail = total, avail
	if total == 0 {
		snap.Error = "磁盘总容量为 0（挂载信息异常）"
		return snap
	}
	snap.FreeRatio = float64(avail) / float64(total)
	metrics.Set(metrics.NameStorageDiskFreeRate, snap.FreeRatio)

	usedPercent := (1 - snap.FreeRatio) * 100
	minFreeBytes := uint64(cfg.CapacityWarnMinFreeGB) * 1024 * 1024 * 1024
	switch {
	case cfg.CapacityWarnPercent > 0 && usedPercent >= float64(cfg.CapacityWarnPercent):
		snap.Warn = true
		snap.Warning = fmt.Sprintf("已用 %.1f%% ≥ 阈值 %d%%", usedPercent, cfg.CapacityWarnPercent)
	case minFreeBytes > 0 && avail < minFreeBytes:
		snap.Warn = true
		snap.Warning = fmt.Sprintf("剩余 %.1f GiB < 下限 %d GiB",
			float64(avail)/1024/1024/1024, cfg.CapacityWarnMinFreeGB)
	}
	return snap
}
