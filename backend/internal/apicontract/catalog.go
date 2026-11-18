package apicontract

// 本文件是**唯一**的端点声明处。顺序即文档左侧导航顺序。
//
// 加/删端点的唯一正确做法：改这里 + 改 router.go 的注册，两者由
// router/api_scope_gate_test.go 的双向对账强制一致。**不要**在别处再维护一份清单。
//
// 路径必须是 gin 形式（`:id`）。写 `{id}` 的后果不是编译错误，而是运行期
// `c.FullPath()` 与声明不相等 -> 守卫 fail-closed -> 该端点永远 403，
// 而文档页上它看起来完全正常。

// 分组名（文档左侧导航的一级目录）。顺序在 catalogOrder 之前由 GroupOrder 决定。
//
// 「应用」独立成组而不是塞进「账号」：应用是接入方视角的第一入口
// （先有应用、再谈密钥与调用），而账号组是"我这个用户"的事。
// 两者混在一起时，终端开发者在"账号"里找应用公开信息，只能靠搜索。
const (
	GroupAccount  = "账号"
	GroupApp      = "应用"
	GroupAuth     = "认证"
	GroupSecurity = "安全"
	GroupKeys     = "密钥"
	GroupUsage    = "用量"
	GroupBilling  = "账单"
	GroupTasks    = "任务"
	GroupFiles    = "文件"
)

// GroupOrder 返回分组在文档导航中的位置（数字小的在前）。
//
// 未登记的分组排在最后：新增分组却忘了登记时，页面不会挤到第一位，
// 而是明确地"在最下面"，比静默错位更容易发现。
func GroupOrder(g string) int {
	switch g {
	case GroupAccount:
		return 0
	case GroupApp:
		return 1
	case GroupAuth:
		return 2
	case GroupSecurity:
		return 3
	case GroupKeys:
		return 4
	case GroupUsage:
		return 5
	case GroupBilling:
		return 6
	case GroupTasks:
		return 7
	case GroupFiles:
		return 8
	default:
		return 99
	}
}

