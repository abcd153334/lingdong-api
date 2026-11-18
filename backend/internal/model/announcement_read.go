package model

import "time"

// AnnouncementRead 记录用户对站点公告的已读状态（S10）。
// 以公告 id 为锚（键 `id:<id>`）：同一 id 编辑内容不重现，新增 id 视为未读重现。
type AnnouncementRead struct {
	UserID       uint64    `gorm:"primaryKey;autoIncrement:false"`
	Announcement string    `gorm:"primaryKey;size:64"`
	CreatedAt    time.Time `gorm:"type:timestamptz"`
}

func (AnnouncementRead) TableName() string { return "announcement_reads" }
