package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/response"
)

// RateLimit 是基于 Redis 固定窗口的限流占位；Redis 不可用时降级跳过。
func RateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !db.RedisReady() {
			noteDegraded("redis_not_ready")
			if rateLimitFailClosed(c, "redis_not_ready", false) {
				return
			}
			c.Next()
			return
		}
		limit := rateLimitRPM()
		key := fmt.Sprintf("rl:%s:%s:%d", c.ClientIP(), c.FullPath(), time.Now().Unix()/60)
		n, err := db.RDB.Incr(c.Request.Context(), key).Result()
		if err != nil {
			// Redis 中途出错与「没起来」同口径：写面必须拒绝。
			// 否则把 Redis 打慢就等于亲手关掉限速 —— 攻击者只需要制造错误，不需要打穿。
			if rateLimitFailClosed(c, "redis_error", false) {
				return
			}
			noteDegraded("redis_error")
			c.Next()
			return
		}
		if n == 1 {
			db.RDB.Expire(c.Request.Context(), key, time.Minute)
		}
		if int(n) > limit {
			response.FailDef(c, errs.RateLimited)
			return
		}
		c.Next()
	}
}

// RateLimitN 是同款固定窗口限流，但用显式阈值，并按 admin 身份（而不是 IP）计数。
//
// 为什么单独一个：出网类端点（AI 编排的 dry-run 探针）有"替别人打第三方"的性质，
// 需要比全站默认值严格得多；按 IP 计数在多运营共用出口时会互相挤，
// 按 admin id 计数才是"每人 6 次/分钟"的真实语义。
func RateLimitN(limit int, bucket string) gin.HandlerFunc {
	return rateLimitWith(limit, bucket, false)
}

// RateLimitPublic 按契约声明的公开面策略挂限流（按 IP 计数）。
//
// 两条约束同时在这里落地：
//  1. 额度只在 `apicontract.PublicPolicy` 声明一次 —— 此前 600/300/120 同时出现在
//     路由、controller 注释与文档脚本里，改一处漏一处的表现是"文档写 600、线上是 300"；
//  2. 匿名面**不能 fail-open** —— `/api/apps/*` 是唯一免凭证入口，限流是它唯一的
//     滥用闸，而 notices/release 每条请求都要查库。Redis 抖动的几十秒里放行
//     等于给匿名请求一个无限打库窗口，那是把缓存故障放大成数据库故障。
//     兜底用进程内计数而不是 503：多实例各自限速仍有上限，公开面也保持可用。
func RateLimitPublic(key string) gin.HandlerFunc {
	return rateLimitWith(apicontract.MustPublicPolicy(key).RouteLimit, "public:"+key, true)
}

func rateLimitWith(limit int, bucket string, strict bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limit <= 0 {
			c.Next()
			return
		}
		who := c.GetString("username")
		if id := c.GetUint64("user_id"); id != 0 {
			who = strconv.FormatUint(id, 10)
		}
		if who == "" {
			who = c.ClientIP()
		}
		localKey := bucket + ":" + who

		if !db.RedisReady() {
			if strict && !localRateAllow(localKey, limit) {
				response.FailDef(c, errs.RateLimited)
				return
			}
			// 写面在 Redis 不可用时**拒绝**：fail-open 对读是降级，对写是
			// "这几十秒没有限速"——换码、下单、发码都能被无限打，而换 client_order_no
			// 就能绕过幂等。读面继续 fail-open（拦下来等于整个门户白屏）。
			//
			// 只对**已认证**请求生效：限流挂在鉴权之前，匿名请求在这里返回 503
			// 会盖掉本该给出的 401（客户端据此判断"要不要重新登录"），而匿名写
			// 面（注册/登录/发码）的 Redis 依赖由 service 层自己 fail-closed。
			if rateLimitFailClosed(c, "redis_not_ready", true) {
				return
			}
			noteDegraded("redis_not_ready")
			c.Next()
			return
		}
		key := fmt.Sprintf("rl:%s:%s:%d", bucket, who, time.Now().Unix()/60)
		n, err := db.RDB.Incr(c.Request.Context(), key).Result()
		if err != nil {
			// 与「Redis 没起来」同口径：中途出错时写面同样必须拒绝。
			if strict && !localRateAllow(localKey, limit) {
				response.FailDef(c, errs.RateLimited)
				return
			}
			if rateLimitFailClosed(c, "redis_error", true) {
				return
			}
			noteDegraded("redis_error")
			c.Next()
			return
		}
		if n == 1 {
			db.RDB.Expire(c.Request.Context(), key, time.Minute)
		}
		if int(n) > limit {
			response.FailDef(c, errs.RateLimited)
			return
		}
		c.Next()
	}
}

