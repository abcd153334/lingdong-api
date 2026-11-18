package model

import "time"

// AppNotice 是**应用级公告**：只对使用该应用的终端用户可见。
//
// 与站点公告（system_config.announcements + announcement_reads）完全独立，
// 不要复用那一套：
//   - 站点公告面向门户/管理台的登录用户，有"已读"状态；
//   - 应用公告面向接入方的终端，不需要已读，但需要"生效窗口"与"级别"，
//     并且按 app_id 隔离 —— 混用会让 A 应用的公告出现在 B 应用的终端里。
//
// 没有 DeletedAt：公告是运营文案，误删可以从审计日志重建；加软删会让
// 每一次列表查询都要带 `deleted_at IS NULL`，而这条链路没有引用完整性需求。
type AppNotice struct {
	ID    uint64 `gorm:"primaryKey"`
	AppID uint64 `gorm:"not null"`
	// Title 是终端列表里的一行，过长会被截断，所以在入口就限制长度。
	Title string `gorm:"size:100;not null"`
	// Content 是纯文本：终端渲染环境不可控（WebView/原生），
	// 存 Markdown 或 HTML 会让"谁来转义"变成每个接入方各自回答的问题。
	Content string `gorm:"type:text;not null;default:''"`
	// Level 见下方常量；未知值在 service 层归一成 info。
	Level string `gorm:"size:16;not null;default:'info'"`
	// Enabled 是运营的"上下架"开关，与时间窗是**与**关系：
	// 只有 Enabled && 落在窗口内才下发。
	//
	// 带 default:true ⇒ 新建 enabled=false 必须插入后显式 Update，
	// 否则零值会被列默认值替换（与 apps.allow_self_issue 同源的坑）。
	Enabled bool `gorm:"not null;default:true"`
	// StartsAt/EndsAt 为 NULL 表示不限（立即生效 / 长期有效）。
	// 用时间窗而不是状态机：定时上架只能靠窗口表达，靠 cron 改状态会引入
	// "到点了但任务没跑"的静默失败。
	StartsAt  *time.Time `gorm:"type:timestamptz"`
	EndsAt    *time.Time `gorm:"type:timestamptz"`
	CreatedAt time.Time  `gorm:"type:timestamptz"`
	UpdatedAt time.Time  `gorm:"type:timestamptz"`
}

// TableName 固定表名（不依赖 GORM 的复数推断）。
func (AppNotice) TableName() string { return "app_notices" }

// 公告级别。取值进库也进对外 JSON，不要随意改名。
const (
	AppNoticeLevelInfo     = "info"
	AppNoticeLevelWarn     = "warn"
	AppNoticeLevelCritical = "critical"
)

// AppNoticeLevels 是合法的级别集合，校验入口唯一使用。
var AppNoticeLevels = map[string]bool{
	AppNoticeLevelInfo:     true,
	AppNoticeLevelWarn:     true,
	AppNoticeLevelCritical: true,
}
