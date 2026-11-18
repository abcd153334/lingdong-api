package model

import "time"

// Upload 对应 uploads 表（v2.0 §4.2.10）。开发者文件上传，主键为 ULID；无软删除列。
type Upload struct {
	ID          string `gorm:"size:26;primaryKey"`
	UserID      uint64 `gorm:"index;not null"`
	KeyID       uint64 `gorm:"index"`
	Filename    string `gorm:"size:255"`
	SafeName    string `gorm:"size:80"`
	SizeBytes   int64  `gorm:"not null;default:0"`
	ContentType string `gorm:"size:100"`
	// StorageBackend / StorageKey 与 files、icons 三张表同形（见 storage 包的三条约定）：
	// **每条记录自己带后端标识**，读/删按它路由，永不看当前配置里的 active。
	//
	// not null + default '' 不是排版细节：允许 NULL 的表会让
	// `storage_backend IN (...)` 与 `storage_backend <> ''` 两种谓词在两张表上
	// 得到不同结果（NULL 既不满足 IN 也不满足 <>），清理任务的过滤条件因此要
	// 分表写两遍 —— 统一成 '' 才能只维护一个谓词。
	StorageBackend string `gorm:"size:16;not null;default:''"`
	// StorageBucket 是写入时该后端的桶（本地后端为空串），与 backend/key 一起构成
	// 对象位置的唯一真相。删除必须按它路由：换桶之后历史对象仍在旧桶里，
	// 用当前配置的桶去删会得到 404，而 404 很容易被当成「已经删干净了」。
	StorageBucket string    `gorm:"size:128;not null;default:''"`
	StorageKey    string    `gorm:"size:512"`
	CreatedAt     time.Time `gorm:"type:timestamptz"`
	ExpiresAt     time.Time `gorm:"type:timestamptz;index"`
}

func (Upload) TableName() string { return "uploads" }
