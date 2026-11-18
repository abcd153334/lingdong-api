// Package middleware 提供鉴权 / 审计 / 限流 / gzip / CORS / recover 通用中间件。
package middleware

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"lingdong-api/internal/db"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/authcookie"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/keyscope"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	appsvc "lingdong-api/internal/service/app"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/mfa"
	"lingdong-api/internal/service/session"
)

// UserAuth 是门户用户鉴权：校验 Cookie 会话票，加载用户并检查账号状态。
//
// 凭证只有一个来源：`ld_session` Cookie。这里刻意**不读** Authorization 头 ——
// 若同时接受两种来源，CSRF 就必须按「凭证来自哪里」分支豁免，
// 而那个分支一旦写错就是 CSRF 绕过，且从行为上看不出来（功能都正常）。
func UserAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, sess, ok := authenticate(c)
		if !ok {
			return
		}
		if !mustChangeGate(c, u) {
			return
		}
		setUserContext(c, u, sess)
		c.Next()
	}
}

// mustChangeGate 拦住「初始口令还没改」的账号。
//
// 为什么放在中间件而不是各个 handler：初始口令只可能来自环境变量或随机生成，
// 属于一次性的交接凭证。只要有一条业务路径能用它跑通，这个账号就会长期带着
// 一个可能已经出现在日志/截图里的口令在跑。
//
// 三个鉴权出口都要过这里（UserAuth / TokenAuth / UserTokenAuth 的两个分支）：
// 只在门户口拦、不拦 Bearer 与 API KEY，等于给临时口令留了三条侧门 ——
// `POST /api/user/login` 换成票、再拿票调 `/api/user/*`，门禁就整段被绕过。
func mustChangeGate(c *gin.Context, u *model.User) bool {
	if !mustChangeBlocked(c, u) {
		return true
	}
	response.FailDef(c, errs.PasswordChangeRequired)
	return false
}

// mustChangeBlocked 是 gate 的判据本体（不写响应）。
//
// 单独拆出来是因为 `/v1` 的错误体必须是 OpenAI 形状 —— 那里只能自己写响应。
// 判据仍然只有一份：形状不同 ≠ 判据不同，两处各写一遍 `MustChangePassword && !exempt`
// 迟早会改一处漏一处。
func mustChangeBlocked(c *gin.Context, u *model.User) bool {
	return u != nil && u.MustChangePassword && !passwordSetupExemptPath(c.Request.URL.Path)
}

// passwordSetupExempt 是「临时口令未改」时仍可访问的**精确路径**白名单。
//
// 刻意用精确匹配而不是前缀：前缀放行 `HasPrefix(path, "/api/portal/auth/")`
// 会让 contact/*（换绑邮箱手机）与 mfa/*（重绑二次验证）一并放行 ——
// 那两条正好是账号接管路径，临时口令必须先失效才能用。
//
// revoke-all 两条保留：它是止损动作（怀疑泄露先踢会话），只能踢自己的会话，
// 无越权面；拦掉会让「临时口令账号」连自救都做不了。
var passwordSetupExempt = map[string]struct{}{
	"/api/portal/me":                       {}, // 前端 /settings 首屏依赖
	"/api/portal/auth/password/change":     {}, // 改密本身
	"/api/portal/auth/logout":              {}, // 登出
	"/api/portal/auth/sessions/revoke-all": {}, // 止损：踢掉全部会话
	"/api/user/self":                       {}, // 账号面读自己（同 /portal/me）
	"/api/user/password/change":            {}, // 账号面改密
	"/api/user/logout":                     {}, // 账号面登出
	"/api/user/sessions/revoke-all":        {}, // 账号面止损
}

// passwordSetupExemptPath 只做一次查表。
func passwordSetupExemptPath(path string) bool {
	_, ok := passwordSetupExempt[path]
	return ok
}

