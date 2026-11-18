package errs

import (
	"errors"
	"sort"
)

// 错误码常量表（step-00 §6.3，含公共 / 文件域 / 计费域）。
// 统一响应体的 code 字段为 int（0 = 成功），错误时取 Def.HTTP；Code 字符串用于日志与 relay OpenAI 错误体。

const (
	// 公共域
	CodeInvalidParam = "invalid_param"

	// 以下 10 个码此前是 handler 里的**裸字符串**（gateway.Fail(c, 400, "xxx", ...)），
	// 因此既不在 registry 里、也不被 `Get` 认识、更不受「文档码表 ⊆ errs」门禁约束。
	// 表现：`relay/preflight` 拿到这些码当陌生命令、文档里查不到、改名时没人发现。
	// 现在它们和其余错误一样：唯一声明点在这里，调用方只写 gateway.FailDef(...)。
	CodeAsyncStreamConflict        = "async_stream_conflict"
	CodeB64NotSupportedAsync       = "b64_not_supported_async"
	CodeInlineImageNotSupported    = "inline_image_not_supported"
	CodeInstrumentalLyricsConflict = "instrumental_lyrics_conflict"
	CodePromptTooLong              = "prompt_too_long"
	CodeNotImplemented             = "not_implemented"
	CodeUpstreamTimeout            = "gateway_timeout"
	CodeUpstreamSubmitAmbiguous    = "submit_ambiguous"
	CodeUpstreamResultUnknown      = "result_unknown"
	CodeUpstreamBodyError          = "bad_gateway_response"
	CodeEmptyResponse              = "empty_response"
	// CodeMultipartRequired 表示该端点只接受 multipart/form-data 表单。
	//
	// 必须与 CodeInvalidParam 分开：接入方按 OpenAI SDK 的习惯把图片编辑/语音
	// 转写打成了 JSON（文件塞 base64），报「参数不合法」时他唯一能做的动作是
	// 反复检查 JSON 字段——但真正要做的是改成 multipart 表单（或先走
	// /v1/files 上传）。两个码分开后，错误文案能直接指出要改的动作。
	CodeMultipartRequired = "multipart_required"
	CodeUnauthorized      = "unauthorized"
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	// CodeMethodNotAllowed 表示路径存在、方法不对（405）。
	//
	// 必须与 not_found 分开：对 /v1/chat/completions 用 GET 是**配置错误**，
	// 报「地址不存在」会让人去检查 base_url，而真正要改的是请求方法。
	CodeMethodNotAllowed = "method_not_allowed"
	CodeRateLimited      = "rate_limited"
	// CodeRateLimitUnavailable 表示限流**自身**不可用（Redis 挂掉）。
	//
	// 必须与 CodeRateLimited 分开：429 的语义是"你太快了，慢一点再来"，
	// 而这个码的意思是"我这边判不了限流，先别写"。两者的正确动作完全相反
	// —— 让调用方去退避重试 429 的那个策略，会让写请求在故障窗口里反复打进来。
	CodeRateLimitUnavailable = "rate_limit_unavailable"
	CodeInternal             = "internal_error"
	// CodeCSRFRejected 是来源/令牌校验失败。
	//
	// 必须与 CodeForbidden 分开：两者都是 403，但语义完全不同 ——
	// 「来源校验失败」刷新页面就能继续，「禁止访问」是权限不足。
	// 合成一个码时前端只能靠状态码猜，结果把「刷新一下」说成
	// 「账号已被禁用」（这个误导曾让用户以为自己的账号被管理员封了）。
	CodeCSRFRejected = "csrf_rejected"
	// CodeSessionRequired 表示该操作只接受登录会话票，API KEY 不够。
	//
	// 为什么不复用 CodeForbidden：/api/user/* 同时接受两种 Bearer 凭证，
	// 调用方看到 403 必须能分辨「我这把 KEY 权限不够，请改用登录会话」和
	// 「我就是没权限」。合成一个码时客户端只能整体重试，改密/建 KEY 这类
	// 敏感操作永远失败，而错误文案看起来像风控误拦。
	CodeSessionRequired = "session_required"
	// CodeScopeDenied 表示凭证本身有效，但缺少执行该操作所需的权限点。
	//
	// 与 CodeForbidden 分开的理由同 SessionRequired：/api/user/* 上同时存在
	// 「本人自建密钥（只读）」与「应用托管密钥（可写）」两类凭证，403 必须能
	// 让接入方分清「换凭证可以」与「这个账号就没有这个权限」。
	CodeScopeDenied = "scope_denied"
	// CodeScopeUndeclared 表示该路由没有登记对外契约。
	//
	// 这是**守卫自身的兜底错误**，不是业务错误：出现它意味着「路由注册了但契约
	// 没写」。对外文案刻意说成「未开放外部调用」，而不是暴露内部术语，
	// 因为调用方唯一能做的动作就是「不要调这个地址」。
	CodeScopeUndeclared = "scope_undeclared"
	// CodeHostedKeyManaged 表示这把密钥由接入应用托管，用户不能从门户直接操作。
	//
	// 必须与 Forbidden 分开：用户点"重置"时如果只看到"禁止访问"，会以为系统坏了、
	// 反复点；看到"该密钥由应用托管"，才知道要去应用里操作。
	// 这也是**防止应用被静默打挂**的唯一提示：门户重置托管密钥会让应用立刻 401，
	// 而应用侧对此毫无感知（它持有的是旧明文）。
	CodeHostedKeyManaged = "hosted_key_managed"
	// CodeAppNotFound 表示按 slug 找不到应用，**或**应用已停用且调用方尚未持有密钥。
	//
	// 两者合并成同一个码是刻意的：分开会让调用方用两次请求就能判断"这个 slug
	// 存在但被停用了"，而 slug 是管理员手填的可读串（无随机化），枚举成本极低。
	CodeAppNotFound = "app_not_found"
	// CodeAppSlugExists 表示应用标识已被占用（后台新建时的唯一冲突）。
	CodeAppSlugExists = "app_slug_exists"
	// CodeAppMaintenance 表示该应用正处于维护模式，托管密钥的 /v1 调用被拒。
	//
	// 用 503 而不是 403：调用方唯一正确的动作是退避重试（维护会结束），
	// 而 403 会被理解成"权限配置错了"，进而触发人去排查。
	CodeAppMaintenance = "app_maintenance"
	// CodeAppNoticeNotFound 表示该应用下没有这条公告（或不属于该应用）。
	CodeAppNoticeNotFound = "app_notice_not_found"
	// CodeAppReleaseNotFound 表示该应用下没有这个版本。
	CodeAppReleaseNotFound = "app_release_not_found"
	// CodeAppReleaseVersionExists 表示同一应用内版本号已被占用。
	CodeAppReleaseVersionExists = "app_release_version_exists"
	// CodeAppReleaseStateInvalid 表示当前状态不允许这个操作
	// （如已发布版本改版本号、删除已发布版本、把已发布版本退回草稿）。
	CodeAppReleaseStateInvalid = "app_release_state_invalid"

	// 一键更新域（管理台，非对外协议面）。
	//
	// 三个码而不是一个：前端要按 code 决定"该刷新列表"还是"该换个操作"。
	// 具体原因（同版本刚失败/降级/更新程序离线）放在 message 里 —— 这一面只有管理员看。
	CodeReleaseUpdateInProgress = "release_update_in_progress"
	CodeReleaseUpdateBlocked    = "release_update_blocked"
	CodeReleaseNotFound         = "release_not_found"

	// 应用插件（声明式）域。
	//
	// 刻意**不复用**已下线插件系统（2026-10-25）的命名与 URL：那套是"执行第三方代码"，
	// 这里是"解释一份声明"，混用会让人以为执行型插件复活了（见 controller/health_test.go）。
	CodePluginNotFound         = "plugin_not_found"
	CodePluginManifestInvalid  = "plugin_manifest_invalid"
	CodePluginAlreadyInstalled = "plugin_already_installed"
	CodePluginNotInstalled     = "plugin_not_installed"
	CodePluginDisabled         = "plugin_disabled"
	CodePluginEntityNotFound   = "plugin_entity_not_found"
	CodePluginRecordNotFound   = "plugin_record_not_found"
	CodePluginRecordConflict   = "plugin_record_conflict"
	CodePluginVersionConflict  = "plugin_version_conflict"
	CodePluginLimitExceeded    = "plugin_limit_exceeded"
	CodePluginInUse            = "plugin_in_use"

	// 文件域（S8，口径 = v2.0 §4.3.2「文件域」清单）
	CodeFileDisabled        = "file_disabled"         // 总闸 uploads.enabled=false
	CodeFileMissing         = "file_missing"          // 未携带 file 字段
	CodeFileEmpty           = "file_empty"            // 空文件
	CodeFileTooLarge        = "file_too_large"        // 超单文件上限
	CodeFileTypeUnsupported = "file_type_unsupported" // 扩展名/MIME/魔数白名单外
	CodeFileDailyLimit      = "file_daily_limit_exceeded"
	CodeFileStorageFull     = "file_storage_full"
	CodeFileConcurrency     = "file_concurrency_limit" // 并发上传超限（带 Retry-After）
	CodeFileNotFound        = "file_not_found"
	CodeFileExpired         = "file_expired"
	CodeStorageUnavailable  = "storage_unavailable"
	// CodeStorageBackendMissing 表示「这条记录当初写入的存储后端已从配置里消失」。
	//
	// 必须与 storage_unavailable 分开：那一个是**暂时**不可用（退避重试会好），
	// 这一个是**配置问题**（重试一万次也不行，要管理员把后端加回来）。
	// 混成一个码的后果是客户端无限重试、运维看着「503 抖动」去查网络。
	CodeStorageBackendMissing = "storage_backend_missing"
	// CodeStorageNotConfigured 表示平台**没有配置存储位置**（system_config.storage 缺失或 active 为空）。
	//
	// 与 storage_unavailable 分开：那个是后端抖了（重试可能好），这个是
	// 「现在写哪个位置都没定」，重试永远不会好，要管理员去配置页选一个。
	// 它也**不会**回落到本地磁盘 —— 一旦回落，文件会写进容器层并在重建时消失，
	// 而管理台看起来一切正常。
	CodeStorageNotConfigured = "storage_not_configured"

	// 计费域
	CodeModelNotFound     = "model_not_found"
	CodeChannelNameExists = "channel_name_exists" // 渠道名重复（应用层查重，仅管理台）

	// 账号域（S2）
	CodeUsernameTaken      = "username_taken"
	CodeEmailTaken         = "email_taken"
	CodeTargetTaken        = "target_taken"
	CodeInvalidCredential  = "invalid_credential"
	CodeUserDisabled       = "user_disabled"
	CodeWeakPassword       = "weak_password"
	CodeAgreementRequired  = "agreement_required"
	CodeVerifyCodeInvalid  = "verify_code_invalid"
	CodeVerifyRateLimited  = "verify_rate_limited"
	CodeBindMethodDisabled = "bind_method_disabled"
	CodeTargetUnchanged    = "target_unchanged"
	CodeVerifyUnavailable  = "verify_unavailable"

	// 会话与认证准入（登录/注册重构）
	//
	// session_expired 与 unauthorized 刻意分开：前者表示「凭证曾经有效但已失效」
	// （前端直接跳登录、不弹错），后者表示「压根没带凭证」（可能是脚本少带 Cookie）。
	CodeSessionExpired     = "session_expired"
	CodeAuthUnavailable    = "auth_unavailable"
	CodeCaptchaRequired    = "captcha_required"
	CodeCaptchaInvalid     = "captcha_invalid"
	CodeCaptchaUnavailable = "captcha_unavailable"
	CodeRegisterClosed     = "register_closed"
	// CodePortalDisabled 与 register_closed 刻意分开：后者是运营策略（注册模式可改），
	// 前者是部署形态（门户端整体关闭，只有管理员能登录）。
	CodePortalDisabled        = "portal_disabled"
	CodeContactRequired       = "contact_required"
	CodeContactVerifyRequired = "contact_verify_required"
	CodeInviteRequired        = "invite_required"
	CodeResetTicketInvalid    = "reset_ticket_invalid"
	// CodeLastAdminProtected 是管理台专用码：最后一个可用管理员不能被降级/禁用。
	CodeLastAdminProtected = "last_admin_protected"

	// 二次验证（TOTP）。
	//
	// 登录是两个阶段：密码对 + 已开 MFA → **不发 Cookie**，只回一个挑战令牌；
	// 第二阶段的失败统一收口到 mfa_challenge_expired（不区分「过期」「试错超限」，
	// 否则攻击者可以据此判断「这个 challenge 是否存在」）。
	CodeMFAChallengeExpired = "mfa_challenge_expired"
	CodeMFACodeInvalid      = "mfa_code_invalid"
	CodeMFASetupRequired    = "mfa_setup_required"
	CodeMFAAlreadyEnabled   = "mfa_already_enabled"
	CodeMFANotEnabled       = "mfa_not_enabled"

	// 初始口令未修改：只放行改密/会话/MFA 相关路径，其余一律 403。
	CodePasswordChangeRequired = "password_change_required"

	// 密钥与模型目录域（S3）
	CodeKeyConflict = "key_conflict" // 密钥哈希极低概率碰撞
	// CodeIdempotentReplay 表示「这个 Idempotency-Key 已经用于一次相同请求」。
	//
	// 用 409 而不是 200 重放原响应：同步端点可能返回 base64 图片（几 MB），
	// 平台不保存响应体；回放就必须重放一次，那是另一套真相源。
	// 调用方拿到 409 后应当认为"上一次已经受理"，不需要重试。
	CodeIdempotentReplay = "idempotent_replay"

	// ── new-api SUNO 兼容面（`/suno/*`）的七个码 ──
	//
	// 这七个字符串**必须与 new-api 一致**：接入方是照 new-api 的 SUNO 客户端
	// 写的，它们按 `code` 分流（`task_not_found` 去重试提交、`request_conflict`
	// 当作已受理、`authentication_error` 去换 Key）。换成我们自己的词表
	// （`not_found`/`invalid_param`），客户端会全部落进 default 分支。
	//
	// 为什么不直接改 WireOf：那是全站共用的投影，改它会连带改掉 /v1 的码，
	// 而 OpenAI SDK 认的是 /v1 的词表。协议面各用一套码，靠 Def 区分。
	CodeSunoInvalidRequest = "invalid_request"
	CodeSunoAuth           = "authentication_error"
	CodeSunoPermission     = "permission_denied"
	CodeSunoTaskNotFound   = "task_not_found"
	CodeSunoConflict       = "request_conflict"
	CodeSunoRateLimited    = "rate_limit_exceeded"
	CodeSunoServerError    = "server_error"
	// CodeKeyRotateBlocked 表示"要轮换的那把钥被平台封禁，轮换无法解除"。
	//
	// 必须与 CodeKeyDisabled 分开：后者是"**你带来的**凭证被禁用"（HTTP 401），
	// 用来让调用方换凭证；这里调用方带的是会话票且完全有效，出问题的是**目标**
	// 那把钥 —— 用 401 表达会让浏览器端把"目标钥被封"当成"你的登录掉了"，
	// 表现就是点一次轮换被踢回登录页。
	CodeKeyRotateBlocked = "key_rotate_blocked"
	CodeKeyRotateBusy    = "key_rotate_busy"   // 同一把托管密钥正在被并发轮换
	CodeKeyLimitReached  = "key_limit_reached" // 单用户的 KEY 数量上限
	CodeModelExists      = "model_exists"      // 模型 ID 已存在
	CodeModelProtected   = "model_protected"   // 模型被绑定/默认引用，禁止删除
	CodeVendorProtect    = "vendor_protected"  // 内置供应商分组不可删除
	CodeVendorInUse      = "vendor_in_use"     // 供应商分组下仍有模型，禁止删除
	CodeVendorExists     = "vendor_exists"     // 供应商分组名重复
	// CodeIconInvalid 是模型/渠道/供应商三处共用的图标取值错误。
	// 单独给码（而不是复用 invalid_param）是因为处置动作不同：图标错了要重选或重新上传，
	// 价格/名称错了是改字段。
	CodeIconInvalid = "icon_invalid"

	// 渠道与适配器域（S4）
	CodeChannelBound       = "channel_bound"       // 渠道仍被模型绑定，禁止删除
	CodeAdapterUnavailable = "adapter_unavailable" // 适配器未注册/不可用

	// L2 规格域（adapter_specs）
	CodeAdapterSpecExists   = "adapter_spec_exists"   // 规格标识（slug）重复
	CodeAdapterSpecInUse    = "adapter_spec_in_use"   // 仍有渠道引用，禁止删除
	CodeAdapterSpecConflict = "adapter_spec_conflict" // 乐观锁冲突（已被他人修改）
	CodeAdapterSpecInvalid  = "adapter_spec_invalid"  // 规格内容不合法（带字段定位）
	// CodeChannelConflict 是渠道乐观锁冲突（密钥面板与表单并发编辑）。
	// 与 adapter_spec_conflict 分开：前端要按 code 决定"重新拉哪份数据"。
	CodeChannelConflict = "channel_conflict"

	// 网关域（S5，v2.0 §4.3.2 对外错误体 code）
	CodeInvalidAPIKey       = "invalid_api_key"      // KEY 不存在/非法
	CodeKeyExpired          = "key_expired"          // KEY 已过期
	CodeKeyDisabled         = "key_disabled"         // KEY 已被禁用（可区分于不存在/非法）
	CodeKeyRetired          = "key_retired"          // KEY 已被轮换取代（重叠窗口已结束）
	CodeKeyIPDenied         = "key_ip_denied"        // 来源 IP 不在白名单
	CodeInsufficientBalance = "insufficient_balance" // 余额/KEY 额度不足
	CodeModelNotAllowed     = "model_not_allowed"    // 模型不在 KEY 白名单
	CodeModalityNotAllowed  = "modality_not_allowed" // 模态不在 KEY 白名单
	CodeEndpointUnsupported = "endpoint_unsupported" // 端点不受该模型支持
	CodePriceNotConfigured  = "price_not_configured" // 模型未配置价格，拒绝调用
	// 对外错误码只有两档「中性」语义，禁止再按内部原因细分：
	//
	//   model_unavailable   → 这个模型现在用不了（候选为空、上游拒绝、全体失败都算）
	//   service_unavailable → 我方或链路上有东西坏了（配置错、探测失败、上游 5xx）
	//
	// 细分成 channel_unavailable / no_available_channel / all_channels_failed 的直接后果，
	// 是把「有没有渠道、渠道挂没挂、挂了几条」写进用户可见的错误码里 —— 等同于泄漏运营拓扑。
	// 内部归因仍然完整：具体原因进 usages.error_msg、call_io 与日志，管理台照旧可查。
	CodeModelUnavailable   = "model_unavailable"
	CodeServiceUnavailable = "service_unavailable"
	CodeInvalidTier        = "invalid_tier" // 模型价目与本请求不匹配（端点/档位/引擎上限）
	// CodeRequestQuotaExceeded 表示单次请求预估金额超出上限（预扣前拒绝，避免冻结不合理额度）。
	CodeRequestQuotaExceeded = "request_quota_exceeded"
	// CodeRequestTooLarge 表示网关请求体超过上限（413，禁止截断后转发）。
	CodeRequestTooLarge = "request_too_large"

	// 异步任务域（S7，v2.0 §4.7）
	CodeTaskNotCancelable = "task_not_cancelable" // 任务已提交上游或已终结，无法取消
	CodeTaskNotRetryable  = "task_not_retryable"  // 任务非 failed/timeout，无法重试（幂等 409）
	CodeReferenceInvalid  = "reference_invalid"   // 素材链接非法（SSRF/协议/格式）
	// CodeFileGone 表示任务产物已不可用（410）。
	//
	// 与 CodeFileExpired 的区别只有"谁让它消失"：file_expired 是**链接/签名过期**
	// （产物可能还在），file_gone 是**产物本身被上游清理或主动删除**。
	// 两者对外都是 410，客户端动作一致（重新生成），但排障时能区分原因。
	CodeFileGone = "file_gone"
	// CodeArtifactNotReady 表示任务还在跑（或从未成功），产物还不存在（409）。
	//
	// 必须与 410 分开：409 的意思是「还没好，等会儿再来查」，客户端应当继续轮询；
	// 410 是「永久没了，别再试」。合成 410 时客户端会在任务刚提交时就把产物
	// 判成已失效，于是放弃轮询 —— 而 30 秒后产物根本会出来。
	CodeArtifactNotReady = "artifact_not_ready"
	// CodeArtifactNotFound 表示该 key 不在产物清单里（404）。
	//
	// 与 not_found（任务不存在）分开：任务在、清单也在，是这个序号/别名写错了
	// （或该任务只有 1 个产物而客户端按多产物枚举）。文案要能指出「去看清单」。
	CodeArtifactNotFound = "artifact_not_found"
	// CodeTaskNotTerminal 表示对未终结（queued/running）任务执行物理删除被拒（B7）。
	//
	// 必须是独立错误码而不是复用 task_not_cancelable：运营看到「不可取消」会以为该去点取消，
	// 实际上任务是「还没跑完，现在不能删」，处置动作完全不同。
	CodeTaskNotTerminal = "task_not_terminal"
	// CodeTaskPurgeFailed 表示产物对象删除失败、任务行被保留待重试（B7）。
	CodeTaskPurgeFailed = "task_purge_failed"
	// CodeRangeNotSatisfiable 表示 Range 请求的起始位置超出对象长度（416）。
	CodeRangeNotSatisfiable = "range_not_satisfiable"

	// 钱包与支付域（S9，v2.0 §4.3.10 / §4.8）
	CodeCustomTopupDisabled  = "custom_topup_disabled"  // 自定义充值总闸关闭
	CodeAmountBelowMin       = "amount_below_min"       // 低于 min_topup
	CodeBalanceLimitExceeded = "balance_limit_exceeded" // 入账后超出 balance_limit
	CodeOrderNotFound        = "order_not_found"
	CodeOrderNotPaid         = "order_not_paid"      // 非 paid 订单不可退款（409）
	CodeOrderRefunded        = "order_refunded"      // 订单已退款，禁止重复提交（409）
	CodeOrderRefunding       = "order_refunding"     // 退款处理中，禁止重复提交（409）
	CodeRefundUnsupported    = "refund_unsupported"  // 该协议未接入原路退款（400）
	CodeRefundModeInvalid    = "refund_mode_invalid" // 该状态不允许原路退款（400）
	CodeRefundNotPending     = "refund_not_pending"  // 订单没有待确认的退款（409）
	// CodeRefundNoPaymentID 缺原路退款所需的支付凭据（Stripe 的 payment_intent）。
	// 历史订单 / 回调未带 pi_ 时必然缺 —— 不能拿 session id 硬发，那是一次必败的退款。
	CodeRefundNoPaymentID = "refund_no_payment_id" // 缺原路退款凭据（400）
	// CodeRefundEvidenceRequired 确认「退款失败」时必须提供上游核实凭证（400）。
	CodeRefundEvidenceRequired = "refund_evidence_required"
	CodeTopupInProgress        = "topup_in_progress" // 下单并发保护（429）
	CodePayMethodDisabled      = "pay_method_disabled"
	// CodePayMethodUnavailable 渠道还在、但用户选的那个支付方式没有可用渠道。
	// 与 pay_channel_disabled 分开：前者让用户「换一个方式」，后者只能「联系管理员」。
	CodePayMethodUnavailable = "pay_method_unavailable"
	CodePaySignInvalid       = "pay_sign_invalid"
	// 充值套餐域（后台配置 + 门户下单）：
	//   PackageUnavailable 套餐存在但已停用（门户点下单时命中，与「记录不存在」区分开：
	//                     「资源不存在」会让用户以为站点坏了并反复重试）
	//   PackageNameExists  套餐重名（应用层查重 + 唯一索引 23505 兜底）
	//   PackageInUse       有订单引用时禁止删除，只能停用（订单无套餐名快照列，行没了无法追账）
	CodePackageUnavailable = "package_unavailable"
	CodePackageNameExists  = "package_name_exists"
	CodePackageInUse       = "package_in_use"
	// 支付网关交互（虎皮椒接入新增）：
	//   PayGatewayError  网关不可用/返回不可信（502，可重试但只能转查单）
	//   PayGatewayBusy   网关限频/繁忙（429，稍后重试）
	//   PayOrderExists   上游已有同号订单（409，应转查单而不是重建）
	//   PayNotConfigured 渠道参数缺失或回调地址不可用（400，是配置问题，要告警给管理员）
	//   OrderNotPayable  订单不可再发起支付（409，已终态或已过期）
	//   PayQRUnavailable 二维码不可用（410，过期或上游返回异常）
	//   AmountTooLarge   单笔金额超上限（400，防 float 溢出与不合理大额）
	CodePayGatewayError    = "pay_gateway_error"
	CodePayGatewayBusy     = "pay_gateway_busy"
	CodePayOrderExists     = "pay_order_exists"
	CodePayNotConfigured   = "pay_not_configured"
	CodeOrderNotPayable    = "order_not_payable"
	CodePayQRUnavailable   = "pay_qr_unavailable"
	CodeAmountTooLarge     = "amount_too_large"
	CodeRedeemInvalid      = "redeem_invalid"
	CodeRedeemUsed         = "redeem_used"
	CodeRedeemExpired      = "redeem_expired"
	CodeRedeemCodeConflict = "redeem_code_conflict" // 批量生成时单码冲突重试耗尽（S16）
	CodeAffBelowMin        = "aff_below_min"
	CodeAffNothing         = "aff_nothing_to_transfer"

	// 系统配置与通知域（S10）
	CodeNotifyChannelDisabled = "notify_channel_disabled" // 通知通道未启用/未配置
	CodeNotifySendFailed      = "notify_send_failed"      // 通知发送失败

	// AI 编排域（agent）
	//
	// 全部只出现在 /api/admin/* 下，不进对外错误体。
	// 为什么提案类错误要分成五个码而不是一个「提案不可用」：
	// 运营看到之后的**处置动作完全不同** —— hash 不匹配要刷新页面、stale 要重新生成、
	// not_applyable 是状态已终态（别点了）、drift 要去手工核对、plan_invalid 要改需求。
	// 合成一个码的结果是运营只能反复点同一个按钮。
	CodeAgentDisabled         = "agent_disabled"              // 总闸关闭
	CodeAgentBusy             = "agent_busy"                  // 该会话上一轮未结束
	CodeAgentPlanInvalid      = "agent_plan_invalid"          // 提案内容不合法（带字段定位）
	CodeAgentProposalNotFound = "agent_proposal_not_found"    // 提案不存在
	CodeAgentProposalConflict = "agent_proposal_conflict"     // plan_hash 与存档不一致
	CodeAgentProposalState    = "agent_proposal_state"        // 当前状态不可 apply/reject
	CodeAgentOutOfScope       = "agent_proposal_out_of_scope" // 提案含当前入口不允许的实体
	CodeAgentProposalStale    = "agent_proposal_stale"        // 基于旧数据生成，需重新生成
	CodeAgentProposalDrift    = "agent_proposal_drift"        // 应用后又被改动，无法自动回滚
	CodeAgentBudgetExceeded   = "agent_budget_exceeded"       // 今日成本预算已用尽
	CodeAgentDocTooLarge      = "agent_doc_too_large"         // 文档超过单份上限
	CodeAgentDocEmpty         = "agent_doc_empty"             // 文档内容为空
	CodeAgentDocBinary        = "agent_doc_binary"            // 内容看起来是二进制，读不出正文
	CodeAgentAskExpired       = "agent_ask_expired"           // 提问所属的那一轮已结束
	CodeAgentAskAnswered      = "agent_ask_answered"          // 该提问已经回答过
	CodeAgentAwaitingAnswer   = "agent_awaiting_answer"       // 上一轮正在等回答
	// 需要先做连通性探测：状态可修复（探一次就好），所以是 409 而不是 400，
	// 且必须带 targets（缺哪个地址、为什么）—— 只给一句话的后果是运营反复点同一个按钮。
	CodeAgentProbeRequired = "agent_probe_required"

	// 管理台专用补漏（2026-10-29 错误码收口）
	//
	// 这三条原先走 response.Fail（不写 error_code），前端只能按中文文案分流：
	//   - ask_invalid：AI 提问的回答不在选项内（原 400 + 自由文案）
	//   - dryrun_failed：试运行连通性探测失败（原 400 + err.Error()）
	CodeAgentAskInvalid   = "agent_ask_invalid"
	CodeAgentDryRunFailed = "agent_dryrun_failed"
	// CodeAdapterNotRegistered 与 CodeAdapterUnavailable(503) 分开：
	// 前者是**请求里的 adapter_id 不存在**（400，调用方能自己改对），
	// 后者是注册表整体不可用（503，调用方只能等）。合成一个会让管理台
	// 把"填错适配器"显示成"服务暂时不可用"。
	CodeAdapterNotRegistered = "adapter_not_registered"

	// 文档工作台（v7）。三条码对应三种**处置动作不同**的失败：
	//   - doc_version_conflict：别人先保存了，动作 = 刷新后重做（保存会被丢弃）
	//   - doc_publish_blocked：内容自己不合格，动作 = 按 problems 逐条改
	//   - doc_key_exists：身份（key）被占用，动作 = 换一个 key
	// 合成一个 400 的话，前端只能全部提示「保存失败」，而三条的下一步完全不同。
	CodeDocVersionConflict = "doc_version_conflict"
	CodeDocPublishBlocked  = "doc_publish_blocked"
	CodeDocKeyExists       = "doc_key_exists"
)

