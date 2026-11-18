package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M56：视频提交页按新的字段契约重播（只删视频提交页，其余文档一行不动）。
//
// 背景：视频提交的字段表从「每族各写一遍」改成「公共骨架 + 各族增量」。
// 已发布的视频页里存的还是旧字段表，而 `docdocs.SeedDocs` 的规则是
// 「已存在就跳过」—— 不先删，页面上会永远停在旧契约。
//
// 与 M53 的区别：M53 删的是**全部**种子接口页（当时任务面整体换了形状）；
// 这次只换视频提交，所以用 `key LIKE 'ep-%video-submit%'` 精确圈定 ——
// 全删会让全站文档在重启窗口里短暂消失，没有必要为一个板块付这个代价。
//
// 历史注记：本迁移当时同时新增过一个「视频生成」通用节点（`video-generate`），
// 该节点后来被下架 —— 删它的是 M57（它也要在播种之前跑，理由同上）。
//
// 幂等：`data_migrations` 标记。失败阻断启动 —— 只删一半的库会让文档站
// 静默少页（没有报错、只是空白），比起不来难查得多。
const migrationDocsVideoShape = "m56_docs_video_shape"

// MigrateDocsVideoShape 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsVideoShape(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsVideoShape).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询视频文档重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	// 子表没有外键（历史上刻意不加），所以必须逐张显式删；漏一张就是孤儿行，
	// 而孤儿行不会报错，只会在下一次重播时被当成"已有内容"挡住。
	const videoEndpoints = `SELECT id FROM doc_endpoints
	                          WHERE origin = 'seed' AND key LIKE 'ep-%video-submit%'`
	const videoFields = `SELECT f.id FROM doc_endpoint_fields f
	                       JOIN doc_endpoints e ON e.id = f.endpoint_id
	                      WHERE e.origin = 'seed' AND e.key LIKE 'ep-%video-submit%'`

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			`DELETE FROM doc_field_enums WHERE field_id IN (` + videoFields + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + videoEndpoints + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + videoEndpoints + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + videoEndpoints + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + videoEndpoints + `)`,
			`DELETE FROM doc_endpoints WHERE origin = 'seed' AND key LIKE 'ep-%video-submit%'`,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播视频文档种子: %w", err)
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
			Name: migrationDocsVideoShape, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsVideoShape 只在真的执行过时打一行。
func LogDocsVideoShape(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("视频提交页已按新字段契约重播（M56）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
