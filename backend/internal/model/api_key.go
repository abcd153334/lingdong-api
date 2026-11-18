package model

import (
	"time"

	"gorm.io/gorm"
)

// APIKey 对应 api_keys 表（v2.0 §4.2.2 / §4.14.1）。
// key_cipher = AES-GCM 密文；key_hash = SHA-256 索引（鉴权查找用）。
//
// 这张表同时承担两个角色，靠 Kind 区分（**不要**用 AppID>0 判别）：
//
//	kind='user'    用户在门户/开放 API 自助创建的密钥。AppID 恒为 0（"平台直连"），
//	               Scopes 恒为 NULL（它不该有权限点概念，引入"默认 scope 集"
//	               就是第二套权限语义）。对 AuthWriter 端点一律拒绝。
//	kind='hosted'  应用后端为某个用户换取的托管密钥。AppID>0 且 Scopes 非空。
//	               它归因到应用（usages.key_id -> api_keys.app_id），
//	               并且是对外写操作 **唯一** 可用的长期凭证。
//
// 为什么托管密钥不放一张独立表：`TokenAuth`（/v1 网关）与 `billing.Freeze`
// 只认这一张表。新建第二张凭证表要么让 /v1 查两张（破坏"网关零改动"），
// 要么让托管密钥不能调模型（那它就没有存在的意义）。
type APIKey struct {
	ID     uint64 `gorm:"primaryKey"`
	UserID uint64 `gorm:"index;not null"`
	// AppID：0 = 平台直连（门户自建密钥）；>0 = 某接入应用。
	// 零值合法，所以 default:0 与 Go 零值一致，不存在被替换成别的值的风险。
	AppID uint64 `gorm:"column:app_id;not null;default:0"`
	// Kind 见上方类型注释。零值会被 GORM 替换成 'user'（default 标签），
	// 这正是想要的：所有存量创建路径不传 Kind 就是普通密钥。
	Kind string `gorm:"size:16;not null;default:'user'"`
	// Scopes 只对 kind='hosted' 有意义，内容由 apicontract.HostedScopes() 派生。
	Scopes              JSON       `gorm:"type:jsonb"`
	KeyCipher           string     `gorm:"type:text;not null"`
	KeyHash             string     `gorm:"size:64;uniqueIndex;not null"`
	KeyPrefix           string     `gorm:"size:32"`
	Name                string     `gorm:"size:64"`
	Status              int16      `gorm:"not null;default:1"` // 1 启用 / 2 禁用
	ExpiredAt           *time.Time `gorm:"type:timestamptz"`
	AmountLimit         int64      `gorm:"column:amount_limit;not null;default:0"` // 0 = 不限（唯一判定点 billing.checkKeyLimit）
	Used                int64      `gorm:"column:used;not null;default:0"`         // 已发生消费，含冻结中占用
	ModelsAllowed       JSON       `gorm:"type:jsonb"`
	ModalitiesAllowed   JSON       `gorm:"type:jsonb"`
	AllowIPs            JSON       `gorm:"type:jsonb"`
	Group               string     `gorm:"size:32;not null;default:''"`
	AllowUserQuotaQuery bool       `gorm:"not null;default:true"`
	RPMLimit            int        `gorm:"not null;default:0"`
	ConcurrencyLimit    int        `gorm:"not null;default:0"`
	ResetSecretAt       *time.Time `gorm:"type:timestamptz"`
	// LastUsedAt 最近使用时间；NULL=从未使用。唯一写点 apikey.touchKey（节流）。
	// 闲置过期语义的基准：COALESCE(last_used_at, created_at)，无回填依赖。
	LastUsedAt *time.Time `gorm:"type:timestamptz"`
	// PrevKeyHash / PrevKeyExpiresAt 是轮换重叠窗口：轮换时把**上一把**明文的
	// 哈希挪到这里，并记下它最晚可用到什么时候。
	//
	// 为什么必须留一代：轮换与新明文的交付之间必然存在时间差（部署、灰度、配置
	// 中心推送）。旧实现是"换完即失效"，于是轮换那一刻所有还在用旧值的实例同时
	// 401 —— 把一次运维动作变成一次故障。有窗口后，旧钥在窗口内照常可用，
	// 应用只需在窗口内完成替换。
	//
	// 只保留**一代**：第二次轮换会覆盖它（更早的钥本就该死了）。永不回退到更老的明文，
	// 否则窗口会无限延长，等于把"轮换"退化成"多把长期密钥并存"。
	PrevKeyHash string `gorm:"size:64;not null;default:''"`
	// PrevKeyExpiresAt 为 NULL 表示没有重叠（窗口 0 或从未轮换）。
	// 判定唯一处：apikey.Authenticate（`prev_key_expires_at > now`）。
	PrevKeyExpiresAt *time.Time `gorm:"type:timestamptz"`
	// DisabledReason 记录 status=2 的**成因**，决定这把密钥能不能被轮换自愈。
	//
	// 为什么必须有：禁用只有一个 status 值时，「管理员封禁」与「轮换退役/闲置超期」
	// 长得一模一样，实现层只能二选一 —— 要么任何轮换都解禁（管理员处置可被
	// 一次请求绕过），要么任何禁用都不可自愈（误禁/退役变成永久死局）。
	// 分成两类后：admin_block 永不可自愈，其余可由轮换恢复。
	DisabledReason string         `gorm:"size:24;not null;default:''"`
	CreatedAt      time.Time      `gorm:"type:timestamptz"`
	UpdatedAt      time.Time      `gorm:"type:timestamptz"`
	DeletedAt      gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (APIKey) TableName() string { return "api_keys" }

// 密钥类型常量（落库值，改变即数据迁移）。
const (
	// KeyKindUser 是用户自建密钥：只读面可用，写面与密钥管理一律不可用。
	KeyKindUser = "user"
	// KeyKindHosted 是应用托管密钥：按 Scopes 允许受限写操作。
	KeyKindHosted = "hosted"
)

// IsHosted 判断是否应用托管密钥。
//
// 用方法而不是到处写 `k.Kind == model.KeyKindHosted`：判据只有一处，
// 将来加第三种类型（如"临时密钥"）时改动点在编译器能指出的地方。
func (k APIKey) IsHosted() bool { return k.Kind == KeyKindHosted }

// status=2（Disabled）的成因取值，落库在 disabled_reason。
//
// 唯一的判据是 `apikey` 两处：`rotateHosted`（能否被轮换恢复，见 CanRotateRecover）
// 与写入点（管理台 / 强制重置 / 真闲置超期）。
const (
	// KeyDisabledAdminBlock 是管理员动作：管理台手动封禁、账号失陷后的强制重置。
	// 语义是"人认定这把凭证不可信"，任何自助动作都不得覆盖。
	KeyDisabledAdminBlock = "admin_block"
	// KeyDisabledExpired 是真闲置超期（超过 expire_days 没有任何调用），
	// 由 apikey.Authenticate 在判定时落库。
	//
	// 为什么要落成状态而不是只在鉴权时拒绝：否则门户里这把钥看起来"一切正常"
	// （status=1、expired_at 甚至还没到），而应用每次调用都 401 —— 用户唯一
	// 能看到的线索是一条错误码，运营也无法统计"有多少把钥已经名存实亡"。
	KeyDisabledExpired = "expired"
)

// CanRotateRecover 判断这把密钥的禁用能否被一次轮换解除。
//
// **白名单判定（fail-closed）**：只有显式标记为 expired 的可恢复。
// 空串不能当成"可恢复"—— 改造前的历史禁用**全部**来自管理员（当时没有别的
// 禁用来源），把空串判成可恢复，等于让存量封禁被一次轮换绕过。
func (k APIKey) CanRotateRecover() bool {
	return k.DisabledReason == KeyDisabledExpired
}