// catalog 是对外端点的全量声明。
//
// 顺序即文档顺序；同一分组内按"读 -> 写 -> 危险"排列，让接入方先看到能安全试的
// 端点，再看会改状态的端点。
var catalog = []Endpoint{
	// ---------------- 认证（无凭证） ----------------
	{
		Key: "auth.capabilities", Group: GroupAuth, Title: "探测本站接入能力",
		Method: "GET", Path: "/api/auth/capabilities", Auth: AuthOpen, Media: MediaJSON,
		Notes: "接入前先读它：注册模式（open/invite_only/closed）、是否需要联系方式验证、" +
			"允许用哪些联系方式（邮箱/短信）、它们是否真的能发出（email_enabled/sms_enabled）、" +
			"三个场景的验证码策略、登录失败阈值、换绑与兑换是否可用、用量默认查询窗口。" +
			"**不含**站点品牌/公告/价格 —— 那是门户渲染契约，不是接入契约",
	},
	{
		Key: "auth.captcha", Group: GroupAuth, Title: "获取图形验证码",
		Method: "GET", Path: "/api/user/captcha", Auth: AuthOpen, Media: MediaJSON,
		Notes: "query 参数 scene 取 register / login / password_reset；注册恒要求图形码，" +
			"返回 {id, image}（dataURL 图片可直接渲染，id 随注册提交）",
	},
	{
		Key: "auth.register_contact_code", Group: GroupAuth, Title: "发送注册验证码",
		Method: "POST", Path: "/api/user/register/contact-code", Auth: AuthOpen, Media: MediaJSON,
		Idempotency: "同一收件人 60 秒内只发一次，超频返回 verify_code_rate_limited",
	},
	{
		Key: "security.contact_code", Group: GroupSecurity, Title: "发送换绑验证码",
		Method: "POST", Path: "/api/user/contact/code", Auth: AuthSession, Media: MediaJSON,
		Idempotency: "同一收件人 60 秒内只发一次；超频返回 verify_code_rate_limited",
		Notes: "给**当前登录人自己**换绑邮箱/手机号前的发码。仅接受会话票（API KEY 返回 " +
			"session_required）：换绑会踢掉其它设备并禁用全部 API KEY，不能让应用代做",
	},
	{
		Key: "security.contact_bind", Group: GroupSecurity, Title: "换绑邮箱或手机号",
		Method: "POST", Path: "/api/user/contact/bind", Auth: AuthSession, Media: MediaJSON,
		Idempotency: "**非幂等**：验证码一次性消费；重复提交返回 verify_code_invalid",
		Notes: "成功后撤销该用户**除当前会话外**的全部会话，并禁用其**全部 API KEY**" +
			"（响应里的 revoked_keys 是数量，需提示用户重建）。目标地址被占用返回 target_taken",
	},
	{
		Key: "auth.register", Group: GroupAuth, Title: "注册账号",
		Method: "POST", Path: "/api/user/register", Auth: AuthOpen, Media: MediaJSON,
		Idempotency: "邮箱或手机号唯一，重复注册返回 email_taken / target_taken",
	},
	{
		Key: "auth.login", Group: GroupAuth, Title: "登录并获取会话票",
		Method: "POST", Path: "/api/user/login", Auth: AuthOpen, Media: MediaJSON,
		Notes: "响应只下发 token，不下发任何 Cookie；已开启二次验证时只返回挑战，不返回 token",
	},
	{
		Key: "auth.login_mfa", Group: GroupAuth, Title: "二次验证登录",
		Method: "POST", Path: "/api/user/login/mfa", Auth: AuthOpen, Media: MediaJSON,
		Idempotency: "挑战一次性，重复使用返回 mfa_challenge_expired",
	},
	{
		Key: "auth.reset_contact_code", Group: GroupAuth, Title: "发送找回密码验证码",
		Method: "POST", Path: "/api/user/password/reset/contact-code", Auth: AuthOpen, Media: MediaJSON,
		Idempotency: "同一账号发送频率受限，超限返回 verify_code_rate_limited",
	},
	{
		Key: "auth.reset_verify", Group: GroupAuth, Title: "校验验证码换取重置凭据",
		Method: "POST", Path: "/api/user/password/reset/verify", Auth: AuthOpen, Media: MediaJSON,
		Idempotency: "凭据一次性，重复使用返回 reset_ticket_invalid",
	},
	{
		Key: "auth.reset_password", Group: GroupAuth, Title: "用重置凭据设置新密码",
		Method: "POST", Path: "/api/user/password/reset", Auth: AuthOpen, Media: MediaJSON,
		Idempotency: "凭据一次性，成功后该账号全部会话失效",
	},

	// ---------------- 账号 ----------------
	{
		Key: "account.self", Group: GroupAccount, Title: "查询当前账号与余额",
		Method: "GET", Path: "/api/user/self", Auth: AuthReader,
		Media: MediaJSON,
		Notes: "余额字段是否下发取决于该 API KEY 的「允许查询账户额度」开关；会话票恒可见",
	},

	// ---------------- 安全（仅会话票） ----------------
	{
		Key: "security.logout", Group: GroupSecurity, Title: "退出当前会话",
		Method: "POST", Path: "/api/user/logout", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "幂等：重复退出同样返回成功",
	},
	{
		Key: "security.password_change", Group: GroupSecurity, Title: "修改密码",
		Method: "POST", Path: "/api/user/password/change", Auth: AuthSession,
		Media: MediaJSON, Notes: "成功后其它会话全部失效，当前会话保留",
	},
	{
		Key: "security.sessions_list", Group: GroupSecurity, Title: "列出登录设备",
		Method: "GET", Path: "/api/user/sessions", Auth: AuthSession,
		Media: MediaJSON,
	},
	{
		Key: "security.sessions_revoke_all", Group: GroupSecurity, Title: "退出全部其它设备",
		Method: "POST", Path: "/api/user/sessions/revoke-all", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "幂等：无其它会话时同样返回成功",
	},
	{
		Key: "security.sessions_revoke", Group: GroupSecurity, Title: "退出指定设备",
		Method: "POST", Path: "/api/user/sessions/:id/revoke", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "二次调用返回 not_found（该会话已不存在）",
	},
	{
		Key: "security.mfa_status", Group: GroupSecurity, Title: "查询二次验证状态",
		Method: "GET", Path: "/api/user/mfa/status", Auth: AuthSession,
		Media: MediaJSON,
	},
	{
		Key: "security.mfa_setup", Group: GroupSecurity, Title: "生成二次验证密钥",
		Method: "POST", Path: "/api/user/mfa/setup", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "重复调用会覆盖上一次未确认的密钥",
		Notes: "需当前密码重认证",
	},
	{
		Key: "security.mfa_enable", Group: GroupSecurity, Title: "确认开启二次验证",
		Method: "POST", Path: "/api/user/mfa/enable", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "二次调用返回 409",
		Notes: "需当前密码重认证；响应里的 10 个备份码只返回这一次",
	},
	{
		Key: "security.mfa_disable", Group: GroupSecurity, Title: "关闭二次验证",
		Method: "POST", Path: "/api/user/mfa/disable", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "幂等：未开启时同样返回成功",
	},
	{
		Key: "security.mfa_backup_codes", Group: GroupSecurity, Title: "重新生成备份码",
		Method: "POST", Path: "/api/user/mfa/backup-codes/regenerate", Auth: AuthSession,
		Media: MediaJSON,
		Notes: "非幂等：每次调用都会让旧备份码立即失效，新备份码只返回这一次",
	},

	// ---------------- 密钥（仅会话票） ----------------
	{
		Key: "keys.list", Group: GroupKeys, Title: "列出我的 API 密钥",
		Method: "GET", Path: "/api/user/keys", Auth: AuthSession,
		Media: MediaJSON, Notes: "永不返回明文，只返回前缀",
	},
	{
		Key: "keys.create", Group: GroupKeys, Title: "创建 API 密钥",
		Method: "POST", Path: "/api/user/keys", Auth: AuthSession,
		Media: MediaJSON, Notes: "明文只在这条响应里返回一次",
	},
	{
		Key: "keys.update", Group: GroupKeys, Title: "修改 API 密钥设置",
		Method: "PUT", Path: "/api/user/keys/:id", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "整体替换：未传的字段被重置为默认值",
	},
	{
		Key: "keys.status", Group: GroupKeys, Title: "启用或停用 API 密钥",
		Method: "PATCH", Path: "/api/user/keys/:id/status", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "幂等：重复设置同一状态返回成功",
	},
	{
		Key: "keys.reset", Group: GroupKeys, Title: "重置 API 密钥",
		Method: "POST", Path: "/api/user/keys/:id/reset", Auth: AuthSession,
		Media: MediaJSON, Notes: "非幂等：每次调用都生成新密钥，旧密钥立即失效；明文只返回一次",
	},
	{
		Key: "keys.plaintext", Group: GroupKeys, Title: "查看 API 密钥明文",
		Method: "GET", Path: "/api/user/keys/:id/plaintext", Auth: AuthSession,
		Media: MediaJSON, Notes: "只对本人在门户创建的密钥有效；应用托管密钥返回 409",
	},
	{
		Key: "keys.delete", Group: GroupKeys, Title: "删除 API 密钥",
		Method: "DELETE", Path: "/api/user/keys/:id", Auth: AuthSession,
		Media: MediaJSON, Idempotency: "二次调用返回 not_found",
	},

	// ---------------- 用量 ----------------
	{
		Key: "usage.list", Group: GroupUsage, Title: "查询调用记录",
		Method: "GET", Path: "/api/user/usage", Auth: AuthReader,
		Media: MediaJSON,
		Notes: "可按 key_id 过滤；分页 page / page_size（上限 100）。items[].key_name 即该次调用所用密钥名",
	},
	{
		Key: "usage.summary", Group: GroupUsage, Title: "查询用量汇总",
		Method: "GET", Path: "/api/user/usage/summary", Auth: AuthReader,
		Media: MediaJSON, Notes: "按模型分组汇总；时间范围与列表端点同参数",
	},
	{
		Key: "usage.export", Group: GroupUsage, Title: "导出调用记录",
		Method: "GET", Path: "/api/user/usage/export", Auth: AuthReader,
		Media: MediaCSV,
		Notes: "UTF-8 BOM 的 CSV，最多 10000 行；成功时是文件流，失败时是 JSON 信封（按 Content-Type 区分）；限 2 次/分钟",
	},
	{
		Key: "usage.detail", Group: GroupUsage, Title: "查询单条调用详情",
		Method: "GET", Path: "/api/user/usage/:id", Auth: AuthReader,
		Media: MediaJSON,
	},

	// ---------------- 账单 ----------------
	{
		Key: "billing.transactions", Group: GroupBilling, Title: "查询资金流水",
		Method: "GET", Path: "/api/user/transactions", Auth: AuthReader,
		Media: MediaJSON, Notes: "包含 balance_after 快照，可直接拼出对账曲线",
	},
	{
		Key: "billing.packages", Group: GroupBilling, Title: "查询充值套餐",
		Method: "GET", Path: "/api/user/packages", Auth: AuthReader,
		Media: MediaJSON, Notes: "下发 pay_options（可用的支付方式）与 balance_limit（余额上限）",
	},
	{
		Key: "billing.orders_list", Group: GroupBilling, Title: "查询订单列表",
		Method: "GET", Path: "/api/user/orders", Auth: AuthReader,
		Media: MediaJSON, Notes: "client_order_no 为精确匹配，用于确认某次下单是否已创建",
	},
	{
		Key: "billing.orders_get", Group: GroupBilling, Title: "查询订单详情",
		Method: "GET", Path: "/api/user/orders/:id", Auth: AuthReader,
		Media: MediaJSON, Notes: "支付结果请轮询本端点；状态唯一真相源是支付回调",
	},
	{
		Key: "billing.orders_qrcode", Group: GroupBilling, Title: "获取订单支付二维码",
		Method: "GET", Path: "/api/user/orders/:id/qrcode", Auth: AuthReader,
		Media: MediaJSON,
		Notes: "qr 为 dataURL 或二维码内容（mode 区分），渲染成图后供用户扫码；" +
			"订单无二维码时返回 mode=none，二维码失效返回 pay_qr_unavailable",
	},

	// ---------------- 任务 ----------------
	{
		Key: "tasks.list", Group: GroupTasks, Title: "查询异步任务列表",
		Method: "GET", Path: "/api/user/tasks", Auth: AuthReader,
		Media: MediaJSON, Notes: "支持 status / model / key_id 过滤",
	},
	{
		Key: "tasks.detail", Group: GroupTasks, Title: "查询任务详情",
		Method: "GET", Path: "/api/user/tasks/:id", Auth: AuthReader,
		Media: MediaJSON, Notes: "contents[] 给出产物数量与类型，不含直链",
	},
	{
		Key: "tasks.content", Group: GroupTasks, Title: "取回任务产物",
		Method: "GET", Path: "/api/user/tasks/:id/contents/:index", Auth: AuthReader,
		Media: MediaBinary,
		Notes: "index 从 0 开始；支持 Range 续传（206）；成功时是字节流，失败时是 JSON 信封",
	},

	// ---------------- 文件 ----------------
	{
		Key: "files.list", Group: GroupFiles, Title: "查询我的文件",
		Method: "GET", Path: "/api/user/files", Auth: AuthReader,
		Media: MediaJSON, Notes: "跨全部密钥；可按 key_id 过滤；直链域名由平台配置决定，不要用响应里的 Host 推断",
	},

	// ---------------- 应用 ----------------
	//
	// 三条公开面排在最前：它们是终端接入的第一站，也是本组唯一不需要凭证的接口。
	// 三条而不是一条聚合，判据是「变更频率 × 鉴权等级 × 缓存策略」是否一致 ——
	// 合成一条就只能取最严的那个（公告要求新鲜 ⇒ 整条 no-store ⇒ 周级变更的品牌
	// 每次冷调用都要重查）。详见 controller/app_public.go 的包注释。
	{
		Key: "apps.profile", Group: GroupApp, Title: "查询应用公开资料",
		Method: "GET", Path: "/api/apps/:slug/profile", Auth: AuthOpen,
		Media:  MediaJSON,
		Public: &PublicPolicy{RouteLimit: 600, MaxAge: 60},
		Notes: "**不需要凭证**：终端在用户登录之前就要展示品牌与维护状态。" +
			"只下发终端用户本来就该看到的内容（用户数、消费、密钥数、在线状态、" +
			"应用启用状态都不在其中）。应用处于维护模式时**仍然返回 200** —— " +
			"终端要能解释「为什么用不了」；此时调用模型接口会得到 503 `app_maintenance`。" +
			"停用（status=2）不影响本接口：停用只拒绝新签发，已有密钥仍可调用。" +
			"标识可枚举，所以「不存在」与「标识格式不合法」返回同一个码。",
	},
	{
		Key: "apps.notices", Group: GroupApp, Title: "查询应用生效公告",
		Method: "GET", Path: "/api/apps/:slug/notices", Auth: AuthOpen,
		Media:  MediaJSON,
		Public: &PublicPolicy{RouteLimit: 300, MaxAge: 30},
		Notes: "**不需要凭证**。只含**此刻生效**的公告（已启用且落在时间窗内），最多 20 条；" +
			"无公告时是 `[]` 而不是 `null`，终端可以直接遍历。" +
			"这是最易变、体积最大的一份数据（最坏约 40KB），所以单独一条端点 —— " +
			"不要与品牌/版本合拉，否则三者只能一起过期。",
	},
	{
		Key: "apps.release", Group: GroupApp, Title: "查询应用最近发布",
		Method: "GET", Path: "/api/apps/:slug/release", Auth: AuthOpen,
		Media:  MediaJSON,
		Public: &PublicPolicy{RouteLimit: 120, MaxAge: 300},
		Notes: "**不需要凭证**。取最近一次**发布**的版本，按发布时间排序而不是按版本号 —— " +
			"hotfix 回填旧版本号是合法的，按版本号取最大会让线上静默降级。" +
			"无已发布版本时 `release` 为 `null`，终端据此隐藏「检查更新」。",
	},
	{
		Key: "apps.plugin_records", Group: GroupApp, Title: "查询应用插件的公开数据",
		Method: "GET", Path: "/api/apps/:slug/p/:plugin/:entity", Auth: AuthOpen,
		Media:  MediaJSON,
		Public: &PublicPolicy{RouteLimit: 120, MaxAge: 30},
		Notes: "**不需要凭证，但默认关闭**：要同时满足「管理台给该应用打开了对外只读」" +
			"与「manifest 的 `entities[].public` 声明了字段白名单」才下发数据，否则一律 404 —— " +
			"不区分「没这个插件 / 没打开 / 对象没声明」，区分它们就是给匿名调用方一份枚举面。" +
			"只回白名单里的字段：`data` 里不会出现未声明的列，也不含写入用的版本号。" +
			"分页 `page` / `page_size`（默认 20、上限 50），越界按边界收敛而不是报错。",
	},
	{
		Key: "apps.list", Group: GroupApp, Title: "查询我已接入的应用",
		Method: "GET", Path: "/api/user/apps", Auth: AuthReader,
		Media: MediaJSON,
		Notes: "只返回**已持有托管密钥**的应用；app_id 用于按应用过滤用量与订单",
	},
	{
		Key: "apps.hosted_key", Group: GroupApp, Title: "签发应用托管密钥",
		Method: "POST", Path: "/api/user/apps/:slug/hosted-key", Auth: AuthSession,
		Media: MediaJSON,
		Idempotency: "同一应用重复调用返回同一把密钥（plaintext 为空串）；" +
			"rotate=true 才换新的：新明文立即生效，旧明文进入重叠窗口（默认 72h，可配 0–720），" +
			"窗口内旧明文仍可用，也可用旧明文自救轮换（但窗口不延长）",
		Notes: "**只认会话票**：明文只在首次签发/轮换时返回一次，平台不提供回显，丢了只能轮换。" +
			"应用停用只拒绝**首次签发**，已签发的密钥仍可用且仍可轮换；" +
			"但平台管理员设置的封禁（disabled_reason=admin_block）轮换**不会**解除，需联系平台解锁",
	},
	{
		Key: "keys.self_rotate", Group: GroupAccount, Title: "自举轮换托管密钥",
		Method: "POST", Path: "/api/user/key/rotate", Auth: AuthHostedKey,
		Media: MediaJSON,
		Idempotency: "**非幂等**：每次调用都生成新明文。" +
			"同一把钥 10 分钟内重复调用返回 409 key_rotate_busy（当前钥仍有效，不要重试）",
		Notes: "只认应用托管密钥（会话票一律 401、自建 KEY 一律 403）。" +
			"用当前有效的钥换一把新的：新明文立即生效，旧明文进入重叠窗口；" +
			"窗口期**旧钥也可调用**（自救），但窗口不延长。" +
			"**明文只返回给发起调用的实例** —— 多副本部署必须自行分发（配置中心/Secrets Manager）。",
	},
	{
		Key: "keys.health", Group: GroupAccount, Title: "自检托管密钥",
		Method: "GET", Path: "/api/user/key/health", Auth: AuthHostedKey,
		Media: MediaJSON,
		Notes: "只认应用托管密钥，且**只验明文、不判能否使用**：被平台封禁、闲置超期、" +
			"来源 IP 不符的钥同样能查到，这正是它存在的意义 —— 回答\"为什么用不了\"。" +
			"它**不放行任何数据调用**（同一把钥打数据端点仍按鉴权结果拒绝）。" +
			"返回有效期剩余、重叠窗口剩余、禁用成因、IP 白名单、额度上限与账号状态；" +
			"**没有 `healthy` 布尔**（那只是几个事实的再组合，会与事实漂移）。",
	},

	// ---------------- 写操作（AuthWriter）----------------
	// 三条都不是"用户自建 KEY 能做"的操作：一个动钱、两个动数据。
	// 会话票（全权）或托管密钥（须带 scope）可用，判定在 middleware.ScopeGuard。
	{
		Key: "billing.orders_create", Group: GroupBilling, Title: "创建充值订单",
		Method: "POST", Path: "/api/user/orders", Auth: AuthWriter, Scope: ScopeBillingWrite,
		Media: MediaJSON,
		Idempotency: "同号重复提交永远返回同一张订单（200），已付返回 status=paid，" +
			"已过期返回 status=expired（此时请换新号重下）；**不传幂等号就没有幂等保护**",
		Notes: "支付确认不开放；支付结果以回调为准，请轮询本单 status 或订单详情",
	},
	{
		Key: "billing.redeems", Group: GroupBilling, Title: "我的兑换记录",
		Method: "GET", Path: "/api/user/redeems", Auth: AuthSession, Media: MediaJSON,
		Notes: "按 used_at 倒序分页返回本人已兑换的码（id/code/amount/batch/used_at/expires_at）。" +
			"仅会话票：托管密钥代表某个应用，不该读账户级账单。**不提供码有效性预校验**" +
			"（那会变成批量试码的接口）",
	},
	{
		Key: "billing.redeem", Group: GroupBilling, Title: "兑换充值码",
		Method: "POST", Path: "/api/user/redeem", Auth: AuthWriter, Scope: ScopeBillingWrite,
		Media: MediaJSON,
		Idempotency: "**非幂等**：兑换成功后同一码重复使用返回 redeem_used；" +
			"码不存在/失效返回 redeem_invalid / redeem_expired",
		Notes: "兑换直接给账户余额充值；自建 API KEY 无此权限（403 scope_denied），请用会话票",
	},
	{
		Key: "tasks.cancel", Group: GroupTasks, Title: "取消异步任务",
		Method: "POST", Path: "/api/user/tasks/:id/cancel", Auth: AuthWriter, Scope: ScopeTaskWrite,
		Media: MediaJSON,
		Idempotency: "**非幂等**：仅排队中且尚未开始执行的任务可取消；" +
			"重复取消返回 task_not_cancelable（不会二次释放额度）",
		Notes: "取消会全额释放该任务预扣的额度；running/succeeded/expired 阶段不可取消",
	},
	{
		Key: "files.delete", Group: GroupFiles, Title: "删除我的文件",
		Method: "DELETE", Path: "/api/user/files/:id", Auth: AuthWriter, Scope: ScopeFileWrite,
		Media:       MediaEmpty,
		Idempotency: "**非幂等**：删除成功后重复调用返回 not_found（对象已不存在）",
		Notes:       "删除会同时移除对象存储里的实体，不可恢复；已过期的文件同样按不存在处理",
	},
}

