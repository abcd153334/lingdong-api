package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M53：文档内容重播（只删接口页，节点保留）。
//
// 背景：`/v1` 的任务面从「富对象 + contents」改成 new-api 扁平结构 +
// 独立 artifacts 端点，7 个族的异步提交页、任务读面三篇、再生成页的
// 返回字段与状态码全变了。而 `docdocs.SeedDocs` 的规则是「只在不存在时插入」
// —— 内容是运营的，启动不许覆盖。
//
// 因此形状变更只能走「先删后播」：删掉 `origin='seed'` 的接口行（含 5 张子表），
// 启动期播种按新契约重建。运营手写的页（`origin='admin'`）一行不动。
//
// **不删节点**：节点文案本次不改，删节点会让运营挂在下面的接口变成孤儿
// （node_id 悬空、站点上直接消失）。新增的 compat 节点由 ensureNode 自动插入。
//
// 幂等：`data_migrations` 标记。失败阻断启动 —— 只删一半的库会让文档站
// 静默少页（没有报错、只是空白），比起不来难查得多。
const migrationDocsTaskShape = "m53_docs_task_shape"

// MigrateDocsTaskShape 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsTaskShape(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsTaskShape).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询文档重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	// 子表没有外键（历史上刻意不加），所以必须逐张显式删；漏一张就是孤儿行，
	// 而孤儿行不会报错，只会在下一次重播时被当成"已有内容"挡住。
	const seedEndpoints = `SELECT id FROM doc_endpoints WHERE origin = 'seed'`
	const seedFields = `SELECT f.id FROM doc_endpoint_fields f
	                       JOIN doc_endpoints e ON e.id = f.endpoint_id
	                      WHERE e.origin = 'seed'`

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			`DELETE FROM doc_field_enums WHERE field_id IN (` + seedFields + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + seedEndpoints + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + seedEndpoints + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + seedEndpoints + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + seedEndpoints + `)`,
			`DELETE FROM doc_endpoints WHERE origin = 'seed'`,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播文档种子: %w", err)
			}
		}
		// 节点若因为"最后一篇接口被删"变成草稿，重建后不会被自动发布
		// （seedOneNode 对已存在节点不碰状态）。这里把种子节点纠正成已发布：
		// 它们的可见性本来就该由新播种的接口决定。
		if err := tx.Exec(`
			UPDATE doc_nodes
			   SET status = 'published', published_at = COALESCE(published_at, now())
			 WHERE origin = 'seed' AND status <> 'published'`).Error; err != nil {
			return fmt.Errorf("恢复种子节点发布态: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationDocsTaskShape, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsTaskShape 只在真的执行过时打一行。
func LogDocsTaskShape(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("文档任务面已按扁平结构重播（M53）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