// Def 是一个错误码的完整定义。
type Def struct {
	Code    string
	HTTP    int
	Message string
}

func (d Def) Error() string { return d.Message }

var (
	InvalidParam = Def{CodeInvalidParam, 400, "参数不合法"}
	// ── 协议面/网关域：文案即对外文案（原先是散在 handler 里的字符串）──
	AsyncStreamConflict = Def{CodeAsyncStreamConflict, 400,
		"async 与 stream 互斥：任务模式不返回逐帧结果，去掉其中一个"}
	B64NotSupportedAsync = Def{CodeB64NotSupportedAsync, 400,
		"异步出图不支持 response_format=b64_json：产物只能通过 /v1/tasks/{id}/artifacts 取回"}
	InlineImageNotSupported = Def{CodeInlineImageNotSupported, 400,
		"image / mask_url 不支持 data: 内联，请先用 /v1/files 上传后改用公网直链"}
	InstrumentalLyricsConflict = Def{CodeInstrumentalLyricsConflict, 400,
		"instrumental=true（纯音乐）与 lyrics 互斥，请二选一"}
	PromptTooLong = Def{CodePromptTooLong, 400, "歌词 prompt 过长（最多 2000 字符）"}
	// NotImplemented 与 endpoint_not_implemented 的区别：前者是「这条能力我们不提供」
	// （协议差异，例如 /v1/messages 的 count_tokens），后者是「这个地址我们没有」。
	// 两者都必须是 501 —— 客户端的降级分支按 501 判断「不支持」，404 会让它以为路径写错。
	NotImplemented = Def{CodeNotImplemented, 501, "该能力暂未实现"}
	// 文案与码都**不许含 "上游"/"upstream"**：门户泄漏门禁（portal_leak_test）
	// 会把它们判成供应链信息 —— 用户看到"上游超时"就知道我们背后在转卖别家的服务。
	// 中性说法一样能表达"不是你的参数问题、也不扣费"。
	UpstreamTimeout         = Def{CodeUpstreamTimeout, 504, "请求处理超时，本次未扣费，请重试"}
	UpstreamSubmitAmbiguous = Def{CodeUpstreamSubmitAmbiguous, 502,
		"请求可能已被受理但响应异常，任务尚未创建，请勿直接重试以免重复计费"}
	// UpstreamResultUnknown：请求**已经提交过**，但等结果时超时/断流 —— 结果未知，
	// 且极可能已经产生了费用。
	//
	// 它不能是 504：504 在主流 SDK（OpenAI / Anthropic）的重试白名单里，自动重试
	// 会再提交一次、再计一次费，用户拿到两份产物、平台付两次成本。也不能是 502
	// （同样在重试白名单里）。409 是「这个请求可能已经被应用」的标准语义，
	// 客户端据此应当去查结果而不是重发；再叠一个 X-Should-Retry: false 双保险。
	UpstreamResultUnknown = Def{CodeUpstreamResultUnknown, 409,
		"请求可能已生成结果，请勿直接重试（会产生第二次费用），请先到调用记录/产物列表确认"}
	UpstreamBodyError = Def{CodeUpstreamBodyError, 502, "服务响应异常，本次未扣费"}
	EmptyResponse     = Def{CodeEmptyResponse, 502, "未收到有效响应，本次未扣费，请重试"}
	// MultipartRequired 的处置动作是「换请求形态」而不是「改字段」：
	// 见 CodeMultipartRequired 注释。错误文案直接给出两条可行路径。
	MultipartRequired = Def{CodeMultipartRequired, 400,
		"该端点只接受 multipart/form-data 表单：文件请走表单字段提交，或先用 /v1/files 上传后以 URL 引用"}
	Unauthorized = Def{CodeUnauthorized, 401, "未授权"}
	Forbidden    = Def{CodeForbidden, 403, "禁止访问"}
	CSRFRejected = Def{CodeCSRFRejected, 403, "请求来源校验失败，请刷新页面后重试"}
	// SessionRequired 必须与 Forbidden 分开：前者是「换一种凭证再来」，
	// 后者是「你没有这个权限」。见 CodeSessionRequired 的注释。
	SessionRequired = Def{CodeSessionRequired, 403, "该操作需要登录会话，API KEY 不能执行"}
	ScopeDenied     = Def{CodeScopeDenied, 403, "该凭证没有此操作的权限"}
	ScopeUndeclared = Def{CodeScopeUndeclared, 403, "该接口未开放外部调用"}
	// HostedKeyManaged 用 409 而不是 403：请求本身合法、身份也合法，
	// 只是对象当前的状态不允许这个操作（同"资源冲突"语义）。
	HostedKeyManaged        = Def{CodeHostedKeyManaged, 409, "该密钥由应用托管，请在应用设置中轮换"}
	AppNotFound             = Def{CodeAppNotFound, 404, "应用不存在"}
	AppSlugExists           = Def{CodeAppSlugExists, 409, "应用标识已被占用"}
	AppMaintenance          = Def{CodeAppMaintenance, 503, "该应用正在维护中，请稍后重试"}
	AppNoticeNotFound       = Def{CodeAppNoticeNotFound, 404, "公告不存在"}
	AppReleaseNotFound      = Def{CodeAppReleaseNotFound, 404, "版本不存在"}
	AppReleaseVersionExists = Def{CodeAppReleaseVersionExists, 409, "该版本号已存在"}
	AppReleaseStateInvalid  = Def{CodeAppReleaseStateInvalid, 409, "当前版本状态下不允许该操作"}

	ReleaseUpdateInProgress = Def{CodeReleaseUpdateInProgress, 409, "已有更新正在进行中"}
	ReleaseUpdateBlocked    = Def{CodeReleaseUpdateBlocked, 409, "本次更新被拒绝"}
	ReleaseNotFound         = Def{CodeReleaseNotFound, 404, "更新任务不存在"}

	PluginNotFound         = Def{CodePluginNotFound, 404, "插件不存在"}
	PluginManifestInvalid  = Def{CodePluginManifestInvalid, 422, "插件定义不合法"}
	PluginAlreadyInstalled = Def{CodePluginAlreadyInstalled, 409, "该应用已安装此插件"}
	PluginNotInstalled     = Def{CodePluginNotInstalled, 404, "该应用未安装此插件"}
	PluginDisabled         = Def{CodePluginDisabled, 409, "该插件当前不可用"}
	PluginEntityNotFound   = Def{CodePluginEntityNotFound, 404, "插件里没有这个数据对象"}
	PluginRecordNotFound   = Def{CodePluginRecordNotFound, 404, "记录不存在"}
	PluginRecordConflict   = Def{CodePluginRecordConflict, 409, "该值已被占用"}
	PluginVersionConflict  = Def{CodePluginVersionConflict, 409, "内容已被他人修改，请刷新后重试"}
	PluginLimitExceeded    = Def{CodePluginLimitExceeded, 400, "超出插件容量上限"}
	PluginInUse            = Def{CodePluginInUse, 409, "仍有应用在使用该插件"}

	NotFound         = Def{CodeNotFound, 404, "资源不存在"}
	MethodNotAllowed = Def{CodeMethodNotAllowed, 405, "请求方法不被允许"}
	RateLimited      = Def{CodeRateLimited, 429, "请求过于频繁"}
	// RateLimitUnavailable 只用于**写面**（动钱/动凭证/建资源）。
	//
	// 读面在 Redis 故障时仍然放行：读是幂等的，放行的代价是"这几十秒没有限速"，
	// 而拦下来的代价是整个门户白屏。写面相反 —— 放行意味着可以无限兑换/下单/发码。
	RateLimitUnavailable = Def{CodeRateLimitUnavailable, 503, "限流服务暂不可用，请稍后重试"}
	Internal             = Def{CodeInternal, 500, "服务器内部错误"}

	FileDisabled        = Def{CodeFileDisabled, 403, "文件上传已关闭"}
	FileMissing         = Def{CodeFileMissing, 400, "缺少上传文件"}
	FileEmpty           = Def{CodeFileEmpty, 400, "文件内容为空"}
	FileTooLarge        = Def{CodeFileTooLarge, 413, "文件超过单文件上限"}
	FileTypeUnsupported = Def{CodeFileTypeUnsupported, 415, "不支持的文件类型"}
	FileDailyLimit      = Def{CodeFileDailyLimit, 429, "今日上传次数已达上限"}
	FileStorageFull     = Def{CodeFileStorageFull, 429, "在存容量已达上限"}
	FileConcurrency     = Def{CodeFileConcurrency, 429, "并发上传数已达上限"}
	FileNotFound        = Def{CodeFileNotFound, 404, "文件不存在"}
	FileExpired         = Def{CodeFileExpired, 410, "文件已过期"}
	// StorageUnavailable 用 503 而不是 502：502 的语义是"上游网关给了我一个坏响应"，
	// 而这里是**我方**对象存储/磁盘暂时不可用，属于"稍后重试可能成功"。
	// 更实际的区别在客户端与网关：502 通常被重试策略当成"别重试"，503 才会退避重试；
	// 项目铁律也明确要求存储层错误必须是 503。
	StorageUnavailable = Def{CodeStorageUnavailable, 503, "存储服务不可用"}
	// StorageBackendMissing 仍是 503（对象还在桶里，补配置就能读），
	// **绝不能折成 410**：410 的语义是「永久删除，别再来找」，会让用户去重建文件。
	StorageBackendMissing = Def{CodeStorageBackendMissing, 503, "文件所在的存储后端已不可用，请联系管理员"}

	// StorageNotConfigured 是写路径的拒绝：平台还没选存储位置。
	StorageNotConfigured = Def{CodeStorageNotConfigured, 503, "平台尚未配置存储位置，暂时无法接收文件，请联系管理员"}

	ModelNotFound     = Def{CodeModelNotFound, 404, "模型不存在"}
	ChannelNameExists = Def{CodeChannelNameExists, 409, "渠道名已存在，请换一个名称"}

	UsernameTaken      = Def{CodeUsernameTaken, 409, "用户名已被占用"}
	EmailTaken         = Def{CodeEmailTaken, 409, "邮箱已被占用"}
	TargetTaken        = Def{CodeTargetTaken, 409, "该邮箱或手机号已被其他账号绑定"}
	InvalidCredential  = Def{CodeInvalidCredential, 401, "用户名或密码错误"}
	UserDisabled       = Def{CodeUserDisabled, 403, "账号已被禁用"}
	WeakPassword       = Def{CodeWeakPassword, 400, "密码强度不足"}
	AgreementRequired  = Def{CodeAgreementRequired, 400, "请先同意用户协议"}
	VerifyCodeInvalid  = Def{CodeVerifyCodeInvalid, 400, "验证码无效或已过期"}
	VerifyRateLimited  = Def{CodeVerifyRateLimited, 429, "验证码发送过于频繁"}
	BindMethodDisabled = Def{CodeBindMethodDisabled, 400, "该绑定方式未启用"}
	TargetUnchanged    = Def{CodeTargetUnchanged, 400, "目标与当前绑定一致"}
	VerifyUnavailable  = Def{CodeVerifyUnavailable, 503, "验证码服务暂不可用"}

	// 会话与认证准入。
	SessionExpired        = Def{CodeSessionExpired, 401, "登录已过期，请重新登录"}
	AuthUnavailable       = Def{CodeAuthUnavailable, 503, "认证服务暂不可用，请稍后重试"}
	CaptchaRequired       = Def{CodeCaptchaRequired, 400, "请先完成人机验证"}
	CaptchaInvalid        = Def{CodeCaptchaInvalid, 400, "人机验证未通过，请重试"}
	CaptchaUnavailable    = Def{CodeCaptchaUnavailable, 503, "人机验证服务暂不可用"}
	RegisterClosed        = Def{CodeRegisterClosed, 403, "当前未开放注册"}
	PortalDisabled        = Def{CodePortalDisabled, 403, "门户端已关闭，仅管理员可登录"}
	ContactRequired       = Def{CodeContactRequired, 400, "账号未绑定邮箱或手机号，请联系管理员重置密码"}
	ContactVerifyRequired = Def{CodeContactVerifyRequired, 400, "请先完成邮箱或手机号验证"}
	InviteRequired        = Def{CodeInviteRequired, 403, "注册需要有效邀请码"}
	ResetTicketInvalid    = Def{CodeResetTicketInvalid, 400, "重置凭据无效或已过期，请重新获取验证码"}
	LastAdminProtected    = Def{CodeLastAdminProtected, 400, "这是最后一个可用的管理员，不能降级或禁用"}

	MFAChallengeExpired = Def{CodeMFAChallengeExpired, 400, "验证已超时或尝试次数过多，请重新登录"}
	MFACodeInvalid      = Def{CodeMFACodeInvalid, 400, "动态码或备份码不正确"}
	// MFASetupRequired 是管理台专用码：策略要求管理员必须绑定二次验证。
	// 前端收到它直接跳绑定页，而不是弹一个「禁止访问」的死胡同。
	MFASetupRequired  = Def{CodeMFASetupRequired, 403, "管理员必须完成二次验证绑定"}
	MFAAlreadyEnabled = Def{CodeMFAAlreadyEnabled, 409, "二次验证已开启，如需重置请先关闭"}
	MFANotEnabled     = Def{CodeMFANotEnabled, 400, "尚未开启二次验证"}

	// PasswordChangeRequired 与 MFASetupRequired 同构：前端据此跳改密页，
	// 而不是把用户丢在一个"什么都点不动"的死胡同里。
	PasswordChangeRequired = Def{CodePasswordChangeRequired, 403, "请先修改初始口令"}

	KeyConflict = Def{CodeKeyConflict, 409, "密钥生成冲突，请重试"}
	// IdempotentReplay：同一个幂等键的重复提交（不是错误，是"已受理过"）。
	IdempotentReplay = Def{CodeIdempotentReplay, 409, "该 Idempotency-Key 已用过，本次未重复扣费"}

	// SUNO 兼容面（`/suno/*`）的七条出口。文案是**占位**：控制器会带上具体原因，
	// 这里只保证「码与状态码」这一层是对的。
	SunoInvalidRequest = Def{CodeSunoInvalidRequest, 400, "请求参数不合法"}
	SunoAuth           = Def{CodeSunoAuth, 401, "鉴权失败"}
	SunoPermission     = Def{CodeSunoPermission, 403, "没有权限"}
	SunoTaskNotFound   = Def{CodeSunoTaskNotFound, 404, "任务不存在"}
	SunoConflict       = Def{CodeSunoConflict, 409, "请求冲突"}
	SunoRateLimited    = Def{CodeSunoRateLimited, 429, "请求过于频繁"}
	SunoServerError    = Def{CodeSunoServerError, 500, "服务端异常"}
	// KeyRotateBusy：并发轮换的保护。必须能重试，且**不能**返回任何明文 ——
	// 返回一把已被覆盖的钥等于给调用方一个必然 401 的配置。
	KeyRotateBusy = Def{CodeKeyRotateBusy, 409, "该密钥正在被轮换，请稍后重试"}
	// KeyRotateBlocked 是"目标钥被平台封禁"：409 而不是 401，因为调用方
	// 带来的凭证（会话票）是有效的，冲突在资源状态上。语义上是**不可重试**的，
	// 文案必须直接指向人工通道，否则应用会反复重试一个永远不会成功的动作。
	KeyRotateBlocked = Def{CodeKeyRotateBlocked, 409, "该密钥已被平台封禁，轮换无法解除，请联系平台"}
	KeyLimitReached  = Def{CodeKeyLimitReached, 409, "API KEY 数量已达上限，请先删除不再使用的密钥"}
	ModelExists      = Def{CodeModelExists, 409, "模型 ID 已存在"}
	ModelProtected   = Def{CodeModelProtected, 400, "该模型被渠道绑定或作为类型默认模型引用，请先解除引用"}
	VendorProtect    = Def{CodeVendorProtect, 400, "内置供应商分组不可删除"}
	VendorInUse      = Def{CodeVendorInUse, 400, "该分组下仍有模型，请先迁移后再删除"}
	VendorExists     = Def{CodeVendorExists, 409, "供应商分组名已存在"}
	IconInvalid      = Def{CodeIconInvalid, 400, "图标不合法，请从内置图标中选择或重新上传"}

	ChannelBound       = Def{CodeChannelBound, 400, "该渠道仍绑定模型，请先解除绑定后再删除"}
	AdapterUnavailable = Def{CodeAdapterUnavailable, 503, "适配器不可用"}

	// 规格（L2）：exists/in_use/conflict 都是 409 ——
	// 语义都是「当前状态与请求冲突，改一下参数就能成功」，不是客户端写错了字段（那是 400）。
	AdapterSpecExists   = Def{CodeAdapterSpecExists, 409, "规格标识已存在，请换一个"}
	AdapterSpecInUse    = Def{CodeAdapterSpecInUse, 409, "仍有渠道使用该规格，请先调整这些渠道"}
	AdapterSpecConflict = Def{CodeAdapterSpecConflict, 409, "规格已被他人修改，请刷新后重试"}
	AdapterSpecInvalid  = Def{CodeAdapterSpecInvalid, 400, "规格内容不合法"}
	ChannelConflict     = Def{CodeChannelConflict, 409, "渠道已被他人修改，请刷新后重试"}

	InvalidAPIKey = Def{CodeInvalidAPIKey, 401, "API KEY 无效"}
	KeyExpired    = Def{CodeKeyExpired, 401, "API KEY 已过期"}
	KeyDisabled   = Def{CodeKeyDisabled, 401, "API KEY 已被禁用"}
	// KeyRetired 与 InvalidAPIKey 分开：前者是"该换钥了"（有序运维动作），
	// 后者是"这个值根本不存在"（配置写错）。混在一起会让接入方排查方向完全跑偏。
	KeyRetired          = Def{CodeKeyRetired, 401, "API KEY 已被轮换，请更新为新密钥"}
	KeyIPDenied         = Def{CodeKeyIPDenied, 401, "来源 IP 不在白名单"}
	InsufficientBalance = Def{CodeInsufficientBalance, 402, "额度不足"}
	ModelNotAllowed     = Def{CodeModelNotAllowed, 403, "该模型不在 API KEY 白名单"}
	ModalityNotAllowed  = Def{CodeModalityNotAllowed, 403, "该模态不在 API KEY 白名单"}
	EndpointUnsupported = Def{CodeEndpointUnsupported, 400, "该模型不支持此接口"}
	PriceNotConfigured  = Def{CodePriceNotConfigured, 400, "模型未配置价格，拒绝调用"}
	// ModelUnavailable / ServiceUnavailable 是对外唯一的两档网关故障语义（见错误码常量的注释）。
	// 两者都 503：用户能做的事只有「换个模型」或「稍后重试」，状态码不必再细分。
	ModelUnavailable     = Def{CodeModelUnavailable, 503, "该模型暂不可用，请更换模型或稍后重试"}
	ServiceUnavailable   = Def{CodeServiceUnavailable, 503, "服务暂时不可用，本次未扣费，请稍后重试"}
	InvalidTier          = Def{CodeInvalidTier, 400, "该模型在此接口上没有可用价目（或张数超出引擎上限），请联系管理员检查计费配置"}
	RequestQuotaExceeded = Def{CodeRequestQuotaExceeded, 400, "单次请求金额超出上限"}
	RequestTooLarge      = Def{CodeRequestTooLarge, 413, "请求体超过上限"}

	TaskNotCancelable = Def{CodeTaskNotCancelable, 409, "任务已开始处理或已结束，无法取消"}
	TaskNotRetryable  = Def{CodeTaskNotRetryable, 409, "任务当前状态不可重试（仅失败/超时可重试）"}
	ReferenceInvalid  = Def{CodeReferenceInvalid, 400, "素材链接不合法"}
	FileGone          = Def{CodeFileGone, 410, "任务产物已不可用"}
	// ArtifactNotReady 对应 409：任务还没产出该产物（继续轮询即可）。
	ArtifactNotReady = Def{CodeArtifactNotReady, 409, "任务尚未产出产物，请稍后再取"}
	// ArtifactNotFound 对应 404：清单里没有这个 key（先取清单再按 key 下载）。
	ArtifactNotFound = Def{CodeArtifactNotFound, 404, "产物序号不存在，请先获取产物清单"}
	TaskNotTerminal  = Def{CodeTaskNotTerminal, 409, "任务尚未终结，不可物理删除"}
	TaskPurgeFailed  = Def{CodeTaskPurgeFailed, 502, "任务产物删除失败，请稍后重试"}
	// RangeNotSatisfiable 对应 HTTP 416：Range 起点超出对象长度（播放器会据此回退到 0）。
	RangeNotSatisfiable = Def{CodeRangeNotSatisfiable, 416, "请求的字节范围不可满足"}

	CustomTopupDisabled    = Def{CodeCustomTopupDisabled, 400, "自定义金额充值已关闭"}
	AmountBelowMin         = Def{CodeAmountBelowMin, 400, "充值金额低于最小限额"}
	BalanceLimitExceeded   = Def{CodeBalanceLimitExceeded, 400, "入账后超出账户额度上限"}
	OrderNotFound          = Def{CodeOrderNotFound, 404, "订单不存在"}
	OrderNotPaid           = Def{CodeOrderNotPaid, 409, "仅已支付订单可退款"}
	OrderRefunded          = Def{CodeOrderRefunded, 409, "订单已退款，请勿重复提交"}
	OrderRefunding         = Def{CodeOrderRefunding, 409, "退款正在处理中，请等待结果或稍后刷新"}
	RefundUnsupported      = Def{CodeRefundUnsupported, 400, "该支付方式不支持原路退款，请选择「仅回退平台余额」"}
	RefundModeInvalid      = Def{CodeRefundModeInvalid, 400, "该订单状态不支持原路退款，请改选「仅回退平台余额」"}
	RefundNotPending       = Def{CodeRefundNotPending, 409, "该订单没有待确认的退款"}
	RefundNoPaymentID      = Def{CodeRefundNoPaymentID, 400, "该订单缺少原路退款所需的支付凭据，请选择「仅回退平台余额」或到支付渠道后台退款"}
	RefundEvidenceRequired = Def{CodeRefundEvidenceRequired, 400, "确认退款失败必须填写上游核实凭证（网关后台单号/工单号），否则无法证明钱没有退出去"}
	TopupInProgress        = Def{CodeTopupInProgress, 429, "上一笔充值正在处理，请稍后再试"}
	PayMethodDisabled      = Def{CodePayMethodDisabled, 400, "当前无可用的支付方式"}
	PayMethodUnavailable   = Def{CodePayMethodUnavailable, 400, "所选支付方式暂不可用，请重新选择"}
	PaySignInvalid         = Def{CodePaySignInvalid, 400, "支付回调验签失败"}
	PackageUnavailable     = Def{CodePackageUnavailable, 409, "该套餐已下架，请刷新页面后重新选择"}
	PackageNameExists      = Def{CodePackageNameExists, 409, "套餐名称已存在，请换一个名称"}
	PackageInUse           = Def{CodePackageInUse, 409, "该套餐已被订单引用，无法删除；请改为「停用」"}
	PayGatewayError        = Def{CodePayGatewayError, 502, "支付网关暂不可用，请稍后重试"}
	PayGatewayBusy         = Def{CodePayGatewayBusy, 429, "支付网关繁忙，请稍后重试"}
	PayOrderExists         = Def{CodePayOrderExists, 409, "该订单已创建，请点击「核对支付」获取支付信息"}
	PayNotConfigured       = Def{CodePayNotConfigured, 400, "支付方式配置有误，请联系管理员"}
	OrderNotPayable        = Def{CodeOrderNotPayable, 409, "订单不可再发起支付（已支付或已过期）"}
	PayQRUnavailable       = Def{CodePayQRUnavailable, 410, "支付二维码已失效，请刷新后重试"}
	AmountTooLarge         = Def{CodeAmountTooLarge, 400, "单笔充值金额超出上限"}
	RedeemInvalid          = Def{CodeRedeemInvalid, 400, "兑换码不存在"}
	RedeemUsed             = Def{CodeRedeemUsed, 400, "兑换码已被使用"}
	RedeemExpired          = Def{CodeRedeemExpired, 400, "兑换码已过期"}
	RedeemCodeConflict     = Def{CodeRedeemCodeConflict, 409, "兑换码生成冲突，请重试"}
	AffBelowMin            = Def{CodeAffBelowMin, 400, "邀请奖励未达最低转出额度"}
	// AffNothing 与 AffBelowMin 分开：前者是「没有可转出的钱」，
	// 后者是「有但没到门槛」。混用会让用户在返利为 0 时看到「未达最低转出额度」，
	// 误以为平台扣了他的钱。
	AffNothing = Def{CodeAffNothing, 400, "暂无可转出的邀请奖励"}

	NotifyChannelDisabled = Def{CodeNotifyChannelDisabled, 400, "通知通道未启用或未配置"}
	NotifySendFailed      = Def{CodeNotifySendFailed, 502, "通知发送失败"}

	AgentDisabled         = Def{CodeAgentDisabled, 403, "AI 编排功能未启用"}
	AgentBusy             = Def{CodeAgentBusy, 409, "该会话正在处理上一条消息，请稍候"}
	AgentPlanInvalid      = Def{CodeAgentPlanInvalid, 400, "提案内容不合法"}
	AgentProposalNotFound = Def{CodeAgentProposalNotFound, 404, "提案不存在"}
	AgentProposalConflict = Def{CodeAgentProposalConflict, 409, "提案内容与确认时不一致，请刷新后重试"}
	AgentProposalState    = Def{CodeAgentProposalState, 409, "该提案当前状态不可执行此操作"}
	// AgentOutOfScope 用于「新建适配器」这类受限入口：提案里出现了该入口不允许的实体。
	// 必须是独立码而不是复用 state：前端要给出不同的引导（去助手页处理），
	// 复用 state 会让用户以为"提案过期了，重新生成一次就好"。
	AgentOutOfScope     = Def{CodeAgentOutOfScope, 409, "该提案包含当前入口不允许的变更，请到 AI 接入助手页处理"}
	AgentProposalStale  = Def{CodeAgentProposalStale, 409, "提案基于旧数据生成，请重新生成"}
	AgentProposalDrift  = Def{CodeAgentProposalDrift, 409, "提案应用后相关配置又被修改，无法自动回滚"}
	AgentBudgetExceeded = Def{CodeAgentBudgetExceeded, 429, "今日 AI 编排成本预算已用完"}
	AgentDocTooLarge    = Def{CodeAgentDocTooLarge, 413, "文档内容超出单份上限"}
	AgentDocEmpty       = Def{CodeAgentDocEmpty, 400, "文档内容为空"}
	AgentDocBinary      = Def{CodeAgentDocBinary, 415, "这份内容读不出正文（看起来是二进制文件）"}
	AgentAskExpired     = Def{CodeAgentAskExpired, 410, "这个问题所属的一轮已结束，请重新发送"}
	AgentAskAnswered    = Def{CodeAgentAskAnswered, 409, "这个问题已经回答过了"}
	AgentAwaitingAnswer = Def{CodeAgentAwaitingAnswer, 409, "上一轮还在等你回答问题"}
	AgentProbeRequired  = Def{CodeAgentProbeRequired, 409, "该提案会新建渠道或改上游地址，必须先做一次连通性探测"}

	// 管理台补漏（原先只回 message、不回 error_code）
	AgentAskInvalid      = Def{CodeAgentAskInvalid, 400, "回答内容不合法（选项不在本题范围内）"}
	AgentDryRunFailed    = Def{CodeAgentDryRunFailed, 400, "连通性探测失败"}
	AdapterNotRegistered = Def{CodeAdapterNotRegistered, 400, "适配器不存在或未注册"}
	DocVersionConflict   = Def{CodeDocVersionConflict, 409, "这篇文档刚被其他人保存过，请刷新后再改"}
	// DocPublishBlocked 用 422 而不是 400：请求本身完全合法（JSON 形状对、字段都在），
	// 只是**内容不满足发布条件**，且响应用的 `data.problems` 逐条说明改哪里。
	// 用 400 会与「你传错了参数」混在一个分支里，前端就没法把问题列表渲染成清单。
	DocPublishBlocked = Def{CodeDocPublishBlocked, 422, "文档体检未通过，不能发布"}
	DocKeyExists      = Def{CodeDocKeyExists, 409, "这个标识已被占用，请换一个"}
)