// localRateAllow 是 Redis 不可用时的进程内固定窗口（只给 RateLimitPublic 用）。
//
// 窗口与 Redis 版一致（自然分钟），所以恢复后不会出现"两个窗口叠加"的双倍额度。
// map 有上限清理：键是 IP，公网扫描会在几天内堆满，不清理就是一条内存泄漏。
func localRateAllow(key string, limit int) bool {
	minute := time.Now().Unix() / 60
	localRateMu.Lock()
	defer localRateMu.Unlock()
	if len(localRateWin) > 4096 {
		for k, v := range localRateWin {
			if v.minute != minute {
				delete(localRateWin, k)
			}
		}
	}
	w := localRateWin[key]
	if w.minute != minute {
		w = localRateWindow{minute: minute}
	}
	w.n++
	localRateWin[key] = w
	return w.n <= limit
}

type localRateWindow struct {
	minute int64
	n      int
}

var (
	localRateMu  sync.Mutex
	localRateWin = make(map[string]localRateWindow, 64)
)

// writeSensitive 判断本次请求是否属于「Redis 挂了也必须拦」的写面。
//
// 判据是**方法 + 路径**（不是 bucket 名）：bucket 是路由注册时手工写的标签，
// 新增端点时极易漏登记，而漏掉的后果是"Redis 抖动窗口内该端点无限可写"。
// 名单只覆盖动钱 / 动凭证 / 建资源三类；漏一个的后果是回到 fail-open 的现状，
// 不会比现在更差，所以这里宁可少写不要错杀（比如把 logout 也拦下来）。
func writeSensitive(c *gin.Context) bool {
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	path := c.FullPath()
	for _, p := range writeSensitivePaths {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

// rateLimitFailClosed 在 Redis 不可用/出错时决定「拦」还是「放」。
//
// 读面一律放行（拦下来等于整个门户白屏）；写面必须拒绝：Redis 抖动的那几十秒里，
// 兑换码换码、下单、支付发起、KEY 重置都会失去限速，而攻击者只要换一个
// client_order_no 就能绕过幂等，把上游与数据库一起打满。
//
// requireUser 表达两条挂载链的差别，不是可调策略：
//   - 全局 RateLimit 挂在鉴权**之前**，此时 user_id 恒为 0，不能拿它当判据
//     （加了就等于全线 fail-open）；
//   - RateLimitN 多数挂在鉴权**之后**，匿名请求在这里回 503 会盖掉本该给出的
//     401，让客户端以为"服务挂了"而不是"该重新登录"。
//
// 返回 true 表示已经写过响应（调用方必须直接 return）。
func rateLimitFailClosed(c *gin.Context, reason string, requireUser bool) bool {
	if !writeSensitive(c) {
		return false
	}
	if requireUser && c.GetUint64("user_id") == 0 {
		return false
	}
	noteDegraded(reason + "_write_closed")
	response.FailDef(c, errs.RateLimitUnavailable)
	return true
}

var writeSensitivePaths = []string{
	"/redeem",        // 兑换码：动余额
	"/orders",        // 下单 / 支付相关
	"/keys",          // 建/改/删 KEY
	"/auth/register", // 注册（含发码）
	"/auth/login",    // 登录（Redis 挂时认证本身也会 503，这里提前拦）
	"/contact-code",  // 发码（真实短信/邮件，是钱）
	"/contact/bind",  // 换绑（账号接管面）
	"/password/reset",
}

func rateLimitRPM() int {
	raw, ok := conf.System().Get("rate_limit_defaults")
	if !ok {
		return defaultRPM
	}
	var v struct {
		RPM int `json:"rpm"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.RPM <= 0 {
		return defaultRPM
	}
	return v.RPM
}

// defaultRPM 是全站默认每分钟请求数（系统配置里没配时用它）。
const defaultRPM = 60

// degradeLoggedAt 记录上次"限流降级"告警的时间（unix 秒），用于把自己压到每分钟一条。
var degradeLoggedAt atomic.Int64

// noteDegraded 让 fail-open 变得**可见**。
//
// Redis 抖动/被打满时，全站限流、KEY 的 RPM、KEY 并发闸、登录失败锁定四道闸
// 会同时消失，而改造前这件事在日志里连一行都没有 —— 攻击者只要把 Redis 打慢，
// 就等于亲手关掉了全站限流。每分钟最多一条，避免告警自身把日志打爆。
func noteDegraded(reason string) {
	now := time.Now().Unix()
	prev := degradeLoggedAt.Load()
	if now-prev < 60 || !degradeLoggedAt.CompareAndSwap(prev, now) {
		return
	}
	logger.L().Warn("限流降级：Redis 不可用，本次放行（全站限流与登录锁定同时失效）",
		zap.String("reason", reason))
	// 日志只有人看得到；指标才能告警。
	//
	// 压测实测：Redis 停 60s 期间 `/v1` 的 429 从 140/200 掉到 0/200，
	// 而 `ratelimit_degraded_total` 毫无变化（它此前只统计匿名资源面）。
	// 口径是「每分钟最多一条」：这里记录的是**降级事件**，不是被放行的请求数。
	metrics.Inc(metrics.NameRateLimitDegraded, "scope", "gateway", "result", reason)
}
