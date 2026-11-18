package apicontract

import (
	"lingdong-api/internal/pkg/modality"
)

// 本文件是全部端点的**第二层契约**：参数、返回字段、错误码、示例、试运行风险。
//
// 与 catalog.go 的分工，不是按"重要性"分的，是按**审查强度**分的：
//
//	catalog.go  —— 权限边界（谁能调）。改错是越权。必须一眼扫完。
//	detail.go   —— 交互契约（怎么调）。改错是文档难看。字段多、体量大。
//
// 两处用 Key 关联。Key 打错字不会报错，只会让参数表静默变空（页面看起来
// 完全正常，只是"没有参数"），由 detail_gate_test.go 的 TestNoOrphanDetail
// 双向差集封死。
//
// 填充顺序按**分组**（同组端点参数高度同构，review 时一次核对一个 controller 域）：
//
//	批次 A  认证 6 + 账号 3 + 安全 10 = 19
//	批次 B  密钥 7 + 用量 4        = 11
//	批次 C  账单 5 + 任务 4 + 文件 2 = 11
//
// 每批写之前，先把该组 controller 里**三条通路**各查一次：
//  1. handler 内的 `c.Query(...)` / `c.Param(...)`
//  2. 跨包 helper（如 `relay.parseUsageFilter`、`controller.optionalUintQuery`）
//  3. `ShouldBindJSON` 的结构体字段
//
// 只查第一条必漏：`files.list` 的 key_id 藏在 portal_file.go 的 optionalUintQuery
// 里，跟同函数的 type/status 不是同一种写法。
//
// 另一个必须防的坑：**文档写的"必填"要与服务端真实校验一致**。
// `billing.orders_create` 的 client_order_no 在代码里空串是合法的
// （validClientOrderNo("") == true），若按业务愿望写成"必填"，
// 接入方会以为不传就 400，实际不传会每次多建一笔订单 —— 比 400 危险得多。

