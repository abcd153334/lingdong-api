package db

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"go.uber.org/zap"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M49：文档 schema 变形后的一次性重置。
//
// 为什么是"清空重建"而不是"逐列回填"：这一轮同时做了四件结构改动
//   - 板块从 Go 常量变成 `doc_sections` 表；
//   - 示例代码从投影期现算变成 `doc_endpoints.samples` / `doc_presets.samples` 列；
//   - 示例表 `doc_examples` 删除（前台从不渲染它，内容各有更好的归属）；
//   - 代表模型列删除（model 属于字段示例值，不再有第二个来源）。
//
// 回填只能补齐"缺的列"，补不了"多余的旧行"，也补不了"行内文案与契约已分叉"。
// 当前是全新开发阶段、没有用户数据，清空重建是唯一能让库与种子逐字一致的做法；
// 之后播种只看"行存不存在"，库就是唯一真相源。
//
// **失败阻断启动**：只重置了一半的库会让文档站静默少内容 —— 不报错、只是空白，
// 这种状态比启动失败更难被发现。
const migrationDocsReset = "m49_docs_reset"

// MigrateDocsReset 只执行一次（`data_migrations` 标记）。
// 返回 true 表示这次真的执行了重置。
func MigrateDocsReset(gdb *gorm.DB) (bool, error) {
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsReset).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询迁移标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	err := gdb.Transaction(func(tx *gorm.DB) error {
		// 1) 示例表整张删除（不再是模型的一部分，留着只会成为"没人读的第二份内容"）。
		if err := tx.Exec(`DROP TABLE IF EXISTS doc_examples`).Error; err != nil {
			return fmt.Errorf("删除 doc_examples: %w", err)
		}
		// 2) 内容表清空并重置自增：播种会按种子把 8 张表重新填满。
		//    CASCADE 兜住子表外键/孤儿行；RESTART IDENTITY 让 id 从 1 开始。
		if err := tx.Exec(`TRUNCATE TABLE
			doc_field_enums, doc_response_variants, doc_presets,
			doc_endpoint_errors, doc_endpoint_fields, doc_endpoints,
			doc_nodes, doc_sections
			RESTART IDENTITY CASCADE`).Error; err != nil {
			return fmt.Errorf("清空文档内容表: %w", err)
		}
		// 3) 代表模型列删除。GORM 不会因为模型里删了字段就删列，必须显式 DROP。
		has, err := columnExists(tx, "doc_nodes", "representative")
		if err != nil {
			return err
		}
		if has {
			if err := tx.Exec(`ALTER TABLE doc_nodes DROP COLUMN representative`).Error; err != nil {
				return fmt.Errorf("删除 doc_nodes.representative: %w", err)
			}
		}
		// 4) 落标记：重跑时直接跳过。
		return tx.Create(&model.DataMigration{
			Name: migrationDocsReset, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsReset 只在真的重置过时打一行：正常重启不应该刷这条日志。
func LogDocsReset(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("文档内容已按新结构重置并重建（M49）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
