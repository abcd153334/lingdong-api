package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M50：接口文档从「一份共享模板 + 按族删字段」改成「每族一份完整文档」后的一次性重置。
//
// 为什么要再清一次：M49 之后库里的内容是按**旧结构**播种出来的，
// 那批行有三个无法就地修补的特征：
//   - 同一路由在三个图像族里逐字相同（字段表由"删掉别家的"得到）；
//   - 响应变体、示例、预设都是 Kind 级共享的（Seedream 的 model 出现在 Nano Banana 页上）；
//   - 任务读面在 8 个族里各存一份（同一路径 8 个 URL）。
//
// 这些是"行与行之间的关系"，不是列值 —— 回填补不了。当前没有外部用户数据，
// 清空重建是唯一能让库与种子逐字一致的做法。
//
// **失败阻断启动**：只清一半的库会让文档站静默少内容（不报错、只是空白或重复页）。
const migrationDocsFamilySplit = "m50_docs_family_split"

// MigrateDocsFamilySplit 只执行一次（`data_migrations` 标记）。
func MigrateDocsFamilySplit(gdb *gorm.DB) (bool, error) {
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsFamilySplit).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询迁移标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	err := gdb.Transaction(func(tx *gorm.DB) error {
		// 与 M49 同一组表、同一顺序：播种会按新的族清单重新填满。
		if err := tx.Exec(`TRUNCATE TABLE
			doc_field_enums, doc_response_variants, doc_presets,
			doc_endpoint_errors, doc_endpoint_fields, doc_endpoints,
			doc_nodes, doc_sections
			RESTART IDENTITY CASCADE`).Error; err != nil {
			return fmt.Errorf("清空文档内容表: %w", err)
		}
		// 接口标识从「节点内唯一」改成「全站唯一」：三族的同名页面现在各有自己的
		// 标识（`ep-images-gpt-image-images-generations` 等）。旧索引必须在这里
		// 真正换掉 —— EnsureIndexes 跑在本次迁移之前，旧库里的重复 key 让它建不出来。
		if err := tx.Exec(`DROP INDEX IF EXISTS uq_doc_endpoints_key`).Error; err != nil {
			return fmt.Errorf("删除旧的接口标识索引: %w", err)
		}
		if err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_doc_endpoints_key_v2
			ON doc_endpoints (key) WHERE key <> ''`).Error; err != nil {
			return fmt.Errorf("建立全站唯一的接口标识索引: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationDocsFamilySplit, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsFamilySplit 只在真的重置过时打一行（正常重启不刷日志）。
func LogDocsFamilySplit(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("文档内容已按「每族一份完整文档」重建（M50）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
