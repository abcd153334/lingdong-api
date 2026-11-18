package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M61：把 `usages.upstream_spec`（含归档表）加宽到 32。
//
// 背景：`model.Usage.UpstreamSpec` 的列宽是 16，而 `billing.upstreamSpecMaxLen`
// 与写入侧的注释都按 32 设计（档位键上限就是 32 字符）。上游回报 17–32 字符的
// 尺寸原文时，Settle 的 UPDATE 会因 varchar(16) 报 `value too long for type
// character varying(16)` —— 整个结算事务回滚，usage 停在 pending，
// 20 分钟后被 UsageReclaimJob 按「回收」口径重算（而回收期还缺维度列，
// 那正是 M60 之前另一条少收路径）。
//
// 为什么不选「写入侧截断到 16」：截断出来的 `2048x11` 看起来像正常值，
// 事后无法分辨是上游本来就给了半截，还是平台截的 —— 丢证据比报错更难查。
//
// 为什么必须显式迁移而不是只改 tag：AutoMigrate 不会把 varchar(16) 收窄/放宽，
// 老库上的列宽不会跟着变（它只在缺列时建列）。
//
// 幂等：`data_migrations` 标记 + `ALTER ... TYPE` 本身可重复执行。
const migrationUsagesUpstreamSpec = "m61_usages_upstream_spec"

// MigrateUsagesUpstreamSpec 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateUsagesUpstreamSpec(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.Usage{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationUsagesUpstreamSpec).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询 usages.upstream_spec 加宽标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	tables := []string{"usages"}
	if gdb.Migrator().HasTable(&model.UsageArchive{}) {
		tables = append(tables, "usages_archive")
	}

	err := gdb.Transaction(func(tx *gorm.DB) error {
		for _, tbl := range tables {
			// 已经是 varchar(32) 时这条语句是 no-op（改到同类型不重写表）。
			if err := tx.Exec(fmt.Sprintf(
				`ALTER TABLE %s ALTER COLUMN upstream_spec TYPE varchar(32)`, tbl)).Error; err != nil {
				return fmt.Errorf("db: 加宽 %s.upstream_spec 失败: %w", tbl, err)
			}
		}
		return tx.Create(&model.DataMigration{
			Name: migrationUsagesUpstreamSpec, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogUsagesUpstreamSpec 只在真的执行过时打一行。
func LogUsagesUpstreamSpec(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("usages.upstream_spec 已加宽到 32（M61）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
