package model

import (
	"time"

	"gorm.io/gorm"
)

// Vendor 对应 vendors 表（v2.0 §4.2.5）。供应商分组，与计费分组正交；
// builtin=true 只表示**不可删除**（名称/图标/颜色均可改，见 controller.UpdateAdminVendor）。
type Vendor struct {
	ID string `gorm:"size:32;primaryKey"`
	// Name 是展示名。改名安全：models.vendor 引用的是本表的 **ID**（裸字符串），
	// 不是名字，所以改名不会让任何模型失去归属。
	Name  string `gorm:"size:64;not null"`
	Color string `gorm:"size:16"`
	// Icon 是供应商图标 token（"" | builtin:<key> | upload:<ulid>）。
	// 它是"模型没有单独设图标时"的回落来源（brandicon.Effective）。
	Icon      string         `gorm:"size:255;not null;default:''"`
	Priority  int            `gorm:"not null;default:0"`
	Sort      int            `gorm:"not null;default:0"`
	Builtin   bool           `gorm:"not null;default:false"`
	CreatedAt time.Time      `gorm:"type:timestamptz"`
	UpdatedAt time.Time      `gorm:"type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (Vendor) TableName() string { return "vendors" }
