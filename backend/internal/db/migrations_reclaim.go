package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// MigrationNameReclaimAnchor 是「回收锚点回填」的数据动作标识。
const MigrationNameReclaimAnchor = "m36_reclaim_anchor"

// reclaimIndexes 是回收扫描要用的索引（全部 CONCURRENTLY）。
//
// 为什么不放进 EnsureIndexes：那批是建表期索引，普通 CREATE INDEX 会持 SHARE 锁
// 阻塞全表写入；usages 在生产上可能几千万行，一次启动就把网关打到全站 502。
// CONCURRENTLY 不阻塞读写，代价是不能放进事务、失败也不能阻断启动。
var reclaimIndexes = []struct {
	name string
	sql  string
}{
	{
		// 扫描语句是 `WHERE status='pending' AND created_at<? AND id>? ORDER BY id`：
		// 旧索引 idx_usages_pending 建在 (created_at) 上，与 ORDER BY id 不匹配，
		// PG 只能取出全部 pending 再排序。pending 集合本身很小，但对 (id) 的部分索引
		// 让游标翻页变成纯索引范围扫。
		name: "idx_usages_pending_id",
		sql:  `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usages_pending_id ON usages (id) WHERE status = 'pending'`,
	},
}

// ensureReclaimIndexes 创建回收扫描索引（幂等、失败只告警）。
//
// 自愈逻辑与 M18 一致：CONCURRENTLY 中途失败会留下 indisvalid=false 的同名索引，
// 之后 IF NOT EXISTS 永远跳过它 —— 索引看起来"建过"，实际不可用。
func ensureReclaimIndexes(gdb *gorm.DB) {
	failed := 0
	for _, idx := range reclaimIndexes {
		if valid, exists := indexValid(gdb, idx.name); exists && !valid {
			if err := gdb.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + idx.name).Error; err != nil {
				logger.L().Warn("M36 删除失效索引失败（下次启动重试）",
					zap.String("index", idx.name), zap.Error(err))
				failed++
				continue
			}
		}
		if err := gdb.Exec(idx.sql).Error; err != nil {
			logger.L().Warn("M36 创建索引失败（下次启动重试）",
				zap.String("index", idx.name), zap.Error(err))
			failed++
		}
	}
	if failed == 0 {
		logger.L().Info("回收扫描索引已就绪")
	}
}

// MigrateReclaimAnchor 回填 usages.task_id（legacy 行）。
//
// 背景：回收任务改用 `usages.task_id` 作为唯一锚点（tasks.usage_id 会被重试改写）。
// 但历史行可能只有 tasks.usage_id 指向它、usages.task_id 仍为 0 —— 这类行在新 SQL 里
// 会落到「没有任务」分支：方向保守（退款），但会把「该补结的交付单」当成孤儿。
//
// 因此回填与代码**同版本发布**：只处理 pending 行（终态行的回收路径不会再看锚点）。
// 幂等：data_migrations 标记 + `u.task_id = 0` 判据；重跑不二次放大。
func MigrateReclaimAnchor(gdb *gorm.DB, dryRun bool) (int64, error) {
	if !gdb.Migrator().HasTable("usages") || !gdb.Migrator().HasTable("tasks") {
		return 0, nil
	}
	if !dryRun {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", MigrationNameReclaimAnchor).Count(&n).Error; err != nil {
			return 0, fmt.Errorf("db: 查询回收锚点迁移标记失败: %w", err)
		}
		if n > 0 {
			return 0, nil
		}
	}

	// 只回填 pending：终态行已经不会再被回收扫描，回填它们纯属白写。
	// 条件里带 t.usage_id <> 0 是防御性的（0 是"没有会话"的零值，不是锚点）。
	q := `UPDATE usages u SET task_id = t.id
	        FROM tasks t
	       WHERE u.task_id = 0
	         AND u.status = 'pending'
	         AND t.usage_id = u.id
	         AND t.usage_id <> 0
	         AND t.deleted_at IS NULL`
	if dryRun {
		var n int64
		if err := gdb.Raw(`SELECT count(*) FROM usages u
		                     JOIN tasks t ON t.usage_id = u.id
		                    WHERE u.task_id = 0 AND u.status = 'pending'
		                      AND t.usage_id <> 0 AND t.deleted_at IS NULL`).
			Scan(&n).Error; err != nil {
			return 0, err
		}
		return n, nil
	}
	res := gdb.Exec(q)
	if res.Error != nil {
		// 阻断启动：回填缺失会让"该补结的交付单"被当成孤儿退款，是真实少收。
		return 0, fmt.Errorf("db: 回填回收锚点失败: %w", res.Error)
	}
	if err := gdb.Create(&model.DataMigration{
		Name: MigrationNameReclaimAnchor, AppliedAt: time.Now(),
	}).Error; err != nil {
		return res.RowsAffected, fmt.Errorf("db: 写入回收锚点迁移标记失败: %w", err)
	}
	if res.RowsAffected > 0 {
		logger.L().Info("已回填回收锚点（usages.task_id）", zap.Int64("rows", res.RowsAffected))
	}
	return res.RowsAffected, nil
}
