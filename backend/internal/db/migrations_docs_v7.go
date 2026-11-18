package db

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// MigrateDocsV7DropLegacy 删除文档站 v6 的五张表。
//
// 为什么可以硬删：这五张表里只有「组结构 / 成员归属 / 组文案 / 说明页 / 接口文案」，
// 全部由启动期的 `Reconcile` + `EnsureGenericDocs` + `SeedPages` 从契约**派生**出来
// （见 v6 的 docdocs 包注释），没有任何一列是人工录入后无法重建的决策数据。
// 运营真正写过的文字在 v7 的 `doc_*` 内容表里 —— M45 之后文本已按 v7 的节点/接口重建。
//
// 为什么不落 data_migrations 标记：`DROP TABLE IF EXISTS` 本身就是幂等的，
// 标记只会多出一个失败态（标记写进去了、删表失败 → 永远不再删）。
// 这里改成「先看表在不在」，只为了日志能说清这次到底动了没有。
func MigrateDocsV7DropLegacy(gdb *gorm.DB) (bool, error) {
	// 不写 schema 前缀：靠 search_path 解析。写死 `public.` 会让测试库（独立 schema）
	// 与自定义 schema 的部署都判成"没有旧表"，而真正的旧表还留在库里。
	var exists bool
	if err := gdb.Raw(`SELECT to_regclass('doc_groups') IS NOT NULL`).Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("db: 检查旧文档表是否存在失败: %w", err)
	}
	if !exists {
		return false, nil
	}
	// 一条语句删五张表：子表在前，避免任何潜在的引用顺序问题；
	// 不加 CASCADE —— 若真有别的对象依赖它们，宁可让迁移失败并被人看见。
	err := gdb.Exec(`DROP TABLE IF EXISTS
		doc_group_members,
		doc_group_texts,
		doc_pages,
		doc_endpoint_docs,
		doc_groups`).Error
	if err != nil {
		return false, fmt.Errorf("db: 删除旧文档表失败: %w", err)
	}
	return true, nil
}

// LogDocsV7DropLegacy 打一行说明（删表只跑一次，出事后的唯一线索就是它）。
func LogDocsV7DropLegacy(applied bool) {
	if !applied {
		return
	}
	logger.L().Info("已删除旧文档表（doc_groups / doc_group_members / doc_group_texts / doc_pages / doc_endpoint_docs）",
		zap.String("reason", "文档内容已全部迁到 v7 的 doc_* 九张表"))
}
