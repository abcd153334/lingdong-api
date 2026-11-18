package db

import (
	"fmt"

	"gorm.io/gorm"

	"go.uber.org/zap"
	"lingdong-api/internal/pkg/logger"
)

// M37：清掉 storage 配置里的三个死键。
//
// 这三个键曾经是本项目的正式配置项，现已全部删除：
//   - retain_local_days：零消费（本地副本提前清理从未接线）；
//   - direct_redirect / direct_redirect_ttl：预签名 302 整套被删除，
//     地址改由 storage.PublicURL 按记录后端派生（桶公开读，永久直链）。
//
// 留着它们不是"无害的脏数据"：配置页不再渲染，运营看到的值与实际行为无关，
// 而排查「为什么没走 302」时会先怀疑这个键 —— 一次误判的成本远高于删掉它。
//
// 幂等：jsonb 的 `-` 对不存在的键是空操作，每次启动重跑无副作用。
type StorageMigrationReport struct {
	// StripedRows 是本次（或 dry-run 下将要）清理的配置行数：0 或 1。
	StripedRows int
}

// storageDeadKeys 是待删除的键（单一清单，SQL 与报告共用）。
var storageDeadKeys = []string{"retain_local_days", "direct_redirect", "direct_redirect_ttl"}

// MigrateStorageKeys 删除 storage 配置里的死键；dryRun 只统计不写。
func MigrateStorageKeys(gdb *gorm.DB, dryRun bool) (StorageMigrationReport, error) {
	var rep StorageMigrationReport
	var n int64
	if err := gdb.Raw(`SELECT count(*) FROM system_config
		WHERE key = 'storage'
		  AND (value ? 'retain_local_days' OR value ? 'direct_redirect' OR value ? 'direct_redirect_ttl')`).
		Scan(&n).Error; err != nil {
		return rep, fmt.Errorf("db: M37 检查 storage 死键失败: %w", err)
	}
	if n == 0 || dryRun {
		rep.StripedRows = int(n)
		return rep, nil
	}
	if err := gdb.Exec(`UPDATE system_config
		SET value = value - 'retain_local_days' - 'direct_redirect' - 'direct_redirect_ttl',
		    updated_at = now()
		WHERE key = 'storage'`).Error; err != nil {
		return rep, fmt.Errorf("db: M37 清理 storage 死键失败: %w", err)
	}
	rep.StripedRows = int(n)
	return rep, nil
}

// LogStorageMigration 打印 M37 结果；无事可做时保持安静（每次启动都会跑）。
func LogStorageMigration(rep StorageMigrationReport) {
	if rep.StripedRows == 0 {
		return
	}
	logger.L().Info("db: 已清理 storage 配置的历史死键",
		zap.Int("rows", rep.StripedRows), zap.Strings("keys", storageDeadKeys))
}
