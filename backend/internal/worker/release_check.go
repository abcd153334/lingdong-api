package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/releasecheck"
)

// releaseCheckInterval 是自动检查新版本的兜底节拍（30 分钟）。
//
// 主机制不是它：管理台打开「版本与更新」就是一次刷新动作，会用
// `POST /admin/system/release/check` 立即出站（服务端 15s 冷却兜住连点）。
// 这里的定时任务只负责"没人打开页面时，侧栏红点也能自己出现"。
//
// 为什么不是 6 小时：清单在发版那一刻就变了，而"当天能知道"意味着最坏等 6 小时，
// 实际体验是"我明明发了版，后台还是旧的"。30 分钟 = 48 次/天，对清单站点仍然可以忽略。
// 为什么不再更短：再短就退化成拿对方站点当心跳，收益已经为零。
const releaseCheckInterval = 30 * time.Minute

// ReleaseCheckJob 定期抓一次发布清单，让侧栏版本号旁的红点自己出现。
//
// 它**不做任何更新动作**：只往 releasecheck 的进程内缓存写一份结果。
// 是否允许更新、按钮灰不灰，仍由 controller.buildReleasePayload 按同一份缓存判定。
type ReleaseCheckJob struct{}

// NewReleaseCheckJob 构造自动检查任务。
func NewReleaseCheckJob() *ReleaseCheckJob { return &ReleaseCheckJob{} }

// Name 返回任务标识。
func (*ReleaseCheckJob) Name() string { return "release_check" }

// Interval 返回检查节拍。
func (*ReleaseCheckJob) Interval() time.Duration { return releaseCheckInterval }

// Run 抓一次清单。
//
// 未配置 / 显式关闭（off）时直接返回：releasecheck.ManifestURL() 已经把这层语义
// 收在内部，这里不再重复判断，否则两处规则迟早会不一致。
func (*ReleaseCheckJob) Run(ctx context.Context) error {
	url := releasecheck.ManifestURL()
	if url == "" {
		return nil
	}
	st := releasecheck.Shared().Check(ctx, true)
	if !st.OK() {
		// 抓失败不算任务失败：离线部署、DNS 暂时不可用都属正常。
		// 但必须留日志 —— 否则"侧栏一直没有红点"会变成一个无从查起的问题。
		logger.L().Warn("自动检查新版本未得到结果",
			zap.String("error_kind", st.ErrorKind),
			zap.Int("http_status", st.HTTPStatus))
		return nil
	}
	logger.L().Info("自动检查新版本完成",
		zap.String("latest", st.Latest.Version), zap.String("url", url))
	return nil
}
