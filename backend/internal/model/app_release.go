package model

import "time"

// AppRelease 是**单通道**版本发布记录：一个应用一条主通道。
//
// 为什么不做多通道/灰度：这一版是「最小可用」的版本管理 —— 版本号 + 更新说明 +
// 下载地址 + 是否强制更新 + 发布状态。多通道需要额外的分发策略模型与判定逻辑，
// 在只有一两个应用时纯属负担。
//
// 没有 DeletedAt：`status=3（已撤回）`承担"下架但不丢历史"的职责。
// 已发布/已撤回的行不可删除 —— 版本历史是排查"用户装的到底是哪个包"的唯一依据。
type AppRelease struct {
	ID    uint64 `gorm:"primaryKey"`
	AppID uint64 `gorm:"not null"`
	// Version 是 semver 文本（不解析成整数）：只校验格式，不做大小比较。
	//
	// 为什么不按版本号大小取"最新"：hotfix 回填（发 1.1.9 去补 1.2.0 的漏洞）
	// 是合法操作；按 semver 取最大会让线上静默降级。对外一律按发布时间取。
	Version      string `gorm:"size:32;not null"`
	ReleaseNotes string `gorm:"type:text;not null;default:''"`
	DownloadURL  string `gorm:"size:1024;not null;default:''"`
	ForceUpdate  bool   `gorm:"not null;default:false"`
	// Status 见下方常量。
	Status int16 `gorm:"not null;default:1"`
	// PublishedAt 只在"发布"时写入，撤回后**保留**（审计要知道它何时上过线）。
	PublishedAt *time.Time `gorm:"type:timestamptz"`
	CreatedAt   time.Time  `gorm:"type:timestamptz"`
	UpdatedAt   time.Time  `gorm:"type:timestamptz"`
}

// TableName 固定表名（不依赖 GORM 的复数推断）。
func (AppRelease) TableName() string { return "app_releases" }

// 版本状态。数字进库、进审计，不要随意调整。
const (
	// AppReleaseDraft 草稿：对外不可见，可编辑、可删除。
	AppReleaseDraft int16 = 1
	// AppReleasePublished 已发布：对外可见，可撤回，不可删除。
	AppReleasePublished int16 = 2
	// AppReleaseRevoked 已撤回：对外不可见，可重新发布，不可删除。
	AppReleaseRevoked int16 = 3
)
