package controller

// 本文件是**托管密钥自举**（凭证操作它自己）的两条端点：
//
//	POST /api/user/key/rotate   用当前有效的托管密钥换一把新明文
//	GET  /api/user/key/health   自检：这把钥现在什么状态、还剩多久、为什么用不了
//
// 两者都只认 API KEY（会话票一律 401、自建 KEY 一律 403）。目标行由鉴权时
// 命中的那把钥决定，**不接受任何入参指定** —— 这是"不存在越权面"的结构性保证，
// 而不是靠 handler 里再判一次权限。
//
// 为什么需要它们：轮换端点原先只认会话票，而接入方后端没有会话票，于是
// "密钥需要换新"必然需要真人介入 —— 一个本该自动化的运维动作变成了人工工单，
// 中间还可能拖到窗口结束、应用彻底断服。

import (
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/middleware"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/authevent"
)

const (
	// selfRotateMinInterval 是同一把钥两次自举轮换的最小间隔。
	//
	// 防的是"应用重启风暴"：3 个副本同时起来各自轮换，会把刚发出的明文互相挤进
	// 重叠窗口，窗口 0 时先拿到的那把当场失效。10 分钟足以覆盖一次发布，又不至于
	// 让"确认泄露后立刻再换一次"变得不可行。
	//
	// 依据 reset_secret_at（轮换的唯一写点），不引入新字段。门户轮换同样写它，
	// 所以"刚在门户换过、应用马上再换"也会被挡 —— 这正是想要的（刚换过）。
	selfRotateMinInterval = 10 * time.Minute
)

