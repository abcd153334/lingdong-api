package worker

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/releasecheck"
)

// releaseCheckInterval 是自动检查新版本的节拍（6 小时）。
//
// 为什么不在前端定时轮询：管理台可能同时开着十个标签页，而"有没有新版本"
// 是个全站共享的事实。放在后台单点抓取，前端继续只读缓存（`GET /admin/system/release`
// 不出站），一个实例一天只发 4 次请求。
//
// 为什么是 6 小时：更新提示的价值在"当天能知道"，而清单只在发版时变。
// 更频繁只是把对方站点当心跳用。
const releaseCheckInterval = 6 * time.Hour

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
