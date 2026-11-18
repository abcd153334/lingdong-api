package model

import (
	"time"

	"gorm.io/gorm"
)

// RedeemCode 对应 redeem_codes 表（v2.0 §4.2.9）。
type RedeemCode struct {
	ID        uint64         `gorm:"primaryKey"`
	Code      string         `gorm:"size:64;uniqueIndex;not null"`
	Amount    int64          `gorm:"column:amount;not null;default:0"`
	Batch     string         `gorm:"size:64;index"`
	UsedBy    *uint64        `gorm:"index"`
	UsedAt    *time.Time     `gorm:"type:timestamptz"`
	ExpiresAt *time.Time     `gorm:"type:timestamptz"`
	CreatedAt time.Time      `gorm:"type:timestamptz"`
	UpdatedAt time.Time      `gorm:"type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (RedeemCode) TableName() string { return "redeem_codes" }
