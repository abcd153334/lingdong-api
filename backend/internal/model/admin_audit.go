package model

import "time"

// AdminAudit 对应 admin_audit 表（v2.0 §4.2.10）。管理写操作自动审计。
type AdminAudit struct {
	ID         uint64    `gorm:"primaryKey"`
	AdminID    uint64    `gorm:"index"`
	Action     string    `gorm:"size:64;index"`
	TargetType string    `gorm:"size:32"`
	TargetID   string    `gorm:"size:64"`
	Detail     JSON      `gorm:"type:jsonb"`
	CreatedAt  time.Time `gorm:"type:timestamptz;index"`
}

func (AdminAudit) TableName() string { return "admin_audit" }
