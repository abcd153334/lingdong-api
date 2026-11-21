package db

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// M64：同步链路的幂等键（usages.idem_key / idem_scope + 部分唯一索引）。
//
// 背景：异步提交（`POST /v1/video/generations` 等）早在 B4-2 就有幂等键，
// 落在 tasks 表上；而**同步**端点（图片/音频/对话）没有任何幂等 —— 客户端 SDK
// 超时重试一次就是一次新的冻结与扣费，两次结果都留在账上，用户看到"我只发了一次"。
//
// 为什么落在 usages：同步调用没有 tasks 行，"这次请求已经扣过钱"的唯一凭据
// 就是计费记录本身；写在同一个事务里，唯一索引才能和余额变更同生共死。
//
// 索引必须是**部分唯一**（`WHERE idem_key <> ''`）：
// 绝大多数请求不带这个头，列默认空串，普通唯一索引会让第二条无键请求直接冲突。
const migrationUsagesIdemKey = "m64_usages_idem_key"

// MigrateUsagesIdemKey 建列与索引（幂等：可重复执行）。
func MigrateUsagesIdemKey(gdb *gorm.DB) error {
	if !gdb.Migrator().HasTable(&model.Usage{}) {
		return nil
	}
	stmts := []string{
		`ALTER TABLE usages ADD COLUMN IF NOT EXISTS idem_key varchar(128) NOT NULL DEFAULT ''`,
		`ALTER TABLE usages ADD COLUMN IF NOT EXISTS idem_scope varchar(64) NOT NULL DEFAULT ''`,
		// 作用域 + 键 + 密钥：同一个 Key 换一个 API Key 重放不应被拦（不同调用方向）。
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_usages_idem
		   ON usages (key_id, idem_scope, idem_key)
		 WHERE idem_key <> ''`,
	}
	for _, q := range stmts {
		if err := gdb.Exec(q).Error; err != nil {
			return fmt.Errorf("M64 建幂等列/索引: %w", err)
		}
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationUsagesIdemKey).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			return gdb.Create(&model.DataMigration{
				Name: migrationUsagesIdemKey, AppliedAt: time.Now(),
			}).Error
		}
	}
	return nil
}