// details 是 Key -> 第二层契约。
var details = map[string]Detail{
	// ==================== 认证（6 条，无凭证） ====================
	//
	// 对照：controller/auth.go（registerReq / loginReq / loginMFAReq /
	// resetVerifyReq / resetPasswordReq）+ controller/user_api.go（token 形态出口）。
	// 数据来源：service/auth、service/verify。

	"auth.capabilities": {
		Returns: []Return{
			ret("register_mode", TypeString, "注册模式").WithNote("open 开放 / invite_only 需邀请码 / closed 关闭"),
			ret("invite_required", TypeBoolean, "注册是否必须邀请码"),
			ret("contact_verify", TypeString, "注册时联系方式是否必须验证").WithNote("required / off"),
			ret("contact_channels", TypeArray, "平台允许使用的联系方式").
				WithNote("元素为 email / sms；只表示平台允许，**不代表已配置好发送服务**；恒为数组"),
			ret("available_contact_channels", TypeArray, "实际生效的联系方式").
				WithNote("平台允许且已配置好发送服务；按优先级排序（当前短信优先），**首项即默认**；恒为数组，不会返回 null。注册/找回页应只展示这些"),
			ret("captcha_register", TypeString, "注册场景的人机验证策略").WithNote("恒为 always"),
			ret("captcha_login", TypeString, "登录场景的人机验证策略").WithNote("always / after_fails / off"),
			ret("captcha_reset", TypeString, "找回密码场景的人机验证策略").WithNote("恒为 always"),
			ret("login_fail_threshold", TypeInteger, "连续失败多少次后登录需要人机验证"),
			ret("email_enabled", TypeBoolean, "邮箱方式是否已配置好（能真的发出邮件）").
				WithNote("换绑入口据此放行；与注册时允许的联系方式无关"),
			ret("sms_enabled", TypeBoolean, "短信方式是否已配置好（能真的发出短信）").
				WithNote("换绑入口据此放行；与注册时允许的联系方式无关"),
			ret("contact_bind_enabled", TypeBoolean, "换绑接口是否可用（至少一种方式真的能发）").
				WithNote("仅看是否配置好发送服务，不受 contact_channels 限制"),
			ret("custom_topup_enabled", TypeBoolean, "是否允许自定义金额充值"),
			ret("redeem_enabled", TypeBoolean, "是否支持兑换码兑换"),
			ret("min_topup", TypeInteger, "自定义充值的最小金额（微）"),
			ret("usage_default_window_days", TypeInteger, "用量查询不传时间时的默认窗口（天）"),
		},
		Errors: []ErrCode{
			errCode("rate_limited", "请求过于频繁"),
		},
		RespExample: map[string]any{
			"register_mode": "open", "invite_required": false,
			"contact_verify": "off", "contact_channels": []any{"email"},
			"available_contact_channels": []any{"sms", "email"},
			"captcha_register":           "always", "captcha_login": "after_fails", "captcha_reset": "always",
			"login_fail_threshold": 5, "email_enabled": true, "sms_enabled": false,
			"contact_bind_enabled": true, "custom_topup_enabled": true, "redeem_enabled": true,
			"min_topup": 1000000, "usage_default_window_days": 30,
		},
		TrialRisk: RiskSafe,
	},

	"auth.captcha": {
		Params: []Param{
			query("scene", TypeString, ReqRequired, "验证码场景").
				WithEnum("register", "login", "password_reset").WithExample("register"),
		},
		Returns: []Return{
			ret("id", TypeString, "挑战 ID，随后续提交放在 captcha_id 字段").
				WithNote("与图片在同一次响应里配对下发，不依赖 Cookie"),
			ret("image", TypeString, "验证码图片（data:image/png;base64 形式）").
				WithNote("直接放进 <img src> 即可；每次调用都是新挑战，不要缓存"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "scene 不在枚举内"),
			errCode("captcha_unavailable", "验证码服务暂不可用"),
		},
		RespExample: map[string]any{
			"id": "cap_xxxxxxxx", "image": "data:image/png;base64,iVBORw0KGgo...",
		},
		TrialRisk: RiskSafe,
	},

	"auth.register_contact_code": {
		Params: []Param{
			body("channel", TypeString, ReqRequired, "验证码通道").
				WithEnum("email", "sms").WithDefault("email").WithExample("email"),
			body("target", TypeString, ReqRequired, "接收验证码的邮箱或手机号").
				WithNote("必须与 channel 匹配；注册提交时 target 必须与本字段一致").WithExample("demo@example.com"),
		},
		Returns: []Return{
			ret("expires_in", TypeInteger, "验证码有效期（秒）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 channel 或 target"),
			errCode("verify_rate_limited", "同一收件人 60 秒内只发一次；同 IP 每小时上限 20 次"),
			errCode("bind_method_disabled", "该联系方式不在平台启用范围内，或未配置发送服务"),
			errCode("verify_unavailable", "验证码服务暂不可用"),
		},
		ReqExample:  map[string]any{"channel": "email", "target": "demo@example.com"},
		RespExample: map[string]any{"expires_in": 600},
		TrialRisk:   RiskConfirm,
	},

	"auth.register": {
		Params: []Param{
			body("username", TypeString, ReqRequired, "登录用户名").
				WithExample("demo_user").
				WithLimit("min_len", 3, "3-32 位").
				WithLimit("max_len", 32, "3-32 位"),
			// 联系方式三项是**条件必填**，不是恒必填：
			// contact_verify=off 时注册可以不收联系方式（target 为空即跳过验证），
			// 标成 required 会让接入方以为"不传就 400"，从而硬编码一段没人要的
			// 发码流程 —— 而真实行为是"传了就必须验证，不传就不用"。
			body("channel", TypeString, ReqConditional, "验证码方式").
				WithReq("平台要求验证联系方式时必须传；不传按 email 处理").
				WithEnum("email", "sms").WithDefault("email").WithExample("email"),
			body("target", TypeString, ReqConditional, "接收验证码的邮箱或手机号").
				WithReq("平台要求验证联系方式时必填；必须与 channel 匹配").
				WithNote("传了就一定会校验：未验证的地址绝不入库").
				WithExample("demo@example.com"),
			body("code", TypeString, ReqConditional, "上一步收到的验证码").
				WithReq("传了 target 就必须传").
				WithExample("123456").WithLimit("max_len", 8, "≤ 8 位"),
			body("password", TypeString, ReqRequired, "登录密码").
				WithNote("强度不足返回 weak_password").WithExample("Passw0rd!"),
			body("confirm_password", TypeString, ReqRequired, "确认密码，必须与 password 一致").
				WithExample("Passw0rd!"),
			body("aff_code", TypeString, ReqOptional, "邀请码").
				WithNote("非法邀请码静默忽略，不影响注册结果"),
			body("agreed", TypeBoolean, ReqRequired, "是否已阅读并同意服务协议").
				WithNote("传 false 返回 agreement_required").WithExample(true),
			body("captcha_id", TypeString, ReqOptional, "图形验证码 ID").
				WithNote("先 GET /api/user/captcha?scene=register 取 {id, image}；未传返回 captcha_required"),
			body("captcha_answer", TypeString, ReqOptional, "图形验证码答案（图上的 5 位数字）").
				WithExample("37105").WithLimit("max_len", 5, "5 位"),
		},
		Returns: []Return{
			ret("id", TypeInteger, "新账号的 ID"),
			ret("username", TypeString, "用户名（原样回显）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "必填字段缺失，或密码与确认密码不一致"),
			errCode("username_taken", "用户名已被占用"),
			errCode("email_taken", "该邮箱已注册"),
			errCode("target_taken", "该手机号已注册"),
			errCode("verify_code_invalid", "验证码错误或已过期"),
			errCode("verify_rate_limited", "验证码发送过于频繁"),
			errCode("contact_verify_required", "平台要求验证联系方式，但未传 target 或 code"),
			errCode("bind_method_disabled", "所选联系方式未启用"),
			errCode("captcha_required", "当前策略要求人机验证，但未提供"),
			errCode("captcha_invalid", "人机验证未通过或已过期"),
			errCode("weak_password", "密码强度不足"),
			errCode("agreement_required", "未同意服务协议"),
			errCode("register_closed", "平台已关闭自主注册"),
			errCode("invite_required", "当前仅允许邀请注册"),
		},
		ReqExample: map[string]any{
			"username": "demo_user", "channel": "email", "target": "demo@example.com",
			"code": "123456", "password": "Passw0rd!", "confirm_password": "Passw0rd!",
			"agreed": true,
		},
		RespExample: map[string]any{"id": 10001, "username": "demo_user"},
		TrialRisk:   RiskConfirm,
	},

	"auth.login": {
		Params: []Param{
			body("account", TypeString, ReqRequired, "用户名").
				WithNote("也接受邮箱或手机号，取决于平台的登录方式配置").WithExample("demo_user"),
			body("password", TypeString, ReqRequired, "登录密码").WithExample("Passw0rd!"),
			body("captcha_id", TypeString, ReqOptional, "图形验证码 ID").
				WithNote("策略为 always 或连续失败达阈值时要求；服务端要求而未带会返回 captcha_required，取图后重试"),
			body("captcha_answer", TypeString, ReqOptional, "图形验证码答案（5 位数字）"),
		},
		Returns: []Return{
			ret("token", TypeString, "会话票（`lds_` 前缀），后续请求放在 Authorization: Bearer 里").
				WithNote("已开启二次验证时**不下发**，改为返回 mfa_required + challenge"),
			ret("token_type", TypeString, "恒为 Bearer"),
			ret("expires_in", TypeInteger, "会话有效期（秒）").
				WithNote("mfa_required=true 时表示挑战的有效期"),
			ret("user", TypeObject, "账号信息，字段同 GET /api/user/self").
				WithNote("mfa_required=true 时不返回"),
			ret("mfa_required", TypeBoolean, "是否需要第二步二次验证"),
			ret("challenge", TypeString, "二次验证挑战票据，传给 POST /api/user/login/mfa").
				WithNote("一次性，绑本次调用的出口 IP"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 account 或 password"),
			errCode("invalid_credential", "账号或密码错误"),
			errCode("user_disabled", "账号已被停用"),
			errCode("captcha_required", "当前需要人机验证但未提供"),
			errCode("captcha_invalid", "人机验证未通过"),
			errCode("rate_limited", "登录过于频繁"),
			errCode("auth_unavailable", "认证服务暂不可用"),
		},
		ReqExample: map[string]any{"account": "demo_user", "password": "Passw0rd!"},
		RespExample: map[string]any{
			"token": "lds_xxxxxxxxxxxx", "token_type": "Bearer", "expires_in": 604800,
		},
		TrialRisk: RiskConfirm,
	},

	"auth.login_mfa": {
		Params: []Param{
			body("challenge", TypeString, ReqRequired, "第一步返回的挑战票据").
				WithNote("一次性；挑战过期后必须重新登录").WithExample("mfc_xxxxxxxx"),
			body("code", TypeString, ReqRequired, "6 位动态码，或 10 位备份码").
				WithNote("同一个输入框，服务端自己分辨").WithExample("123456"),
		},
		Returns: []Return{
			ret("token", TypeString, "会话票（`lds_` 前缀）"),
			ret("token_type", TypeString, "恒为 Bearer"),
			ret("expires_in", TypeInteger, "会话有效期（秒）"),
			ret("user", TypeObject, "账号信息，字段同 GET /api/user/self"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 challenge 或 code"),
			errCode("mfa_challenge_expired", "挑战已过期、已被使用，或不是同 IP 发起"),
			errCode("mfa_code_invalid", "动态码或备份码错误"),
			errCode("user_disabled", "账号已被停用"),
		},
		ReqExample:  map[string]any{"challenge": "mfc_xxxxxxxx", "code": "123456"},
		RespExample: map[string]any{"token": "lds_xxxxxxxxxxxx", "token_type": "Bearer", "expires_in": 604800},
		TrialRisk:   RiskConfirm,
	},

	"auth.reset_contact_code": {
		Params: []Param{
			body("channel", TypeString, ReqRequired, "验证码通道").
				WithEnum("email", "sms").WithDefault("email").WithExample("email"),
			body("target", TypeString, ReqRequired, "接收验证码的邮箱或手机号").
				WithExample("demo@example.com"),
			body("captcha_id", TypeString, ReqOptional, "图形验证码 ID").
				WithNote("发码前**恒要求**（scene=password_reset），否则这个端点就是一条免费短信通道"),
			body("captcha_answer", TypeString, ReqOptional, "图形验证码答案（5 位数字）"),
		},
		Returns: []Return{
			ret("expires_in", TypeInteger, "验证码有效期（秒）").
				WithNote("账号不存在时同样返回 200 与本字段，无法据此判断账号是否存在"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 channel 或 target"),
			errCode("captcha_required", "未提供人机验证解"),
			errCode("captcha_invalid", "人机验证未通过"),
			errCode("verify_rate_limited", "同一账号或 IP 发送过于频繁"),
			errCode("bind_method_disabled", "该联系方式不在平台启用范围内，或未配置发送服务"),
			errCode("verify_unavailable", "验证码服务暂不可用"),
			errCode("target_unchanged", "新联系方式与当前一致（换绑场景）"),
		},
		ReqExample:  map[string]any{"channel": "email", "target": "demo@example.com"},
		RespExample: map[string]any{"expires_in": 600},
		TrialRisk:   RiskConfirm,
	},

	"auth.reset_verify": {
		Params: []Param{
			body("channel", TypeString, ReqRequired, "验证码通道").
				WithEnum("email", "sms").WithDefault("email").WithExample("email"),
			body("target", TypeString, ReqRequired, "接收验证码的邮箱或手机号").
				WithExample("demo@example.com"),
			body("code", TypeString, ReqRequired, "收到的验证码").WithExample("123456"),
		},
		Returns: []Return{
			ret("reset_ticket", TypeString, "一次性重置凭据，传给 POST /api/user/password/reset").
				WithNote("只校验不消费：本步不会烧掉验证码，可以重复校验"),
			ret("expires_in", TypeInteger, "凭据有效期（秒）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 channel / target / code"),
			errCode("verify_code_invalid", "验证码错误或已过期"),
			errCode("verify_rate_limited", "校验过于频繁"),
		},
		ReqExample:  map[string]any{"channel": "email", "target": "demo@example.com", "code": "123456"},
		RespExample: map[string]any{"reset_ticket": "rst_xxxxxxxx", "expires_in": 600},
		TrialRisk:   RiskSafe,
	},

	"auth.reset_password": {
		Params: []Param{
			body("reset_ticket", TypeString, ReqRequired, "上一步拿到的一次性重置凭据").
				WithNote("用一次即失效").WithExample("rst_xxxxxxxx"),
			body("new_password", TypeString, ReqRequired, "新密码").
				WithNote("强度不足返回 weak_password，此时凭据**仍可复用**").WithExample("NewPassw0rd!"),
		},
		NoData:     true,
		ReqExample: map[string]any{"reset_ticket": "rst_xxxxxxxx", "new_password": "NewPassw0rd!"},
		TrialRisk:  RiskConfirm,
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 reset_ticket 或 new_password"),
			errCode("reset_ticket_invalid", "凭据无效、已使用或已过期"),
			errCode("weak_password", "新密码强度不足"),
		},
	},

	// ==================== 账号（3 条） ====================
	//
	// 对照：controller/user_api.go（UserSelf / selfPayload）、
	// controller/user_hosted_key.go（UserAppList / UserHostedKeyIssue）。

	"account.self": {
		Returns: []Return{
			ret("id", TypeInteger, "账号 ID"),
			ret("username", TypeString, "用户名"),
			ret("role", TypeInteger, "角色：1 普通用户，数字越大权限越高"),
			ret("status", TypeInteger, "账号状态：1 正常，2 停用"),
			ret("group", TypeString, "所属分组（决定可用模型与折扣）"),
			ret("group_discount", TypeInteger, "分组折扣，**微比例**（1 单位 = 1e-6；1000000 = 不打折）"),
			ret("group_configured", TypeBoolean, "该分组是否配置过折扣；false 表示按原价"),
			ret("balance", TypeInteger, "可用余额（**微**，1 单位货币 = 1000000）"),
			ret("frozen", TypeInteger, "冻结中的额度（微），任务结束会释放或结算"),
			ret("used", TypeInteger, "累计已消费（微）"),
			ret("request_count", TypeInteger, "累计请求次数"),
			ret("key_count", TypeInteger, "名下 API 密钥数量（**含已停用**）"),
			ret("key_count_enabled", TypeInteger, "其中处于启用状态的数量（status=1）"),
			ret("email", TypeString, "绑定邮箱，未绑定为空串"),
			ret("phone", TypeString, "绑定手机号，未绑定为空串"),
			ret("must_change_password", TypeBoolean,
				"临时口令未改：为 true 时除改密/登出/踢会话外一律 403 password_change_required"),
			ret("auth_channel", TypeString, "本次调用用的凭证类型").
				WithEnum("session", "api_key").
				WithNote("读能力两者一致，写能力不同"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的 Authorization: Bearer"),
			errCode("session_expired", "凭证已过期，请重新登录"),
			errCode("forbidden", "所用 API 密钥未开启「允许查询账户额度」"),
		},
		RespExample: map[string]any{
			"id": 10001, "username": "demo_user", "balance": 12500000, "frozen": 0,
			"used": 3750000, "key_count": 2, "auth_channel": "api_key",
		},
		TrialRisk: RiskSafe,
	},

	"apps.profile": {
		Params: []Param{
			path("slug", "应用标识（终端内置的那个串，不是应用名称）"),
		},
		Returns: []Return{
			ret("slug", TypeString, "应用标识（可用于校验拿到的是不是同一个应用）"),
			ret("brand.display_name", TypeString, "展示名称").
				WithNote("未配置时已回落为应用名称，可直接展示"),
			ret("brand.tagline", TypeString, "一句话标语；未配置为空串"),
			ret("brand.intro", TypeString, "简介；未配置为空串"),
			ret("brand.logo_url", TypeString, "图标地址；未配置为空串").
				WithNote("空串不表示加载失败，终端应回落到自己的默认图，不要重试"),
			ret("brand.contact_name", TypeString, "客服/联系人名称；未配置为空串"),
			ret("brand.contact_email", TypeString, "联系邮箱；未配置为空串"),
			ret("brand.support_url", TypeString, "帮助与支持地址；未配置为空串"),
			ret("brand.privacy_url", TypeString, "隐私政策地址；未配置为空串"),
			ret("brand.terms_url", TypeString, "服务条款地址；未配置为空串"),
			ret("maintenance.enabled", TypeBoolean, "是否处于维护模式"),
			ret("maintenance.message", TypeString, "维护提示文案；未配置为空串").
				WithNote("空串时请展示你自己的默认文案，不要把空字符串弹给用户"),
		},
		Errors: []ErrCode{
			errCode("app_not_found", "该标识不存在；标识格式不合法也返回此码（不区分，避免枚举）"),
			errCode("rate_limited", "单 IP 请求过于频繁"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"slug": "default",
			"brand": map[string]any{
				"display_name": "灵动", "tagline": "一句话标语", "intro": "简介",
				"logo_url": "", "contact_name": "客服", "contact_email": "support@example.com",
				"support_url": "", "privacy_url": "", "terms_url": "",
			},
			"maintenance": map[string]any{"enabled": false, "message": ""},
		},
		TrialRisk: RiskSafe,
	},

	"apps.notices": {
		Params: []Param{
			path("slug", "应用标识（终端内置的那个串，不是应用名称）"),
		},
		Returns: []Return{
			ret("notices[].id", TypeInteger, "公告 ID"),
			ret("notices[].title", TypeString, "公告标题"),
			ret("notices[].content", TypeString, "公告正文（纯文本，按原样展示）"),
			ret("notices[].level", TypeString, "级别：`info` / `warn` / `critical`").
				WithNote("只用于决定提示样式；`critical` 应显著区别于 `info`，不要只改颜色"),
			ret("notices[].starts_at", TypeString, "生效开始时间（RFC3339）；`null` 表示立即生效"),
			ret("notices[].ends_at", TypeString, "失效时间（RFC3339）；`null` 表示长期有效"),
		},
		Errors: []ErrCode{
			errCode("app_not_found", "该标识不存在；标识格式不合法也返回此码（不区分，避免枚举）"),
			errCode("rate_limited", "单 IP 请求过于频繁"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"notices": []any{map[string]any{
				"id": 12, "title": "本周末维护", "content": "周六 02:00–04:00 暂停服务",
				"level": "info", "starts_at": nil, "ends_at": nil,
			}},
		},
		TrialRisk: RiskSafe,
	},

	"apps.release": {
		Params: []Param{
			path("slug", "应用标识（终端内置的那个串，不是应用名称）"),
		},
		Returns: []Return{
			ret("release.version", TypeString, "最新已发布版本号；无已发布版本时 `release` 为 `null`"),
			ret("release.release_notes", TypeString, "更新说明"),
			ret("release.download_url", TypeString, "下载地址"),
			ret("release.force_update", TypeBoolean, "是否强制更新").
				WithNote("为 true 时请阻断旧版本继续使用，不要只弹一个可关闭的提示"),
			ret("release.published_at", TypeString, "发布时间（RFC3339）"),
		},
		Errors: []ErrCode{
			errCode("app_not_found", "该标识不存在；标识格式不合法也返回此码（不区分，避免枚举）"),
			errCode("rate_limited", "单 IP 请求过于频繁"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"release": map[string]any{
				"version": "1.4.0", "release_notes": "修复若干问题",
				"download_url": "https://dl.example.com/app-1.4.0.apk",
				"force_update": false, "published_at": "2026-11-01T10:00:00Z",
			},
		},
		TrialRisk: RiskSafe,
	},

	"apps.plugin_records": {
		Params: []Param{
			path("slug", "应用标识（终端内置的那个串，不是应用名称）"),
			path("plugin", "插件标识（manifest 的 `key`）"),
			path("entity", "数据对象标识（manifest 的 `entities[].key`，不是对象名称）"),
			query("page", TypeInteger, ReqOptional, "页码，从 1 开始（默认 1）"),
			query("page_size", TypeInteger, ReqOptional, "每页条数（默认 20，上限 50；越界按边界收敛）"),
		},
		Returns: []Return{
			ret("plugin.key", TypeString, "插件标识"),
			ret("plugin.name", TypeString, "插件名称"),
			ret("plugin.version", TypeString, "目录当前版本（与安装时的版本可能不同）"),
			ret("entity.key", TypeString, "数据对象标识"),
			ret("entity.name", TypeString, "数据对象名称"),
			ret("items[].record_key", TypeString, "记录标识（不透明；只适合当列表 key，不承担查询语义）"),
			ret("items[].data", TypeObject, "只含 `entities[].public` 声明的字段").
				WithNote("字段集合由插件定义，这里不逐列列出；未声明的字段**不会**出现"),
			ret("items[].updated_at", TypeString, "更新时间（`YYYY-MM-DD HH:MM:SS`，服务器本地时区）"),
			ret("total", TypeInteger, "符合条件的总数（用于分页；不是本页条数）"),
			ret("page", TypeInteger, "当前页码"),
			ret("page_size", TypeInteger, "当前每页条数"),
		},
		Errors: []ErrCode{
			errCode("plugin_not_found", "该应用下没有这个插件，或未开放对外读取（不区分，避免枚举）"),
			errCode("plugin_entity_not_found", "插件里没有这个数据对象，或该对象没有声明可公开字段"),
			errCode("rate_limited", "单 IP 请求过于频繁"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"plugin": map[string]any{"key": "ticket", "name": "工单", "version": "1.0.0"},
			"entity": map[string]any{"key": "ticket", "name": "工单"},
			"items": []any{map[string]any{
				"record_key": "01J8Z0K3M4N5P6Q7R8S9T0V1W2",
				"data":       map[string]any{"title": "登录后首页空白", "status": "open"},
				"updated_at": "2026-11-18 10:00:00",
			}},
			"total": 1, "page": 1, "page_size": 20,
		},
		TrialRisk: RiskSafe,
	},

	"apps.list": {
		Returns: []Return{
			ret("items[].app_id", TypeInteger, "应用 ID"),
			ret("items[].slug", TypeString, "应用标识（用于签发托管密钥的路径段）"),
			ret("items[].name", TypeString, "应用名称"),
			ret("items[].key_id", TypeInteger, "当前托管密钥的 ID"),
			ret("items[].key_prefix", TypeString, "密钥前缀（可直接展示，不含秘密）"),
			ret("items[].scopes", TypeArray, "该密钥具备的权限点"),
			ret("items[].created_at", TypeString, "密钥签发时间（RFC3339）"),
			ret("items[].rotated_at", TypeString, "最近一次轮换时间，未轮换为 null"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的凭证"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"items": []any{map[string]any{
				"app_id": 7, "slug": "default", "name": "默认应用",
				"key_id": 42, "key_prefix": "ld-abc123", "scopes": []any{"billing:write"},
			}},
		},
		TrialRisk: RiskSafe,
	},

	"apps.hosted_key": {
		Params: []Param{
			path("slug", "应用标识"),
			body("rotate", TypeBoolean, ReqOptional, "是否换成一把新密钥").
				WithDefault("false").
				WithNote("true 时立即签发新明文；**旧明文默认继续可用 72 小时**（平台可配，见响应 previous_expires_at），应用应在该窗口内完成替换").
				WithExample(false),
			body("name", TypeString, ReqOptional, "密钥备注名").
				WithNote("只在首次签发时生效，轮换不改名").WithExample("生产环境"),
		},
		Returns: []Return{
			ret("app.id", TypeInteger, "应用 ID"),
			ret("app.slug", TypeString, "应用标识"),
			ret("app.name", TypeString, "应用名称"),
			ret("key.id", TypeInteger, "密钥 ID"),
			ret("key.prefix", TypeString, "密钥前缀"),
			ret("key.scopes", TypeArray, "该密钥具备的权限点（由契约派生，不可自定义）"),
			ret("key.created", TypeBoolean, "本次是否**新建**了密钥"),
			ret("key.rotated", TypeBoolean, "本次是否执行了轮换"),
			ret("key.issued_at", TypeString, "密钥签发时间"),
			ret("key.status", TypeInteger, "1 启用 / 2 禁用").
				WithNote("2 有两种成因：`admin_block`（管理员封禁，轮换**不会**解禁）与闲置超期（轮换即恢复）"),
			ret("key.expired_at", TypeString, "有效期").
				WithNote("托管密钥按「闲置过期」判定：活跃使用会自动续期；`null` 表示不过期"),
			ret("key.amount_limit", TypeInteger, "消费上限（微）；0 = 不限制").
				WithNote("顶额且余额充足时会自动上调，硬顶为账户余额"),
			ret("key.used", TypeInteger, "该密钥累计消费（微）"),
			ret("key.rpm_limit", TypeInteger, "每分钟请求上限；0 = 不限制"),
			ret("key.previous_expires_at", TypeString, "上一把明文的最后可用时间（RFC3339）").
				WithNote("轮换后旧钥的重叠窗口终点；`null` = 没有上一代可用（首次签发或窗口为 0）"),
			ret("plaintext", TypeString, "密钥明文").
				WithNote("**空串表示没有新明文**（命中已有密钥且未轮换）。绝不能用空串覆盖你存的凭证"),
		},
		Errors: []ErrCode{
			errCode("app_not_found", "应用标识不存在；或应用已停用且你**尚未**持有该应用的密钥"+
				"（两者合并返回，避免用两次请求枚举平台应用）"),
			errCode("session_required", "本端点只接受登录会话票，API 密钥不可用"),
			errCode("forbidden", "该应用已关闭自助签发（**只挡首次接入**；已持有密钥的轮换不受影响）"),
			errCode("invalid_param", "请求体不是合法 JSON"),
			errCode("key_rotate_busy", "该密钥正在被并发轮换，请稍后重试（响应不含明文）"),
			errCode("key_rotate_blocked", "该钥已被平台封禁，轮换无法解除，需要平台解锁（响应不含明文）"+
				"；**409 而非 401** —— 你的登录会话是有效的，冲突在目标钥的状态上"),
			errCode("service_unavailable", "平台未声明任何写权限点，无法签发（部署配置问题）"),
			errCode("internal_error", "服务端异常"),
		},
		ReqExample: map[string]any{"rotate": false, "name": "生产环境"},
		RespExample: map[string]any{
			"app": map[string]any{"id": 7, "slug": "default", "name": "默认应用"},
			"key": map[string]any{
				"id": 42, "prefix": "ld-abc123", "created": true, "rotated": false,
				"issued_at": "2026-11-03T10:00:00Z", "status": 1,
				"expired_at": "2027-05-02T10:00:00Z", "amount_limit": 100000000,
				"previous_expires_at": nil,
				"used":                0, "rpm_limit": 600,
			},
			"plaintext": "ld-abc123def456",
		},
		SessionWhy: WhyKeyIssue,
	},

	// ==================== 托管密钥自举（仅托管密钥） ====================
	//
	// 这两条的目标行由**鉴权时命中的那把钥**决定，请求里没有任何 id 参数 ——
	// 这是"不存在越权面"的结构性保证。对照 controller/user_key_self.go。

	"keys.self_rotate": {
		Returns: []Return{
			ret("key.id", TypeInteger, "密钥 ID"),
			ret("key.prefix", TypeString, "新的密钥前缀"),
			ret("key.scopes", TypeArray, "该密钥换到的权限点（契约 ∩ 平台策略）").
				WithNote("平台关掉 billing:write 时，轮换后该权限点会被裁掉 —— 配置说不要就是不要"),
			ret("key.status", TypeInteger, "1 启用 / 2 禁用").
				WithNote("只在**可自愈**的禁用（闲置超期）下才会被恢复；管理员封禁直接返回 401 key_disabled，不返回明文"),
			ret("key.expired_at", TypeString, "新的有效期（自本次轮换起重新拉满）"),
			ret("key.previous_expires_at", TypeString, "上一把明文的最后可用时间（RFC3339）").
				WithNote("`null` = 没有上一代可用（窗口配成 0，或自救时原窗口已结束）。" +
					"**多副本部署必须在它到点前把新明文分发到所有实例**"),
			ret("key.amount_limit", TypeInteger, "消费上限（微）；0 = 不限制"),
			ret("key.used", TypeInteger, "该密钥累计消费（微）"),
			ret("key.rpm_limit", TypeInteger, "每分钟请求上限；0 = 不限制"),
			ret("plaintext", TypeString, "新的密钥明文").
				WithNote("**恒非空**（每次轮换都生成新钥）。明文只返回给发起调用的这个实例"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少凭证，或用的是会话票/自建 KEY（本端点只认应用托管密钥）"),
			errCode("scope_denied", "凭证是一把**自建** API KEY，不是托管密钥"),
			errCode("key_disabled", "该钥已被平台封禁，轮换不会解除；请停止重试并联系平台"),
			errCode("key_rotate_busy", "10 分钟内已轮换过，或另一个请求正在并发轮换。**当前钥仍然有效，不要重试**"),
			errCode("invalid_api_key", "该钥已不存在（鉴权后到轮换前被拆除）"),
			errCode("service_unavailable", "平台未声明任何写权限点，无法签发（部署配置问题）"),
			errCode("rate_limited", "超过该钥的 RPM/并发上限，或触发了自举轮换的频率限制"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"key": map[string]any{
				"id": 42, "prefix": "ld-new789", "scopes": []any{"file:write", "task:write"},
				"status": 1, "expired_at": "2027-05-02T10:00:00Z",
				"previous_expires_at": "2026-11-06T22:30:00Z",
				"amount_limit":        100000000, "used": 5000000, "rpm_limit": 600,
			},
			"plaintext": "ld-new789abc",
		},
		// 浏览器无法提供托管密钥（只有会话 Cookie），文档页不提供试运行。
		SessionWhy: WhyHostedKey,
	},

	"keys.health": {
		Returns: []Return{
			ret("server_time", TypeString, "服务器当前时间（RFC3339）").
				WithNote("用它算窗口剩余，不依赖调用方本机时钟"),
			ret("key.id", TypeInteger, "密钥 ID"),
			ret("key.prefix", TypeString, "密钥前缀"),
			ret("key.status", TypeInteger, "1 启用 / 2 禁用").
				WithNote("本端点只验明文、不看状态，所以**禁用/过期的钥也能查**它。这是它与数据端点的分界：它只解释原因，不放行任何调用"),
			ret("key.disabled_reason", TypeString, "禁用成因").
				WithNote("`admin_block` = 平台封禁（轮换不会解除，需联系平台）；" +
					"`expired` = 闲置超期（轮换即可恢复）；空串 = 启用中"),
			ret("key.via_previous", TypeBoolean, "本次是否用**上一代明文**鉴权").
				WithNote("true 且 `previous_expires_in_seconds` 为 `null` = 这一代已退役，必须换成新钥"),
			ret("key.expired_at", TypeString, "有效期；`null` = 不过期"),
			ret("key.expires_in_seconds", TypeInteger, "距有效期末尾的秒数；`null` = 不过期").
				WithNote("服务端算好的相对值（不必处理时钟漂移）。**负值 = 已过期**，不钳到 0"),
			ret("key.previous_expires_at", TypeString, "上一代明文的最后可用时间").
				WithNote("非 `null` 表示**还在窗口期**，须在它到点前完成替换；窗口结束后回 `null`"),
			ret("key.previous_expires_in_seconds", TypeInteger, "窗口剩余秒数；`null` = 不在窗口期"),
			ret("key.scopes", TypeArray, "该密钥具备的权限点"),
			ret("key.allow_ips", TypeArray, "IP 白名单；空数组 = 不限制").
				WithNote("本端点**不校验 IP**，所以被 401 key_ip_denied 拒绝时也能查到这里，确认是不是白名单的问题"),
			ret("key.amount_limit", TypeInteger, "消费上限（微）；0 = 不限制"),
			ret("key.used", TypeInteger, "该密钥累计消费（微）"),
			ret("key.rpm_limit", TypeInteger, "每分钟请求上限；0 = 不限制"),
			ret("key.created_at", TypeString, "签发时间"),
			ret("key.reset_secret_at", TypeString, "最近一次轮换时间；`null` = 从未轮换"),
			ret("account.status", TypeInteger, "账号状态；1 正常 / 2 停用").
				WithNote("为 2 时**所有**调用都会被 403 `forbidden` 拒绝，与本钥无关"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少凭证，或用的是会话票/自建 KEY（本端点只认应用托管密钥）"),
			errCode("scope_denied", "凭证是一把**自建** API KEY，不是托管密钥"),
			errCode("invalid_api_key", "这把明文不对应任何密钥（含已被删除）"),
			errCode("rate_limited", "超过本端点的轮询频率上限"),
			errCode("service_unavailable", "认证依赖（数据库）暂时不可用，退避重试"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"server_time": "2026-11-03T22:30:00Z",
			"key": map[string]any{
				"id": 42, "prefix": "ld-abc123", "status": 1, "disabled_reason": "",
				"via_previous": false,
				"expired_at":   "2027-05-02T10:00:00Z", "expires_in_seconds": 15552000,
				"previous_expires_at": nil, "previous_expires_in_seconds": nil,
				"scopes": []any{"file:write", "task:write"}, "allow_ips": []any{},
				"amount_limit": 100000000, "used": 5000000, "rpm_limit": 600,
				"created_at": "2026-05-02T10:00:00Z", "reset_secret_at": nil,
			},
			"account": map[string]any{"status": 1},
		},
		SessionWhy: WhyHostedKey,
	},

	// ==================== 安全（10 条，仅会话票） ====================
	//
	// 对照：controller/user_api.go（UserLogout / UserChangePassword /
	// UserRevokeSession / UserRevokeAllSessions）、controller/session.go（ListSessions）、
	// controller/mfa.go（MFA 5 条）。

	"security.logout": {
		NoData: true,
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的会话票"),
			errCode("session_required", "API 密钥不能用于本端点"),
		},
		SessionWhy: WhySession,
	},

	"security.password_change": {
		Params: []Param{
			body("old_password", TypeString, ReqRequired, "当前密码").WithExample("Passw0rd!"),
			body("new_password", TypeString, ReqRequired, "新密码").
				WithNote("成功后该账号的**全部**会话失效（含当前），必须重新登录").
				WithExample("NewPassw0rd!"),
		},
		Returns: []Return{
			ret("revoked_all", TypeBoolean, "恒为 true：表示已撤销全部会话").
				WithNote("客户端应把它当作「请重新登录」的信号，而不是可选项"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 old_password 或 new_password"),
			errCode("invalid_credential", "当前密码错误"),
			errCode("weak_password", "新密码强度不足"),
			errCode("session_required", "API 密钥不能改密"),
		},
		ReqExample:  map[string]any{"old_password": "Passw0rd!", "new_password": "NewPassw0rd!"},
		RespExample: map[string]any{"revoked_all": true},
		SessionWhy:  WhyCredential,
	},

	"security.contact_code": {
		Params: []Param{
			body("channel", TypeString, ReqRequired, "联系方式类型").WithEnum("email", "sms"),
			body("target", TypeString, ReqRequired, "新的邮箱或手机号").WithLimit("max_len", 254, "≤ 254"),
		},
		Returns: []Return{
			ret("expires_in", TypeInteger, "验证码有效期（秒）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "channel 或 target 格式不正确"),
			errCode("session_required", "API 密钥不能换绑联系方式"),
			errCode("target_unchanged", "与当前绑定值相同"),
			errCode("bind_method_disabled", "该联系方式未启用"),
			errCode("verify_rate_limited", "发送过于频繁"),
			errCode("verify_unavailable", "发码服务暂不可用"),
		},
		ReqExample:  map[string]any{"channel": "email", "target": "new@example.com"},
		RespExample: map[string]any{"expires_in": 600},
		SessionWhy:  WhyCredential,
	},

	"security.contact_bind": {
		Params: []Param{
			body("channel", TypeString, ReqRequired, "联系方式类型").WithEnum("email", "sms"),
			body("target", TypeString, ReqRequired, "新的邮箱或手机号").WithLimit("max_len", 254, "≤ 254"),
			body("code", TypeString, ReqRequired, "收到的验证码").WithLimit("max_len", 8, "≤ 8"),
		},
		Returns: []Return{
			ret("email", TypeString, "换绑后的邮箱").WithNote("空串表示未绑定"),
			ret("phone", TypeString, "换绑后的手机号").WithNote("空串表示未绑定"),
			ret("revoked_keys", TypeInteger, "被禁用的 API 密钥数量").
				WithNote("换绑会禁用该账号全部密钥（账号接管止血）；大于 0 时请提示用户重建"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "参数缺失或格式不正确"),
			errCode("verify_code_invalid", "验证码错误或已过期"),
			errCode("target_taken", "该联系方式已被其他账号使用"),
			errCode("target_unchanged", "与当前绑定值相同"),
			errCode("session_required", "API 密钥不能换绑联系方式"),
		},
		ReqExample: map[string]any{"channel": "email", "target": "new@example.com", "code": "123456"},
		RespExample: map[string]any{
			"email": "new@example.com", "phone": "", "revoked_keys": 1,
		},
		SessionWhy: WhyCredential,
	},

	"security.sessions_list": {
		Returns: []Return{
			ret("sessions[].id", TypeInteger, "会话 ID（撤销时作为路径参数）"),
			ret("sessions[].current", TypeBoolean, "是否是本次请求所用的会话"),
			ret("sessions[].ip", TypeString, "登录时的出口 IP"),
			ret("sessions[].ua", TypeString, "登录时的 User-Agent"),
			ret("sessions[].created_at", TypeString, "登录时间（RFC3339）"),
			ret("sessions[].last_seen_at", TypeString, "最近一次活动时间"),
			ret("sessions[].expires_at", TypeString, "绝对过期时间"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的会话票"),
			errCode("session_required", "API 密钥不能管理会话"),
		},
		RespExample: map[string]any{
			"sessions": []any{map[string]any{
				"id": 88, "current": true, "ip": "203.0.113.7",
				"ua": "curl/8.4.0", "created_at": "2026-10-28T10:00:00+08:00",
			}},
		},
		SessionWhy: WhySession,
	},

	"security.sessions_revoke_all": {
		Returns: []Return{
			ret("revoked", TypeInteger, "被撤销的会话数量").
				WithNote("包含当前会话；返回后本张票立即失效"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的会话票"),
			errCode("session_required", "API 密钥不能管理会话"),
		},
		RespExample: map[string]any{"revoked": 3},
		SessionWhy:  WhySession,
	},

	"security.sessions_revoke": {
		Params: []Param{
			path("id", "会话 ID（取自设备列表）"),
		},
		NoData: true,
		Errors: []ErrCode{
			errCode("invalid_param", "id 不是正整数"),
			errCode("not_found", "会话不存在、已失效，或不属于当前账号"),
			errCode("session_required", "API 密钥不能管理会话"),
		},
		SessionWhy: WhySession,
	},

	"security.mfa_status": {
		Returns: []Return{
			ret("enabled", TypeBoolean, "是否已开启二次验证"),
			ret("policy", TypeString, "平台策略：是否强制、对哪些角色强制").
				WithEnum("off", "optional", "required_for_admin").
				WithNote("required_for_admin 时管理员访问管理面会被拦到设置页"),
			ret("backup_codes_left", TypeInteger, "剩余可用备份码数量").
				WithNote("接近 0 时应提示重新生成"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的会话票"),
			errCode("session_required", "API 密钥不能查看二次验证状态"),
		},
		RespExample: map[string]any{"enabled": true, "backup_codes_left": 8},
		SessionWhy:  WhyCredential,
	},

	"security.mfa_setup": {
		Params: []Param{
			body("password", TypeString, ReqRequired, "当前账户密码（重认证）").
				WithNote("会话被窃取时，绑定验证器等于把临时劫持变成永久接管"),
		},
		Returns: []Return{
			ret("secret", TypeString, "TOTP 密钥（Base32 明文）").
				WithNote("**只此一次**：之后库里只有密文，任何接口都不会再回显"),
			ret("otpauth_uri", TypeString, "otpauth:// 链接，可直接生成二维码"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的会话票"),
			errCode("invalid_credential", "当前密码错误"),
			errCode("mfa_already_enabled", "二次验证已开启，需先关闭才能重新生成"),
			errCode("session_required", "API 密钥不能操作二次验证"),
		},
		ReqExample: map[string]any{"password": "your-password"},
		RespExample: map[string]any{
			"secret":      "JBSWY3DPEHPK3PXP",
			"otpauth_uri": "otpauth://totp/Example:demo_user?secret=JBSWY3DPEHPK3PXP",
		},
		SessionWhy: WhyCredential,
	},

	"security.mfa_enable": {
		Params: []Param{
			body("code", TypeString, ReqRequired, "用上一步的密钥在验证器里算出的一次性码").
				WithNote("6 位；同一时间步只能消费一次").WithExample("123456"),
			body("password", TypeString, ReqRequired, "当前账户密码（重认证）"),
		},
		Returns: []Return{
			ret("backup_codes", TypeArray, "10 个备份码，每个只能使用一次").
				WithNote("**只此一次**：遗失后只能重新生成，旧码会立即失效"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 code 或 password"),
			errCode("invalid_credential", "当前密码错误"),
			errCode("mfa_code_invalid", "动态码错误"),
			errCode("mfa_already_enabled", "已经开启过"),
			errCode("session_required", "API 密钥不能操作二次验证"),
		},
		ReqExample:  map[string]any{"code": "123456", "password": "your-password"},
		RespExample: map[string]any{"backup_codes": []any{"abcde-fghjk", "lmnop-qrstu"}},
		SessionWhy:  WhyCredential,
	},

	"security.mfa_disable": {
		Params: []Param{
			body("code", TypeString, ReqRequired, "当前动态码或任一未使用的备份码").
				WithExample("123456"),
		},
		NoData: true,
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 code"),
			errCode("mfa_code_invalid", "动态码或备份码错误"),
			errCode("mfa_not_enabled", "当前未开启二次验证"),
			errCode("session_required", "API 密钥不能操作二次验证"),
		},
		ReqExample: map[string]any{"code": "123456"},
		SessionWhy: WhyCredential,
	},

	"security.mfa_backup_codes": {
		Params: []Param{
			body("code", TypeString, ReqRequired, "当前动态码或任一未使用的备份码").
				WithExample("123456"),
		},
		Returns: []Return{
			ret("backup_codes", TypeArray, "10 个新备份码").
				WithNote("**非幂等**：每次调用都会让旧的立即失效，新码只返回这一次"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 code"),
			errCode("mfa_code_invalid", "动态码或备份码错误"),
			errCode("mfa_not_enabled", "当前未开启二次验证"),
			errCode("session_required", "API 密钥不能操作二次验证"),
		},
		ReqExample:  map[string]any{"code": "123456"},
		RespExample: map[string]any{"backup_codes": []any{"k1l2m3n4o5", "p6q7r8s9t0"}},
		SessionWhy:  WhyCredential,
	},

	// ==================== 密钥（7 条，仅会话票） ====================
	//
	// 对照：controller/api_key.go（keyReq / keyItem / parseKeyReq / ownEditableKey）。
	// 注意：这 7 条都对**应用托管密钥**返回 409 hosted_key_managed ——
	// 托管密钥只能由应用自己轮换，门户重置会让应用静默断流。

	"keys.list": {
		Returns: []Return{
			ret("items[].id", TypeInteger, "密钥 ID"),
			ret("items[].key_prefix", TypeString, "密钥前缀（可直接展示）").
				WithNote("列表**永不返回明文**，只给前缀"),
			ret("items[].kind", TypeString, "密钥类型").
				WithEnum("user", "hosted").
				WithNote("hosted 表示由接入应用托管，本组端点对它一律 409"),
			ret("items[].app_id", TypeInteger, "归属应用 ID；0 表示平台直连（门户自建）"),
			ret("items[].scopes", TypeArray, "权限点，只对托管密钥有意义（自建密钥恒为空数组）"),
			ret("items[].name", TypeString, "备注名"),
			ret("items[].group", TypeString, "生效分组（= 账号当前分组）").
				WithNote("密钥不能单独设分组；这里回的是计费侧实际使用的那个值"),
			ret("items[].status", TypeInteger, "状态：1 启用，2 停用"),
			ret("items[].amount_limit", TypeInteger, "额度上限（微）；0 表示不限制"),
			ret("items[].used", TypeInteger, "已消费（微）"),
			ret("items[].models_allowed", TypeArray, "模型白名单；空数组表示不限制"),
			ret("items[].modalities_allowed", TypeArray, "模态白名单；空数组表示不限制"),
			ret("items[].allow_ips", TypeArray, "IP 白名单（CIDR）；空数组表示不限制"),
			ret("items[].rpm_limit", TypeInteger, "每分钟请求上限；0 表示不限制"),
			ret("items[].concurrency_limit", TypeInteger, "并发上限；0 表示不限制"),
			ret("items[].expired_at", TypeString, "过期时间；null 表示永不过期"),
			ret("items[].allow_user_quota_query", TypeBoolean, "是否允许用这把密钥查询账户额度"),
			ret("items[].created_at", TypeString, "创建时间（RFC3339）"),
			ret("items[].reset_secret_at", TypeString, "最近一次重置时间；null 表示从未重置"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的会话票"),
			errCode("session_required", "API 密钥不能列出 API 密钥"),
		},
		RespExample: map[string]any{"items": []any{map[string]any{
			"id": 42, "key_prefix": "ld-abc123", "kind": "user", "app_id": 0,
			"name": "生产环境", "status": 1, "amount_limit": 0, "used": 1250000,
		}}},
		SessionWhy: WhyKeyManage,
	},

	"keys.create": {
		Params: []Param{
			body("name", TypeString, ReqRequired, "备注名").
				WithExample("生产环境").WithLimit("max_len", 30, "≤ 30 字"),
			// 分组已改为**账号**属性：传非空值一律 400 invalid_param。
			// 它同时决定折扣倍率与可用渠道池，能自填=能给自己打折。
			body("group", TypeString, ReqOptional, "分组（已废弃，请勿传）").
				WithNote("分组由账号决定，传非空值返回 invalid_param；生效值见「列出我的 API 密钥」"),
			body("amount_limit", TypeInteger, ReqOptional, "额度上限（**微**，1 单位货币 = 1000000）").
				WithDefault("0").WithNote("0 表示不限制").WithExample(0),
			body("expired_at", TypeString, ReqOptional, "过期日期").
				WithNote("格式 YYYY-MM-DD，服务端按当天 23:59:59 UTC 生效；不传表示永不过期").
				WithExample("2027-12-31").
				WithLimit("pattern", 0, `^\d{4}-\d{2}-\d{2}$`),
			body("models_allowed", TypeArray, ReqOptional, "模型白名单").
				WithNote("空数组表示不限制；传了就只能调这些模型"),
			body("modalities_allowed", TypeArray, ReqOptional, "模态白名单").
				WithEnum(modality.All...).
				WithNote("取值越界直接 400；空数组表示不限制"),
			body("allow_ips", TypeArray, ReqOptional, "IP 白名单（CIDR 或单 IP）").
				WithNote("格式非法直接 400，不会静默丢弃").WithExample([]any{"203.0.113.0/24"}),
			body("rpm_limit", TypeInteger, ReqOptional, "每分钟请求上限").
				WithDefault("0").WithNote("0 表示不限制").WithExample(0),
			body("concurrency_limit", TypeInteger, ReqOptional, "并发上限").
				WithDefault("0").WithNote("0 表示不限制").WithExample(0),
			body("allow_user_quota_query", TypeBoolean, ReqOptional, "是否允许用这把密钥查询账户余额").
				WithDefault("true").WithNote("关掉后 GET /api/user/self 会返回 forbidden").WithExample(true),
		},
		Returns: []Return{
			ret("key", TypeObject, "新密钥的元数据，字段同 keys.list 的 items[]"),
			ret("plaintext", TypeString, "密钥明文").
				WithNote("**只在这条响应里返回一次**，之后只能靠 keys.plaintext 回显"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "name 为空或过长、额度为负、IP 段非法、模态越界、日期格式错"),
			errCode("key_conflict", "连续多次生成的密钥哈希碰撞（极低概率，可重试）"),
			errCode("session_required", "API 密钥不能创建 API 密钥"),
		},
		ReqExample: map[string]any{
			"name": "生产环境", "amount_limit": 0, "rpm_limit": 0,
			"concurrency_limit": 0, "allow_user_quota_query": true,
		},
		RespExample: map[string]any{"plaintext": "ld-abc123def456", "key": map[string]any{"id": 43}},
		SessionWhy:  WhyKeyManage,
	},

	"keys.update": {
		Params: []Param{
			path("id", "密钥 ID"),
			body("name", TypeString, ReqRequired, "备注名（必填：本端点是整体替换）").
				WithExample("生产环境").
				WithLimit("max_len", 30, "≤ 30 字"),
			body("group", TypeString, ReqOptional, "分组（已废弃，请勿传）").
				WithNote("分组由账号决定，传非空值返回 invalid_param"),
			body("amount_limit", TypeInteger, ReqOptional, "额度上限（微）；0 表示不限制").
				WithNote("本端点整体替换：不传会被重置为 0（= 不限制），不是「保持不变」"),
			body("expired_at", TypeString, ReqOptional, "过期日期 YYYY-MM-DD").
				WithNote("不传 = 永不过期"),
			body("models_allowed", TypeArray, ReqOptional, "模型白名单").
				WithNote("不传 = 清空（变成不限制）"),
			body("modalities_allowed", TypeArray, ReqOptional, "模态白名单"),
			body("allow_ips", TypeArray, ReqOptional, "IP 白名单"),
			body("rpm_limit", TypeInteger, ReqOptional, "每分钟请求上限；0 表示不限制"),
			body("concurrency_limit", TypeInteger, ReqOptional, "并发上限；0 表示不限制"),
			body("allow_user_quota_query", TypeBoolean, ReqOptional, "是否允许查询账户余额").
				WithNote("不传 = 恢复默认（允许）"),
		},
		NoData: true,
		Errors: []ErrCode{
			errCode("invalid_param", "字段越界（同 keys.create）"),
			errCode("not_found", "密钥不存在或不属于当前账号"),
			errCode("hosted_key_managed", "该密钥由接入应用托管，请在应用侧轮换"),
			errCode("session_required", "API 密钥不能修改 API 密钥"),
		},
		ReqExample: map[string]any{"name": "生产环境", "amount_limit": 0, "rpm_limit": 60},
		SessionWhy: WhyKeyManage,
	},

	"keys.status": {
		Params: []Param{
			path("id", "密钥 ID"),
			body("status", TypeInteger, ReqRequired, "目标状态").
				WithEnum("1", "2").
				WithNote("1 启用，2 停用；其它值一律 400").WithExample(2),
		},
		NoData: true,
		Errors: []ErrCode{
			errCode("invalid_param", "status 缺失或不等于 1/2"),
			errCode("not_found", "密钥不存在或不属于当前账号"),
			errCode("hosted_key_managed", "该密钥由接入应用托管，停用会让应用立刻断流"),
			errCode("session_required", "API 密钥不能修改 API 密钥"),
		},
		ReqExample: map[string]any{"status": 2},
		SessionWhy: WhyKeyManage,
	},

	"keys.reset": {
		Params: []Param{
			path("id", "密钥 ID"),
		},
		Returns: []Return{
			ret("plaintext", TypeString, "新密钥明文").
				WithNote("**只返回一次**；旧密钥在同一事务里立即失效"),
		},
		Errors: []ErrCode{
			errCode("not_found", "密钥不存在或不属于当前账号"),
			errCode("hosted_key_managed", "该密钥由接入应用托管，重置会让应用立刻 401"),
			errCode("key_conflict", "新密钥哈希碰撞（极低概率，可重试）"),
			errCode("session_required", "API 密钥不能重置 API 密钥"),
		},
		RespExample: map[string]any{"plaintext": "ld-new789xyz"},
		SessionWhy:  WhyKeyManage,
	},

	"keys.plaintext": {
		Params: []Param{
			path("id", "密钥 ID"),
		},
		Returns: []Return{
			ret("plaintext", TypeString, "密钥明文").
				WithNote("只对**本人在门户创建**的密钥有效；托管密钥返回 409"),
		},
		Errors: []ErrCode{
			errCode("not_found", "密钥不存在或不属于当前账号"),
			errCode("hosted_key_managed", "该密钥由接入应用托管，平台不提供回显（丢了只能轮换）"),
			errCode("session_required", "API 密钥不能回显 API 密钥"),
		},
		RespExample: map[string]any{"plaintext": "ld-abc123def456"},
		SessionWhy:  WhyKeyManage,
	},

	"keys.delete": {
		Params: []Param{
			path("id", "密钥 ID"),
		},
		NoData: true,
		Errors: []ErrCode{
			errCode("not_found", "密钥不存在、已删除，或不属于当前账号"),
			errCode("hosted_key_managed", "该密钥由接入应用托管，删除会让应用立刻断流"),
			errCode("session_required", "API 密钥不能删除 API 密钥"),
		},
		SessionWhy: WhyKeyManage,
	},

	// ==================== 用量（4 条） ====================
	//
	// 对照：relay/relay.go（parseUsageFilter / usagePage / PortalUsage /
	// PortalUsageDetail / PortalUsageExport）+ relay/summary.go（PortalUsageSummary）。
	//
	// 三条通路都查过：参数全部来自 handler 内的 c.Query（经 parseUsageFilter 与
	// usagePage 两个 helper），没有 ShouldBindJSON。
	// **全部 strict**：日期或 key_id 非法一律 400，不会静默忽略 ——
	// 静默忽略的表现是"筛了单个 KEY 却返回全部记录"，用户会以为账单错了。

	"usage.list": {
		Params: []Param{
			query("start", TypeString, ReqOptional, "起始时间（RFC3339，含）").
				WithNote("格式非法返回 400，不是静默忽略").WithExample("2026-10-01T00:00:00+08:00"),
			query("end", TypeString, ReqOptional, "结束时间（RFC3339）").
				WithNote("必须不早于 start，否则 400"),
			query("modality", TypeString, ReqOptional, "按模态过滤").
				WithEnum(modality.All...),
			query("status", TypeString, ReqOptional, "按调用状态过滤").
				WithNote("取值由服务端状态机决定，传未知值不会报错（作为普通筛选值处理）"),
			query("model", TypeString, ReqOptional, "按模型名精确过滤").WithExample("gpt-4o-mini"),
			query("key_id", TypeInteger, ReqOptional, "按密钥过滤").
				WithNote("不是正整数返回 400；不传表示全部密钥"),
			query("page", TypeInteger, ReqOptional, "页码，从 1 开始").
				WithDefault("1").WithExample(1).WithLimit("min", 1, "≥ 1"),
			query("page_size", TypeInteger, ReqOptional, "每页条数").
				WithDefault("20").WithExample(20).WithLimit("max", 100, "1-100，超出按 100 截断"),
		},
		Returns: []Return{
			ret("total", TypeInteger, "符合条件的总数"),
			ret("page", TypeInteger, "当前页码"),
			ret("page_size", TypeInteger, "实际生效的每页条数"),
			ret("items[].id", TypeInteger, "调用记录 ID"),
			ret("items[].key_id", TypeInteger, "所用密钥 ID"),
			ret("items[].key_name", TypeString, "所用密钥的备注名").
				WithNote("多把密钥时用它确认「这笔是哪把花的」"),
			ret("items[].created_at", TypeString, "调用时间（RFC3339）"),
			ret("items[].model", TypeString, "模型名"),
			ret("items[].model_icon_url", TypeString, "模型图标的绝对地址").
				WithNote("可能为空（没有配图标）。空值时按首字占位，不要自己拼 `/api/icons/**` 路径"),
			ret("items[].modality", TypeString, "模态"),
			ret("items[].status", TypeString, "调用状态"),
			ret("items[].http_status", TypeInteger, "HTTP 状态码"),
			ret("items[].prompt_tokens", TypeInteger, "输入 token"),
			ret("items[].completion_tokens", TypeInteger, "输出 token"),
			ret("items[].cache_tokens", TypeInteger, "命中缓存的 token"),
			ret("items[].image_tokens", TypeInteger, "图片 token"),
			ret("items[].audio_input_tokens", TypeInteger, "音频输入 token"),
			ret("items[].audio_output_tokens", TypeInteger, "音频输出 token"),
			ret("items[].charge", TypeInteger, "本次扣费（**微**）"),
			ret("items[].latency_ms", TypeInteger, "总耗时（含重试与排队）"),
			ret("items[].service_ms", TypeInteger, "最后一次服务调用耗时"),
			ret("items[].first_token_ms", TypeInteger, "首字延迟"),
			ret("items[].is_stream", TypeBoolean, "是否流式"),
			ret("items[].request_id", TypeString, "请求号（报障用）"),
			ret("items[].error_summary", TypeString, "失败时的分类文案；成功为空"),
			ret("items[].video_seconds", TypeInteger, "视频计费秒数"),
			ret("items[].currency", TypeString, "该条记录的结算币种"),
			ret("modality_dist[].modality", TypeString, "模态"),
			ret("modality_dist[].requests", TypeInteger, "该模态的请求数"),
			ret("modality_dist[].charge", TypeInteger, "该模态的扣费（微）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "start/end 不是 RFC3339、end 早于 start、key_id 非数字"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"total": 128, "page": 1, "page_size": 20,
			"items": []any{map[string]any{
				"id": 9001, "key_id": 42, "key_name": "生产环境", "model": "gpt-4o-mini",
				"modality": "chat", "status": "succeeded", "prompt_tokens": 120,
				"completion_tokens": 340, "charge": 1250, "currency": "CNY",
			}},
		},
		TrialRisk: RiskSafe,
		// Sortable 是**唯一**的白名单来源：handler 据此校验、文档据此渲染
		// 可选值、OpenAPI 据此生成 enum。加字段时只改这里。
		Sortable: []string{"created_at", "latency_ms", "charge"},
	},

	"usage.summary": {
		Params: []Param{
			query("start", TypeString, ReqOptional, "起始时间（RFC3339）").
				WithExample("2026-10-01T00:00:00+08:00"),
			query("end", TypeString, ReqOptional, "结束时间（RFC3339）"),
			query("modality", TypeString, ReqOptional, "按模态过滤").
				WithEnum(modality.All...),
			query("status", TypeString, ReqOptional, "按状态过滤"),
			query("model", TypeString, ReqOptional, "按模型精确过滤"),
			query("key_id", TypeInteger, ReqOptional, "按密钥过滤").
				WithNote("不是正整数返回 400"),
		},
		Returns: []Return{
			ret("items[].model", TypeString, "模型名"),
			ret("items[].requests", TypeInteger, "请求数"),
			ret("items[].prompt_tokens", TypeInteger, "输入 token 合计"),
			ret("items[].completion_tokens", TypeInteger, "输出 token 合计"),
			ret("items[].charge", TypeInteger, "扣费合计（**微**）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "时间或 key_id 格式非法"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{"items": []any{map[string]any{
			"model": "gpt-4o-mini", "requests": 128, "prompt_tokens": 15360,
			"completion_tokens": 43520, "charge": 160000,
		}}},
		TrialRisk: RiskSafe,
	},

	"usage.export": {
		Params: []Param{
			query("start", TypeString, ReqOptional, "起始时间（RFC3339）").
				WithExample("2026-10-01T00:00:00+08:00"),
			query("end", TypeString, ReqOptional, "结束时间（RFC3339）"),
			query("modality", TypeString, ReqOptional, "按模态过滤"),
			query("status", TypeString, ReqOptional, "按状态过滤"),
			query("model", TypeString, ReqOptional, "按模型精确过滤"),
			query("key_id", TypeInteger, ReqOptional, "按密钥过滤"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "时间或 key_id 格式非法"),
			errCode("unauthorized", "缺少或无效的凭证"),
			errCode("rate_limited", "本端点单独限 2 次/分钟（其余读端点共享 600 次/分钟）"),
		},
		TrialRisk: RiskSafe,
		// 成功时是文件流：UTF-8 BOM + text/csv + Content-Disposition。
		// 失败时是 JSON 信封 —— 调用方必须按 Content-Type 分流，不能只看状态码。
		// 因此不声明 Returns（不是 JSON data），也不标 NoData（那不是"null"）。
	},

	"usage.detail": {
		Params: []Param{
			path("id", "调用记录 ID"),
		},
		Returns: append([]Return{
			ret("id", TypeInteger, "记录 ID"),
			ret("key_id", TypeInteger, "所用密钥 ID"),
			ret("key_name", TypeString, "密钥备注名"),
			ret("created_at", TypeString, "调用时间（RFC3339）"),
			ret("model", TypeString, "模型名"),
			ret("model_icon_url", TypeString, "模型图标的绝对地址（可能为空）"),
			ret("modality", TypeString, "模态"),
			ret("status", TypeString, "调用状态"),
			ret("http_status", TypeInteger, "HTTP 状态码"),
			ret("prompt_tokens", TypeInteger, "输入 token"),
			ret("completion_tokens", TypeInteger, "输出 token"),
			ret("cache_tokens", TypeInteger, "缓存命中 token"),
			ret("charge", TypeInteger, "扣费（**微**）"),
			ret("currency", TypeString, "结算币种"),
			ret("latency_ms", TypeInteger, "总耗时"),
			ret("service_ms", TypeInteger, "服务耗时"),
			ret("first_token_ms", TypeInteger, "首字延迟"),
			ret("billing_mode", TypeString, "计费模式").
				WithEnum("tokens", "call", "spec").
				WithNote("决定金额怎么算出来：按 token、按次、按规格档位"),
			ret("image_spec", TypeString, "归一后的计费档位（1K / 2K / 3K / 4K 之一）"),
			ret("image_n", TypeInteger, "出图张数"),
			ret("audio_duration_secs", TypeInteger, "音频时长（秒）"),
			ret("audio_chars", TypeInteger, "音频字符数"),
			ret("video_resolution", TypeString, "视频档位"),
			ret("video_seconds", TypeInteger, "视频计费秒数"),
			ret("frozen", TypeInteger, "本笔的冻结额（微）"),
			ret("call_io", TypeObject, "本次调用的报文明细（未留存时为 null）").
				WithNote("分「请求（你发出的）」与「响应（平台交给你的）」两部分；" +
					"服务商侧报文与其地址永不下发"),
			ret("call_io.req_raw", TypeString, "客户端原始请求体（已脱敏；超过留存上限时只保留前缀）"),
			ret("call_io.req_bytes", TypeInteger, "请求体原始字节数（截断前）"),
			ret("call_io.req_truncated", TypeBoolean, "请求体是否被留存上限截断"),
			ret("call_io.req_source", TypeString, "请求体来源").
				WithEnum("raw", "rebuilt", "snapshot", "none").
				WithNote("snapshot 是任务参数快照，字段顺序可能已丢失；rebuilt 表示提交的文件已转为内联数据"),
			ret("call_io.body_mode", TypeString, "正文口径").
				WithEnum("full", "meta").
				WithNote("meta 表示平台按合规要求只留结构摘要，不含正文"),
			ret("call_io.resp_body", TypeString, "平台返回给客户端的响应体（已脱敏）").
				WithNote("失败调用时这里是平台写出的错误 JSON；流式调用是拼接后的完整结果"),
			ret("call_io.resp_bytes", TypeInteger, "响应体字节数"),
			ret("call_io.sse_frames", TypeInteger, "流式响应的帧数（0 表示非流式）"),
			ret("call_io.output_text", TypeString, "从响应体抽出的纯文本输出（≤8 KiB）").
				WithNote("对话类请求可直接读它；没有文本输出时缺省"),
			ret("call_io.media", TypeArray, "产物清单（图片/视频/音频）").
				WithNote("只给类型、格式、大小与序号，不给服务商地址；字节走 /api/portal/usage/{id}/media/{index}"),
			ret("task", TypeObject, "关联的异步任务，可能为 null").
				WithNote("任务型调用的进度与产物都在这里；同步调用没有这个对象。" +
					"字段与 tasks.detail 是**同一份声明**，可直接复用同一份解析"),
		}, taskReturns("task.")...),
		Errors: []ErrCode{
			errCode("invalid_param", "id 不是正整数"),
			errCode("not_found", "记录不存在或不属于当前账号"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"id": 9001, "model": "gpt-4o-mini", "modality": "chat",
			"status": "succeeded", "charge": 1250, "billing_mode": "tokens",
			"call_io": map[string]any{
				"req_raw":       `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"你好"}]}`,
				"req_bytes":     62,
				"req_truncated": false,
				"req_source":    "raw",
				"body_mode":     "full",
				"resp_body":     `{"choices":[{"message":{"role":"assistant","content":"你好！"}}]}`,
				"resp_bytes":    58,
				"sse_frames":    0,
				"output_text":   "你好！",
				"media":         []any{},
			},
			"task": map[string]any{
				"id":           taskIDExample,
				"object":       "task",
				"model":        "kling-v3",
				"modality":     "video",
				"status":       "succeeded",
				"kind":         "video",
				"progress":     100,
				"created_at":   1762318830,
				"completed_at": 1762318992,
				"error":        nil,
				"usage":        map[string]any{"images": 0, "seconds": 5, "ref_images": 0},
				"contents": []any{map[string]any{
					"index": 0, "url": "https://api.example.com/api/user/tasks/" + taskIDExample + "/contents/0",
					"content_type": "video/mp4", "bytes": 3145728, "kind": "video",
				}},
			},
		},
		TrialRisk: RiskSafe,
	},

	// ==================== 账单（5 条） ====================
	//
	// 对照：controller/order.go（PortalTransactionList / PortalPackages /
	// PortalOrderList）、controller/user_order.go（UserOrderGet / UserCreateOrder）、
	// service/order/portal.go（PortalOrderView / PortalCreateResult / PortalPayOption）。

	"billing.transactions": {
		Params: []Param{
			query("type", TypeString, ReqOptional, "按流水类型过滤").
				WithEnum("freeze", "settle", "release", "recharge", "redeem",
					"refund", "adjust", "invite", "refund_revert").
				WithNote("未知类型返回 400，不是静默忽略"),
			query("page", TypeInteger, ReqOptional, "页码，从 1 开始").WithDefault("1").WithExample(1),
			query("page_size", TypeInteger, ReqOptional, "每页条数").WithDefault("20").WithExample(20),
		},
		Returns: []Return{
			ret("total", TypeInteger, "总条数"),
			ret("page", TypeInteger, "当前页码"),
			ret("page_size", TypeInteger, "每页条数"),
			ret("items[].id", TypeInteger, "流水 ID"),
			ret("items[].type", TypeString, "流水类型").
				WithNote("freeze/settle/release 是任务冻结的三态；recharge 是充值到账"),
			ret("items[].amount", TypeInteger, "变动额（**微**，正为入账、负为出账）"),
			ret("items[].balance_after", TypeInteger, "变动后余额（微）").
				WithNote("用它可以直接拼出对账曲线：本行 balance_after == 上一行 balance_after + 本行 amount"),
			ret("items[].reason", TypeString, "变动原因（人话）"),
			ret("items[].created_at", TypeString, "发生时间（RFC3339）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "type 不在枚举内，或 page/page_size 越界"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"total": 320, "page": 1, "page_size": 20,
			"items": []any{map[string]any{
				"id": 5501, "type": "recharge", "amount": 10000000,
				"balance_after": 22500000, "reason": "充值到账",
			}},
		},
		TrialRisk: RiskSafe,
		// Sortable 是**唯一**的白名单来源：handler 据此校验、文档据此渲染
		// 可选值、OpenAPI 据此生成 enum。加字段时只改这里。
		Sortable: []string{"created_at", "amount"},
	},

	"billing.packages": {
		Returns: []Return{
			ret("items[].id", TypeInteger, "套餐 ID"),
			ret("items[].name", TypeString, "套餐名"),
			ret("items[].price_cent", TypeInteger, "售价（**分**，不是微）").
				WithNote("单位与 credited 不同：price_cent 是分，credited 是微（1 分 = 10000 微）"),
			ret("items[].base", TypeInteger, "基础额度（微）"),
			ret("items[].bonus", TypeInteger, "赠送额度（微）"),
			ret("items[].credited", TypeInteger, "到账总额（微）= base + bonus"),
			ret("items[].badge", TypeString, "角标").
				WithEnum("recommend", "hot", "value").
				WithNote("空串表示不显示；只影响展示，不参与排序或过滤"),
			ret("custom_topup_enabled", TypeBoolean, "是否允许自定义金额充值"),
			ret("min_topup", TypeInteger, "自定义充值的最小金额（微）"),
			ret("balance_limit", TypeInteger, "余额上限（微）；0 表示不限制"),
			ret("scale", TypeInteger, "换算常量：1 单位货币 = scale 微").
				WithNote("金额格式化一律按它换算，不要在客户端写死 1000000"),
			ret("cny_per_usd", TypeInteger, "汇率（分），仅用于展示换算"),
			ret("currency", TypeString, "结算币种代码"),
			ret("currency_symbol", TypeString, "币种符号"),
			ret("pay_options[].method", TypeString, "支付方式代码，下单时原样回传").
				WithNote("取值随平台配置变化，**不要写死**；以本字段为准"),
			ret("pay_options[].label", TypeString, "支付方式中文名（展示用）"),
		},
		Errors: []ErrCode{
			errCode("unauthorized", "缺少或无效的凭证"),
			errCode("internal_error", "服务端异常"),
		},
		RespExample: map[string]any{
			"items": []any{map[string]any{
				"id": 1, "name": "入门包", "price_cent": 1000,
				"base": 10000000, "bonus": 500000, "credited": 10500000,
			}},
			"custom_topup_enabled": true, "min_topup": 1000000,
			"scale": 1000000, "currency": "CNY", "currency_symbol": "¥",
			"pay_options": []any{map[string]any{"method": "alipay", "label": "支付宝"}},
		},
		TrialRisk: RiskSafe,
	},

	"billing.orders_list": {
		Params: []Param{
			query("status", TypeString, ReqOptional, "按订单状态过滤").
				WithEnum("pending", "paid", "refunding", "expired", "refunded", "need_manual").
				WithNote("未知状态返回 400"),
			query("client_order_no", TypeString, ReqOptional, "按调用方幂等号**精确**查询").
				WithNote("最常用的用法：确认「我上次那单到底建成了没有」").
				WithExample("order-20261028-001").
				WithLimit("max_len", 64, "≤ 64"),
			query("page", TypeInteger, ReqOptional, "页码，从 1 开始").WithDefault("1").WithExample(1),
			query("page_size", TypeInteger, ReqOptional, "每页条数").WithDefault("20").WithExample(20),
		},
		Returns: []Return{
			ret("total", TypeInteger, "总条数"),
			ret("page", TypeInteger, "当前页码"),
			ret("page_size", TypeInteger, "每页条数"),
			ret("items[].id", TypeInteger, "订单 ID"),
			ret("items[].out_trade_no", TypeString, "平台订单号"),
			ret("items[].amount_cent", TypeInteger, "支付金额（**分**）"),
			ret("items[].currency", TypeString, "币种"),
			ret("items[].credited", TypeInteger, "到账额度（微）= 售价 × 1e4 + 赠送"),
			ret("items[].status", TypeString, "订单状态").
				WithEnum("pending", "paid", "refunding", "expired", "refunded", "need_manual"),
			ret("items[].pay_method", TypeString, "支付方式代码"),
			ret("items[].pay_method_label", TypeString, "支付方式中文名"),
			ret("items[].package_name", TypeString, "套餐名；自定义金额单为空"),
			ret("items[].expire_at", TypeString, "支付截止时间；过期后需换新幂等号重下"),
			ret("items[].created_at", TypeString, "下单时间"),
			ret("items[].paid_at", TypeString, "到账时间；未支付为 null"),
			ret("items[].refund_status", TypeString, "退款状态；无退款为 null"),
			ret("items[].refund_amount_cent", TypeInteger, "退款金额（分）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "status 不在枚举内、client_order_no 超长、分页越界"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"total": 3, "page": 1, "page_size": 20,
			"items": []any{map[string]any{
				"id": 7001, "out_trade_no": "LD20261028001", "amount_cent": 1000,
				"currency": "CNY", "credited": 10000000, "status": "paid",
				"pay_method": "alipay", "pay_method_label": "支付宝",
			}},
		},
		TrialRisk: RiskSafe,
		// Sortable 是**唯一**的白名单来源：handler 据此校验、文档据此渲染
		// 可选值、OpenAPI 据此生成 enum。加字段时只改这里。
		Sortable: []string{"created_at", "amount"},
	},

	"billing.orders_get": {
		Params: []Param{
			path("id", "订单 ID"),
		},
		Returns: []Return{
			ret("id", TypeInteger, "订单 ID"),
			ret("out_trade_no", TypeString, "平台订单号"),
			ret("amount_cent", TypeInteger, "支付金额（分）"),
			ret("currency", TypeString, "币种"),
			ret("credited", TypeInteger, "到账额度（微）"),
			ret("status", TypeString, "订单状态").
				WithEnum("pending", "paid", "refunding", "expired", "refunded", "need_manual").
				WithNote("**轮询本字段即可**；状态变更的唯一真相源是支付回调"),
			ret("pay_method", TypeString, "支付方式代码"),
			ret("pay_method_label", TypeString, "支付方式中文名"),
			ret("subject", TypeString, "订单标题"),
			ret("package_name", TypeString, "套餐名"),
			ret("expire_at", TypeString, "支付截止时间"),
			ret("created_at", TypeString, "下单时间"),
			ret("paid_at", TypeString, "到账时间"),
			ret("refund_status", TypeString, "退款状态"),
			ret("refund_mode", TypeString, "退款方式"),
			ret("refund_amount_cent", TypeInteger, "退款金额（分）"),
			ret("refund_reason", TypeString, "退款原因"),
			ret("refund_at", TypeString, "申请退款时间"),
			ret("refund_done_at", TypeString, "退款完成时间"),
		},
		Errors: []ErrCode{
			errCode("order_not_found", "订单不存在或不属于当前账号（两者不区分）"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"id": 7001, "out_trade_no": "LD20261028001", "amount_cent": 1000,
			"currency": "CNY", "credited": 10000000, "status": "paid",
		},
		TrialRisk: RiskSafe,
	},

	"billing.orders_qrcode": {
		Params: []Param{
			path("id", "订单 ID"),
		},
		Returns: []Return{
			ret("qr.mode", TypeString, "二维码形态").
				WithEnum("image", "content", "none").
				WithNote("image=直接渲染 data_url；content=用前端二维码库按 content 生成；" +
					"none=该订单没有二维码（跳转式收银台），请改用 cashier_url / pay_url"),
			ret("qr.data_url", TypeString, "二维码图片（data URL，mode=image 时有值）"),
			ret("qr.content", TypeString, "二维码内容原值（mode=content 时有值）"),
			ret("expires_in", TypeInteger, "二维码剩余有效期（秒）"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "订单 ID 非法"),
			errCode("order_not_found", "订单不存在或不属于当前账号（两者不区分）"),
			errCode("pay_qr_unavailable", "二维码暂时无法获取，请稍后重取"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"qr":         map[string]any{"mode": "content", "content": "weixin://wxpay/xxx"},
			"expires_in": 900,
		},
		TrialRisk: RiskSafe,
	},

	"billing.orders_create": {
		Params: []Param{
			body("package_id", TypeInteger, ReqConditional, "充值套餐 ID").
				WithReq("未传 amount 时必填；两者都传时以套餐为准").
				WithExample(1),
			body("amount", TypeString, ReqConditional, "自定义充值金额，十进制文本").
				WithReq("未传 package_id 时必填；需要平台开启自定义充值").
				WithNote("最多两位小数；传数字也会被接受，但请用字符串避免浮点误差").
				WithExample("10.50").
				WithLimit("pattern", 0, `^\d{1,8}(\.\d{1,2})?$`),
			body("method", TypeString, ReqRequired, "支付方式代码").
				WithNote("取值来自 GET /api/user/packages 的 pay_options[].method，**不要写死**").
				WithExample("alipay"),
			// 空串**合法**（见 controller/user_order.go validClientOrderNo）：
			// 写成必填会让接入方以为"不传就 400"，实际不传是"每次新建一笔订单" ——
			// 那个后果比 400 危险得多（重复充值、重复扣款）。
			body("client_order_no", TypeString, ReqOptional, "调用方幂等号（强烈建议传）").
				WithNote("不传 = 没有幂等保护，重复提交会建出多笔订单").
				WithNote("同号重复提交永远返回同一张订单；**不传就没有幂等保护**，"+
					"一次网络重试会真的多出一笔待付单").
				WithExample("order-20261028-001").
				WithLimit("max_len", 64, "1-64，限 [A-Za-z0-9_.:-]").
				WithLimit("pattern", 0, `^[A-Za-z0-9_.:-]{1,64}$`),
		},
		Returns: []Return{
			ret("id", TypeInteger, "订单 ID"),
			ret("out_trade_no", TypeString, "平台订单号"),
			ret("amount_cent", TypeInteger, "支付金额（**分**）"),
			ret("currency", TypeString, "币种"),
			ret("credited", TypeInteger, "到账额度（微）"),
			ret("status", TypeString, "订单当前状态").
				WithEnum("pending", "paid", "expired").
				WithNote("**幂等重放时靠它区分「还没付」与「已经付过了」**；" +
					"paid 就不要再发起支付，expired 请换新号重下"),
			ret("pay_method", TypeString, "支付方式代码"),
			ret("pay_method_label", TypeString, "支付方式中文名"),
			ret("cashier_url", TypeString, "平台收银台页面地址").
				WithNote("可选：把用户导到这个页面也能完成支付"),
			ret("pay_url", TypeString, "支付跳转地址"),
			ret("qr_ref", TypeString, "二维码原值").
				WithNote("请自行渲染二维码，不要把地址转给第三方渲染服务"),
			ret("expires_in", TypeInteger, "支付链接有效期（秒）"),
			ret("auto_redirect", TypeBoolean, "pay_url 是否是必须整页跳转的收银台页").
				WithNote("true 时直接跳转，不要尝试渲染二维码"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "幂等号非法、金额格式错或 ≤ 0、method 缺失"),
			errCode("package_unavailable", "套餐已下架或不属于当前币种"),
			errCode("custom_topup_disabled", "平台未开启自定义金额充值"),
			errCode("amount_below_min", "自定义金额低于起充额"),
			errCode("amount_too_large", "自定义金额超出上限"),
			errCode("balance_limit_exceeded", "充值后会超过余额上限"),
			errCode("topup_in_progress", "已有在途订单占用了余额上限额度"),
			errCode("pay_method_unavailable", "所选支付方式当前不可用"),
			errCode("pay_method_disabled", "所选支付方式已被平台关闭"),
			errCode("pay_not_configured", "平台未配置任何可用支付方式"),
			errCode("pay_gateway_busy", "支付网关繁忙，请用**同一个幂等号**重试"),
			errCode("pay_gateway_error", "支付网关返回错误，请用同一个幂等号重试"),
			errCode("scope_denied", "所用凭证没有 billing:write 权限"),
		},
		ReqExample: map[string]any{
			"package_id": 1, "method": "alipay", "client_order_no": "order-20261028-001",
		},
		RespExample: map[string]any{
			"id": 7002, "out_trade_no": "LD20261028002", "amount_cent": 1000,
			"currency": "CNY", "credited": 10000000, "status": "pending",
			"pay_method": "alipay", "pay_method_label": "支付宝",
			"pay_url": "https://example.com/pay/xxx", "expires_in": 900,
			"auto_redirect": false,
		},
		TrialRisk:     RiskConfirm,
		ScopeEnforced: true,
	},

	"billing.redeems": {
		Params: []Param{
			query("page", TypeInteger, ReqOptional, "页码").WithExample("1"),
			query("page_size", TypeInteger, ReqOptional, "每页条数（≤ 100）").WithExample("20"),
		},
		Returns: []Return{
			ret("items[].id", TypeInteger, "兑换记录 ID"),
			ret("items[].code", TypeString, "兑换码"),
			ret("items[].amount", TypeInteger, "面额（微）"),
			ret("items[].batch", TypeString, "批次"),
			ret("items[].used_at", TypeString, "兑换时间（RFC3339）"),
			ret("items[].expires_at", TypeString, "码的过期时间（可能为空）"),
			ret("total", TypeInteger, "总条数"),
			ret("page", TypeInteger, "当前页码"),
			ret("page_size", TypeInteger, "每页条数"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "分页参数非法"),
			errCode("session_required", "API 密钥不能查看兑换记录"),
		},
		RespExample: map[string]any{
			"items": []any{map[string]any{
				"id": 12, "code": "LD-XXXX", "amount": 10000000, "batch": "202610",
				"used_at": "2026-10-20T10:00:00+08:00", "expires_at": nil,
			}},
			"total": 1, "page": 1, "page_size": 20,
		},
		SessionWhy: WhySession,
	},

	"billing.redeem": {
		Params: []Param{
			body("code", TypeString, ReqRequired, "兑换码").WithExample("LD-XXXX-XXXX"),
		},
		Returns: []Return{
			ret("amount", TypeInteger, "到账额度（**微**）").
				WithNote("入账即生效，可对照 GET /api/user/self 的 balance 与流水 type=redeem 核对"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "缺少 code"),
			errCode("redeem_invalid", "兑换码不存在"),
			errCode("redeem_used", "兑换码已被使用"),
			errCode("redeem_expired", "兑换码已过期"),
			errCode("scope_denied", "自建 API KEY 无此权限，请用会话票或托管密钥"),
		},
		ReqExample:    map[string]any{"code": "LD-XXXX-XXXX"},
		RespExample:   map[string]any{"amount": 10000000},
		TrialRisk:     RiskConfirm,
		ScopeEnforced: true,
	},

	// ==================== 任务（4 条） ====================
	//
	// 对照：controller/portal_task.go（PortalTaskList / PortalTaskGet /
	// PortalTaskCancel / PortalTaskContentAtForAPI）+ service/task/portal.go（PortalItem）。

	"tasks.list": {
		Params: []Param{
			query("status", TypeString, ReqOptional, "按任务状态过滤").
				WithNote("传未知值不会报错（作为普通筛选值处理，结果为空）"),
			query("model", TypeString, ReqOptional, "按模型名精确过滤"),
			query("key_id", TypeInteger, ReqOptional, "按密钥过滤").
				WithNote("不是正整数返回 400"),
			query("page", TypeInteger, ReqOptional, "页码，从 1 开始").WithDefault("1").WithExample(1),
			query("page_size", TypeInteger, ReqOptional, "每页条数").WithDefault("20").WithExample(20),
		},
		Returns: append([]Return{
			ret("total", TypeInteger, "总条数"),
			ret("page", TypeInteger, "当前页码"),
			ret("page_size", TypeInteger, "每页条数"),
		}, portalTaskReturns("items[].")...),
		Errors: []ErrCode{
			errCode("invalid_param", "key_id 非数字，或分页越界"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"total": 12, "page": 1, "page_size": 20,
			"items": []any{map[string]any{
				"id": taskIDExample, "object": "task", "model": "suno-v4", "modality": "music",
				"status": "succeeded", "kind": "music", "progress": 100,
				"created_at": 1762318830, "error": nil, "contents": []any{},
				"usage":   map[string]any{"images": 0, "seconds": 0, "ref_images": 0},
				"frozen":  500000,
				"settled": 500000,
			}},
		},
		TrialRisk: RiskSafe,
		// Sortable 是**唯一**的白名单来源：handler 据此校验、文档据此渲染
		// 可选值、OpenAPI 据此生成 enum。加字段时只改这里。
		Sortable: []string{"created_at"},
	},

	"tasks.detail": {
		Params: []Param{
			path("id", "任务 ID（`task_` 前缀，原样回传）"),
		},
		Returns: portalTaskReturns(""),
		Errors: []ErrCode{
			errCode("invalid_param", "id 不是 `task_` 前缀的合法任务号"),
			errCode("not_found", "任务不存在或不属于当前账号"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"id": taskIDExample, "object": "task", "model": "suno-v4", "modality": "music",
			"status": "succeeded", "kind": "music", "progress": 100,
			"created_at": 1762318830, "completed_at": 1762318992, "error": nil,
			"contents": []any{map[string]any{
				"index": 0, "url": "https://api.example.com/api/user/tasks/" + taskIDExample + "/contents/0",
				"content_type": "audio/mpeg", "bytes": 3145728, "kind": "audio",
			}},
			"usage":   map[string]any{"images": 0, "seconds": 0, "ref_images": 0},
			"frozen":  500000,
			"settled": 500000,
		},
		TrialRisk: RiskSafe,
	},

	"tasks.content": {
		Params: []Param{
			path("id", "任务 ID（`task_` 前缀，原样回传）"),
			path("index", "产物序号，从 0 开始；取自 tasks.detail 的 contents[].index"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "id 或 index 不是整数"),
			errCode("not_found", "任务不存在或不属于当前账号"),
			errCode("task_not_terminal", "任务尚未结束，产物还不存在"),
			errCode("range_not_satisfiable", "Range 头超出产物长度"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		TrialRisk: RiskSafe,
		// 成功时是字节流（真实 MIME + nosniff + 支持 Range 206）；
		// 失败时是 JSON 信封。调用方必须按 Content-Type 分流。
	},

	"tasks.cancel": {
		Params: []Param{
			path("id", "任务 ID（`task_` 前缀，原样回传）"),
		},
		// 取消返回**整个任务对象**（不是 {id,status} 摘要）：客户端不该为
		// "取消成功" 再写一个解析分支。status 恒为 canceled，其余字段原样回显。
		Returns: portalTaskReturns(""),
		Errors: []ErrCode{
			errCode("invalid_param", "id 不是 `task_` 前缀的合法任务号"),
			errCode("not_found", "任务不存在或不属于当前账号"),
			errCode("task_not_cancelable", "任务已开始执行或已结束，无法取消"),
			errCode("scope_denied", "所用凭证没有 task:write 权限"),
		},
		RespExample: map[string]any{
			"id": taskIDExample, "object": "task", "model": "kling-v3", "modality": "video",
			"status": "canceled", "kind": "video", "progress": 100,
			"created_at": 1762318830, "error": nil, "contents": []any{},
			"usage":   map[string]any{"images": 0, "seconds": 5, "ref_images": 0},
			"frozen":  0,
			"settled": 0,
		},
		TrialRisk:     RiskConfirm,
		ScopeEnforced: true,
	},

	// ==================== 文件（2 条） ====================
	//
	// 对照：controller/portal_file.go（PortalFileList / PortalFileDelete）
	// + service/upload（ListFilter / Item / Delete）。
	//
	// 注意 PortalFileDelete 的返回是 `c.Status(204)` —— **真的没有响应体**，
	// 不是 `{"code":0}`。文档若写成 200 有 data，生成的 SDK 会去解一个空 body。

	"files.list": {
		Params: []Param{
			query("key_id", TypeInteger, ReqOptional, "按密钥过滤").
				WithNote("不是正整数返回 400").WithExample(42),
			query("page", TypeInteger, ReqOptional, "页码，从 1 开始").WithDefault("1").WithExample(1),
			query("page_size", TypeInteger, ReqOptional, "每页条数").WithDefault("20").WithExample(20),
		},
		Returns: []Return{
			ret("total", TypeInteger, "总条数"),
			ret("page", TypeInteger, "当前页码"),
			ret("page_size", TypeInteger, "每页条数"),
			ret("items[].id", TypeString, "文件 ID（字符串，不是数字）"),
			ret("items[].filename", TypeString, "原始文件名"),
			ret("items[].safe_name", TypeString, "落盘安全名"),
			ret("items[].size_bytes", TypeInteger, "字节数"),
			ret("items[].content_type", TypeString, "MIME 类型"),
			ret("items[].url", TypeString, "直链").
				WithNote("域名由平台配置决定，不要用响应里的 Host 推断"),
			ret("items[].key_id", TypeInteger, "上传时所用的密钥 ID"),
			ret("items[].key_name", TypeString, "密钥备注名"),
			ret("items[].created_at", TypeString, "上传时间"),
			ret("items[].expires_at", TypeString, "过期时间").
				WithNote("过期后列表里不再出现，删除也按「不存在」处理"),
			ret("items[].remaining_seconds", TypeInteger, "剩余可用秒数"),
		},
		Errors: []ErrCode{
			errCode("invalid_param", "key_id 非数字，或分页越界"),
			errCode("unauthorized", "缺少或无效的凭证"),
		},
		RespExample: map[string]any{
			"total": 2, "page": 1, "page_size": 20,
			"items": []any{map[string]any{
				"id": "f_abc123", "filename": "ref.png", "size_bytes": 20480,
				"content_type": "image/png", "remaining_seconds": 86000,
			}},
		},
		TrialRisk: RiskSafe,
		// Sortable 是**唯一**的白名单来源：handler 据此校验、文档据此渲染
		// 可选值、OpenAPI 据此生成 enum。加字段时只改这里。
		Sortable: []string{"created_at", "size"},
	},

	"files.delete": {
		Params: []Param{
			path("id", "文件 ID（字符串形态）"),
		},
		Errors: []ErrCode{
			errCode("file_not_found", "文件不存在、已过期，或不属于当前账号"),
			errCode("scope_denied", "所用凭证没有 file:write 权限"),
		},
		TrialRisk:     RiskConfirm,
		ScopeEnforced: true,
		// 成功是 **204 无响应体**（不是 `{"code":0}`），失败才是 JSON 信封。
		// 这条由 catalog 的 Media=empty 表达，不声明 Returns、不标 NoData。
	},
}