// UserKeySelfRotate 处理 POST /api/user/key/rotate。
//
// 非幂等：每次调用都生成新明文。重复调用由 10 分钟间隔与 CAS 双重挡住，
// 返回 409 key_rotate_busy —— 语义是"不要重试，当前钥仍然有效"。
func UserKeySelfRotate(c *gin.Context) {
	k, ok := middleware.HostedKeyFromCtx(c)
	if !ok {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	now := time.Now()
	if k.ResetSecretAt != nil && now.Sub(*k.ResetSecretAt) < selfRotateMinInterval {
		// 当前钥仍然有效，重试只会把刚发出去的那把挤进窗口。
		response.FailDef(c, errs.KeyRotateBusy)
		return
	}

	// viaPrev 由中间件算好：这次鉴权走的是重叠窗口里的上一代明文。
	// 语义见 apikey.rotateHosted —— 允许自救，但窗口不延长。
	viaPrev := c.GetBool("key_via_prev")
	res, err := apikey.RotateSelf(c.Request.Context(), k.ID, viaPrev)
	if err != nil {
		// 这里带的就是那把钥，`key_disabled` 语义正确（401：换凭证或等人工）。
		response.FailDef(c, apikey.DefOr(err, errs.Internal))
		return
	}

	// 无人值守的换钥必须留痕：事后追溯"凌晨三点密钥被换了三次"时，
	// 这条事件是唯一能区分"应用自愈"与"有人登进来操作"的凭据。
	recordKeyEvent(c, authevent.EventKeySelfRotate, res.Key.ID, map[string]any{
		"kind": model.KeyKindHosted, "app_id": res.Key.AppID,
		"via_prev":            viaPrev,
		"previous_expires_at": formatTimePtr(res.PreviousExpiresAt),
	})
	response.OK(c, gin.H{
		"key":       newHostedKeyView(res.Key, res.PreviousExpiresAt),
		"plaintext": res.Plaintext,
	})
}

// hostedKeyHealth 是自检响应里的 key 段。
type hostedKeyHealth struct {
	ID     uint64 `json:"id"`
	Prefix string `json:"prefix"`
	// Status / DisabledReason 在这里是**真实可读**的：自检走"只验明文"的鉴权
	// （middleware.HostedKeyInspectAuth），被禁、过期、IP 不符的钥也能进来问原因。
	// 若自检也要求"先能用"，这两个字段就永远是 1 / 空串 —— 挂诊断的名义、
	// 拿不到诊断的信息。
	Status int `json:"status"`
	// DisabledReason：admin_block = 平台封禁（轮换不会解除，需联系平台）；
	// expired = 闲置超期（门户/自举轮换即可恢复）；空串 = 启用中。
	DisabledReason string `json:"disabled_reason"`
	// ViaPrevious 表示这次带的是重叠窗口里的上一代明文。
	// 与 previous_expires_in_seconds 一起看：非 null = 还有时间换，
	// 为 null（且 ViaPrevious） = 这一代已经退役，必须换新钥。
	ViaPrevious bool       `json:"via_previous"`
	ExpiredAt   *time.Time `json:"expired_at"`
	// ExpiresInSeconds 距有效期还有多少秒；null = 不过期；**负值 = 已过期**。
	ExpiresInSeconds         *int64     `json:"expires_in_seconds"`
	PreviousExpiresAt        *time.Time `json:"previous_expires_at"`
	PreviousExpiresInSeconds *int64     `json:"previous_expires_in_seconds"`
	Scopes                   []string   `json:"scopes"`
	// AllowIPs 是 IP 白名单。被 401 key_ip_denied 拒绝时应用进不来自检 ——
	// 这条端点**不校验 IP**，正是为了让它能回答"是不是 IP 的问题"。
	AllowIPs      []string   `json:"allow_ips"`
	AmountLimit   int64      `json:"amount_limit"`
	Used          int64      `json:"used"`
	RPMLimit      int        `json:"rpm_limit"`
	CreatedAt     time.Time  `json:"created_at"`
	ResetSecretAt *time.Time `json:"reset_secret_at"`
}

// UserKeyHealth 处理 GET /api/user/key/health。
//
// 它回答四个问题，且**只**回答这四个：
//  1. 这把钥是什么状态（status + disabled_reason）
//  2. 还剩多久到期（expires_in_seconds；负值 = 已过期）
//  3. 是否还在重叠窗口期、剩多久（previous_expires_in_seconds）
//  4. 账号是否被停用（account.status；2 = 所有调用都会被 403 拒绝）
//
// **刻意没有 `healthy` 布尔**：它是 status / expires_in_seconds / account.status
// 三个事实的再组合，多一个派生字段就多一处会与事实漂移的地方。
// 判定"能不能用"仍然只有 apikey.Authenticate 一处。
//
// 也**不返回**余额，不返回"能否调模型"：那由 billing 判据决定（欠费闸
// billing.ArrearsBlock、KEY 顶额还要算本次金额）。健康端点拿不到本次金额，
// 任何布尔都会变成与 billing 并行的第二套真相源 —— 一处今天对、明天改价就错。
func UserKeyHealth(c *gin.Context) {
	k, ok := middleware.HostedKeyFromCtx(c)
	if !ok {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	now := time.Now()
	var u model.User
	if err := db.DB.WithContext(c.Request.Context()).Select("id", "status").
		First(&u, k.UserID).Error; err != nil {
		// 用户已被拆除：这把钥没有归属了，按无效凭证处理（不是 500）。
		response.FailDef(c, errs.InvalidAPIKey)
		return
	}
	// 窗口过期后 prev_key_expires_at 不会被清理（唯一写点是轮换），
	// 直接下发会让"非 null = 还在窗口期"永久失真。
	prevAt := liveUntil(k.PrevKeyExpiresAt, now)
	response.OK(c, gin.H{
		// 让应用算窗口剩余时不必依赖本机时钟（容器时钟漂移很常见）。
		"server_time": now,
		"key": hostedKeyHealth{
			ID: k.ID, Prefix: k.KeyPrefix,
			Status: int(k.Status), DisabledReason: k.DisabledReason,
			ViaPrevious:      c.GetBool("key_via_prev"),
			ExpiredAt:        k.ExpiredAt,
			ExpiresInSeconds: secondsUntil(k.ExpiredAt, now),

			PreviousExpiresAt:        prevAt,
			PreviousExpiresInSeconds: secondsUntil(prevAt, now),
			Scopes:                   apikey.DecodeStrings(k.Scopes),
			AllowIPs:                 apikey.DecodeStrings(k.AllowIPs),
			AmountLimit:              k.AmountLimit, Used: k.Used, RPMLimit: k.RPMLimit,
			CreatedAt: k.CreatedAt, ResetSecretAt: k.ResetSecretAt,
		},
		// 账号状态必须下发：它决定 403 forbidden，而这把钥本身完全正常。
		// 不返回它，应用就会拿"钥是好的"去解释一次 403，排错方向直接跑偏。
		"account": gin.H{"status": int(u.Status)},
	})
}
