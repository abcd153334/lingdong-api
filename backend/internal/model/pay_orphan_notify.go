package model

import "time"

// PayOrphanNotify 记录「验签通过但本地订单不存在」的回调。
//
// 为什么必须有这张表：这类回调的应答必须是 success（否则网关重试 6 次后放弃，
// 而本地根本没有订单可变出来），但**止血不能止成静默**——落库 + ERROR 日志
// 是「我方丢单」唯一可观测的证据。没有它，丢单只能靠用户投诉才发现。
type PayOrphanNotify struct {
	ID         uint64    `gorm:"primaryKey"`
	ChannelID  string    `gorm:"size:32;not null;default:''"`
	OutTradeNo string    `gorm:"size:40;not null;default:'';index"`
	TradeNo    string    `gorm:"size:64;not null;default:''"`
	AmountCent int64     `gorm:"not null;default:0"`
	Raw        string    `gorm:"type:text"`
	CreatedAt  time.Time `gorm:"type:timestamptz;index"`
}

func (PayOrphanNotify) TableName() string { return "pay_orphan_notify" }
