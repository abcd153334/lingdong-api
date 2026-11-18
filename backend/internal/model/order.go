package model

import (
	"time"

	"gorm.io/gorm"
)

// Order 对应 orders 表（v2.0 §4.2.9）。
// status：pending/paid/refunding/refunded/expired/need_manual。
//
// 支付相关字段分两类，语义必须分清：
//   - 身份与配置：PayChannelID/PayMethod/PayProtocol。**解析一律用 PayChannelID**，
//     PayChannel 只是下单时的展示快照（"虎皮椒-聚合支付"），改名不影响在途订单。
//   - 支付产物：PayURL/QRRef/ChannelOrderID。QRRef 是上游 url_qrcode 的**原值**，
//     可能是图片地址，也可能是二维码内容，由展示层判定（见 payment 包）。
//
// 时间只有一个真相源：ExpireAt。不再引入 pay_expires_at —— 虎皮椒二维码 5 分钟与
// pendingTTL 5 分钟本就对齐，两个字段必然分叉，分叉后「倒计时用哪个、关单用哪个」没有唯一答案。
type Order struct {
	ID     uint64 `gorm:"primaryKey"`
	UserID uint64 `gorm:"index;not null"`
	// AppID：0 = 平台直连（用户在门户或开放 API 自己充值）；>0 = 某接入应用代用户发起。
	//
	// 为什么订单要单独带 app_id，而 usages 不用：充值是资金入账，不经过调用计费，
	// 因此没有 key_id 可以回溯归属；同理它也不会写进 transactions 的 usage 维度。
	AppID uint64 `gorm:"column:app_id;not null;default:0"`
	// ClientOrderNo 是**客户端**提供的幂等号（空串 = 未启用幂等）。
	//
	// 唯一性由 db/indexes.go 的 uq_orders_client_no 保证：
	//   (user_id, client_order_no) WHERE client_order_no <> '' AND deleted_at IS NULL
	// 刻意不用 gorm 的 uniqueIndex tag：那是全表唯一，软删的订单会永久占号，
	// 用户删掉一笔单之后同号再也建不出来，而报错信息只是"重复"。
	ClientOrderNo string  `gorm:"column:client_order_no;size:64;not null;default:''"`
	OutTradeNo    string  `gorm:"size:40;uniqueIndex;not null"`
	TradeNo       *string `gorm:"size:64;uniqueIndex"`
	// AmountCent 是**订单币种**的最小单位（分）。支付渠道只认分，所以订单金额
	// 始终以分存储与传输；到账金额 Credited 才是微。两者之间只有一次
	// money.FromCents，不夹任何倍率。
	AmountCent int64 `gorm:"column:amount_cent;not null;default:0"`
	// Currency 是订单币种（下单时固化）。回调比对的**第一顺位**是币种、其次才是金额：
	// 只比金额的话，¥72 与 $72 会被判为「金额一致」而直接入账。
	Currency string `gorm:"size:3;not null;default:'CNY'"`
	Credited int64  `gorm:"column:credited;not null;default:0"`
	// 刻意没有"下单时的比例/倍率快照"：这两个概念已从系统中删除。
	// 到账额 Credited 本身就是完整快照（= 实付换算 + 赠送），
	// 回调与人工补单都只认它，不重算 —— 少两列，也少两条"改了配置历史单怎么显示"的分支。
	Status     string `gorm:"size:16;not null;default:pending"`
	PayChannel string `gorm:"size:64"` // 展示快照，不参与解析

	// —— 支付身份（稳定 ID）——
	PayChannelID string `gorm:"size:32;not null;default:''"`
	PayMethod    string `gorm:"size:32;not null;default:''"`
	// PayProtocol 是下单时的协议快照，供查单 worker 筛选；**验签以渠道当前配置为准**。
	PayProtocol string `gorm:"size:16;not null;default:''"`

	// —— 支付产物 ——
	PayURL         string `gorm:"type:text"`                   // 移动端跳转地址
	QRRef          string `gorm:"type:text"`                   // url_qrcode 原值（可能是 URL，也可能是内容）
	ChannelOrderID string `gorm:"size:64;not null;default:''"` // 上游内部单号
	// ChannelPaymentID 是**原路退款真正要用的凭据**，与 ChannelOrderID 不是一回事：
	// Stripe 建单返回的是 Checkout Session（cs_...），退款接口要的是 PaymentIntent（pi_...）。
	// 旧实现把 cs_ 当 pi_ 传给 Stripe，原路退款必然失败（而退款是「先扣额度再出网」，
	// 用户额度已消费时直接 ErrNegativeBalance 卡死）。
	// 历史行（本列上线前的已支付订单）为空 → 拒绝原路退款并提示走「仅回退余额」。
	ChannelPaymentID string `gorm:"size:128;not null;default:''"`

	// —— 业务上下文 ——
	Subject   string `gorm:"size:128;not null;default:''"` // 清洗后的订单标题
	PackageID uint64 `gorm:"not null;default:0"`           // 套餐归属（0 = 自定义/续付）

	// —— 生命周期 ——
	// ExpireAt 是**唯一时间真相源**：前端倒计时、关单、查单窗口全用它。
	// 为 NULL 的行由 ExpirePending 用 created_at 兜底关闭（防手工插单绕过写入路径）。
	ExpireAt *time.Time `gorm:"type:timestamptz"`

	// —— 创建支付失败的可观测信息（不新增 failed 状态：上游超时 ≠ 未建单）——
	PayError    string `gorm:"size:200;not null;default:''"`
	PayAttempts int    `gorm:"not null;default:0"`

	// —— 主动查单 ——
	LastQueryAt *time.Time `gorm:"type:timestamptz"`

	// —— 回调观测（notify_count 在**终态判断之前**自增，否则重复回调永远不计数）——
	NotifyCount  int        `gorm:"not null;default:0"`
	LastNotifyAt *time.Time `gorm:"type:timestamptz"`

	// —— 退款 ——
	//
	// 两个维度必须分开记，否则「钱退没退」和「额度扣没扣」会被混为一谈：
	//   - RefundMode   gateway = 已调上游原路退款；balance = 只回退平台余额（未原路退款）
	//   - RefundStatus pending = 已受理待确认 / success = 上游确认已退 / failed = 确认失败（额度已回滚）
	//
	// status 与 RefundStatus 的关系：进入退款后 status=refunding，
	// 由回调 CD/UD、主动查单或人工确认推进到 refunded / paid，两者不可互相推导。
	RefundStatus string `gorm:"size:16;not null;default:''"`
	RefundMode   string `gorm:"size:16;not null;default:''"`
	RefundNo     string `gorm:"size:64;not null;default:''"` // 上游 out_refund_no
	// RefundAmountCent 是**申请退款时**的订单金额快照（分）。上游没有金额入参，
	// 这个字段是核对上游 refund_fee 的唯一依据。
	RefundAmountCent int64 `gorm:"not null;default:0"`
	// RefundCredited 是**实际扣回的平台额度**（微），与 RefundAmountCent（分）是两回事：
	//   - gateway 模式：钱原路退回，额度按 credited 扣回 → 两者都有值；
	//   - balance 模式：没有一分钱回到渠道，只是把额度扣回来 → RefundAmountCent=0，
	//     这里才是真实动作金额；
	//   - 订单从未入账（need_manual 的另一种来源）：两者都是 0。
	// 旧实现把 refund_amount_cent 恒写成订单金额，等于**谎报**「钱已经退给用户了」，
	// 对账和客诉都据此判断，错得离谱。
	RefundCredited int64  `gorm:"column:refund_credited;not null;default:0"`
	RefundReason   string `gorm:"size:128;not null;default:''"`
	RefundError    string `gorm:"size:200;not null;default:''"`
	// RefundAttempts 记录发起过几次退款请求（含并发被拒的尝试），用于识别重复操作。
	RefundAttempts int `gorm:"not null;default:0"`
	// RefundAt 是**发起**退款的时间；RefundDoneAt 是确认终结（成功或失败）的时间。
	// 两者分开：滞留巡检要按「发起时间」判断，而不是按终结时间（未终结时为空）。
	RefundAt          *time.Time `gorm:"type:timestamptz"`
	RefundDoneAt      *time.Time `gorm:"type:timestamptz"`
	LastRefundQueryAt *time.Time `gorm:"type:timestamptz"`

	NotifyRaw string         `gorm:"type:text"`
	PaidAt    *time.Time     `gorm:"type:timestamptz"`
	CreatedAt time.Time      `gorm:"type:timestamptz"`
	UpdatedAt time.Time      `gorm:"type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (Order) TableName() string { return "orders" }

// orders.refund_status 的全部取值（唯一声明处）。
//
// 放在 model 而不是 service/order：**别的包也要按它筛数据** ——
// 对账（service/reconcile 的 checkAffiliate）要按它判「退款发生在返利到期前、
// 却没把返利作废」，而那个包不 import order。
// 以前那边只能硬写字符串，改一处漏一处就是「扫错行 / 永远扫不到」。
const (
	// RefundStatePending 已受理、结果待确认（超时 / 响应不可信）。
	RefundStatePending = "pending"
	// RefundStateSuccess 上游确认已退款，或余额模式（不涉及上游）已完成。
	RefundStateSuccess = "success"
	// RefundStateFailed 上游确认失败，额度已回滚。
	RefundStateFailed = "failed"
	// RefundStateUpstreamOnly 上游已退款，但**我们从未发起过退款**（钱退了、账没动）。
	RefundStateUpstreamOnly = "upstream_only"
)
