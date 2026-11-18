package controller

import (
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/worker"
)

// AdminCapacity 处理 GET /api/admin/data/capacity：最近一次容量体检结果（B7 §2.10）。
//
// 为什么单独一个端点而不是塞进 /admin/data/board：
//   - board 是**经营**看板，容量是**运维**信号，两者刷新频率与权限关注点都不同；
//   - board 的响应形状已被前端类型与 e2e 断言锁定，往里加字段的回归面远大于新增端点。
//
// 快照由 CapacityJob 每小时刷新；进程刚起、Job 还没跑过时**按需即时采集一次**，
// 否则运营打开页面只会看到「从未采集」，误以为监控坏了。
func AdminCapacity(c *gin.Context) {
	cfg := conf.EnvConfig()
	snap := worker.Capacity()
	if snap.At.IsZero() {
		snap = worker.CollectCapacity(c.Request.Context(), time.Now())
	}
	response.OK(c, gin.H{
		"snapshot": snap,
		"thresholds": gin.H{
			"warn_percent": cfg.CapacityWarnPercent,
			"min_free_gb":  cfg.CapacityWarnMinFreeGB,
		},
		"task_retention_days": cfg.TaskRetentionDays,
		"metrics_enabled":     cfg.MetricsHTTP,
	})
}
