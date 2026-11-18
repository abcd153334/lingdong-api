package model

import "time"

// PaymentDedup 对应 payment_dedup 表（v2.0 §4.2.9）。支付回调幂等去重。
//
// ChannelID 只作**排障用**，不参与唯一键。
// 为什么不把唯一键改成 (channel_id, trade_no)：
//   - 收益≈0：epay 与虎皮椒的单号空间正交，跨渠道碰撞可忽略；
//   - GORM AutoMigrate 不会删除旧唯一索引，改完旧索引仍在，等于没改；
//   - 存量行 channel_id 为空 → 新复合键查询命中不到老行 → dedup 静默失效。
//
// 幂等的真正基石是「订单行锁 + 状态机 + orders.trade_no 唯一」，dedup 只是补强。
type PaymentDedup struct {
	ID        uint64    `gorm:"primaryKey"`
	ChannelID string    `gorm:"size:32;not null;default:''"`
	TradeNo   string    `gorm:"size:64;uniqueIndex;not null"`
	CreatedAt time.Time `gorm:"type:timestamptz"`
}

func (PaymentDedup) TableName() string { return "payment_dedup" }
