// Package router 注册 HTTP 路由分组（step-00 §6.1）。
package router

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/controller"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/relay"
	_ "lingdong-api/internal/relay/adapters" // 注册内置渠道适配器（openai 等）
	"lingdong-api/internal/service/task"
)

// validateTrustedProxies 拒绝会让 X-Forwarded-For 完全可控的网段。
//
// 规则（v4 / v6 各一条）：整体网段（/0）与超大网段（v4 < /16、v6 < /48）都不接受。
// 真实部署里反代只会是"某台机器 / 某个内网段"，写成 /8 或 /0 一定是配错了，
// 而配错的后果不是"多信任了几台机器"，是**任何人都能自称任意 IP**。
func validateTrustedProxies(list []string) error {
	for _, raw := range list {
		_, ipnet, err := net.ParseCIDR(raw)
		if err != nil {
			// 纯 IP 不带掩码：gin 会当 /32（或 /128）处理，是合法的
			if net.ParseIP(raw) != nil {
				continue
			}
			return fmt.Errorf("%q 不是合法的 IP 或 CIDR", raw)
		}
		ones, bits := ipnet.Mask.Size()
		if ones == 0 {
			return fmt.Errorf("%q 信任所有来源，等于关闭 IP 校验", raw)
		}
		if bits == 32 && ones < 16 {
			return fmt.Errorf("%q 网段过宽（v4 需 ≥ /16）", raw)
		}
		if bits == 128 && ones < 48 {
			return fmt.Errorf("%q 网段过宽（v6 需 ≥ /48）", raw)
		}
	}
	return nil
}

