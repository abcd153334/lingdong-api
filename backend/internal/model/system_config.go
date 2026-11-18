package model

import "time"

// SystemConfig 对应 system_config 表（v2.0 §4.2.6）。键名规则「模块.字段」。
type SystemConfig struct {
	Key       string    `gorm:"size:64;primaryKey"`
	Value     JSON      `gorm:"type:jsonb"`
	UpdatedAt time.Time `gorm:"type:timestamptz"`
}

func (SystemConfig) TableName() string { return "system_config" }