var registry = map[string]Def{
	InvalidParam.Code: InvalidParam, MultipartRequired.Code: MultipartRequired,
	Unauthorized.Code: Unauthorized, Forbidden.Code: Forbidden,
	CSRFRejected.Code:            CSRFRejected,
	SessionRequired.Code:         SessionRequired,
	ScopeDenied.Code:             ScopeDenied,
	ScopeUndeclared.Code:         ScopeUndeclared,
	HostedKeyManaged.Code:        HostedKeyManaged,
	AppNotFound.Code:             AppNotFound,
	AppSlugExists.Code:           AppSlugExists,
	AppMaintenance.Code:          AppMaintenance,
	AppNoticeNotFound.Code:       AppNoticeNotFound,
	AppReleaseNotFound.Code:      AppReleaseNotFound,
	AppReleaseVersionExists.Code: AppReleaseVersionExists,
	AppReleaseStateInvalid.Code:  AppReleaseStateInvalid,
	ReleaseUpdateInProgress.Code: ReleaseUpdateInProgress,
	ReleaseUpdateBlocked.Code:    ReleaseUpdateBlocked,
	ReleaseNotFound.Code:         ReleaseNotFound,
	PluginNotFound.Code:         PluginNotFound,
	PluginManifestInvalid.Code:  PluginManifestInvalid,
	PluginAlreadyInstalled.Code: PluginAlreadyInstalled,
	PluginNotInstalled.Code:     PluginNotInstalled,
	PluginDisabled.Code:         PluginDisabled,
	PluginEntityNotFound.Code:   PluginEntityNotFound,
	PluginRecordNotFound.Code:   PluginRecordNotFound,
	PluginRecordConflict.Code:   PluginRecordConflict,
	PluginVersionConflict.Code:  PluginVersionConflict,
	PluginLimitExceeded.Code:    PluginLimitExceeded,
	PluginInUse.Code:            PluginInUse,
	NotFound.Code:                NotFound,
	MethodNotAllowed.Code:        MethodNotAllowed,
	RateLimited.Code:             RateLimited,
	RateLimitUnavailable.Code:    RateLimitUnavailable,
	Internal.Code:                Internal,
	FileDisabled.Code:            FileDisabled, FileMissing.Code: FileMissing, FileEmpty.Code: FileEmpty,
	FileTooLarge.Code: FileTooLarge, FileTypeUnsupported.Code: FileTypeUnsupported,
	FileDailyLimit.Code: FileDailyLimit, FileStorageFull.Code: FileStorageFull,
	FileConcurrency.Code: FileConcurrency, FileNotFound.Code: FileNotFound,
	FileExpired.Code: FileExpired, StorageUnavailable.Code: StorageUnavailable,
	StorageBackendMissing.Code: StorageBackendMissing,
	StorageNotConfigured.Code:  StorageNotConfigured,
	ModelNotFound.Code: ModelNotFound,
	ChannelNameExists.Code: ChannelNameExists,

	UsernameTaken.Code: UsernameTaken, EmailTaken.Code: EmailTaken, TargetTaken.Code: TargetTaken,
	InvalidCredential.Code: InvalidCredential, UserDisabled.Code: UserDisabled,
	WeakPassword.Code: WeakPassword, AgreementRequired.Code: AgreementRequired,
	VerifyCodeInvalid.Code: VerifyCodeInvalid, VerifyRateLimited.Code: VerifyRateLimited,
	BindMethodDisabled.Code: BindMethodDisabled, TargetUnchanged.Code: TargetUnchanged,
	VerifyUnavailable.Code: VerifyUnavailable,

	KeyConflict.Code: KeyConflict, KeyLimitReached.Code: KeyLimitReached,
	// 幂等重放：409 + 首次记录号，调用方据此判断"已受理"。
	IdempotentReplay.Code: IdempotentReplay,
	// SUNO 兼容面七码
	SunoInvalidRequest.Code: SunoInvalidRequest,
	SunoAuth.Code:           SunoAuth,
	SunoPermission.Code:     SunoPermission,
	SunoTaskNotFound.Code:   SunoTaskNotFound,
	SunoConflict.Code:       SunoConflict,
	SunoRateLimited.Code:    SunoRateLimited,
	SunoServerError.Code:    SunoServerError,
	KeyRotateBusy.Code:    KeyRotateBusy, KeyRotateBlocked.Code: KeyRotateBlocked,
	ModelExists.Code: ModelExists, ModelProtected.Code: ModelProtected,
	VendorProtect.Code: VendorProtect, VendorInUse.Code: VendorInUse, VendorExists.Code: VendorExists,
	IconInvalid.Code: IconInvalid,

	ChannelBound.Code: ChannelBound,
	AdapterUnavailable.Code: AdapterUnavailable,

	AdapterSpecExists.Code: AdapterSpecExists, AdapterSpecInUse.Code: AdapterSpecInUse,

	AgentDisabled.Code: AgentDisabled, AgentBusy.Code: AgentBusy,
	AgentPlanInvalid.Code: AgentPlanInvalid, AgentProposalNotFound.Code: AgentProposalNotFound,
	AgentProposalConflict.Code: AgentProposalConflict, AgentProposalState.Code: AgentProposalState,
	AgentProposalStale.Code: AgentProposalStale, AgentProposalDrift.Code: AgentProposalDrift,
	AgentOutOfScope.Code:     AgentOutOfScope,
	AgentBudgetExceeded.Code: AgentBudgetExceeded, AgentDocTooLarge.Code: AgentDocTooLarge,
	AgentDocEmpty.Code:        AgentDocEmpty,
	AgentDocBinary.Code:       AgentDocBinary,
	AgentAskExpired.Code:      AgentAskExpired,
	AgentAskAnswered.Code:     AgentAskAnswered,
	AgentAwaitingAnswer.Code:  AgentAwaitingAnswer,
	AgentProbeRequired.Code:   AgentProbeRequired,
	AgentAskInvalid.Code:      AgentAskInvalid,
	AgentDryRunFailed.Code:    AgentDryRunFailed,
	DocVersionConflict.Code:   DocVersionConflict,
	DocPublishBlocked.Code:    DocPublishBlocked,
	DocKeyExists.Code:         DocKeyExists,
	AdapterNotRegistered.Code: AdapterNotRegistered,
	AdapterSpecConflict.Code:  AdapterSpecConflict, AdapterSpecInvalid.Code: AdapterSpecInvalid,
	ChannelConflict.Code: ChannelConflict,

	InvalidAPIKey.Code: InvalidAPIKey, KeyExpired.Code: KeyExpired, KeyDisabled.Code: KeyDisabled,
	KeyRetired.Code:          KeyRetired,
	KeyIPDenied.Code:         KeyIPDenied,
	InsufficientBalance.Code: InsufficientBalance, ModelNotAllowed.Code: ModelNotAllowed,
	ModalityNotAllowed.Code: ModalityNotAllowed, EndpointUnsupported.Code: EndpointUnsupported,
	PriceNotConfigured.Code: PriceNotConfigured,
	ModelUnavailable.Code:   ModelUnavailable, ServiceUnavailable.Code: ServiceUnavailable,
	InvalidTier.Code: InvalidTier, RequestQuotaExceeded.Code: RequestQuotaExceeded,
	RequestTooLarge.Code: RequestTooLarge,

	TaskNotCancelable.Code: TaskNotCancelable, TaskNotRetryable.Code: TaskNotRetryable,
	ReferenceInvalid.Code: ReferenceInvalid, FileGone.Code: FileGone,
	ArtifactNotReady.Code: ArtifactNotReady, ArtifactNotFound.Code: ArtifactNotFound,
	TaskNotTerminal.Code: TaskNotTerminal, TaskPurgeFailed.Code: TaskPurgeFailed,
	RangeNotSatisfiable.Code: RangeNotSatisfiable,

	CustomTopupDisabled.Code: CustomTopupDisabled, AmountBelowMin.Code: AmountBelowMin,
	BalanceLimitExceeded.Code: BalanceLimitExceeded, OrderNotFound.Code: OrderNotFound,
	OrderNotPaid.Code: OrderNotPaid, OrderRefunded.Code: OrderRefunded,
	OrderRefunding.Code:    OrderRefunding,
	RefundUnsupported.Code: RefundUnsupported, RefundModeInvalid.Code: RefundModeInvalid,
	RefundNotPending.Code: RefundNotPending, RefundNoPaymentID.Code: RefundNoPaymentID,
	RefundEvidenceRequired.Code: RefundEvidenceRequired,
	TopupInProgress.Code:        TopupInProgress, PayMethodDisabled.Code: PayMethodDisabled,
	PayMethodUnavailable.Code: PayMethodUnavailable,
	PaySignInvalid.Code:       PaySignInvalid, RedeemInvalid.Code: RedeemInvalid,
	PackageUnavailable.Code: PackageUnavailable, PackageNameExists.Code: PackageNameExists,
	PackageInUse.Code:    PackageInUse,
	PayGatewayError.Code: PayGatewayError, PayGatewayBusy.Code: PayGatewayBusy,
	PayOrderExists.Code: PayOrderExists, PayNotConfigured.Code: PayNotConfigured,
	OrderNotPayable.Code: OrderNotPayable, PayQRUnavailable.Code: PayQRUnavailable,
	AmountTooLarge.Code: AmountTooLarge,
	RedeemUsed.Code:     RedeemUsed, RedeemExpired.Code: RedeemExpired,
	RedeemCodeConflict.Code: RedeemCodeConflict,
	AffBelowMin.Code:        AffBelowMin,
	AffNothing.Code:         AffNothing,

	NotifyChannelDisabled.Code: NotifyChannelDisabled,
	NotifySendFailed.Code:      NotifySendFailed,

	// 以下 16 条是**补登记**：它们的 Def 早就存在并被 handler 使用，但一直没进
	// registry，导致 `Get(code)` 对它们返回 false。
	//
	// 影响面：`relay/preflight.go` 用 Get 把已知码翻回 Def，未知码才走兜底文案 ——
	// 缺登记的直接表现是"这些错误在中转层被当成陌生命令"。
	// 由 `registry_test.go` 的 TestEveryDefIsRegistered 兜住（它读本文件源码
	// 比对变量名，能发现任何新增 Def 忘登记）。
	//
	// 之所以单独成段而不是就地插入：这 16 条分散在 5 个域里，就地插入会让
	// 一次"补漏"看起来像"改了 5 个域"，review 时反而看不出改了什么。
	AuthUnavailable.Code:        AuthUnavailable,
	RegisterClosed.Code:         RegisterClosed,
	PortalDisabled.Code:         PortalDisabled,
	InviteRequired.Code:         InviteRequired,
	ContactRequired.Code:        ContactRequired,
	ContactVerifyRequired.Code:  ContactVerifyRequired,
	ResetTicketInvalid.Code:     ResetTicketInvalid,
	SessionExpired.Code:         SessionExpired,
	CaptchaRequired.Code:        CaptchaRequired,
	CaptchaInvalid.Code:         CaptchaInvalid,
	CaptchaUnavailable.Code:     CaptchaUnavailable,
	MFAAlreadyEnabled.Code:      MFAAlreadyEnabled,
	MFAChallengeExpired.Code:    MFAChallengeExpired,
	MFACodeInvalid.Code:         MFACodeInvalid,
	MFANotEnabled.Code:          MFANotEnabled,
	MFASetupRequired.Code:       MFASetupRequired,
	PasswordChangeRequired.Code: PasswordChangeRequired,
	LastAdminProtected.Code:     LastAdminProtected,

	// 协议面/网关域（原先是 handler 里的裸字符串，见常量区注释）。
	AsyncStreamConflict.Code:        AsyncStreamConflict,
	B64NotSupportedAsync.Code:       B64NotSupportedAsync,
	InlineImageNotSupported.Code:    InlineImageNotSupported,
	InstrumentalLyricsConflict.Code: InstrumentalLyricsConflict,
	PromptTooLong.Code:              PromptTooLong,
	NotImplemented.Code:             NotImplemented,
	UpstreamTimeout.Code:            UpstreamTimeout,
	UpstreamSubmitAmbiguous.Code:    UpstreamSubmitAmbiguous,
	UpstreamResultUnknown.Code:      UpstreamResultUnknown,
	UpstreamBodyError.Code:          UpstreamBodyError,
	EmptyResponse.Code:              EmptyResponse,
}

// Get 按字符串错误码查找定义。
func Get(code string) (Def, bool) {
	d, ok := registry[code]
	return d, ok
}

// All 返回全量错误定义（按 code 字典序，顺序稳定）。
//
// 存在的理由：对外文案需要能被**枚举扫描**（门户泄漏门禁用例逐条检查 Message），
// 而 registry 是包内私有。没有这个出口，门禁只能靠人工维护一份 Def 清单，
// 新增一个带「渠道」字样的 Def 就悄悄绕过去了。
func All() []Def {
	out := make([]Def, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// WithMessage 返回同码同状态、但自定义 message 的错误定义（用于向调用方透出校验细节）。
func (d Def) WithMessage(msg string) Def {
	d.Message = msg
	return d
}

// AsDef 尝试把 error 还原为 Def（service 层返回校验错误、controller 统一映射响应）。
func AsDef(err error) (Def, bool) {
	var d Def
	if errors.As(err, &d) {
		return d, true
	}
	return Def{}, false
}