// NotExposed 是**明确不开放**的能力清单，文档页原样展示。
//
// 存在的理由：接入方最常问的不是"有什么"，而是"为什么这个没有"。
// 把"不开放 + 原因"写进契约，比在接入群里逐个解释可靠。
type NotExposed struct {
	// Method 为空表示这不是一个单一端点（例如"应用增删改"）。
	Method string
	// Path 为空时不渲染成路径。
	Path string
	// Title 是能力名。
	Title string
	// Reason 是给接入方看的原因（一句话，不含内部实现细节）。
	Reason string
}

// notExposed 与 catalog 一样是唯一真相源。
//
// 注意：这里**不写 /api/admin 路径**。对外文档不暴露管理面拓扑，
// "应用怎么建"只说明"由平台管理员在管理后台配置"。
var notExposed = []NotExposed{
	{
		Method: "POST", Path: "/api/portal/orders/:id/pay", Title: "重新发起支付",
		Reason: "重试统一由「创建订单」的幂等分支驱动，避免同一笔充值在支付侧留下两笔订单",
	},
	{
		Method: "POST", Path: "/api/portal/orders/:id/verify", Title: "主动核对支付结果",
		Reason: "订单状态的唯一真相源是支付回调；请轮询订单详情，不要主动核对",
	},
	{
		Method: "POST", Path: "/api/portal/orders/:id/continue", Title: "逾期订单续付",
		Reason: "续付会新建订单，与幂等号语义冲突；请换一个新的 client_order_no 重新下单",
	},
	{
		Method: "GET", Path: "/api/portal/orders/:id/qrcode", Title: "获取支付二维码图片",
		Reason: "二维码原值已在创建订单的响应里返回，请自行渲染，不要把支付页域名下沉到客户端",
	},
	{
		Method: "POST", Path: "/api/user/redeems/check", Title: "预校验兑换码是否可用",
		Reason: "会变成批量试码的接口（成败三态可区分，等于给了码状态的查询机）。" +
			"请直接兑换：失败时 redeem_invalid / redeem_used / redeem_expired 已经能区分原因",
	},
	{
		Method: "POST", Path: "/api/user/files", Title: "上传文件",
		Reason: "上传是独立攻击面（配额、扫描、临时存储），本期请使用推理接口自带的文件参数",
	},
	{
		Title: "应用的新建、改名、停用、品牌与公告维护",
		Reason: "由平台管理员在管理后台配置。接入方可读 `GET /api/user/apps` 拿到自己" +
			"可用的应用清单，并用 `GET /api/apps/:slug/profile`、`GET /api/apps/:slug/notices`、" +
			"`GET /api/apps/:slug/release` 分别读取该应用对外的品牌与维护状态、生效公告、最近发布",
	},
}
