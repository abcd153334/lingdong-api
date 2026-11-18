package model

import "time"

// Transaction 对应 transactions 表（v2.0 §4.2.9）。
// type：freeze/settle/release/recharge/redeem/refund/adjust/invite。余额唯一写入口 = service/billing。
type Transaction struct {
	ID           uint64    `gorm:"primaryKey"`
	UserID       uint64    `gorm:"index;not null"`
	Type         string    `gorm:"size:16;not null"`
	Amount       int64     `gorm:"column:amount;not null;default:0"` // ± 微
	BalanceAfter int64     `gorm:"column:balance_after;not null;default:0"`
	FrozenAfter  int64     `gorm:"column:frozen_after;not null;default:0"`
	RefType      string    `gorm:"size:32"`
	RefID        string    `gorm:"size:64"`
	Reason       string    `gorm:"size:200"`
	CreatedAt    time.Time `gorm:"type:timestamptz;index"`
}

func (Transaction) TableName() string { return "transactions" }
