package controller

import (
	"crypto/subtle"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/response"
)

// holdProbeMaxSeconds 是探针单次最长挂起时长。
//
// 比应用侧同步等待上限（780s）略大一点，才能验证「代理层是否放行完整等待」；
// 但必须有硬上限，否则这个端点本身就成了一个可以占满连接的工具。
const holdProbeMaxSeconds = 900

// HoldProbe 是长等待探针：**保持连接 N 秒且中途不写任何字节**，然后回一个 JSON。
//
// 它存在的唯一理由是把「同步图像为什么总在某一秒被切断」变成可测量的事实。
// 同步图像请求在等待上游期间一个字节都不会写出（期间没有任何可保活的内容），
// 所以这条链路上任意一层的空闲/读超时都会先断：客户端看到那一层的 504，
// 后端日志里却只有一次正常的「客户端断开」。没有这个探针，就只能靠猜。
//
// 使用方法（从外到内逐层量，哪一层先断就是哪一层的问题）：
//
//	curl -m 700 -H 'X-Diag-Token: <LD_DIAG_HOLD_TOKEN>' \
//	     'https://<公网域名>/api/diag/hold?seconds=90' -w '\n%{http_code} %{time_total}\n'
//	curl -m 700 -H 'X-Diag-Token: <token>' 'http://127.0.0.1:8080/api/diag/hold?seconds=90'
//
// 逐层对照的结果可以直接读出「超时阶梯」真正的瓶颈在哪一跳。
//
// 安全：未配置 LD_DIAG_HOLD_TOKEN 时**端点不存在**（404），不返回 403 ——
// 不告诉扫描器这里有个被禁用的口子。
func HoldProbe(c *gin.Context) {
	token := conf.EnvConfig().DiagHoldToken
	if token == "" {
		response.NotFound(c)
		return
	}
	// 只认请求头。**不再支持 ?token=**：URL 会进 nginx access log、代理日志、
	// 浏览器历史与 Referer，口令一旦落进这些地方就等于永久泄漏，
	// 而这条路由挂在 api 组之外（无限流、无审计），泄漏后可以反复建立长连接。
	got := strings.TrimSpace(c.GetHeader("X-Diag-Token"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		response.NotFound(c)
		return
	}

	seconds := 30
	if raw := strings.TrimSpace(c.Query("seconds")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			response.FailDefMsg(c, errs.InvalidParam, "seconds 必须是正整数")
			return
		}
		if n > holdProbeMaxSeconds {
			n = holdProbeMaxSeconds
		}
		seconds = n
	}

	start := time.Now()
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()

	select {
	case <-timer.C:
		response.OK(c, gin.H{
			"held_seconds": seconds,
			"elapsed_ms":   time.Since(start).Milliseconds(),
		})
	case <-c.Request.Context().Done():
		// 客户端（或它前面的代理）先断了：这正是要观察的现象。
		// 回包已经写不出去，只记一行日志，让「本端还在等、对端 60s 就走了」留下证据。
		elapsed := time.Since(start)
		holdProbeLog(seconds, elapsed)
	}
}

// holdProbeLog 记录「探针被提前切断」这一事实与切断时长。
//
// 时长本身就是结论：如果每次都在整 60s 断，那是某一层的默认读超时，
// 而不是上游慢或应用主动放弃（应用侧上限是 780s）。
func holdProbeLog(want int, elapsed time.Duration) {
	logger.L().Warn("长等待探针被提前切断",
		zap.Int("want_seconds", want),
		zap.Float64("elapsed_seconds", elapsed.Seconds()),
		zap.String("hint", "等待时长与实际配置的上限不符时，先查前置代理的读超时（nginx proxy_read_timeout 默认 60s、云负载均衡空闲超时常见 60s）"),
	)
}
