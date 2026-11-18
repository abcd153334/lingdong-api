package db

import (
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// M39：给 uploads / files / icons 补 `storage_bucket`，并回填当前生效的桶。
//
// 为什么必须补：对象位置原来只有 `storage_backend + storage_key`。运营把
// `channels.oss.bucket` 从 A 改成 B 之后，历史行仍指向 A，但删除会用**当前配置**的
// B 去删 A 的 key —— 404 被当成「已经不存在」，于是 DB 行被删、对象永久留在 A，
// 且再无任何线索能定位它。补上 bucket 后，读/删一律按记录路由。
//
// 为什么不留「旧行没有桶」的兼容分支：本项目处于全新开发阶段、没有需要保留的用户数据
// （见项目约定）。留一个 quarantine/assume 开关就等于常驻的第二条路径，
// 而第二条路径迟早会被当成默认。
//
// 幂等判据：只更新 `storage_bucket = ”` 的行。新代码写入必带 bucket，
// 所以跑过一次之后恒为 0 行，不需要额外的 data_migrations 标记。
type StorageBucketReport struct {
	// Skipped 表示当前没有云后端（active 为空或 none），无需回填。
	Skipped bool
	// Bucket 是本次回填使用的桶名。
	Bucket string
	// ByTable 是各表回填行数（uploads / files / icons）。
	ByTable map[string]int64
	// LeftEmpty 是回填后仍为空的行数合计（后端不匹配当前 active 的行，需人工确认）。
	LeftEmpty int64
}

// MigrateStorageBucket 加列并回填；dryRun 只判断不写。
func MigrateStorageBucket(gdb *gorm.DB, dryRun bool) (StorageBucketReport, error) {
	rep := StorageBucketReport{ByTable: map[string]int64{}}

	backend, bucket, err := activeStorageBucket(gdb)
	if err != nil {
		return rep, err
	}
	if backend == "" || backend == "none" || bucket == "" {
		rep.Skipped = true
		return rep, nil
	}
	rep.Bucket = bucket

	if !dryRun {
		for _, table := range []string{"uploads", "files", "icons"} {
			if err := gdb.Exec(fmt.Sprintf(
				`ALTER TABLE %s ADD COLUMN IF NOT EXISTS storage_bucket varchar(128) NOT NULL DEFAULT ''`, table,
			)).Error; err != nil {
				return rep, fmt.Errorf("db: M39 给 %s 加 storage_bucket 失败: %w", table, err)
			}
		}
	}

	for _, table := range []string{"uploads", "files", "icons"} {
		if dryRun {
			var n int64
			if err := gdb.Raw(fmt.Sprintf(
				`SELECT count(*) FROM %s WHERE storage_backend = ? AND storage_bucket = ''`, table,
			), backend).Scan(&n).Error; err != nil {
				return rep, fmt.Errorf("db: M39 统计 %s 待回填行失败: %w", table, err)
			}
			rep.ByTable[table] = n
			continue
		}
		res := gdb.Exec(fmt.Sprintf(
			`UPDATE %s SET storage_bucket = ? WHERE storage_backend = ? AND storage_bucket = ''`,
			table), bucket, backend)
		if res.Error != nil {
			return rep, fmt.Errorf("db: M39 回填 %s 失败: %w", table, res.Error)
		}
		rep.ByTable[table] = res.RowsAffected
	}

	if !dryRun {
		for _, table := range []string{"uploads", "files", "icons"} {
			var n int64
			if err := gdb.Raw(fmt.Sprintf(`SELECT count(*) FROM %s WHERE storage_bucket = ''`, table)).
				Scan(&n).Error; err != nil {
				return rep, fmt.Errorf("db: M39 统计 %s 残留空桶行失败: %w", table, err)
			}
			rep.LeftEmpty += n
		}
	}
	return rep, nil
}

// activeStorageBucket 读当前 storage 配置里的 active 后端与它的桶。
func activeStorageBucket(gdb *gorm.DB) (backend, bucket string, err error) {
	var raw string
	if err := gdb.Raw(`SELECT value::text FROM system_config WHERE key = 'storage'`).Scan(&raw).Error; err != nil {
		return "", "", fmt.Errorf("db: M39 读取 storage 配置失败: %w", err)
	}
	if raw == "" {
		return "", "", nil
	}
	var cfg struct {
		Active   string                            `json:"active"`
		Channels map[string]map[string]interface{} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", "", fmt.Errorf("db: M39 storage 配置不是合法 JSON: %w", err)
	}
	ch := cfg.Channels[cfg.Active]
	if v, ok := ch["bucket"].(string); ok {
		bucket = v
	}
	return cfg.Active, bucket, nil
}

// LogStorageBucketMigration 打印 M39 结果；无事可做时保持安静。
func LogStorageBucketMigration(rep StorageBucketReport) {
	if rep.Skipped {
		return
	}
	logger.L().Info("db: 对象位置已补 storage_bucket",
		zap.String("bucket", rep.Bucket), zap.Any("filled", rep.ByTable))
	if rep.LeftEmpty > 0 {
		// 这些行的后端不是当前 active（或写路径漏记），位置无法自动推断。
		// 不猜：让它们显式停在清理的 blocked 计数里，由运维确认后再处理。
		logger.L().Warn("db: 仍有对象的 storage_bucket 为空，清理会跳过并计入 blocked",
			zap.Int64("rows", rep.LeftEmpty))
	}
}