// AdminAuth 是管理面鉴权：在会话校验基础上要求实时角色为管理员。
func AdminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, sess, ok := authenticate(c)
		if !ok {
			return
		}
		// 判权用 DB 实时角色（authenticate 已加载 u）。
		// 不能用会话里存的快照：管理员被降权后，旧会话必须立刻失去管理权限。
		if int(u.Role) != 0 {
			response.FailDef(c, errs.Forbidden)
			return
		}
		// 初始口令未改的超管同样先改密：放行的是 /api/portal/auth/*（改密页走那里），
		// 管理台业务端点一律 403 —— 否则"随机口令只打印一次"就形同虚设。
		if !mustChangeGate(c, u) {
			return
		}
		// 强制二次验证：只拦「该绑还没绑」的管理员，且必须留出绑定所需的三条路径，
		// 否则管理员一进管理台就被 403，连绑定页都打不开 —— 自己把自己锁在门外。
		if authcfg.MFAPolicy() == authcfg.MFAPolicyRequiredForAdmin && !mfaExemptPath(c.Request.URL.Path) {
			enabled, err := mfa.New(db.DB).IsEnabled(c.Request.Context(), u.ID)
			if err != nil {
				response.FailDef(c, errs.Internal)
				return
			}
			if !enabled {
				response.FailDef(c, errs.MFASetupRequired)
				return
			}
		}
		setUserContext(c, u, sess)
		c.Next()
	}
}

// mfaExemptPath 是「强制绑定 2FA」的放行白名单。
//
// 只放三条：查状态、生成密钥、确认绑定。其余 `/admin/**` 一律 403 ——
// 白名单多一条就等于多一个可以在未完成 2FA 时使用的入口。
//
// 现状备注：这三条实际挂在**门户**组（`/api/portal/auth/mfa/*`），本身不经过
// 本函数，所以按当前路由结构这里是**防御性冗余**（将来若把绑定端点迁到
// `/api/admin` 下，这份白名单会自动生效，不会出现「一进管理台就 403、
// 连绑定页都打不开」的自锁）。保留它，但不要据此认为门户端点也受此闸约束。
func mfaExemptPath(path string) bool {
	switch path {
	case "/api/portal/auth/mfa/status",
		"/api/portal/auth/mfa/setup",
		"/api/portal/auth/mfa/enable":
		return true
	}
	return false
}

// authenticate 校验会话票并加载用户；失败时已写响应。
func authenticate(c *gin.Context) (*model.User, *model.UserSession, bool) {
	ticket := authcookie.Ticket(c)
	if strings.TrimSpace(ticket) == "" {
		response.FailDef(c, errs.Unauthorized)
		return nil, nil, false
	}
	ctx := c.Request.Context()
	sess, err := session.New(db.DB).Authenticate(ctx, ticket)
	if err != nil {
		// 会话失效要把 Cookie 清掉：不清的话浏览器会一直带着一个必然失败的票，
		// 前端每次刷新都收到 401，用户看到的是「登录页 ↔ 首页」来回跳。
		authcookie.Clear(c)
		if def, ok := errs.AsDef(err); ok {
			response.FailDef(c, def)
			return nil, nil, false
		}
		response.FailDef(c, errs.Internal)
		return nil, nil, false
	}

	var u model.User
	// 不取 avatar：这一行每个带会话的请求都要读，头像几 KB 会进 TOAST，
	// 而鉴权只需要 role/status/group。`/api/portal/me` 会重新查完整行。
	if err := db.DB.WithContext(ctx).Omit("avatar").First(&u, sess.UID).Error; err != nil {
		authcookie.Clear(c)
		response.FailDef(c, errs.SessionExpired)
		return nil, nil, false
	}
	if u.Status == 2 {
		// 禁用不清 Cookie：管理员恢复账号后用户不需要重新登录（避免抖动）。
		response.FailDef(c, errs.Forbidden)
		return nil, nil, false
	}
	// 门户端关闭后，只有管理员能持有门户会话（会员只能走 API 面的 Bearer/KEY）。
	//
	// 放在会话层而不是「门户业务面中间件」：/api/portal 与 /api/portal/auth 是
	// 两个路由组，只有这里是共同入口；挂组会漏掉改密/换绑/设备管理那一组。
	// 清 Cookie 是必须的 —— 不清的话浏览器会长期带一张必然失败的票，
	// 而且非管理员连「退出登录」都做不到（登出同样过这里）。
	if int(u.Role) != 0 && !portalOpen() {
		authcookie.Clear(c)
		response.FailDef(c, errs.PortalDisabled)
		return nil, nil, false
	}
	return &u, sess, true
}

