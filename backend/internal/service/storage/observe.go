package storage

import (
	"sort"
	"strings"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
)

// 本文件是「现在到底写哪」的唯一可视面：一条启动/变更日志 + 两个抓取时指标
// + 管理台回显用的 Effective()。
//
// 为什么必须有它：写路径的失败方式是**静默**的 —— 配置丢了之后上传直接 503，
// 而管理台、任务列表、历史文件全都正常，只有新上传会失败。没有这条日志和指标，
// 排障只能靠用户报错，且无法回答「从哪天开始坏的」。

// backendNames 是全部可配置的后端标识（排序后，保证指标序列稳定）。
func backendNames() []string {
	names := make([]string, 0, len(builders))
	for name := range builders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LogEffective 打印当前生效的写入位置（启动一次、storage 配置变更后再打一次）。
//
// 未配置时打 **Error** 而不是 Warn：这不是"降级运行"，是"写不了"。
// 日志里必须写清「不会回落本地磁盘」，否则运维的第一反应是"文件应该还在本地吧"。
func LogEffective() {
	s, err := settingsOf()
	if err != nil {
		logger.L().Error("存储写入位置未确定：上传/产物转存将拒绝（503 storage_not_configured），不会回落本地磁盘",
			zap.String("storage_reason", reasonOf(err)))
		return
	}
	name := strings.TrimSpace(s.Active)
	fields := []zap.Field{zap.String("storage_active", name)}
	if name != "none" {
		ch := s.Channels[name]
		fields = append(fields,
			zap.String("storage_bucket", channelString(ch, "bucket")),
			zap.String("storage_endpoint", channelString(ch, "endpoint")),
		)
	}
	logger.L().Info("存储写入位置", fields...)
}

// reasonOf 把配置错误折成固定的两档：日志/看板按它分组，不按错误原文。
func reasonOf(err error) string {
	if err == ErrNotConfigured {
		return "not_configured"
	}
	return "invalid"
}

// RegisterMetrics 注册存储相关的抓取时指标（启动时调一次，可重复调用）。
//
// 抓取时求值而不是增量维护：配置可能被热更新改掉，增量计数在重启后也会偏移，
// 而这两个指标要回答的正是「此刻是否可写、此刻写哪」。
func RegisterMetrics() {
	metrics.RegisterGaugeFunc(metrics.NameStorageWriteReady, func() float64 {
		if _, err := Active(); err != nil {
			return 0
		}
		return 1
	})
	// 固定序列：每个已知后端一条，命中的为 1、其余为 0。
	// 只发命中的那一条会让看板在切换后端时丢掉旧序列，看不出「从哪切过来」。
	for _, name := range backendNames() {
		backend := name
		metrics.RegisterGaugeFunc(metrics.NameStorageActive, func() float64 {
			s, err := settingsOf()
			if err != nil {
				return 0
			}
			if strings.TrimSpace(s.Active) == backend {
				return 1
			}
			return 0
		}, "backend", backend)
	}
	// 每个后端「能不能构建」：凭据解不开/配置不全时 0，清理会在该后端上停摆。
	// 抓取时求值，与清理任务看到的 UsableBackends 同源（都走 BackendStates）。
	for _, name := range backendNames() {
		backend := name
		metrics.RegisterGaugeFunc(metrics.NameStorageBackendUsable, func() float64 {
			for _, st := range BackendStates() {
				if st.Name == backend && st.Usable {
					return 1
				}
			}
			return 0
		}, "backend", backend)
	}
}

// Effective 返回当前**实际生效**的写入位置（管理台「当前写入」一行用）。
//
// 与 Active() 的差别：这里不构建驱动、不联网，只回显配置事实；
// 未配置时 ok=false —— 调用方据此显示"未配置"红条，而不是编一个"本地磁盘"。
func Effective() (backend, bucket, endpoint string, ok bool) {
	s, err := settingsOf()
	if err != nil {
		return "", "", "", false
	}
	name := strings.TrimSpace(s.Active)
	ch := s.Channels[name]
	return name, channelString(ch, "bucket"), channelString(ch, "endpoint"), true
}
