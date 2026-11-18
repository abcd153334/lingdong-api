package model

import "time"

// UserSession 是一条服务端会话，也是登录态的**唯一真相源**。
//
// 为什么不存 JWT：无状态令牌无法吊销，而「登出 / 踢设备 / 改密后让旧凭证失效」
// 是账号安全的基本要求。这里存的是**票据哈希**，票据明文只在登录响应头里出现一次，
// 库被读走也无法反推出可用的 Cookie。
type UserSession struct {
	ID uint64 `gorm:"primaryKey"`
	// UID 是会话归属用户。列名显式写成 uid（不用 GORM 默认的 u_id）。
	UID uint64 `gorm:"column:uid;not null"`
	// TicketHash = hex(sha256(ticket))，64 字符。
	//
	// 不用 bcrypt：票据是 256 bit 随机值，不存在被猜解的风险，而 bcrypt 会给
	// **每一个已登录请求**加约 60ms 校验开销。等值索引查询本身就是常量时间。
	TicketHash string    `gorm:"size:64;not null"`
	CreatedAt  time.Time `gorm:"type:timestamptz;not null"`
	LastSeenAt time.Time `gorm:"type:timestamptz;not null"`
	// ExpiresAt 是**滑动**过期时间：活跃会话会被续期到 now+absolute_ttl（见 session.Touch）。
	ExpiresAt time.Time `gorm:"type:timestamptz;not null"`
	// RevokedAt 非空即失效。吊销只打时间戳不删行，否则设备列表与审计会丢历史。
	RevokedAt *time.Time `gorm:"type:timestamptz"`
	// RevokeReason 取值见 session 包的 Revoke* 常量。留空表示未吊销。
	RevokeReason string `gorm:"size:32;not null;default:''"`
	IP           string `gorm:"column:ip;size:45;not null;default:''"`
	UA           string `gorm:"column:ua;size:255;not null;default:''"`
}

func (UserSession) TableName() string { return "user_sessions" }

// 会话吊销原因（RevokeReason 的取值域）。
//
// 单独列出来是为了让「设备列表为什么失效」可回答：用户看到「已退出」时，
// 运营需要能分辨是被自己踢掉、被改密踢掉，还是空闲超时。
const (
	SessionRevokeLogout          = "logout"
	SessionRevokeUserRevoked     = "user_revoked"
	SessionRevokeAdminRevoked    = "admin_revoked"
	SessionRevokePasswordChanged = "password_changed"
	SessionRevokeMFAChanged      = "mfa_changed"
	SessionRevokeIdle            = "idle"
	SessionRevokeSuperseded      = "superseded"
	SessionRevokeMaxActive       = "max_active"
	// 门户端关闭后拒绝非管理员登录：会话是登录流程里先建好的，拒绝时必须撤销，
	// 否则每次尝试都留一条从未发出去的「幽灵设备」。原因单独成值，便于排查。
	SessionRevokePortalClosed = "portal_closed"
)

// AuthEvent 是一条认证事件（登录 / 注册 / 改密 / 撤销 …）。
//
// 与 admin_audit 刻意分开：admin_audit 的语义是「管理员做了什么」，
// 用户登录不是管理员操作；混在一张表里会让「谁动了配置」这类查询被登录噪音淹没。
type AuthEvent struct {
	ID uint64 `gorm:"primaryKey"`
	// UID 已知时填真实用户；登录失败（账号不存在）时为 0。
	UID uint64 `gorm:"column:uid;not null;default:0"`
	// Account 是本次尝试的账号原文（截断 64 字节，**绝不含密码**），用于事后追查撞库来源。
	Account string `gorm:"size:64;not null;default:''"`
	// Event 取值见 authevent 包的白名单常量。
	Event string `gorm:"size:32;not null"`
	// Result 是 success | fail。
	Result string `gorm:"size:8;not null"`
	// Reason 是失败原因码（如 invalid_credential / captcha_invalid），成功时通常为空。
	Reason    string    `gorm:"size:32;not null;default:''"`
	IP        string    `gorm:"column:ip;size:45;not null;default:''"`
	UA        string    `gorm:"column:ua;size:255;not null;default:''"`
	SessionID uint64    `gorm:"column:session_id;not null;default:0"`
	Detail    JSON      `gorm:"column:detail;type:jsonb"`
	CreatedAt time.Time `gorm:"type:timestamptz;not null"`
}

func (AuthEvent) TableName() string { return "auth_events" }
