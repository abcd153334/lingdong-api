package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M52：给已有任务行补对外任务号（tasks.public_id）。
//
// 背景：对外任务号从自增十进制改成 `task_` + 26 位不透明串（新行由
// `model.Task.BeforeCreate` 生成 ULID）。库里的历史行没有号，必须回填，
// 否则 `GET /v1/tasks/task_xxx` 对老任务永远 404。
//
// 为什么回填用 `to_hex(id)` 而不是 ULID：迁移要可重复执行、结果稳定；
// 号只作等值查询的键（不解析内容），两种来源混用没有副作用，长度也一致（26）。
//
// 三部曲的顺序不能改：
//  1. 列与唯一索引已由 `AutoMigrate` 建好（新列全 NULL，PG 允许多个 NULL，所以
//     那一步不会因"非空 + 无默认值"失败）；
//  2. 本迁移回填；
//  3. 再 SET NOT NULL —— 反过来（先建非空列）会让已有数据的库直接起不来。
//
// 幂等：`data_migrations` 标记；UPDATE 带 `public_id IS NULL` 判据。
// 失败阻断启动（号缺失会让所有按号查询的入口失效）。
const migrationTasksPublicID = "m52_tasks_public_id"

// MigrateTasksPublicID 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateTasksPublicID(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasColumn(&model.Task{}, "public_id") {
		// 表还不存在（全新库）或 AutoMigrate 没跑到：直接跳过，
		// 让下一次启动按正常顺序处理，而不是在这里 DDL 出半张表。
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationTasksPublicID).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询任务号迁移标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	err := gdb.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
			UPDATE tasks
			   SET public_id = 'task_' || lower(lpad(to_hex(id), 26, '0'))
			 WHERE public_id IS NULL OR public_id = ''`).Error; err != nil {
			return fmt.Errorf("回填任务对外号: %w", err)
		}
		// 回填完才能加非空：空表也安全（没有行就没有违规）。
		if err := tx.Exec(`ALTER TABLE tasks ALTER COLUMN public_id SET NOT NULL`).Error; err != nil {
			return fmt.Errorf("设置任务对外号非空: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationTasksPublicID, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogTasksPublicID 只在真的执行过时打一行。
func LogTasksPublicID(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("任务对外号已回填（M52）", zap.String("at", time.Now().Format(time.RFC3339)))
}