// setUserContext 写入请求上下文。
//
// 不再有 claims：会话实体本身就是权威，任何"从令牌里读到的信息"
// 都可能是过期的快照。
func setUserContext(c *gin.Context, u *model.User, sess *model.UserSession) {
	c.Set("user_id", u.ID)
	c.Set("role", int(u.Role))
	c.Set("username", u.Username)
	c.Set("session_id", sess.ID)
	c.Set("auth_channel", "session")
	// 续期（内部按 touch_interval 节流，绝大多数请求不会写库）。
	session.New(db.DB).Touch(c.Request.Context(), sess)
}

// SessionID 返回当前请求的会话 id（未登录为 0）。
func SessionID(c *gin.Context) uint64 {
	if v, ok := c.Get("session_id"); ok {
		if id, ok := v.(uint64); ok {
			return id
		}
	}
	return 0
}

// TokenAuth 是网关鉴权（API KEY）：解析 Authorization: Bearer ld-... 或 x-api-key，
// 校验状态/有效期/来源 IP，加载用户与 KEY，并做 KEY 维度双闸限流（RPM + 并发，Redis 降级时跳过）。
// 通过后在上下文写入 api_key_record / user_record / user_id / key_id，供网关消费。
// 失败按 /v1 对外契约返回 OpenAI 兼容错误体。
//
// **本函数与用户会话完全无关**，语义不得改动：/v1 的调用方是脚本与第三方，
// 它们没有浏览器 Cookie，用 API KEY 就是它们唯一的凭证形态。
func TokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := gatewayTokenOf(c)
		if token == "" {
			gateway.FailDef(c, errs.InvalidAPIKey)
			return
		}
		// 一条 JOIN 同时取回 KEY 与用户：`/v1` 每请求只付一次数据库往返。
		// 此前是 Authenticate + First(&user) 两条，实测 1.84 事务/请求，
		// 10 VU 就把 PG 打到 9.3k txn/s 饱和，20 VU 吞吐腰斩。
		k, u, err := apikey.AuthenticateWithUser(token)
		if err != nil {
			// 映射表只有 apikey.ErrDef 一份：四个凭证面（/v1、/api/user、
			// 签发、自举）对同一个哨兵必须给出同一个码，差别只在响应体形状。
			gateway.FailDef(c, apikey.DefOr(err, errs.InvalidAPIKey))
			return
		}
		if !ipAllowed(k.AllowIPs, c.ClientIP()) {
			gateway.FailDef(c, errs.KeyIPDenied)
			return
		}
		// 作用域自洽性：托管密钥必须有归属应用，否则拒绝。
		// /v1 的计费与归因全部依赖 `api_keys.app_id`；放行一把没有 app 的
		// hosted 密钥，等于让它以「平台直连」的身份消耗用户余额却无法归因。
		if !keyscope.Consistent(k) {
			gateway.FailDef(c, errs.ScopeDenied)
			return
		}
		// 维护模式闸：只拦托管密钥（平台直连 app_id=0 短路，连缓存都不读）。
		//
		// 位置刻意放在欠费闸/rpm/并发之前：维护拒绝不消耗任何计数，
		// 也不会留下一条 usages 记录。放在后面会让"维护期间每分钟 600 次"
		// 把用户的 RPM 额度烧光，维护结束后仍被限流。
		if k.IsHosted() {
			if msg, blocked := appsvc.MaintenanceOf(c.Request.Context(), k.AppID); blocked {
				if msg == "" {
					msg = errs.AppMaintenance.Message
				}
				gateway.FailDef(c, errs.AppMaintenance.WithMessage(msg))
				return
			}
		}
		if u.Status == 2 {
			gateway.FailDef(c, errs.Forbidden)
			return
		}
		// 临时口令闸：与门户/账号面同一判据（mustChangeBlocked），只是错误体换成
		// OpenAI 形状。放豁免表之外的 /v1 全部拒绝 —— 否则「重置出来的临时口令」
		// 可以原样拿来跑推理，改密就成了可选项。
		// 放在欠费闸与 rpm 之前：这一步不可能通过，先省一次 Redis 往返。
		if mustChangeBlocked(c, u) {
			gateway.FailDef(c, errs.PasswordChangeRequired)
			return
		}
		// 欠费闸：余额低于阈值后仅网关拒绝，不改用户/KEY 状态（充值到账自动恢复，避免连带锁死登录与充值）。
		if u.Balance < -billing.ArrearsBlock {
			gateway.FailDef(c, errs.InsufficientBalance)
			return
		}
		if !rpmAllowed(k) {
			gateway.FailDef(c, errs.RateLimited)
			return
		}
		release, ok := acquireConcurrency(k)
		if !ok {
			// 并发闸的语义是"此刻满了"：1 秒后必然有槽位释放，
			// 不写 Retry-After 会让客户端自己猜（常见做法是立刻重试 → 更挤）。
			c.Header("Retry-After", "1")
			gateway.FailDef(c, errs.RateLimited)
			return
		}
		defer release()

		c.Set("api_key_record", k)
		c.Set("user_record", u)
		c.Set("user_id", k.UserID)
		c.Set("key_id", k.ID)
		c.Set("auth_channel", "api-key")
		c.Next()
	}
}

