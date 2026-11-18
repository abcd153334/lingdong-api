package model

import "time"

// AffiliateReward 是「某个受益人因某一笔订单拿到的返利」，一行 = 一笔钱。
//
// # 为什么一行只装一个受益人
//
// 旧实现一行装两个人（invitee_amount 归付款方、inviter_amount 归邀请人），
// 但只有一个 transferred_at。任何一方先点「转入余额」，整行的 transferred_at
// 就被置位 —— 另一方那一份额度**永远转不出去**，而且退款时 VoidTx 的
// `transferred_at IS NULL` 也不再成立，钱既没进谁的口袋，也收不回来。
// 改成一行一受益人后，转出/作废/状态全部只作用于一个人，该类问题在结构上不存在。
//
// # 为什么额度是派生的
//
// users 上曾经有 aff_balance / aff_history 两个累加器，与这张表构成双真相源：
// 发放要加、退款要减，漏一处就永久漂移且无法自检。现在「可转出 / 冷却中 / 累计」
// 全部由本表的行算出（见 affiliate.Info），行就是钱。
//
// # 生命周期
//
// 发放：一笔订单最多插两行（inviter / invitee，某侧金额为 0 就不插）；
// 作废：退款成功且该行尚未转出 → voided=true（钱从未到用户手里，不倒扣余额）；
// 转出：到期后用户点「转入余额」→ transferred_at 置位（每行只能转一次）。
//
// 金额列是**计算快照**：运营后来改比例或赠金，历史行必须原样解释得通。
type AffiliateReward struct {
	ID uint64 `gorm:"primaryKey"`

	// OrderID + Role 唯一（uq_affiliate_rewards_order_role，见 db/indexes.go）。
	// 不写 uniqueIndex tag：AutoMigrate 建全集索引会让重复回调变成 23505，
	// 打断整个充值事务；正确做法是 ON CONFLICT DO NOTHING 天然幂等。
	OrderID uint64 `gorm:"not null;default:0"`

	// InviteeID 是付款方。一行一受益人后无法从 BeneficiaryID 反推谁是付款方，
	// 而「前 N 笔」必须按 COUNT(DISTINCT order_id) WHERE invitee_id = ? 计数。
	InviteeID uint64 `gorm:"not null;default:0"`
	// BeneficiaryID 是本行的收款人；Role 说明这笔钱是哪种身份拿到的。
	BeneficiaryID uint64 `gorm:"not null;default:0"`
	Role          string `gorm:"size:16;not null"` // inviter | invitee

	// 金额一律「微」，与 users.balance 同单位。
	Amount     int64 `gorm:"not null;default:0"` // 本行金额
	BaseAmount int64 `gorm:"not null;default:0"` // 本单实付（不含套餐赠送）
	Rate       int64 `gorm:"not null;default:0"` // 本单适用比例（微比例）；invitee 行恒 0

	// AvailableAt 是冷却到期时刻（发放时由**数据库** now() 计算）。
	// 用应用侧 time.Now() 会让时钟偏差把刚发的行判成已到期，而这条边界就是钱的分界线。
	AvailableAt time.Time `gorm:"type:timestamptz;not null"`

	// Voided 是「退款作废」标记：只对尚未转出的行成立（转出后归用户，不追回）。
	Voided   bool       `gorm:"not null;default:false"`
	VoidedAt *time.Time `gorm:"type:timestamptz"`
	// TransferredAt 非空表示这笔钱已经转入余额（每行只能转一次）。
	TransferredAt *time.Time `gorm:"type:timestamptz"`
	CreatedAt     time.Time  `gorm:"type:timestamptz"`
}

// 返利行的角色取值（唯一声明处）。
const (
	// AffRoleInviter 是邀请人按比例拿到的返利。
	AffRoleInviter = "inviter"
	// AffRoleInvitee 是被邀请人按笔拿到的赠金。
	AffRoleInvitee = "invitee"
)

// TableName 固定表名，避免 GORM 复数化规则变化导致迁移漂移。
func (AffiliateReward) TableName() string { return "affiliate_rewards" }
