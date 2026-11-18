package model

import "time"

// PaymentEvent 对应 payment_events 表（计费整改 W15）。Stripe 回调事件幂等去重。
//
// 为什么需要它：Stripe 会重放同一个 event（网络重试、签收失败后重发），
// 而现有的 payment_dedup 按 trade_no 去重 —— Stripe 的 event id（evt_...）与
// 交易号（session id / payment_intent）不是一回事，同一个 event 重放时
// trade_no 相同、会被 payment_dedup 挡掉，但「同一 event 被重复处理」这件事
// 本身需要一个 event 级别的幂等锚。
//
// EventID 是 Stripe 事件 ID（evt_...），全局唯一。用 string 而不是 uint64：
// Stripe 的 ID 是前缀 + 下划线 + 随机串，不是数字。
type PaymentEvent struct {
	EventID   string    `gorm:"size:64;primaryKey"`
	Provider  string    `gorm:"size:16;not null;default:''"`
	CreatedAt time.Time `gorm:"type:timestamptz"`
}

func (PaymentEvent) TableName() string { return "payment_events" }