// New 构造 Gin 引擎并注册全局中间件与路由分组。
func New() *gin.Engine {
	r := gin.New()
	// 必须先声明信任哪些代理，再注册任何路由。
	//
	// gin 默认信任所有代理，此时 `c.ClientIP()` 会把 X-Forwarded-For 整条链都当作可信
	//（解析规则：从右往左取第一个不受信任的地址，全部可信时取最左）—— 于是任何请求
	// 自带一个头就能自称任意 IP。而本项目把 IP 当作安全边界：API KEY 的来源白名单
	//（ipAllowed）、全站限流分桶、/f 直链限流都读它。不收敛等于这三道闸同时失效。
	//
	// 默认**不信任任何代理**（直连部署最常见，也是最安全的一档）；
	// 部署在 nginx/SLB 后面时用 LD_TRUSTED_PROXIES 显式声明（逗号分隔 CIDR/IP）。
	var trusted []string
	for _, p := range strings.Split(conf.EnvConfig().TrustedProxies, ",") {
		if p = strings.TrimSpace(p); p != "" {
			trusted = append(trusted, p)
		}
	}
	// 过宽的网段直接拒绝启动：`0.0.0.0/0` / `::/0` 等于信任任何来源，
	// 于是 `X-Forwarded-For` 可被任意伪造，而它是 IP 白名单、全站限流分桶、
	// 登录失败锁定、/f 直链限流四道闸的共同输入。
	// 只留注释劝阻是不够的 —— 一个字符的配置错误就能让这四道闸同时失效。
	if err := validateTrustedProxies(trusted); err != nil {
		panic("LD_TRUSTED_PROXIES 配置非法: " + err.Error())
	}
	if err := r.SetTrustedProxies(trusted); err != nil {
		// 非法 CIDR 直接 panic：带病启动的表现是"限流与白名单按错的值分桶"，
		// 比启动失败难查得多。
		panic("LD_TRUSTED_PROXIES 配置非法: " + err.Error())
	}
	// Recover 必须最先：守卫里的任何 panic 都不该带走进程。
	r.Use(middleware.Recover())
	// 纯 API 节点：在 CORS 之前裁剪对外面。
	//
	// 必须在注册任何路由之前调用 Use —— gin 的全局中间件只作用于**之后**注册的路由
	// 与 NoRoute 兜底；挂在后面等于对已注册的 /api/portal 不生效。
	// 放在 CORS 之前：被拒绝的路径不该回跨域头（否则浏览器把 404 读成跨域失败）。
	if conf.EnvConfig().IsAPIMode() {
		r.Use(middleware.APISurfaceOnly())
	}
	r.Use(middleware.CORS(), middleware.Gzip(), middleware.BodyLimit(), middleware.GatewayMetrics())
	// 方法错（路径存在）单独一条：不开启时 gin 会把它折进 NoRoute，
	// 于是「对 /v1/chat/completions 用了 GET」被答成「地址不存在」。
	r.HandleMethodNotAllowed = true
	r.NoMethod(func(c *gin.Context) {
		// /v1beta 的 405 必须是 Gemini 形状：SDK 按 status 字符串分支，
		// 拿到 OpenAI 的 error.type 只会显示"未知错误"。
		if strings.HasPrefix(c.Request.URL.Path, "/v1beta/") {
			gateway.SetFormat(c, apidoc.ProtocolOfRequestPath(c.Request.URL.Path))
			gateway.MethodNotAllowed(c)
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/v1/") || strings.HasPrefix(c.Request.URL.Path, "/suno/") {
			if c.Request != nil {
				gateway.SetFormat(c, apidoc.ProtocolOfRequestPath(c.Request.URL.Path))
			}
			gateway.MethodNotAllowed(c)
			return
		}
		response.FailDef(c, errs.MethodNotAllowed)
	})

	// 未匹配路由：/v1 走网关 404 兜底（兼容协议语义）、/f 走文件直链域（统一 410 语义），
	// 其余统一 404 平台信封。
	// （Gin 的 catch-all 与静态子路由同层会冲突，故用 NoRoute 前缀分发替代 v1/*path 通配。）
	//
	// NoRoute 也要挂入口链：未注册的 /v1/xxx 以前**不鉴权、不记访问日志**，
	// 于是任何人都能免费探测路径是否存在，而且出问题时连 request_id 都没有。
	// 顺序与 /v1 组一致（AccessLog → IngressFormat → Preflight → TokenAuth）；
	// TokenAuthIfProtocol 只对协议面生效，/f 与 /api 的未知路径仍是 404。
	r.NoRoute(
		middleware.AccessLog(),
		middleware.IngressFormat(),
		relay.PreflightRecorder(),
		middleware.TokenAuthIfProtocol(),
		func(c *gin.Context) {
			switch {
			case strings.HasPrefix(c.Request.URL.Path, "/v1beta/"):
				// 复用网关 404 兜底：它内部会按请求路径判定线格式（含 /v1beta 的静态前缀），
				// 因此这里不需要再设一次格式。
				relay.Handle(c)
			case strings.HasPrefix(c.Request.URL.Path, "/v1/"), strings.HasPrefix(c.Request.URL.Path, "/suno/"):
				// /suno 与 /v1 共用兜底：拼错的路径也必须得到 NewAPI 信封，
				// 否则客户端会把平台信封解析成"成功但 data 为空"，而不是"路径错了"。
				relay.Handle(c)
			case strings.HasPrefix(c.Request.URL.Path, "/f/"):
				controller.FileDirect(c)
			default:
				response.NotFound(c)
			}
		},
	)

	// 健康检查（公开）
	r.GET("/api/health", controller.Health)

	// 长等待探针（公开但**口令保护**，未配 LD_DIAG_HOLD_TOKEN 时返回 404）。
	//
	// 刻意放在 /api 组之外：组上的 RateLimit 是按 IP 60rpm，
	// 而排查超时阶梯时往往是同一个人反复量多次，被限流会污染结论。
	r.GET("/api/diag/hold", controller.HoldProbe)

	// 同步产物的能力 URL（GET/HEAD /v1/media/{token}）：**必须挂在 v1 组之外**。
	//
	// <img src> / <video src> 带不了 Authorization，挂进 v1 组会被 TokenAuth 直接 401，
	// 表现就是「接口返回了图片地址，浏览器一加载全是 401」。
	// 凭证就是 token 本身（HMAC 签名 + 时效，见 pkg/mediaurl），
	// 限流在 handler 内单独做——组上的 RateLimit 在这里也不生效。
	//
	// 注册在 NoRoute 的 /v1/ 前缀兜底之前不会冲突：Gin 静态路由优先于 NoRoute。
	//
	// 用 Any 而不是 GET+HEAD：只注册 GET/HEAD 时，一个 POST /v1/media/.. 会掉进
	// NoRoute 的 /v1/ 兜底（被当成网关端点解析），返回的是「端点不存在」这种误导性答复。
	// 方法判定交给 handler，才能给出准确的 405 + Allow。
	r.Any("/v1/media/:token", controller.MediaProxy)

	// 图标图片（GET/HEAD /api/icons/b/{key} 内置、/api/icons/u/{id} 上传）。
	//
	// 必须挂在 /api 组**之外**，理由与 /v1/media 完全一致：<img src> 带不了凭证，
	// 组上的 RateLimit 是 60rpm/IP，而门户首屏一次就要 30+ 张图 ——
	// 挂进组里等于"看两遍模型广场就被限流"，表现是图标时有时无。
	// 限流在 handler 内单独做（/f 直链同一套 allowBucket）。
	//
	// 内置图标的字节编译进二进制（assets/brandicon），上传的走对象存储；
	// 两者都发一年 max-age + 内容指纹 ETag，内容永不变化（见 controller/icon.go）。
	r.GET("/api/icons/b/:key", controller.BrandIconAsset)
	r.HEAD("/api/icons/b/:key", controller.BrandIconAsset)
	r.GET("/api/icons/u/:id", controller.UploadIconAsset)
	r.HEAD("/api/icons/u/:id", controller.UploadIconAsset)

	// 应用公开面（品牌 / 公告 / 版本）：终端在用户登录**之前**就要展示品牌与
	// "检查更新"，纯 API 节点也必须提供。
	//
	// 为什么挂在 /api 组**之外**：组上的 RateLimit 是 60rpm/IP，而 NAT 后的终端
	// 集群会在开机高峰同时打进来 —— 与上面 /api/icons、/v1/media 是同一个理由。
	// 三条各挂独立桶：RateLimitN 的键里含 bucket 名，同名会共享计数。
	//
	// 缓存时长按资源定（品牌 60s / 公告 30s / 版本 300s），这也正是它们没有合成
	// 一条聚合接口的原因：合起来就只能取最严的那个（no-store）。
	//
	// Audit 是组级中间件，移出 /api 后必须显式挂，否则这几条请求没有 request_id。
	// PublicNoStore 必须排在 RateLimitN 之前：429 由中间件直接写出，handler 盖不到。
	// ScopeGuard 让契约成为**运行时**约束：AuthOpen 写错时请求立刻 403，
	// 而不是只等离线门禁在 CI 里红。
	pubApp := r.Group("/api/apps")
	pubApp.Use(middleware.Audit(), middleware.PublicNoStore(), middleware.ScopeGuard())
	// HEAD 必须与 GET 成对注册：gin 默认只按方法精确匹配，漏注册就落到 NoRoute
	// 变 404 且不带 no-store —— 而 CDN/监控探活用 `curl -I` 是常态，会把"接口正常"
	// 探成"接口不存在"。成对注册写进同一段循环，加端点时不可能只加一半。
	//
	// 额度与缓存时长全部来自 apicontract 的 PublicPolicy（同一个 key 驱动限流、
	// Cache-Control 与文档），这里不出现任何数字。
	for _, p := range []struct {
		path string
		key  string
		h    gin.HandlerFunc
	}{
		{"/:slug/profile", "apps.profile", controller.AppProfilePublic},
		{"/:slug/notices", "apps.notices", controller.AppNoticesPublic},
		{"/:slug/release", "apps.release", controller.AppReleasePublic},
		// 插件数据的对外只读面：默认关闭（安装级 public_enabled + manifest 白名单
		// 两道闸都在 handler 后面），所以它排在这里不改变"三条公开面"的既有结论。
		{"/:slug/p/:plugin/:entity", "apps.plugin_records", controller.AppPluginPublic},
	} {
		pubApp.GET(p.path, middleware.RateLimitPublic(p.key), p.h)
		pubApp.HEAD(p.path, middleware.RateLimitPublic(p.key), p.h)
	}

	// Prometheus 指标（B7）：`LD_METRICS_HTTP=1` **且** 配置了 `LD_METRICS_TOKEN` 才挂载。
	//
	// 为什么必须成对配置：/metrics 会把端点清单、渠道 id、失败率这些内部拓扑一次性交出去，
	// 在没做网络隔离的部署上等于挂在公网。只开开关不配口令的部署曾被当成「默认安全」，
	// 现在这种部署就是 404 —— 不告诉扫描器「这里有个被禁用的指标口」。
	// 抓取端带 `X-Metrics-Token` 头；口令不匹配也回 404（403 等于确认端点存在）。
	if cfg := conf.EnvConfig(); cfg.MetricsHTTP && cfg.MetricsToken != "" {
		registerInflightGauge()
		r.GET("/metrics", metricsTokenAuth(cfg.MetricsToken), gin.WrapH(metrics.Handler()))
	}

	// /api：门户（JWT）与管理面（JWT + role=0），业务端点 S2+ 落地
	api := r.Group("/api")
	api.Use(middleware.RateLimit(), middleware.Audit())

	// 认证入口（公开，不需要会话）。
	//
	// 这里刻意**不再有** /portal/register、/portal/login、/portal/password/reset
	// 的旧路径：留着别名就是两套入口、两套测试、两套文档，迟早漂移。
	// 无存量用户，直接下线。
	//
	// CSRFPublic 只校验来源（Origin/Referer），没有双提交令牌 —— 这些端点还没有会话。
	// 但它必须存在：针对登录接口的 CSRF（把受害者登进攻击者账号）不需要读响应就能完成。
	auth := api.Group("/portal/auth")
	auth.GET("/captcha", controller.Captcha)
	// 门户端关闭时这两条注册入口一并关闭（PortalOpen）。**只挂门户这两条**：
	// /api/user/register* 是对外 API 面，注册真假只看 auth_register.mode。
	auth.POST("/register", middleware.CSRFPublic(), middleware.PortalOpen(), controller.Register)
	auth.POST("/register/contact-code", middleware.CSRFPublic(), middleware.PortalOpen(), controller.RegisterContactCode)
	auth.POST("/login", middleware.CSRFPublic(), controller.Login)
	// 登录第二步（2FA）。与第一步同样在公开组：此时还没有会话，
	// 唯一的凭证是挑战令牌（一次性、绑 IP、5 次试错上限）。
	auth.POST("/login/mfa", middleware.CSRFPublic(), controller.LoginMFA)
	auth.POST("/password/reset/contact-code", middleware.CSRFPublic(), controller.ResetContactCode)
	auth.POST("/password/reset/verify", middleware.CSRFPublic(), controller.ResetVerify)
	auth.POST("/password/reset", middleware.CSRFPublic(), controller.ResetPassword)

	// 接入能力探测（公开）：注册准入、联系方式验证策略、可用渠道、验证码策略。
	//
	// 单独一组而不是塞进 /api/portal：门户配置同时下发品牌/公告/站点正文，
	// 那是渲染契约；接入方要的是"我该按什么流程引导用户"。两组各自演进，
	// 加一个门户文案不会看起来像接口变了。
	authMeta := api.Group("/auth", middleware.Audit(),
		middleware.RateLimitN(60, "user_open"), middleware.ScopeGuard())
	authMeta.GET("/capabilities", controller.AuthCapabilities)

	// 公开定价目录（模型广场 / 价目表单一真相源，v2.0 §4.3.8）
	api.GET("/pricing", controller.Pricing)

	// 对外 API 文档站（公开，无需登录）。
	//
	// 为什么不做成「登录后才能看」：开发者选型的第一步就是读文档，
	// 要求先注册再读等于把接入门槛提前。文档里也不含任何账户级数据。
	//
	// 组 key 与说明页 slug 都走 path param（`/groups/:key`、`/guides/:slug`）：
	// 它们由我们生成、只含 `[a-z0-9.-]`，不存在模型 id 那种 `gpt-image-2.5` 的
	// 「点号被当成扩展名」「斜杠被当成新段」问题。端点本身仍然用 query
	// （`?k=`：Kind 含 `/`，走 path 就得再造一张 slug 映射表）。
	// 文档站：目录 → 节点 → 接口页（一页一接口，没有 Tab 分组）。
	//
	// 只有三条：多一条"按组/按协议/按端点 slug"的旁路，就会出现两套文档身份
	//（v6 的 `/groups/:key` 与 `/endpoints/:slug` 就是这么长出来的）。
	api.GET("/docs/tree", controller.DocsTree)
	api.GET("/docs/nodes/:key", controller.DocsNodeV7)
	api.GET("/docs/nodes/:key/endpoints/:ep", controller.DocsEndpointV7)
	// 单篇 Markdown：页面上的「复制 / 下载」都打它（与全站导出一套 writer）。
	// 放在接口路由的**子路径**下，不新增独立的文档身份。
	api.GET("/docs/nodes/:key/endpoints/:ep/markdown", controller.DocsEndpointMarkdown)
	// 全量 Markdown：同一份投影的另一出口（贴进工单/知识库/邮件）。
	api.GET("/docs/markdown", controller.DocsMarkdown)

	// 公开门户配置（站点品牌 / 调用地址 / 站点内容 / 注册开关，S10）
	api.GET("/portal/config", controller.PortalConfig)

	// 供应商分组（门户模型广场筛选用；与 /api/pricing 的 vendor 字段配套，公开只读）
	api.GET("/portal/vendors", controller.PortalVendors)

	// 支付异步回调（公开；验签 + payment_dedup 幂等）。
	//
	// **刻意不挂 api 组**：api 组挂了 RateLimit（60rpm/IP），而支付回调的源 IP 是共享出口，
	// 超限时返回的是 JSON 429，网关只判断 body 是否等于 success → 当失败重试 → 重试风暴。
	// Audit 保留：它只注入 request_id 并打一行日志，不写审计表，对回调排障有用。
	// **GET 与 POST 都要注册**：易支付系（彩虹/聚合等）的异步通知就是 GET，参数挂在 query 上；
	// 只注册 POST 时 Gin 的 NoRoute 返回 404，网关视为「地址不可用」重试至放弃。
	// controller.readNotify 已把 query 合并进 params（body 优先），两种方法共用同一处理链。
	r.POST("/api/pay/:channel/notify", middleware.Audit(), controller.PayNotifyChannel)
	r.GET("/api/pay/:channel/notify", middleware.Audit(), controller.PayNotifyChannel)
	// 易支付老地址（过渡）：没有渠道参数，只能先查订单再验签。新配置一律用上面带渠道的地址。
	r.POST("/api/pay/notify", middleware.Audit(), controller.PayNotifyLegacy)
	r.GET("/api/pay/notify", middleware.Audit(), controller.PayNotifyLegacy)

	// 门户端点（会话 Cookie + CSRF 双提交）
	//
	// **CSRF 必须挂在组上，不能一个个端点补**：凭证改成 Cookie 之后，浏览器
	// 会自动把会话票带上任何跨站请求；这里下面有 14 个写端点（充值、兑换、
	// 建 KEY、删 KEY、转移返利…），漏挂一个就是一条可被第三方页面触发
	// 的资金操作。CSRF() 内部对 GET/HEAD/OPTIONS 直接放行，所以整组挂不影响读端点。
	portal := api.Group("/portal", middleware.UserAuth(), middleware.CSRF())
	portal.GET("/me", controller.Me)
	// 头像：唯一写入口。不落对象存储，编码成 data URL 直接写 users.avatar。
	// 走门户会话（而非 /api/user/*）：换头像是「人」的操作，API 客户用不到。
	portal.POST("/me/avatar", controller.AvatarUpload)
	portal.DELETE("/me/avatar", controller.AvatarDelete)

	// 已登录后的认证操作（会话 + CSRF 双提交）。
	//
	// 与上面公开的 auth 组同前缀、不同子路径：gin 按具体 (method, path) 存
	// handler 链，两组互不影响。分开声明是为了让「哪些要登录」一眼可见。
	authSelf := api.Group("/portal/auth", middleware.UserAuth(), middleware.CSRF())
	authSelf.POST("/logout", controller.Logout)
	authSelf.GET("/sessions", controller.ListSessions)
	authSelf.POST("/sessions/revoke-all", controller.RevokeAllSessions)
	authSelf.POST("/sessions/:id/revoke", controller.RevokeSession)
	authSelf.POST("/password/change", controller.ChangePassword)

	// 二次验证（TOTP）：绑定 / 开启 / 关闭 / 重生成备份码。
	// status|setup|enable 三条在「强制管理员绑定」时被放行（middleware.mfaExemptPath），
	// 其余的 disable / regenerate 必须先有会话 + 有效第二因素，不可能是逃生通道。
	authSelf.GET("/mfa/status", controller.MFAStatus)
	authSelf.POST("/mfa/setup", controller.MFASetup)
	authSelf.POST("/mfa/enable", controller.MFAEnable)
	authSelf.POST("/mfa/disable", controller.MFADisable)
	authSelf.POST("/mfa/backup-codes/regenerate", controller.MFABackupCodesRegenerate)
	authSelf.POST("/contact/code", controller.ContactCode)
	authSelf.POST("/contact/bind", controller.ContactBind)

	// ---- 对外开放用户 API（/api/user/*）----
	//
	// 与 /api/portal/* 的分工：**同一套 service，不同的凭证面**。
	// 门户给浏览器（HttpOnly Cookie + CSRF），这里给脚本/移动端/第三方服务
	// （`Authorization: Bearer`，不读 Cookie，因此也不需要 CSRF）。
	//
	// 三条硬规则（改这里之前先读，任一条破了都是安全问题）：
	//   1. 本组**永远不读 Cookie**。读了就必须挂 CSRF，而"按凭证来源决定挂不挂 CSRF"
	//      是运行期分支，写反了就是 CSRF 绕过，且功能上完全看不出来。
	//   2. 写操作与密钥管理只挂 SessionTokenAuth：API KEY 是长期有效、常被贴进
	//      代码和配置的凭证，让它能改密码或签发新 KEY = 一次泄露永久接管。
	//   3. 任何一条端点都不许下发 Cookie（登录只回 token），否则浏览器里会出现
	//      "一个没有 CSRF 校验的会话"。
	// **必须挂在根路由，不能挂 api 组**：api 组上的 RateLimit() 按
	// 「IP + FullPath」每分钟 60 次计数，而这里的调用方是第三方应用后端 ——
	// 一个出口 IP 后面是几百个终端用户，第一个用户就能把它打满，全链路 429。
	// 提高 rate_limit_defaults.rpm 是错的方向：那会同时放宽
	// /api/portal/auth/login，等于把密码爆破的闸门一起抬起来。
	//
	// 移到根路由后 Audit() 必须显式补回来：它负责注入 request_id，
	// 下游日志与错误追踪（以及排障时对同一次请求的串联）全靠它。
	//
	// 四层限流的分工（都是固定窗口，Redis 不可用时 fail-open 放行）：
	//   user_reg / user_login / user_verify  按 IP —— 此时还没有身份
	//   user_auth  按 IP，挂在鉴权之前 —— 挡无效凭证扫描（401 在鉴权中间件就返回）
	//   user_ro / user_rw / user_sess  按用户 —— 读到身份之后才有意义
	userAPI := r.Group("/api/user", middleware.Audit())

	// ⓪ 公开读入口：图形验证码。独立分桶（60/IP）而不放进 userReg：
	// 注册/登录前都要先取图，10 次/分钟的 user_reg 桶会被手动刷新打爆。
	// portal 侧同端点（/api/portal/auth/captcha）无独立桶、走 /api 组 60rpm。
	userOpen := userAPI.Group("", middleware.RateLimitN(60, "user_open"), middleware.ScopeGuard())
	userOpen.GET("/captcha", controller.Captcha)

	// ① 无凭证入口。按 IP 严格分桶，但**不能一刀切到 10**：
	// 应用后端代理多用户登录时出口 IP 唯一，10 次/分钟会让第 11 个用户登不进来。
	// 因此把"会产生账号或发码"的动作与"纯校验"分开，前者最严。
	userReg := userAPI.Group("", middleware.RateLimitN(10, "user_reg"), middleware.ScopeGuard())
	userReg.POST("/register", controller.UserRegister)
	// 注册发码：与门户同一 handler（只读 body+IP，不依赖 Cookie）；
	// user 面无 CSRF（/api/user/* 整面不挂 CSRFPublic），发码的滥用防线是
	// 本组的 10/IP 桶 + handler 内 60s 收件人冷却。
	userReg.POST("/register/contact-code", controller.RegisterContactCode)
	// 找回密码三段式：发码 → 验码换一次性凭据 → 改密。
	// 与门户共用 handler（内部会写验证码 id 的 Cookie，但 API 客户端靠 body 传 id）。
	userReg.POST("/password/reset/contact-code", controller.ResetContactCode)

	userLogin := userAPI.Group("", middleware.RateLimitN(60, "user_login"), middleware.ScopeGuard())
	userLogin.POST("/login", controller.UserLogin)
	// 2FA 第二步：此时还没有会话，唯一凭证是一次性挑战（绑 IP、5 次上限）。
	userLogin.POST("/login/mfa", controller.UserLoginMFA)

	userVerify := userAPI.Group("", middleware.RateLimitN(10, "user_verify"), middleware.ScopeGuard())
	userVerify.POST("/password/reset/verify", controller.ResetVerify)
	userVerify.POST("/password/reset", controller.UserResetPassword)

	// ② 只读账户面：会话票或 API KEY 都可以。
	// 两层限流：先按 IP 挡无效凭证扫描，再按用户挡读面滥用。
	userRead := userAPI.Group("",
		middleware.RateLimitN(120, "user_auth"),
		middleware.UserTokenAuth(),
		middleware.RateLimitN(600, "user_ro"),
		middleware.ScopeGuard(),
	)
	userRead.GET("/self", controller.UserSelf)

	// 只读面：完整链路的"读"一半（用量 / 流水 / 套餐 / 订单 / 任务 / 文件）。
	//
	// 全部复用门户同一批 handler：它们的作用域都是 `c.GetUint64("user_id")`，
	// 既不读 Cookie 也不写 Cookie，搬过来不需要改任何业务逻辑。
	//
	// 路径与门户保持一致（`/usage/:id` 之类），但不共用分组 —— 凭证面不同，
	// 混组就必须在组上同时挂 Cookie 与 Bearer 两套鉴权，那是 CSRF 绕过的起点。
	userRead.GET("/usage", relay.PortalUsage)
	userRead.GET("/usage/summary", relay.PortalUsageSummary)
	// 导出会按用户全量扫描并生成 CSV，单独压到 2 次/分（其余读端点共享 user_ro 的 600）。
	userRead.GET("/usage/export", middleware.RateLimitN(2, "user_export"), relay.PortalUsageExport)
	userRead.GET("/usage/:id", relay.PortalUsageDetail)
	userRead.GET("/transactions", controller.PortalTransactionList)
	userRead.GET("/packages", controller.PortalPackages)
	userRead.GET("/orders", controller.PortalOrderList)
	userRead.GET("/orders/:id", controller.UserOrderGet)
	// 支付二维码：与门户同一 handler，归属校验在 order.QRCodeFor 内（user_id 双条件）。
	// 没有订单状态闸：paid 单仍可取码（渠道侧天然失效），QRRef 为空返回 mode=none。
	userRead.GET("/orders/:id/qrcode", controller.PortalOrderQRCode)
	userRead.GET("/tasks", controller.PortalTaskList)
	userRead.GET("/tasks/:id", controller.PortalTaskGet)
	// 产物取回用 ForAPI 版本：同一实现，失败体是平台信封（见 serveArtifact 注释）。
	userRead.GET("/tasks/:id/contents/:index", controller.PortalTaskContentAtForAPI)
	userRead.GET("/files", controller.PortalFileList)
	// 我已接入的应用（含托管密钥元数据）。只返回**已持有托管密钥**的应用：
	// 应用清单是平台拓扑，接入方只需要知道自己那一个。
	userRead.GET("/apps", controller.UserAppList)

	// ③ 受限写面：会话票，或 kind='hosted' 且携带对应 scope 的托管密钥。
	//
	// 为什么单独一组而不是塞进 reader：**级别本身就是契约**。写操作与读操作的
	// 凭证边界不同，混在一组就只能把级别下沉到 handler 里判断，那时它就不再是
	// 静态声明，而是每个 handler 各自的 if。
	//
	// 三个操作的共同点：一个动钱（下单）、两个动数据（取消任务、删上传）。
	// 用户自建 KEY 一律 403 scope_denied —— 它可以被贴在任何地方。
	userWrite := userAPI.Group("",
		middleware.RateLimitN(120, "user_auth"),
		middleware.UserTokenAuth(),
		middleware.RateLimitN(120, "user_rw"),
		middleware.ScopeGuard(),
	)
	// 下单带 client_order_no 幂等号；它**不做**请求级幂等中间件，
	// 因为"回查原单"必须按订单状态分流（见 order.CreateForClient），
	// 通用幂等表存不下这套语义。
	userWrite.POST("/orders", controller.UserCreateOrder)
	// 兑换码：与门户同一 handler（作用域 c.GetUint64("user_id")），动余额，
	// 与"下单"同级归写面——自建 KEY 调用返回 403 scope_denied（刻意边界）。
	userWrite.POST("/redeem", controller.PortalRedeem)
	userWrite.POST("/tasks/:id/cancel", controller.PortalTaskCancel)
	userWrite.DELETE("/files/:id", controller.PortalFileDelete)

	// ④ 敏感面：只认会话票。
	userSess := userAPI.Group("",
		middleware.RateLimitN(120, "user_auth"),
		middleware.SessionTokenAuth(),
		middleware.RateLimitN(300, "user_sess"),
		middleware.ScopeGuard(),
	)
	userSess.POST("/logout", controller.UserLogout)
	userSess.POST("/password/change", controller.UserChangePassword)
	// 联系方式换绑（发码 + 绑定）。
	//
	// **只给会话票**：换绑会踢掉该用户其它设备并禁用其全部 API KEY
	// （账号接管的标准止血动作）。托管密钥代表的是"某个应用"，让它替用户改
	// 绑定邮箱/手机等于把账号接管能力交给接入方 —— 它连"用户本人是否在场"都证明不了。
	//
	// 发码单独限 10/分/用户：它背后是一条真实短信/邮件（就是钱），
	// 与"读列表"共用 300/分 太宽。
	userSess.POST("/contact/code",
		middleware.RateLimitN(10, "user_contact"), controller.ContactCode)
	userSess.POST("/contact/bind",
		middleware.RateLimitN(10, "user_contact"), controller.ContactBind)
	// 我的兑换记录（账户级账单视图，仅会话票）。
	userSess.GET("/redeems", controller.UserRedeemList)
	userSess.GET("/sessions", controller.ListSessions)
	// 静态段先于 :id 注册（与门户组同一顺序，避免 gin 的兄弟节点冲突）。
	userSess.POST("/sessions/revoke-all", controller.UserRevokeAllSessions)
	userSess.POST("/sessions/:id/revoke", controller.UserRevokeSession)

	// KEY 自助管理：与门户同一批 handler（作用域恒为当前 user_id）。
	// 给机器用之后，第三方服务可以「用会话票建 KEY → 只用 KEY 调 /v1」，
	// 不必把会话票塞进长期运行的配置里。
	userSess.GET("/keys", controller.ListKeys)
	userSess.POST("/keys", controller.CreateKey)
	userSess.PUT("/keys/:id", controller.UpdateKey)
	userSess.PATCH("/keys/:id/status", controller.UpdateKeyStatus)
	userSess.POST("/keys/:id/reset", controller.ResetKey)
	userSess.GET("/keys/:id/plaintext", controller.RevealKey)
	userSess.DELETE("/keys/:id", controller.DeleteKey)

	// 托管密钥签发：**只认会话票**。API KEY 能签发 API KEY 是一条自我复制链
	// （一把泄露的密钥可无限繁殖新密钥，撤销只能靠人发现）。
	// 同一 (app,user) 只允许一把，重复调用返回同一把（不返回明文）。
	// 限流 5/min/用户：slug 是管理员手填的可读串（无随机化），二值错误码
	// 还能区分"应用不存在"与"应用停用" —— 不限速时 300/min 足够把平台接入的
	// 应用清单遍历一遍。
	userSess.POST("/apps/:slug/hosted-key",
		middleware.RateLimitN(5, "user_app_issue"), controller.UserHostedKeyIssue)

	// ⑤ 自举面：只认托管密钥（凭证操作它自己）。
	//
	// 为什么不并进 userWrite（那里会话票也放行）：那等于让任意登录用户都能调
	// 自举轮换，把 allow_self_issue 这个开关整个绕过去。为什么不并进 userSess：
	// 会话票表达不了"我要操作哪一把钥"，而同一用户在不同应用下各有一把托管密钥。
	//
	// 鉴权前后各一道限流，且用**独立桶名**（RateLimitN 的键是 bucket+who+分钟，
	// 同名桶共享计数，不同阈值混用会让计数互相干扰）：
	//   user_self_auth   —— 鉴权前，who 为空 ⇒ 按 IP 计，防暴力猜钥
	//   user_self_key    —— 鉴权后，who=user_id ⇒ 按用户计，防轮换风暴
	//   user_key_health  —— 自检面独立桶，轮询不该挤掉轮换的额度
	//
	// 鉴权前那道不能设成"30"：一个应用的后端往往只有一个出口 IP，它名下所有
	// 终端用户的密钥都从这个 IP 进来。30/min 是"每 IP 30 次"而不是"每钥 30 次"，
	// 密钥一多就互相挤爆，而猜钥防护其实由 62^32 的熵与每钥 RPM 承担。
	userSelfKey := userAPI.Group("",
		middleware.RateLimitN(120, "user_self_auth"),
		middleware.HostedKeyAuth(),
		middleware.RateLimitN(10, "user_self_key"),
		middleware.ScopeGuard(),
	)
	userSelfKey.POST("/key/rotate", controller.UserKeySelfRotate)

	// 自检面：只验明文，**不判"能不能用"**（见 HostedKeyInspectAuth 的注释）。
	// 单独一组而不并进 userSelfKey：被禁/过期的钥必须能进来问"为什么"，
	// 而 HostedKeyAuth 恰好在鉴权阶段就把它们拒了。
	userSelfDiag := userAPI.Group("",
		middleware.RateLimitN(120, "user_self_auth"),
		middleware.HostedKeyInspectAuth(),
		middleware.RateLimitN(30, "user_key_health"),
		middleware.ScopeGuard(),
	)
	userSelfDiag.GET("/key/health", controller.UserKeyHealth)

	// 二次验证：绑定/开启/关闭/重生成备份码（同门户 handler）。
	// 注意：enable 会撤销该用户的其它会话（保留当前），token 客户端同样受益。
	userSess.GET("/mfa/status", controller.MFAStatus)
	userSess.POST("/mfa/setup", controller.MFASetup)
	userSess.POST("/mfa/enable", controller.MFAEnable)
	userSess.POST("/mfa/disable", controller.UserMFADisable)
	userSess.POST("/mfa/backup-codes/regenerate", controller.MFABackupCodesRegenerate)

	portal.GET("/usage", relay.PortalUsage)
	// 门户调用记录导出（S17）：作用域恒为当前 JWT 用户
	portal.GET("/usage/export", relay.PortalUsageExport)
	// 门户调用汇总（设计 §4.3.7 L468）：按模型聚合，作用域恒为当前 JWT 用户
	portal.GET("/usage/summary", relay.PortalUsageSummary)
	portal.GET("/usage/:id", relay.PortalUsageDetail)
	// 同步调用的产物代理（上游直链不能下发：域名本身就是供应链信息）。
	// 前端「记录 id + 序号」即可取图/取视频，Range 透传支持拖动。
	portal.GET("/usage/:id/media/:index", controller.PortalUsageMedia)

	// 门户各模态默认模型（设计 §4.3.7 L474）：每项附 any_key_allowed（用户任一 KEY 白名单含该模型即 true）
	portal.GET("/defaults", controller.PortalDefaults)

	// 门户站点内容（设计 §4.3.7 L486 / v1.17 增补，JWT role≥1）：空行分段返回
	portal.GET("/content/about", controller.PortalContentAbout)
	portal.GET("/content/agreement", controller.PortalContentAgreement)

	// 门户主页聚合（S15）：余额/冻结 + 今日/本月用量 + 趋势 + 在途任务数
	portal.GET("/overview", controller.PortalOverview)

	// 门户公告时间线（S10）：时间线读取 + 按 id 锚标记已读
	portal.GET("/announcements", controller.PortalAnnouncements)
	portal.POST("/announcements/read", controller.PortalAnnouncementsRead)

	// 门户异步任务（S7）：列表 / 详情 / 取消 / 产物取回
	//
	// 产物只保留带序号的端点：`/content`（= 第 0 个）与它是同一件事，
	// 留两条入口等于两套前端分支 + 两套「不存在 → 302/410」判据。
	portal.GET("/tasks", controller.PortalTaskList)
	portal.GET("/tasks/:id", controller.PortalTaskGet)
	portal.GET("/tasks/:id/contents/:index", controller.PortalTaskContentAt)
	portal.POST("/tasks/:id/cancel", controller.PortalTaskCancel)

	// 门户文件（S8）：全量列表（跨 KEY）/ 直传 / 删除 / 限额与用量（只读）
	portal.GET("/files", controller.PortalFileList)
	portal.POST("/files", controller.PortalFileUpload)
	portal.GET("/files/config", controller.PortalFileConfig)
	portal.DELETE("/files/:id", controller.PortalFileDelete)

	// 门户钱包与邀请返利（S9）：套餐 / 下单 / 订单 / 续付 / 兑换 / 邀请
	portal.GET("/packages", controller.PortalPackages)
	portal.GET("/orders", controller.PortalOrderList)
	portal.POST("/orders/:id/continue", controller.PortalOrderContinue)
	// 支付三件套：重试创建（同一单号）/ 主动核对 / 二维码数据。
	portal.POST("/orders/:id/pay", controller.PortalOrderRetryPay)
	portal.POST("/orders/:id/verify", controller.PortalOrderVerify)
	portal.GET("/orders/:id/qrcode", controller.PortalOrderQRCode)
	portal.POST("/topup", controller.PortalTopup)
	portal.POST("/redeem", controller.PortalRedeem)
	portal.GET("/transactions", controller.PortalTransactionList)
	portal.GET("/aff", controller.PortalAff)
	portal.POST("/aff/transfer", controller.PortalAffTransfer)
	portal.GET("/aff/records", controller.PortalAffRecords)

	// 门户 API KEY（仅本人可见）
	portal.GET("/keys", controller.ListKeys)
	portal.POST("/keys", controller.CreateKey)
	portal.PUT("/keys/:id", controller.UpdateKey)
	portal.PATCH("/keys/:id/status", controller.UpdateKeyStatus)
	portal.POST("/keys/:id/reset", controller.ResetKey)
	portal.GET("/keys/:id/plaintext", controller.RevealKey)
	portal.DELETE("/keys/:id", controller.DeleteKey)

	// 门户侧托管密钥签发/轮换（与 `/api/user/apps/:slug/hosted-key` **同一个
	// handler**，只是凭证面不同）。
	//
	// 为什么不能省掉这条、让浏览器直接打 `/api/user/...`：
	// `/api/user/*` 整面只认 `Authorization: Bearer`（刻意不读 Cookie，见
	// middleware.UserTokenAuth 的头注释）。浏览器手里的凭证是 httpOnly 会话
	// Cookie，前端**读不到**票值，因此那条请求必然无 Bearer → 401 unauthorized。
	// 而 401 在前端是「会话已失效」的全局语义（apiClient 会清空登录态并让路由
	// 守卫裁决），于是一次写接口失败就被放大成「登录后立刻退出」——
	// 症状与真实掉线完全一样，排查时极易被误导到会话/TTL 方向。
	// 凭证面不能混组（混挂 Cookie+Bearer 就是 CSRF 绕过的起点），所以按
	// 门户既有的双面模式各注册一条路径，handler 复用。
	//
	// 只保留**写**面：读面（应用清单）已由密钥列表直出 app_slug/app_name，
	// 浏览器不再需要它；留一条没有调用方、也没有测试覆盖的 Cookie 读端点，
	// 只是多一个无人验证的鉴权面。
	//
	// 限流 5/分/用户：slug 是无随机化的人类可读串，不限速就能把平台接入的
	// 应用清单遍历一遍（与 `/api/user/apps/:slug/hosted-key` 同口径）。
	portal.POST("/apps/:slug/hosted-key",
		middleware.RateLimitN(5, "portal_app_issue"), controller.UserHostedKeyIssue)

	// 管理面端点（会话 Cookie + role=0 + CSRF 双提交）
	//
	// 同上：管理台的写操作比门户更危险（改价目、改渠道、退款、重置他人密码），
	// CSRF 只挂组上，不给单个端点「记得加/忘了加」的机会。
	admin := api.Group("/admin", middleware.AdminAuth(), middleware.CSRF())
	// 系统健康（Worker 运行态 + 插件执行位置）。原先挂在匿名 /api/health 上，
	// 属于内部拓扑，只能给管理员看。
	admin.GET("/system/health", controller.AdminHealth)
	// 版本与一键更新。check 是 POST：它会真的出站抓一次清单，
	// 放在 GET 上等于让"刷新页面"变成对第三方站点的周期性请求。
	admin.GET("/system/release", controller.AdminReleaseGet)
	admin.POST("/system/release/check", controller.AdminReleaseCheck)
	admin.POST("/system/release/update", controller.AdminReleaseUpdate)
	admin.GET("/system/release/update/current", controller.AdminReleaseCurrent)
	admin.POST("/system/release/update/cancel", controller.AdminReleaseCancel)
	admin.GET("/users", controller.AdminListUsers)
	admin.POST("/users/:id/quota", controller.AdminAdjustQuota)
	admin.PATCH("/users/:id", controller.AdminUpdateUser)
	// 管理员重置用户密码（救人通道：用户邮箱不可用时的唯一出路）。
	// 会踢掉该用户全部会话，所以挂在 CSRF 保护下并要求审计。
	admin.POST("/users/:id/password-reset", controller.AdminResetUserPassword)
	// 用户详情抽屉（S17）：余额流水与密钥列表（只读；密钥不回显明文）
	admin.GET("/users/:id/transactions", controller.AdminUserTransactions)
	admin.GET("/users/:id/keys", controller.AdminUserKeys)
	// KEY 兜底启停（含托管密钥解锁）：status=2 的托管密钥唯一来源是管理员动作，
	// 这里是它的唯一恢复通道；写管理审计 + 用户侧 by_admin 安全事件。
	admin.PATCH("/keys/:id/status", controller.AdminKeyStatus)

	// 接入应用（对外 API 的归属主体）。没有删除端点：应用被密钥与订单引用，
	// 下线用 PATCH status=2（只拦新签发，已签发密钥仍可用）。
	admin.GET("/apps", controller.AdminAppList)
	admin.POST("/apps", controller.AdminAppCreate)
	admin.PUT("/apps/:id", controller.AdminAppUpdate)
	admin.PATCH("/apps/:id/status", controller.AdminAppStatus)
	admin.GET("/apps/:id/keys", controller.AdminAppKeys)

	// 应用级后台：品牌 / 统计 / 公告 / 版本 / 维护。
	//
	// 与 PATCH /apps/:id/status 的分工：status 只拦新签发，maintenance 只拦存量
	// 流量（/v1 503）。两者不联动 —— 一次点击不该同时完成"断新接入"与"断在线流量"。
	//
	// 品牌与统计**分开**：品牌页签只需 1 条 SQL 就能回填表单，不该为它算一次
	// 7 日聚合；概览四格（消费/充值/密钥/用户）由列表接口随卡片下发，弹窗复用。
	admin.GET("/apps/:id/brand", controller.AdminAppBrandGet)
	admin.PUT("/apps/:id/brand", controller.AdminAppBrandUpdate)
	admin.GET("/apps/:id/stats", controller.AdminAppStats)
	admin.PATCH("/apps/:id/maintenance", controller.AdminAppMaintenance)

	admin.GET("/apps/:id/notices", controller.AdminAppNoticeList)
	admin.POST("/apps/:id/notices", controller.AdminAppNoticeCreate)
	admin.PUT("/apps/:id/notices/:nid", controller.AdminAppNoticeUpdate)
	admin.PATCH("/apps/:id/notices/:nid/status", controller.AdminAppNoticeStatus)
	admin.DELETE("/apps/:id/notices/:nid", controller.AdminAppNoticeDelete)

	admin.GET("/apps/:id/releases", controller.AdminAppReleaseList)
	admin.POST("/apps/:id/releases", controller.AdminAppReleaseCreate)
	admin.PUT("/apps/:id/releases/:rid", controller.AdminAppReleaseUpdate)
	admin.PATCH("/apps/:id/releases/:rid/status", controller.AdminAppReleaseStatus)
	admin.DELETE("/apps/:id/releases/:rid", controller.AdminAppReleaseDelete)

	// 应用插件（声明式）：目录 → 安装 → 记录。
	//
	// 路径刻意**不是** /api/admin/plugins：那是 2026-10-25 下线的"执行第三方代码"
	// 插件系统用过的命名空间，复用会让人以为执行型插件复活了。
	// 静态段 /sample 必须写在 /:key 之前（gin 同层静态优先，这里仍按可读顺序摆）。
	admin.GET("/app-plugins", controller.AdminAppPluginList)
	admin.GET("/app-plugins/sample", controller.AdminAppPluginSample)
	admin.GET("/app-plugins/:key", controller.AdminAppPluginGet)
	admin.PUT("/app-plugins/:key", controller.AdminAppPluginImport)
	admin.PATCH("/app-plugins/:key/status", controller.AdminAppPluginStatus)
	admin.DELETE("/app-plugins/:key", controller.AdminAppPluginDelete)

	admin.GET("/apps/:id/plugins", controller.AdminAppPluginInstalls)
	admin.POST("/apps/:id/plugins", controller.AdminAppPluginInstall)
	admin.GET("/apps/:id/plugins/:key", controller.AdminAppPluginDetail)
	admin.PATCH("/apps/:id/plugins/:key", controller.AdminAppPluginPatch)
	admin.DELETE("/apps/:id/plugins/:key", controller.AdminAppPluginUninstall)
	admin.DELETE("/apps/:id/plugins/:key/data", controller.AdminAppPluginPurge)

	// 记录：entity 是必填查询参数（一个插件可以有多个数据对象）。
	// 导出必须写在 :rk 之前（静态段优先），否则 /export 会被当成 record_key。
	admin.GET("/apps/:id/plugins/:key/records", controller.AdminAppPluginRecordList)
	admin.POST("/apps/:id/plugins/:key/records", controller.AdminAppPluginRecordCreate)
	admin.GET("/apps/:id/plugins/:key/records/export", controller.AdminAppPluginRecordExport)
	admin.PUT("/apps/:id/plugins/:key/records/:rk", controller.AdminAppPluginRecordUpdate)
	admin.DELETE("/apps/:id/plugins/:key/records/:rk", controller.AdminAppPluginRecordDelete)

	// 开放接口手册（只读）。**不放公开文档站**：它有应用清单与权限模型的完整语义，
	// 而接入方是平台逐个拉进来的应用，不是任意访客。
	// 响应缓存指令是 private（见 writeAdminDocsJSON），public 会让代理把管理台内容
	// 缓存后发给未授权请求。
	admin.GET("/api-docs", controller.AdminAPIDocsCatalog)
	admin.GET("/api-docs/openapi.json", controller.AdminAPIDocsOpenAPI)
	admin.GET("/api-docs/markdown", controller.AdminAPIDocsMarkdown)
	// Swagger UI：只注册 catch-all，`/ui/` 自身由 handler 兜（见 AdminAPIDocsUIAsset）。
	// 同一 OpenAPI 的第二种渲染器，给习惯 Swagger 的接入方直接试调用。
	admin.GET("/api-docs/ui/*asset", controller.AdminAPIDocsUIAsset)

	// 模型目录与供应商分组（价目变更后失效定价缓存）
	admin.GET("/models", controller.ListAdminModels)
	admin.POST("/models", controller.CreateAdminModel)
	admin.PUT("/models/:id", controller.UpdateAdminModel)
	admin.PATCH("/models/:id", controller.ToggleAdminModel)
	admin.DELETE("/models/:id", controller.DeleteAdminModel)
	admin.PUT("/models/:id/bindings/:channelId", controller.UpsertModelBinding)
	admin.DELETE("/models/:id/bindings/:channelId", controller.DeleteModelBinding)
	admin.GET("/vendors", controller.ListAdminVendors)
	admin.POST("/vendors", controller.CreateAdminVendor)
	admin.PUT("/vendors/:id", controller.UpdateAdminVendor)
	admin.DELETE("/vendors/:id", controller.DeleteAdminVendor)
	// 图标：列表（内置图库 + 最近上传）与上传。
	// 没有删除端点 —— 图标被 models/vendors/channels 的 icon 列引用，
	// 删掉会让这些实体静默退回首字占位（见 model/icon.go）。
	admin.GET("/icons", controller.ListAdminIcons)
	admin.POST("/icons", controller.UploadAdminIcon)
	admin.GET("/upstream/sources", controller.ListUpstreamSources)
	admin.POST("/upstream/preview", controller.PreviewUpstream)

	// 文档工作台（v7）：节点 → 接口 → 字段/响应/示例。
	//
	// 与下面那批 v6 编辑面暂时并存（v6 的表与 handler 在下一批迁移里删除）：
	// v7 只认 doc_nodes / doc_endpoints 这套内容表，写入口全部落到 docdocs。
	// 路径段刻意用 `/docs/nodes/*` 与 v6 的 `/docs/groups/*`、`/docs/endpoints/*` 分开，
	// 两套并存期间不会互相覆盖；删 v6 时这段一行都不用动。
	admin.GET("/docs/tree", controller.AdminDocV7Tree)
	admin.GET("/docs/transports", controller.AdminDocV7Transports)
	admin.GET("/docs/check", controller.AdminDocV7Check)
	admin.POST("/docs/nodes", controller.AdminDocV7NodeCreate)
	admin.GET("/docs/nodes/:key", controller.AdminDocV7NodeGet)
	admin.PUT("/docs/nodes/:key", controller.AdminDocV7NodeSave)
	admin.DELETE("/docs/nodes/:key", controller.AdminDocV7NodeDelete)
	admin.POST("/docs/nodes/:key/publish", controller.AdminDocV7NodePublish)
	admin.POST("/docs/nodes/:key/unpublish", controller.AdminDocV7NodeUnpublish)
	admin.POST("/docs/nodes/:key/endpoints", controller.AdminDocV7EndpointCreate)
	admin.GET("/docs/nodes/:key/endpoints/:ep", controller.AdminDocV7EndpointGet)
	admin.PUT("/docs/nodes/:key/endpoints/:ep", controller.AdminDocV7EndpointSave)
	admin.DELETE("/docs/nodes/:key/endpoints/:ep", controller.AdminDocV7EndpointDelete)
	admin.PUT("/docs/nodes/:key/endpoints/:ep/fields", controller.AdminDocV7EndpointFields)
	admin.POST("/docs/nodes/:key/endpoints/:ep/publish", controller.AdminDocV7EndpointPublish)
	admin.POST("/docs/nodes/:key/endpoints/:ep/unpublish", controller.AdminDocV7EndpointUnpublish)
	admin.GET("/docs/nodes/:key/endpoints/:ep/preview", controller.AdminDocV7EndpointPreview)
	admin.POST("/docs/nodes/:key/endpoints/:ep/preview", controller.AdminDocV7EndpointPreviewDraft)
	admin.POST("/docs/nodes/:key/preview", controller.AdminDocV7NodePreviewDraft)
	admin.POST("/docs/nodes/:key/endpoints/:ep/samples", controller.AdminDocV7SamplesGenerate)
	admin.POST("/docs/nodes/:key/endpoints/:ep/presets/samples", controller.AdminDocV7PresetSamplesGenerate)
	admin.GET("/docs/sections", controller.AdminDocV7Sections)
	admin.PUT("/docs/sections/:key", controller.AdminDocV7SectionSave)

	// 渠道与适配器（S4）：CRUD + 连通测试 + 手动启停
	//
	// **PUT 不受理 keys**：密钥池只有一条写路径（下面的 PATCH /keys）。
	// 原因：keys 与 key_meta 必须落在同一次 UPDATE 里，两个写者必然分叉成
	// 两套「去重 + 属性重建 + 最后一把校验」，而编辑页"留空 = 不改"的语义
	// 又让人以为自己在追加，实际是整池替换 —— 这是"改一把要重录全部"的根因。
	admin.GET("/channels", controller.ListAdminChannels)
	admin.POST("/channels", controller.CreateAdminChannel)
	admin.PUT("/channels/:id", controller.UpdateAdminChannel)
	admin.DELETE("/channels/:id", controller.DeleteAdminChannel)
	// 密钥池运维：一个读（明细）+ 一个原子写。
	// op ∈ add / replace / replace_all / remove / meta / reset_cooldown。
	// 不拆成 6 个 CRUD：中间态（删了还没加）能被线上请求看到，而那正是取不到 key 的时刻。
	admin.GET("/channels/:id/keys", controller.GetAdminChannelKeys)
	admin.PATCH("/channels/:id/keys", controller.PatchAdminChannelKeys)
	admin.PUT("/channel/status", controller.SetChannelStatus)
	admin.POST("/channel/test", controller.TestChannel)
	admin.GET("/adapters", controller.ListAdminAdapters)

	// 平台形状目录（单一真相源）：模态 / 计费方式矩阵 / 可声明端点 / 计价格字段 /
	// 档位白名单 / 内置适配器能力 / 限额。前端下拉与 AI 编排提示词都读它。
	admin.GET("/schema/catalog", controller.AdminSchemaCatalog)

	// L2 规格（adapter_specs）：数据驱动适配器的增删改查。
	// 渠道通过 adapter_id=`spec:<slug>` 引用，所以这条资源是渠道配置的上游。
	// 启停单独一条 PATCH（与 packages 同规矩）：停用会让引用渠道立即失效，需独立留痕。
	admin.GET("/adapter-specs", controller.ListAdminAdapterSpecs)
	admin.POST("/adapter-specs", controller.CreateAdminAdapterSpec)
	admin.GET("/adapter-specs/:id", controller.GetAdminAdapterSpec)
	// 影响面：删除/改端点前必须知道"谁在用"。放在详情之外单独一条，
	// 因为列表页不需要它（会变成 N 次查询），而删除确认框必须要。
	admin.GET("/adapter-specs/:id/impact", controller.GetAdapterSpecImpact)
	admin.PUT("/adapter-specs/:id", controller.UpdateAdminAdapterSpec)
	admin.PATCH("/adapter-specs/:id/status", controller.SetAdapterSpecStatus)
	admin.DELETE("/adapter-specs/:id", controller.DeleteAdminAdapterSpec)

	// AI 编排：会话 / 提案 / 应用 / 回滚 / 设置 / 文档副本 / 统计。
	//
	// 这里**没有**任何直接写渠道或模型的端点：AI 只能产出提案，
	// 生效的唯一入口是 proposals/:id/apply（单事务 + 审计 + 快照）。
	admin.GET("/agent/sessions", controller.ListAdminAgentSessions)
	admin.POST("/agent/sessions", controller.CreateAdminAgentSession)
	admin.PATCH("/agent/sessions/:id", controller.UpdateAdminAgentSession)
	admin.DELETE("/agent/sessions/:id", controller.DeleteAdminAgentSession)
	admin.GET("/agent/sessions/:id/messages", controller.ListAdminAgentMessages)
	// SSE：发一条消息并跑一轮编排。这是**唯一**会出网的 agent 端点，
	// 每轮有 240s 硬上限（见 service/agent/runtime.go 的解释）。
	admin.POST("/agent/sessions/:id/messages", controller.AgentSessionMessage)
	// 回答助手提出的问题。**不取会话锁**：作答是正在跑的那一轮的输入，
	// 取锁必然 409（详见 controller/admin_agent_ask.go 的注释）。
	admin.POST("/agent/asks/:id/answer", controller.AgentAnswerAsk)
	admin.GET("/agent/proposals", controller.ListAdminAgentProposals)
	admin.GET("/agent/proposals/:id", controller.GetAdminAgentProposal)
	admin.POST("/agent/proposals/:id/apply", controller.ApplyAdminAgentProposal)
	admin.POST("/agent/proposals/:id/rollback", controller.RollbackAdminAgentProposal)
	admin.POST("/agent/proposals/:id/reject", controller.RejectAdminAgentProposal)
	admin.GET("/agent/settings", controller.GetAdminAgentSettings)
	admin.PUT("/agent/settings", controller.SaveAdminAgentSettings)
	admin.GET("/agent/stats", controller.GetAdminAgentStats)
	// 零成本探针：会出网，按管理员限 6 次/分钟。
	admin.POST("/agent/dry-run", middleware.RateLimitN(6, "agent_dry_run"), controller.AgentDryRun)
	admin.GET("/agent/docs", controller.ListAdminAgentDocs)
	admin.POST("/agent/docs", controller.CreateAdminAgentDoc)
	admin.GET("/agent/docs/:id/preview", controller.PreviewAdminAgentDoc)
	admin.DELETE("/agent/docs/:id", controller.DeleteAdminAgentDoc)

	// 异步任务（S7）：全量列表 + 详情（含产物清单）+ 产物下载 + 管理员取消 + 失败任务重试
	// + 物理删除（B7：?purge=true，仅终态，写审计）
	admin.GET("/tasks", controller.AdminTaskList)
	admin.GET("/tasks/:id", controller.AdminTaskGet)
	admin.GET("/tasks/:id/contents/:index", controller.AdminTaskContentAt)
	admin.POST("/tasks/:id/cancel", controller.AdminTaskCancel)
	admin.POST("/tasks/:id/retry", controller.AdminTaskRetry)
	admin.DELETE("/tasks/:id", controller.AdminTaskPurge)

	// 订单管理（S9）：列表/导出 + 退款（原路退回/仅回退余额）/退款结果确认/人工入账（原因必填 + 审计）
	admin.GET("/orders", controller.AdminOrderList)
	admin.GET("/orders/export", controller.AdminOrderExport)
	admin.POST("/order/refund", controller.AdminOrderRefund)
	admin.POST("/order/refund/confirm", controller.AdminOrderRefundConfirm)
	admin.POST("/order/manual-credit", controller.AdminOrderManualCredit)

	// 系统配置中心（S10）：全量读写（保存即热生效）+ 云存储/通知通道测试
	admin.GET("/config", controller.AdminConfigGet)
	admin.PUT("/config", controller.AdminConfigUpdate)
	admin.POST("/storage/test", controller.AdminStorageTest)
	admin.POST("/notify/test", controller.AdminNotifyTest)

	// 管理台数据面与可观测（S11）：经营看板 / 调用日志（明细·归档·导出·分析）/ 审计日志
	admin.GET("/data/board", controller.AdminBoard)
	// 容量与保留配置（B7）：产物总量 + 磁盘水位 + 阈值（运维巡检与前端展示同源）
	admin.GET("/data/capacity", controller.AdminCapacity)
	admin.GET("/data/reconcile", controller.AdminReconcile)
	// 补发「已入账但没有任何返利行」的订单（返利发放失败后的唯一修复通道）。
	// 只补凭据、不动余额，所以是管理员手动触发 + 写审计。
	admin.POST("/data/reconcile/affiliate/backfill", controller.AdminAffiliateBackfill)
	admin.GET("/logs", controller.AdminLogList)
	admin.GET("/logs/analytics", controller.AdminLogAnalytics)
	admin.GET("/logs/export", controller.AdminLogExport)
	// 详情必须注册在静态子路径之后：Gin 同层允许「多静态 + 一个 :param」，静态优先，
	// 但把 :id 放前面会让 /logs/export 的语义依赖注册顺序，读代码时更易误判。
	admin.GET("/logs/:id", controller.AdminLogDetail)
	admin.GET("/audits", controller.AdminAuditList)
	admin.GET("/audits/meta", controller.AdminAuditMeta)
	admin.GET("/audits/export", controller.AdminAuditExport)
	// 认证事件（登录/注册/改密/二次验证…）审计：与 admin_audit 分开两张表，
	// 因为「谁动了配置」这类查询不该被登录噪音淹没（见 model.AuthEvent 注释）。
	admin.GET("/auth-events", controller.AdminAuthEventList)
	admin.GET("/auth-events/meta", controller.AdminAuthEventMeta)
	admin.GET("/auth-events/export", controller.AdminAuthEventExport)

	// 兑换码管理（S16）：批量生成/列表/作废/导出
	admin.POST("/redeems", controller.AdminRedeemGenerate)
	admin.GET("/redeems", controller.AdminRedeemList)
	admin.POST("/redeems/delete", controller.AdminRedeemDelete)
	admin.GET("/redeems/export", controller.AdminRedeemExport)

	// 充值套餐（后台配置）：列表/新建/编辑/启停/删除。
	// 启停单独一条 PATCH：PUT 不接收 enabled，避免同一字段两条写路径、两种审计动作。
	admin.GET("/packages", controller.AdminPackageList)
	admin.POST("/packages", controller.AdminPackageCreate)
	admin.PUT("/packages/:id", controller.AdminPackageUpdate)
	admin.PATCH("/packages/:id/status", controller.AdminPackageStatus)
	admin.DELETE("/packages/:id", controller.AdminPackageDelete)

	// /v1：网关（API KEY）——S5 最小闭环端点；未实现端点由 NoRoute 前缀兜底返回 404
	//
	// AccessLog 必须排在 TokenAuth **之前**：TokenAuth 自身会返回 401，
	// 挂在其后的话鉴权失败请求完全不留痕（线上最常见的「key 怎么突然不能用了」就无迹可查）。
	v1 := r.Group("/v1")
	// PreflightRecorder 必须在 TokenAuth **之前**：401/403/429 由鉴权中间件直接返回，
	// 挂在后面就一条都记不到，而那正是预检失败里占比最高的一类。
	// 它只读响应状态与 error_code，不包装 Writer、不碰响应体（gzip/SSE 顺序不受影响）。
	//
	// IngressFormat 同样必须在 TokenAuth 之前：它决定错误体形状，
	// 挂晚了 401/403/429 就永远是 OpenAI 形状（Claude 客户端显示"未知错误"）。
	// 它只读已匹配路由并写 gin.Context，不碰请求体与响应。
	v1.Use(middleware.AccessLog(), middleware.IngressFormat(), relay.PreflightRecorder(), middleware.TokenAuth())
	v1.POST("/chat/completions", relay.ChatCompletions)
	v1.POST("/completions", relay.Completions)
	v1.POST("/embeddings", relay.Embeddings)
	// 图像生成：默认同步出图；请求带 `async:true` 时转任务（同一条路径）。
	//
	// 不另开 /images/generations/async：同步与异步在上游是同一套流程，只差平台等不等。
	// 两个入口会立刻长出两篇同字段文档、两个预设，并且用户永远要问「该用哪个」。
	v1.POST("/images/generations", controller.ImagesCreate)
	v1.POST("/images/edits", relay.ImagesEdits)
	// 旧别名：请求体可以是 JSON（见 relay.jsonBodyAllowed）。
	v1.POST("/edits", relay.ImagesEdits)
	v1.POST("/images/variations", relay.ImagesVariations)
	v1.POST("/audio/speech", relay.AudioSpeech)
	v1.POST("/audio/transcriptions", relay.AudioTranscriptions)
	v1.POST("/audio/translations", relay.AudioTranslations)
	v1.POST("/moderations", relay.Moderations)
	v1.POST("/rerank", relay.Rerank)
	v1.GET("/account", relay.Account)
	v1.GET("/usage", relay.UsageQuery)
	// 网关只读目录（设计 §4.4.x / N10）：模型列表（含 pricing）、各模态默认（含 allowed）、KEY 维度用量汇总
	v1.GET("/models", controller.GatewayModels)
	v1.GET("/models/defaults", controller.GatewayModelDefaults)
	// D4 对齐 new-api / OpenAI 标准 models.retrieve（须在 /models/defaults 之后注册，静态优先）
	v1.GET("/models/:model", controller.GatewayModelGet)
	// Gemini 通配路径的 /v1 版本：兼容实现同时注册了 /v1beta/models/*path 与
	// /v1/models/*path，后者服务那些把 base_url 填成 `https://host/v1` 的客户端
	// （Gemini SDK 会在后面拼 `models/<name>:动作`，于是整条路径落在 /v1 下）。
	// 不注册的表现是 405（`:model` 路径已存在、方法不符）—— 一个"地址看着对、
	// 就是不通"的错误，而真实原因只是少写了一层路由。
	v1.POST("/models/:model", relay.GeminiCall)
	v1.GET("/usage/summary", relay.UsageSummary)
	// D1 对齐 new-api：billing 配额口（生态客户端余额显示依赖）
	v1.GET("/dashboard/billing/subscription", controller.BillingSubscription)
	v1.GET("/dashboard/billing/usage", controller.BillingUsage)
	// OpenAI 兼容新对话端点（设计 L383）：仅 OpenAI 兼容渠道，否则 400 endpoint_unsupported
	v1.POST("/responses", relay.Responses)
	// Codex 的上下文压缩：上游同名路径，响应形状与 responses 相同。
	v1.POST("/responses/compact", relay.ResponsesCompact)
	// Anthropic Messages 协议入站（与对话补全同一能力、不同线格式）。
	// count_tokens 显式返回 501：给错数字比不给更难查（客户端会按它管理上下文）。
	v1.POST("/messages", relay.Messages)
	v1.POST("/messages/count_tokens", relay.MessagesCountTokens)
	// Gemini 的遗留向量化路径：new-api 把它路由到 Gemini 处理器，存量客户端里有在用。
	// 请求体既可能是 Gemini 形状也可能是 OpenAI 形状（见 geminiconv.embedToUpstream）。
	v1.POST("/engines/:model/embeddings", relay.GeminiCall)
	// Codex 的 alpha 搜索（另有上游 /backend-api/codex/alpha/search）本平台不提供：
	// 交给 NoRoute 回 404 Invalid URL，与 new-api 的 404 一致。

	// 文件上传（S8）：24h 临时托管（KEY 鉴权，不接入计费、不受模型/模态白名单约束）
	v1.POST("/files", controller.FileUpload)
	v1.GET("/files", controller.FileList)
	// get / content 是 OpenAI SDK 的 files.retrieve / files.content 对应物。
	// 只有 upload/list/delete 时，SDK 能传能列能删，却读不回元信息、也下不了内容。
	v1.GET("/files/:id", controller.FileGet)
	v1.HEAD("/files/:id", controller.FileGet)
	v1.GET("/files/:id/content", controller.FileContent)
	v1.HEAD("/files/:id/content", controller.FileContent)
	v1.DELETE("/files/:id", controller.FileDelete)

	// 异步任务与视频生成（S7）：video 主端点 + Sora 别名层（共用 tasks 存储）
	v1.POST("/video/generations", controller.VideoCreate)
	// 再生成（升档）：独立端点，单独一个模型 ID 与价目；关掉该模型即可整体下线。
	v1.POST("/video/regenerations", controller.VideoRegenerate)
	// 查询 / 下载 / 取消全部走 /v1/tasks/*：视频不再有专属读面。
	// 保留一条读面（而不是两套 handler、两种响应形状）是"一个真相源"的要求：
	// 两套并存时，用户永远要问"我该用哪个"，而两套的字段还不一样。
	v1.POST("/videos", controller.VideoCreate)

	// 音乐 / 歌词的异步提交面（B6-2）。
	//
	// 为什么不复用 /v1/video/generations：模态闸门由入口声明（ExpectModality），
	// 复用会让「视频入口 + 音乐模型」按秒价冻结 —— 要么少收要么倒挂。
	// 读面统一走下面的 /v1/tasks（全模态同一套状态与产物接口）。
	v1.POST("/audio/music/generations", controller.MusicCreate)
	v1.POST("/audio/lyrics", controller.LyricsCreate)

	// 统一异步任务读面（B1-2）：全模态同一个入口，产物是独立资源。
	//
	// 这里曾有 `/tasks/:id/contents/:index`：它服务的是富对象 `task.View` 的
	// `contents[].url`，而那个形状已经不在协议面使用（`/v1` 统一扁平 `TaskFlat`）。
	// 保留它只会让「产物到底从哪取」有两个答案 —— 现在只有 artifacts 这一条。
	//
	// 只注册读面（查询/产物/取消）：写面（提交）由各模态专用端点负责，
	// 这样各模态的入参校验能保持在自己的 controller 里。
	if tasksAPIEnabled() {
		v1.GET("/tasks", controller.GatewayTaskList)
		v1.GET("/tasks/:id", controller.GatewayTaskGet)
		// 产物是**独立资源**（new-api 同构）：清单与字节分开取，
		// 轮询状态时不必把几十条产物一起拖回来。
		v1.GET("/tasks/:id/artifacts", controller.GatewayTaskArtifacts)
		// HEAD 与 GET 必须成对：CDN/监控用 `curl -I` 探活是常态，
		// 漏注册会落到 NoRoute（对 /v1 是 404「路径不存在」），把「接口正常」
		// 探成「接口没了」。handler 侧对 HEAD 走元信息轻路径，不读对象字节。
		v1.GET("/tasks/:id/artifacts/:key/content", controller.GatewayTaskArtifactContent)
		v1.HEAD("/tasks/:id/artifacts/:key/content", controller.GatewayTaskArtifactContent)
		v1.DELETE("/tasks/:id", controller.GatewayTaskCancel)
		// OpenAI Video 的读面（提交在下面与 /v1/video/generations 共用 handler）。
		// 与 /v1/tasks/* 同源，形状是视频对象；content 固定取第 0 个产物。
		v1.GET("/videos/:id", controller.GatewayVideoGet)
		v1.GET("/videos/:id/content", controller.GatewayVideoContent)
		v1.HEAD("/videos/:id/content", controller.GatewayVideoContent)
	}
	// 视频查询壳（与 POST /v1/video/generations 配对）：结构与 /v1/tasks/{id}
	// 同源，只多一层 {code,message,data} 与 result_url。
	//
	// 必须与 /v1/tasks 同一个开关：它是任务状态的另一条读面，开关关掉却还能读，
	// 等于"关不掉任务读面"（运维以为已经停止对外提供）。
	if tasksAPIEnabled() {
		v1.GET("/video/generations/:id", controller.GatewayVideoTaskGet)
	}

	// /suno：new-api 的 SUNO 协议面（**顶层路径，无 /v1 前缀**）。
	//
	// 为什么单独一个组：路径前缀由协议规定（已经按 new-api 写好的客户端把
	// `/suno/submit/music` 直接拼在 base_url 后面），而**成功/失败的信封**
	// 也不同（`{code,message,data}`）—— 整棵树必须是 NewAPI 形状。
	//
	// 中间件顺序与 /v1 一致，且 IngressFormat 同样必须在 TokenAuth **之前**：
	// 它决定 401/403 的错误体形状，挂晚了鉴权失败会退回平台信封。
	suno := r.Group("/suno")
	suno.Use(middleware.AccessLog(), middleware.IngressFormat(), relay.PreflightRecorder(), middleware.TokenAuth())
	suno.POST("/submit/:action", controller.SunoSubmit)
	// 读面与 /v1/tasks 同一个开关：运维关掉任务读面时，不该还有一条
	// `/suno/fetch` 能读到同样的东西（那等于关不掉）。
	if tasksAPIEnabled() {
		suno.GET("/fetch/:task_id", controller.SunoFetch)
		suno.POST("/fetch", controller.SunoFetchBatch)
	}

	// /v1beta：Gemini（Google Generative Language）协议面。
	//
	// 为什么单独一个组而不是挂到 /v1 下：路径前缀由协议规定（SDK 把它拼在 base_url 后面），
	// 而**错误体形状**也不同 —— 整棵树必须是 Gemini 形状，不能按端点逐个判。
	//
	// 中间件顺序与 /v1 一致，只有一处差别：GeminiSurface 替代 IngressFormat，
	// 且同样必须在 TokenAuth **之前**（否则 401 是 OpenAI 形状，SDK 只会显示 unknown error）。
	beta := r.Group("/v1beta")
	beta.Use(middleware.AccessLog(), middleware.GeminiSurface(), relay.PreflightRecorder(), middleware.TokenAuth())
	beta.GET("/models", controller.GeminiModelList)
	// 一条路由承接 :generateContent / :streamGenerateContent / :embedContent / :countTokens
	// —— 动作在 :model 参数里（gin 的 param 只以 `/` 终止），handler 切出来再分派。
	beta.POST("/models/:model", relay.GeminiCall)
	// Gemini 的「OpenAI 兼容」层：SDK 只改 base_url 就能用，因此它必须**保持 OpenAI 形状**
	// （错误体也必须是 OpenAI 的，见 middleware.GeminiSurface 的例外分支）。
	beta.GET("/openai/models", controller.GatewayModelsOpenAI)

	// D1 对齐 new-api：不带 /v1 前缀的 billing 口（部分客户端按 OpenAI 老习惯直连 /dashboard/billing/*）
	//
	// 中间件必须与 /v1 组**同一套**：同一份 handler 挂两处，链不同就会出现
	// 「有 request_id / 有调用记录」的差异，401 的错误体形状也会分叉（D6）。
	dash := r.Group("/dashboard")
	dash.Use(middleware.AccessLog(), middleware.IngressFormat(), relay.PreflightRecorder(), middleware.TokenAuth())
	dash.GET("/billing/subscription", controller.BillingSubscription)
	dash.GET("/billing/usage", controller.BillingUsage)

	return r
}

