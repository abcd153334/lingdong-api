package model

import (
	"time"

	"gorm.io/gorm"
)

// User 对应 users 表（v2.0 §4.2.1）。
type User struct {
	ID           uint64 `gorm:"primaryKey"`
	Username     string `gorm:"size:20;uniqueIndex;not null"`
	PasswordHash string `gorm:"size:128;not null"`
	Role         int16  `gorm:"not null;default:1"` // 0 管理 / 1 普通
	Status       int16  `gorm:"not null;default:1"` // 1 正常 / 2 禁用
	// Email / Phone 的语义是「已通过验证码验证的地址」，空串 = 未绑定。
	//
	// 唯一性**不靠 tag**：空串会互相冲突，且邮箱要按大小写不敏感去重，
	// 只能写成部分表达式唯一索引（db/indexes.go 的 uq_users_email_lower）。
	// Phone 用值类型而不是 *string：指针只带来了 4 处 `!= nil` 分支，
	// 而空串同样能表达「未绑定」。
	Email string `gorm:"size:128;index"`
	Phone string `gorm:"size:20"`
	// Avatar 是用户头像，存 `data:image/webp;base64,...`（服务端裁 128×128、q82，≤24KB）。
	//
	// 为什么不是对象存储的三列：头像只有几 KB、每次 `/me` 都要下发、且**换头像就是
	// 覆盖同一个标量**。走对象存储会多出「存储未配置→头像 503」「替换后旧对象成孤儿」
	// 「删行要 DeleteRefVerified 复核」三件事，而没有换来任何东西。
	//
	// 不存 `null`：空串 = 未设置，前端渲染**打包进产物**的默认头像（前端资产，不入库 ——
	// 默认图一旦写进 DB，「默认」就有了两个真相源）。`not null` 是必需的：可空列会让
	// 既有行的读取结果变成 NULL，经 JSON 下发成 `avatar_url: null`，前端 `<img src="null">`
	// 会白发一次请求。
	Avatar       string `gorm:"column:avatar;type:text;not null;default:''"`
	Balance      int64  `gorm:"column:balance;not null;default:0"`
	Used         int64  `gorm:"column:used;not null;default:0"`
	RequestCount int64  `gorm:"not null;default:0"`
	Frozen       int64  `gorm:"column:frozen;not null;default:0"`
	Group        string `gorm:"size:32;not null;default:default"`
	// BalanceLimit 是对「全局 balance_limit」的个体覆盖（微），默认 0 = 未设置。
	//
	// 默认值必须是 0 而不是 5 亿：这一列若自带业务默认值，就会让「用户级恒有值」
	// 永久压过全局配置 —— 运营调小上限对新用户完全不生效，而前端又按全局值预览，
	// 两端上限不一致。生效值一律走 conf.BalanceLimitFor（用户级 > 0 优先，否则全局）。
	BalanceLimit int64 `gorm:"column:balance_limit;not null;default:0"`
	// 凭证吊销的唯一真相源是 user_sessions（见 model.UserSession）：
	// 这里不再有 token_version —— 两个吊销开关并存时，任何一处漏写都会让
	// 「改密后旧凭证仍可用」这种缺陷带着绿测试上线。
	InviterID *uint64 `gorm:"index"`
	// AffCode 是本人的邀请码，**懒生成**（注册插入时为 ''，随后由
	// affiliate.OnRegister 补齐）。唯一性由 db/indexes.go 的部分唯一索引
	// `uq_users_aff_code`（WHERE aff_code <> ''）保证。
	//
	// 这里刻意不写 `uniqueIndex` tag：AutoMigrate 会建一条**全集**唯一索引，
	// 于是第二个未生成邀请码的用户插入即 23505 —— 注册随机失败，且报错是
	// 与邮箱无关的「已被占用」。
	AffCode string `gorm:"size:32"`
	// 邀请返利的额度**不存在 users 上**：可转出/冷却中/累计全部由
	// affiliate_rewards 的行派生（见 affiliate.Info）。这里曾经有
	// aff_balance / aff_history / aff_rewarded 三个累加器，与凭据表构成
	// 双真相源，只能靠回收时手工同步 —— 任何一处漏同步都永久漂移。
	// MustChangePassword 表示当前口令是「系统给的初始口令」，必须先改密才能用其它功能。
	//
	// 为什么需要它：默认超管的初始口令只可能来自环境变量或随机生成，
	// 两者都属于"一次性的交接凭证"。没有这个标记时，随机口令一旦被日志/截图带出，
	// 就会长期有效，而系统里没有任何机制要求它被替换。
	MustChangePassword bool           `gorm:"not null;default:false"`
	CreatedAt          time.Time      `gorm:"type:timestamptz"`
	UpdatedAt          time.Time      `gorm:"type:timestamptz"`
	DeletedAt          gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (User) TableName() string { return "users" }
