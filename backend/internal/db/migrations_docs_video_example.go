package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M58：视频提交页按**示例与素材边界**重播。
//
// 背景（全部来自真机测试）：
//  1. 示例里写的是 `"model": "MiniMax-H3"` —— 对外合法 id 是 `minimax-h3`，
//     照抄示例直接 404 model_not_found。文档第一屏就是错的。
//  2. 素材规格缺两条上游硬边界：图片短边 ≥ 256px、参考视频 2–15.5 秒。
//     低于边界会被上游拒绝（真机 E2/E3 实测），而页面没提。
//  3. 兼容写法表新增 `768p → 720p`（上游档位名吸附）。
//
// 为什么不能只改代码：`docdocs.SeedDocs` 的规则是「已存在就跳过」。
// 播种内容变了、库里已发布的页不会跟着变，站点会继续显示旧文案且不报错。
//
// 与 M57 的差别：这次不删任何节点（`video-generate` 早已下架），
// 只重播接口页 —— 影响面比 M57 更小。
//
// 幂等：`data_migrations` 标记 + 全部语句是 `DELETE ... WHERE`。
const migrationDocsVideoExample = "m58_docs_video_example"

// MigrateDocsVideoExample 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsVideoExample(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsVideoExample).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询视频示例重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	// 复用 M57 的目标圈定：所有种子来源的视频提交页（含三族）。
	const endpointIDs = `SELECT e.id FROM doc_endpoints e WHERE ` + videoSeedTargets
	const fieldIDs = `SELECT f.id FROM doc_endpoint_fields f
	                    JOIN doc_endpoints e ON e.id = f.endpoint_id
	                   WHERE ` + videoSeedTargets

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			`DELETE FROM doc_field_enums WHERE field_id IN (` + fieldIDs + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoints e WHERE ` + videoSeedTargets,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播视频示例种子: %w", err)
			}
		}
		// 被删空的节点会被自动转成草稿，这里纠正回发布态。
		if err := tx.Exec(`
			UPDATE doc_nodes
			   SET status = 'published', published_at = COALESCE(published_at, now())
			 WHERE origin = 'seed' AND status <> 'published'`).Error; err != nil {
			return fmt.Errorf("恢复种子节点发布态: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationDocsVideoExample, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsVideoExample 只在真的执行过时打一行。
func LogDocsVideoExample(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("视频提交页已按示例与素材边界重播（M58）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