// tasksAPIEnabled 判断是否注册 /v1/tasks 读面（LD_TASKS_API=0 时关闭，仅作灰度回退手段）。
//
// 默认开启：不开的话非视频任务的产物对外不可达，属于「能力缺失」而不是「风险开关」。
func tasksAPIEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_TASKS_API"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// registerInflightGauge 注册 task_inflight 采集函数（只在 /metrics 开启时调用）。
//
// 必须是「抓取时查库」而不是「进程内计数」：内存计数在重启后归零，
// 而库里 queued/running 的任务是真实存在的 —— 指标一旦长期偏离事实，告警就没人信了。
//
// 采集函数带 2s 超时：Prometheus 默认抓取超时 10s，一次慢查询不能把整个 /metrics 拖死。
// 查询失败时 InflightCount 返回 0（而不是让整个抓取 500）：宁可少一条曲线，
// 也不能因为一个采集器把网关的其它指标全带进黑洞。
func registerInflightGauge() {
	metrics.RegisterGaugeFunc(metrics.NameTaskInflight, func() float64 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return float64(task.InflightCount(ctx))
	})
}

// metricsTokenAuth 是 /metrics 的口令中间件。
//
// 口令不匹配或缺失一律回 404：403 等于确认「这里有个指标端点，只是口令不对」，
// 与「未配置口令就不挂载」的策略保持一致（对扫描器完全不可见）。
// 用常量时间比较，避免按字节回退的时间侧信道（写法与 controller.HoldProbe 一致）。
func metricsTokenAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 空口令 fail-closed：ConstantTimeCompare("","") == 1，不加这一条空口令会变成放行。
		if token == "" {
			response.NotFound(c)
			c.Abort()
			return
		}
		got := strings.TrimSpace(c.GetHeader("X-Metrics-Token"))
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			response.NotFound(c)
			c.Abort()
			return
		}
		c.Next()
	}
}
