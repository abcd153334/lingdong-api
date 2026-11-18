package model

import (
	"time"

	"gorm.io/gorm"
)

// Package 对应 packages 表（充值套餐）。
//
// 字段语义：
//   - PriceCent 是售价，单位「分」—— 支付渠道的最小单位，直接下发不需要再换算。
//   - Bonus 是**赠送金额**（不是到账总额），单位「微」，与余额同单位。
//     到账 = money.FromCents(PriceCent) + Bonus，纯加法，没有倍率也没有刻度。
//     配送为 0 时门户不渲染"赠送"行（0 是"没有赠送"，不是"赠送 0 元"）。
//   - SortOrder 默认 1000 表示「未显式排序」：ORDER BY sort_order, price, id
//     自然退化为价格升序。运营改成 10 即置顶、2000 即置底。
//   - Badge 是门户卡片角标，取值 ""|recommend|hot|value；三种共用同一视觉，只换文案。
//
// 刻意没有 Currency 列：结算币种是全站锁定的（有账后不可改），
// 套餐价必然是站点币种，"每个套餐各自记币种"只会带来"改排序时静默把旧币种套餐
// 重新上架"这类事故（旧实现就有这个 bug），收益为零。
type Package struct {
	ID        uint64         `gorm:"primaryKey"`
	Name      string         `gorm:"size:64;not null"`
	PriceCent int64          `gorm:"column:price_cent;not null;default:0"`
	Bonus     int64          `gorm:"column:bonus;not null;default:0"`
	Enabled   bool           `gorm:"not null;default:true"`
	SortOrder int            `gorm:"not null;default:1000"`
	Badge     string         `gorm:"size:16;not null;default:''"`
	CreatedAt time.Time      `gorm:"type:timestamptz"`
	UpdatedAt time.Time      `gorm:"type:timestamptz"`
	DeletedAt gorm.DeletedAt `gorm:"index;type:timestamptz"`
}

func (Package) TableName() string { return "packages" }