// ipAllowed 校验 KEY 来源 IP 白名单：白名单为空时放行全部；支持精确 IP 与 CIDR。
func ipAllowed(raw model.JSON, ip string) bool {
	list := apikey.DecodeStrings(raw)
	if len(list) == 0 {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == ip {
			return true
		}
		if _, cidr, err := net.ParseCIDR(item); err == nil && cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

// rpmAllowed 执行 KEY 维度每分钟固定窗口限流；Redis 不可用或未设置 RPM 时放行。
func rpmAllowed(k *model.APIKey) bool {
	// 未配置限额是**正常配置**，不是降级：这里不能打降级信号，否则默认
	// （rpm_limit=0=不限）的部署会一直告警。
	if k.RPMLimit <= 0 {
		return true
	}
	// 从这里开始，放行都意味着「这一层的保护消失了」，必须留下信号。
	// 压测实测：Redis 停 60s 期间 `/v1` 的 429 从 140/200 掉到 0/200，
	// 而日志与指标都毫无变化 —— kill 掉 Redis 就等于关掉限流，且无人知晓。
	if !db.RedisReady() {
		noteDegraded("key_rpm_redis_not_ready")
		// fail-open 但**不是无限放行**：额度改由本进程计数执行（见 locallimit.go）。
		// 多副本时额度 = 副本数 × rpm_limit，与渠道 max_concurrency 同一取舍。
		return localKeyRateAllow(k.ID, k.RPMLimit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := fmt.Sprintf("ld:rl:key:%d:%d", k.ID, time.Now().Unix()/60)
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		noteDegraded("key_rpm_redis_error")
		return localKeyRateAllow(k.ID, k.RPMLimit)
	}
	if n == 1 {
		db.RDB.Expire(ctx, key, 90*time.Second)
	}
	return n <= int64(k.RPMLimit)
}

// keyInFlightDefault 是「KEY 未配置 concurrency_limit」时的进程内在途上限。
//
// 与 Redis 版不同，它不跨实例、也不需要 Redis —— 它保护的是**本进程的连接池**
// 与 goroutine，这是单租户能否拖垮全站的关键一环。
const keyInFlightDefault = 32

// ccLeakTTL 是 KEY 并发计数的自愈上限。
//
// 计数只有 INCR/DECR 一对，进程被 SIGKILL（或容器被强制删除）时 DECR 永远不会执行，
// 计数会卡在正的 N —— 没有 TTL 的话这把 KEY 会**永久 429**，只能人工 DEL。
// 多节点共用同一个 Redis 时，一台机器泄漏会让所有节点一起拒绝这把 KEY。
// 2h 远大于最长同步等待（780s），正常请求不可能跨过它。
const ccLeakTTL = 2 * time.Hour

// ccReleaseScript 归还一个并发额度；减到负数时钳回 0。
//
// 为什么必须钳零：TTL 到期后键会消失，此时迟到的 DECR 会把键重新创造成 -1，
// 下一次 INCR 从 0 开始 —— 等于白送一个并发额度，比泄漏更难发现。
var ccReleaseScript = redis.NewScript(`
local v = redis.call("decr", KEYS[1])
if v < 0 then redis.call("set", KEYS[1], 0) return 0 end
return v`)

// acquireConcurrency 执行 KEY 维度并发闸；返回释放函数。未设置上限或 Redis 不可用时为无操作。
func acquireConcurrency(k *model.APIKey) (func(), bool) {
	// 负数 = 显式关闭（确实需要"不限"的部署配 -1，而不是配 0）。
	if k.ConcurrencyLimit < 0 {
		return func() {}, true
	}
	if k.ConcurrencyLimit == 0 {
		// 未配置时执行**本进程**默认在途上限。
		//
		// 为什么默认必须有限：`/v1` 没有任何组级限流，一把 KEY 开 2000 并发就能
		// 占满 100 条 DB 连接，全站一起排队 —— 压测里 20 VU 吞吐从 5,047 掉到
		// 1,565 rps 就是这个机制（0 错误，纯排队）。
		//
		// 32 怎么来的：读面单请求在途约 1ms、同步推理约 600ms；32 并发足以打满
		// 单副本吞吐（实测 10 VU 已是峰值），又远小于连接池（100），留足余量给
		// 其它租户。它是**进程内**计数，多副本时有效上限 = 副本数 × 32。
		return localConcurrency(k.ID, keyInFlightDefault)
	}
	if !db.RedisReady() {
		noteDegraded("key_cc_redis_not_ready")
		// 同 RPM：降级到进程内计数，仍然执行上限（跨副本各自计数）。
		return localConcurrency(k.ID, k.ConcurrencyLimit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := fmt.Sprintf("ld:cc:key:%d", k.ID)
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		noteDegraded("key_cc_redis_error")
		return localConcurrency(k.ID, k.ConcurrencyLimit)
	}
	if n == 1 {
		// 只在首次创建时挂 TTL：之后每次 INCR 不重置，泄漏的计数最多活 2h。
		db.RDB.Expire(ctx, key, ccLeakTTL)
	}
	if n > int64(k.ConcurrencyLimit) {
		ccReleaseScript.Run(context.Background(), db.RDB, []string{key})
		return func() {}, false
	}
	return func() { ccReleaseScript.Run(context.Background(), db.RDB, []string{key}) }, true
}

// queryKeyPaths 是允许从 query 取密钥的路径前缀（Google/Gemini 系 SDK 的用法）。
//
// 只开这几条：`?key=` 会进浏览器历史、Referer 与外围代理日志（我们自己的访问日志
// 已按敏感参数脱敏，管不到别人）。把它限制在「兼容协议里本来就支持这种写法」的
// 端点，等于把暴露面压到最小；其余端点仍只认请求头。
var queryKeyPaths = []string{"/v1/models", "/v1beta/"}

// gatewayTokenOf 解析 /v1 的凭证，优先级：请求头 > query。
//
// 为什么 query 必须排在头之后：两者同时存在时，"头里那把"才是调用方显式选择的
// 身份；若 query 优先，任何能在 URL 上追加参数的中介都能顶替掉真实凭证。
func gatewayTokenOf(c *gin.Context) string {
	if t := strings.TrimSpace(c.GetHeader("x-api-key")); t != "" {
		return t
	}
	if t := bearerToken(c); t != "" {
		return t
	}
	if !queryKeyAllowed(c.Request.URL.Path) {
		return ""
	}
	// x-goog-api-key 是 Google SDK 的头；`?key=` 是它的 query 变体。
	// 大小写不敏感：URL 参数由调用方拼，`?Key=` 与 `?key=` 必须等价，
	// 否则同一份 SDK 在大小写不同的机器上时好时坏。
	if v, ok := c.GetQuery("key"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	for name, vals := range c.Request.URL.Query() {
		if len(vals) == 0 {
			continue
		}
		if strings.EqualFold(name, "key") && strings.TrimSpace(vals[0]) != "" {
			return strings.TrimSpace(vals[0])
		}
	}
	return strings.TrimSpace(c.GetHeader("x-goog-api-key"))
}

// queryKeyAllowed 判断该路径是否允许从 query 取密钥。
func queryKeyAllowed(path string) bool {
	for _, p := range queryKeyPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// bearerToken 解析 Authorization: Bearer <token>。
//
// 保留它只因为 TokenAuth（/v1 网关的 API KEY）需要它；
// 用户会话路径一律走 Cookie，不读这个头。
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
