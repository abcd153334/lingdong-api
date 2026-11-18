package model

import (
	"time"

	"gorm.io/gorm"
)

// ModelBinding 对应 model_bindings 表（v2.0 §4.2.3.1）。编辑入口唯一 = 模型资源。
type ModelBinding struct {
	ID            uint64         `gorm:"primaryKey"`
	ModelID       string         `gorm:"size:64;uniqueIndex:idx_model_channel;not null"`
	ChannelID     uint64         `gorm:"uniqueIndex:idx_model_channel;not null"`
	UpstreamModel string         `gorm:"size:128"` // 别名，空 = 同名
	Weight        int            `gorm:"not null;default:1"`
	Priority      int            `gorm:"not null;default:1"` // 小优先
	Enabled       bool           `gorm:"not null;default:true"`
	CreatedAt     time.Time      `gorm:"type:timestamptz"`
	UpdatedAt     time.Time      `gorm:"type:timestamptz"`
	DeletedAt     gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (ModelBinding) TableName() string { return "model_bindings" }
