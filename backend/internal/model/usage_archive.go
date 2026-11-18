package model

// UsageArchive 是 usages 的归档表（冷热分离，v2.0 §4.2.11 / T10）。
// 与主表同构（内嵌 Usage 复用全部列与索引口径），超期明细迁入后按 archive_retention_days 清理。
type UsageArchive struct {
	Usage
}

func (UsageArchive) TableName() string { return "usages_archive" }
