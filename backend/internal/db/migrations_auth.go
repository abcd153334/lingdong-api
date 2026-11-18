package db

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// AuthMigrationReport 是 M33 的执行结果。
type AuthMigrationReport struct {
	// DroppedTokenVersion 表示本次删掉了 users.token_version 列。
	DroppedTokenVersion bool
	// DroppedConfigKeys 是本次删除的旧配置键数量。
	DroppedConfigKeys int
	// DroppedAffCodeIndex 表示本次删掉了旧的全集唯一索引 idx_users_aff_code。
	DroppedAffCodeIndex bool
}

// MigrateAuth 是 M33：认证重构的一次性数据动作。
//
// 只做两件**无法由 AutoMigrate 表达**的事：
//  1. 删掉 users.token_version —— 凭证吊销的唯一真相源改成 user_sessions 之后，
//     留着这一列就是第二个吊销开关，漏写任何一处都会让「改密后旧凭证仍可用」
//     悄悄存在。GORM 的 AutoMigrate 只加不删，必须显式 DROP。
//  2. 删掉 reg_captcha_enabled / login_lock_enabled —— 它们由 auth_captcha /
//     auth_login 取代。两个开关并存时，运营改了新键却发现旧键还在生效。
//  3. 删掉 users.aff_code 上的**全集**唯一索引 —— 它是 GORM `uniqueIndex` tag
//     建的，而 aff_code 的「未生成」状态就是空串，于是第二个空串用户插入即
//     23505（并发注册随机失败）。正确形态是部分唯一索引
//     `uq_users_aff_code ... WHERE aff_code <> ”`（由 EnsureIndexes 建）。
//
// 新配置键的默认值**不在这里插**：默认值只有一个来源（db/seed.go），
// 迁移只负责删除，避免「两处都能造出默认值」。
func MigrateAuth(gdb *gorm.DB, dryRun bool) (AuthMigrationReport, error) {
	var rep AuthMigrationReport

	var hasCol bool
	if err := gdb.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'users' AND column_name = 'token_version')`).Scan(&hasCol).Error; err != nil {
		return rep, fmt.Errorf("db: M33 检查 users.token_version 失败: %w", err)
	}
	rep.DroppedTokenVersion = hasCol
	if hasCol && !dryRun {
		if err := gdb.Exec(`ALTER TABLE users DROP COLUMN IF EXISTS token_version`).Error; err != nil {
			return rep, fmt.Errorf("db: M33 删除 users.token_version 失败: %w", err)
		}
	}

	var staleKeys int64
	if err := gdb.Raw(`SELECT count(*) FROM system_config
		WHERE key IN ('reg_captcha_enabled','login_lock_enabled')`).Scan(&staleKeys).Error; err != nil {
		return rep, fmt.Errorf("db: M33 检查旧认证配置键失败: %w", err)
	}
	rep.DroppedConfigKeys = int(staleKeys)
	if staleKeys > 0 && !dryRun {
		if err := gdb.Exec(`DELETE FROM system_config
			WHERE key IN ('reg_captcha_enabled','login_lock_enabled')`).Error; err != nil {
			return rep, fmt.Errorf("db: M33 删除旧认证配置键失败: %w", err)
		}
	}
	// 3) 旧的全集唯一索引：存在就必须删，否则 EnsureIndexes 建部分索引失败。
	var hasAffIdx bool
	if err := gdb.Raw(`SELECT EXISTS (
		SELECT 1 FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = 'idx_users_aff_code')`).Scan(&hasAffIdx).Error; err != nil {
		return rep, fmt.Errorf("db: M33 检查 idx_users_aff_code 失败: %w", err)
	}
	rep.DroppedAffCodeIndex = hasAffIdx
	if hasAffIdx && !dryRun {
		if err := gdb.Exec(`DROP INDEX IF EXISTS idx_users_aff_code`).Error; err != nil {
			return rep, fmt.Errorf("db: M33 删除旧 aff_code 唯一索引失败: %w", err)
		}
	}
	return rep, nil
}

// LogAuthMigration 打印 M33 结果；无事可做时保持安静（每次启动都会跑）。
func LogAuthMigration(rep AuthMigrationReport) {
	if !rep.DroppedTokenVersion && rep.DroppedConfigKeys == 0 && !rep.DroppedAffCodeIndex {
		return
	}
	logger.L().Info("M33 认证迁移完成",
		zap.Bool("dropped_token_version", rep.DroppedTokenVersion),
		zap.Int("dropped_config_keys", rep.DroppedConfigKeys),
		zap.Bool("dropped_aff_code_index", rep.DroppedAffCodeIndex))
}
