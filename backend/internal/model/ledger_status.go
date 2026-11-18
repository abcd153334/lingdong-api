package model

// usages.status 的取值全集（唯一常量源）。
//
// 状态机：pending → settled（成功）/ released（失败或取消）/ rejected（预检拒绝，0 费）。
// 改造前这些字面量散落在 billing.go、indexes.go 的部分索引、stats.go 的 WHERE 与
// reconcile.go 的判据里，共 10+ 处；任何一处打错字都不会报错，只会让该状态的
// 记录从统计与对账里静默消失。
const (
	UsageStatusPending  = "pending"
	UsageStatusSettled  = "settled"
	UsageStatusReleased = "released"
	UsageStatusRejected = "rejected"
)

// transactions.type 的取值全集。
//
// 语义：freeze/release 是冻结期的两笔对冲；settle 是差额扣款；recharge/redeem/refund/
// adjust/invite 是外部入账；refund_revert 是退款失败后的补偿入账。
// 对账按 type 分组比对，字符串写错 = 该笔钱消失。
//
// 邀请返利**没有**自己的流水类型：返利额度是 affiliate_rewards 的派生量，
// 只有「转入余额」那一刻才写 invite 流水。退款时只把尚未到期的行作废，
// 不动余额，因此不再有 aff_reclaim。
//
// **写入端必须用这些常量**：曾经 7 处写入全用字面量，于是新增类型时枚举漏了两项
// 而没人发现（`refund_revert` 长期不在枚举里）。
const (
	TxnFreeze       = "freeze"
	TxnSettle       = "settle"
	TxnRelease      = "release"
	TxnRecharge     = "recharge"
	TxnRedeem       = "redeem"
	TxnRefund       = "refund"
	TxnAdjust       = "adjust"
	TxnInvite       = "invite"
	TxnRefundRevert = "refund_revert"
)

// transactions.ref_type 的取值全集（决定唯一索引的幂等维度）。
const (
	RefTypeUsage = "usage"
	RefTypeOrder = "order"
	// RefTypeAffiliate 是「邀请奖励转入余额」这一笔的来源维度。
	// 它不参与资金唯一索引（不是每单一次），但必须与 usage/order 一起被认出来，
	// 否则按 ref_type 分组统计时这一族会被归入"未知"。
	RefTypeAffiliate = "affiliate"
)

// orders.status 的取值全集。
const (
	OrderPending    = "pending"
	OrderPaid       = "paid"
	OrderRefunding  = "refunding"
	OrderRefunded   = "refunded"
	OrderExpired    = "expired"
	OrderNeedManual = "need_manual"
)
