package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M55：给任务补幂等键的**端点作用域**（tasks.idempotency_endpoint）。
//
// 背景：幂等键原来只按 (key_id, idempotency_key) 去重，于是「同一把钥、同一个
// Idempotency-Key」先提音乐再提视频时，第二次请求会直接拿回那首音乐任务
// —— 不报错、模态不符、扣费与产物全错。新唯一索引是
// `uq_tasks_idem_v2 (key_id, idempotency_endpoint, idempotency_key)`。
//
// 为什么必须回填 NULL → ”：Go 侧新行写入空串，而历史行是 SQL NULL；
// 查询判据写的是 `idempotency_endpoint = ”`，NULL 永远不相等 —— 不回填就等于
// 上线后第一次重试绕过旧行、再建一单（正是幂等键要防的事）。
//
// 三部曲与 M52 相同：AutoMigrate 建列（全 NULL）→ 本迁移回填并设默认值 →
// 索引由 EnsureIndexes 建（它跑在迁移之后）。顺序反了会让已有数据的库起不来。
//
// 幂等：`data_migrations` 标记；UPDATE 带 `IS NULL` 判据。
const migrationTasksIdemEndpoint = "m55_tasks_idem_endpoint"

// MigrateTasksIdemEndpoint 只执行一次，返回是否真的执行过。
func MigrateTasksIdemEndpoint(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasColumn(&model.Task{}, "idempotency_endpoint") {
		// 表还不存在（全新库）或 AutoMigrate 没跑到：跳过，让下次启动按序处理。
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationTasksIdemEndpoint).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询幂等端点迁移标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	err := gdb.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
			UPDATE tasks SET idempotency_endpoint = ''
			 WHERE idempotency_endpoint IS NULL`).Error; err != nil {
			return fmt.Errorf("回填幂等端点: %w", err)
		}
		// 默认值 + 非空：新行即使走原生 SQL 插入也不会再落 NULL，
		// 查询判据（= ''）对历史行与新行口径一致。
		if err := tx.Exec(`ALTER TABLE tasks ALTER COLUMN idempotency_endpoint SET DEFAULT ''`).Error; err != nil {
			return fmt.Errorf("设置幂等端点默认值: %w", err)
		}
		if err := tx.Exec(`ALTER TABLE tasks ALTER COLUMN idempotency_endpoint SET NOT NULL`).Error; err != nil {
			return fmt.Errorf("设置幂等端点非空: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationTasksIdemEndpoint, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogTasksIdemEndpoint 只在真的执行过时打一行。
func LogTasksIdemEndpoint(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("任务幂等端点已回填（M55）", zap.String("at", time.Now().Format(time.RFC3339)))
}
