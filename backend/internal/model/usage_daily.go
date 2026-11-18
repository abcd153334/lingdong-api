package model

import "time"

// UsageDaily 对应 usage_daily 表（v2.0 §4.2.7）。Worker 每小时 UPSERT 当日、00:05 冻结前日。
type UsageDaily struct {
	Date     time.Time `gorm:"primaryKey;type:date"`
	Model    string    `gorm:"primaryKey;size:64"`
	Modality string    `gorm:"primaryKey;size:16"`
	UserID   uint64    `gorm:"primaryKey"`
	Requests int64
	// Errors 是失败请求数（released / rejected），requests 仍是**全部请求数**。
	//
	// 为什么不改 requests 的语义：stats_test.go 用
	// `requests == COUNT(*)` 把这个恒等式固化了，改语义会让「聚合与明细恒等」这条
	// 最重要的正确性约束失去意义（两者会各自漂移而测试仍绿）。
	// 失败率 = errors / NULLIF(requests, 0)，读刻度不改、口径多一维。
	Errors           int64 `gorm:"not null;default:0"`
	PromptTokens     int64
	CompletionTokens int64
	Charge           int64 `gorm:"column:charge;not null;default:0"`
	Cost             int64 `gorm:"column:cost;not null;default:0"`
}

func (UsageDaily) TableName() string { return "usage_daily" }
