package model

import "time"

// DataMigration 记录「一次性数据动作」的执行标记。
//
// 为什么需要它：`db.RunMigrations` 没有版本表，**每次启动都重跑**。凡是「只应该
// 做一次」的数据动作（删除历史占位渠道、按新结构改写存量 JSON）都必须先查标记，
// 否则运营手工恢复的数据会在下次重启时被再次删掉 —— 外部表现就是「删了又回来」。
//
// 与 AutoMigrate 的分工：AutoMigrate 管表结构（幂等），这张表管数据动作是否已执行。
type DataMigration struct {
	// Name 是迁移标识（如 `m11_pay_channels_remove_placeholders`）；主键保证只落一行。
	Name string `gorm:"size:96;primaryKey"`
	// AppliedAt 供排障时确认「这条数据动作是什么时候跑的」。
	AppliedAt time.Time `gorm:"type:timestamptz;not null"`
}

// TableName 固定表名（GORM 默认复数化会得到 data_migrations，这里显式声明防止改名）。
func (DataMigration) TableName() string { return "data_migrations" }
