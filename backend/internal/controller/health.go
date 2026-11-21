// Package controller 存放 HTTP 控制器。
package controller

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/worker"
)

// Version 是后端版本号（S20 验收重做交付升版；与 acceptance-report-v2.1.0 一致）。
// v2.1.1：首次从 git tag 部署的版本 —— v2.1.0 这个 tag 已被初始提交占用，
// 复用它会变成"同名 tag 指向两处代码"，所以本次发布前进一位。
// v2.1.2：修复 deploy/scripts/lib/common.sh 的路径少退一级（所有部署脚本因此不可用）。
// v2.1.3：修复后端镜像在 linux/amd64 下启动即 SIGSEGV（退出码 139）——
// gen2brain/webp → purego/fakecgo 在 rt0_go 阶段调 NULL；解法是构建加 -tags nodynamic。
// v2.1.4：真机部署验收修的一批部署面缺陷（脚本/编排，见 deploy/）：升级门禁不核对版本、
// 健康检查探到别人的服务、空库备份必然失败、ld.sh 不认 COMPOSE_FILE、
// nginx 丢端口与不压缩、preflight 假警报。
// v2.1.5：LD_TRUSTED_PROXIES 默认值补 127.0.0.1/32 —— 缺了它，edge 与本机同机部署时
// 所有公网用户的客户端 IP 都会退化成 127.0.0.1（限流/验证码/IP 白名单/审计全塌成一个）。
// v2.2.0：部署形态收敛为「对标 new-api」——前端产物 go:embed 进二进制（单进程同时
// 提供页面与 API），删掉 web/nginx 容器；容器 4→3，对外端口只剩 8088（APP_PORT）；
// ld.sh init 自动生成密钥（0 手填）；LD_COOKIE_SECURE=auto 让明文入口也能登录；
// 发布镜像由 GitHub Actions 推到 GHCR，用户端可 pull 而非 build。
// entrypoint 层的静态缓存、gzip、安全头随之收回应用，那一整类「转发头配错」的故障消失。
//
// 必须是 var：发布流程用 `-ldflags -X` 把镜像标签注入进来，而链接器**只覆盖字符串
// var**，对 const 静默忽略。写成 const 时打出来的镜像，`/api/health.version` 永远是
// 源码里的字面量 —— 回滚时无法确认"跑的是哪个 tag"，发布也就不可追溯。
var Version = "v2.2.7"

var startTime = time.Now()

// nodeName 是本机在编排层里的名字（容器里默认是容器 ID 前缀）。
//
// 只用于 /api/health 回显：多节点部署时运维要能一眼确认"这次探到的是哪台"，
// 以及"模式有没有按预期生效"。不参与鉴权、不参与路由，所以不需要配置项。
var nodeName = func() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}()

// Health 处理 GET /api/health（**匿名公开**），返回 status / version / uptime / mode / node。
//
// `?deep=1` 会真的探一次 DB 与 Redis 并据此给 503：
//   - 编排层的探针必须用 deep —— 只探进程时，"PG 挂了但进程活着"会让容器一直
//     报 healthy，不会重启、也不告警，真实症状是"容器健康但全站 500"。
//   - 默认（浅）行为保持不变：任何扫描器/负载均衡都不该能触发 DB 查询。
//
// 为什么不带 Worker 状态：这个端点没有鉴权，任何人（含扫描器）都能读到
// 「leader 是否就绪、定时任务队列积压多少」——那是内部拓扑。
// 排障需要的这些细节已迁到 `GET /api/admin/system/health`（AdminAuth）。
func Health(c *gin.Context) {
	body := gin.H{
		"status":  "ok",
		"version": Version,
		"uptime":  int64(time.Since(startTime).Seconds()),
		// mode/node 是**部署事实**，不是内部拓扑：模式决定这台机器提不提供前端，
		// 节点名让运维在多副本/多线路里认出目标。两者都不涉及渠道、密钥、队列。
		"mode": conf.EnvConfig().NodeMode,
		"node": nodeName,
	}
	if c.Query("deep") != "1" {
		response.OK(c, body)
		return
	}
	// 2 秒上限：探针自己的超时必须小于编排层的 timeout，否则两者同时到点，
	// 运维看到的是"探针超时"而不是"DB 不可用"。
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	deps, err := probeDependencies(ctx)
	if err != nil {
		body["status"] = "degraded"
		body["deps"] = deps
		// 走统一失败信封（含 error_code）而不是裸 c.JSON：探针读的就是这段 JSON，
		// 而运维脚本按 error_code 分流"依赖挂"与"服务未起"，裸写会让两边都拿不到判据。
		response.FailDefData(c, errs.Def{
			Code: errs.ServiceUnavailable.Code, HTTP: http.StatusServiceUnavailable, Message: "依赖不可用",
		}, body)
		return
	}
	body["deps"] = deps
	response.OK(c, body)
}

// probeDependencies 探 DB 与 Redis，返回可公开的粗粒度状态（不含地址/错误原文）。
func probeDependencies(ctx context.Context) (gin.H, error) {
	out := gin.H{"db": "ok", "redis": "ok"}
	var firstErr error
	if db.DB == nil {
		out["db"] = "down"
		firstErr = errors.New("db not initialized")
	} else {
		var one int
		if err := db.DB.WithContext(ctx).Raw("SELECT 1").Scan(&one).Error; err != nil {
			out["db"] = "down"
			firstErr = err
		}
	}
	if !db.RedisReady() {
		out["redis"] = "down"
		if firstErr == nil {
			firstErr = errors.New("redis not ready")
		}
	}
	return out, firstErr
}

// AdminHealth 处理 GET /api/admin/system/health（管理台，AdminAuth）。
//
// 内容是 Worker 六字段：定时任务是全站资金与任务流转的唯一驱动，它停了的症状是
// 「任务一直排队、订单不过期、没有任何报错」，排障时必须有地方能直接看到
// 「轮询是否在跑、为什么没跑」。
func AdminHealth(c *gin.Context) {
	w := worker.StatusSnapshot()
	response.OK(c, gin.H{
		"status":  "ok",
		"version": Version,
		"uptime":  int64(time.Since(startTime).Seconds()),
		"worker": gin.H{
			"leader":         w.Leader,
			"redis_ready":    w.RedisReady,
			"strict_leader":  w.StrictLeader,
			"polling_paused": w.PollingPaused,
			"reason":         w.Reason,
			"jobs":           w.Jobs,
			// 每个跑过的 Job 的最近一次结果：没有它，「清理没跑」与「清理每轮都失败」
			// 在管理台上完全一样（都表现为「没有删除」）。
			"jobs_detail": w.JobsDetail,
		},
	})
}
