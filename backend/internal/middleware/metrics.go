package middleware

import (
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/metrics"
)

// unmatchedEndpoint 是「没有命中任何注册路由」的端点标签值。
//
// 不能把真实路径直接当标签：`/v1/tasks/12345` 每个 id 都是一条新时间序列，
// 一晚上就能把 Prometheus 的内存与磁盘打满。命中路由时用 FullPath()（含 `:id` 模板），
// 未命中时统一归到这一个是常量，基数恒定为 1。
const unmatchedEndpoint = "unmatched"

// GatewayMetrics 记录 /v1 网关的请求计数与耗时（B7 §2.10）。
//
// 为什么挂在**全局**而不是 v1 组：未注册的 /v1 路径由 NoRoute → relay.Handle 兜底返回 404，
// 这类请求不经过 v1 组中间件。而「客户端打到不存在的端点」恰恰是排障时最想看的信号，
// 挂组上会让它完全不可见。全挂 + 内部只对 /v1 前缀计数，两个目标一起满足。
func GatewayMetrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 非网关流量（/api、/f、/metrics 自身）直接放行：连计时都不做，避免无谓开销。
		if !isGatewayPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()

		endpoint := c.FullPath()
		if endpoint == "" {
			endpoint = unmatchedEndpoint
		}
		status := strconv.Itoa(c.Writer.Status())
		metrics.Inc(metrics.NameGatewayRequests, "endpoint", endpoint, "status", status)
		metrics.Observe(metrics.NameGatewayLatency, time.Since(start).Seconds(), "endpoint", endpoint)
	}
}

// isGatewayPath 判断是否属于 /v1 网关（只认 `/v1` 与 `/v1/...`，不认 `/v1abc`）。
func isGatewayPath(p string) bool {
	return p == "/v1" || strings.HasPrefix(p, "/v1/")
}
