package db

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// AuthMigration2Report 是 M35 的执行结果。
type AuthMigration2Report struct {
	// NormalizedPhones 是本次去掉前导 `+` 的手机号数量。
	NormalizedPhones int
	// ConflictedPhones 是**因冲突而未归一**的行（保留原值）。
	//
	// 冲突 = 去掉 `+` 之后会撞上另一个账号已有的号码。这种情况不能自动合并
	// —— 两个账号背后可能是两个真实的人，静默合并等于删掉一个账号的登录方式。
	// 报告出来由运维人工裁决。
	ConflictedPhones []PhoneConflict
	// ClearedKeyGroups 是本次清空 `api_keys.group` 的密钥数量。
	ClearedKeyGroups int
}

// PhoneConflict 描述一条无法自动归一的手机号。
type PhoneConflict struct {
	UserID   uint64
	Phone    string
	Conflict string // 归一后会撞上的号码
}

// MigrateAuth2 是 M35：手机号归一 + 清理 KEY 自填分组。
//
// 两件事都必须在 EnsureIndexes **之前**做，否则：
//   - 手机号：`uq_users_phone_norm`（表达式 ltrim）会在存在 `+86…`/`86…` 两行时
//     撞 23505，而它是 required 索引 → 启动直接被拒绝。先把行归一，索引才建得起来。
//   - KEY 分组：不清空的话，历史上被用户填成 `vip` 的密钥会继续按 VIP 折扣计费，
//     而新代码不再写这一列 —— 脏数据留在库里等于修复只对新数据生效。
//
// 幂等：每次都只处理「还带 + 的」与「group 非空的 user KEY」，第二次运行时为 0。
func MigrateAuth2(gdb *gorm.DB, dryRun bool) (AuthMigration2Report, error) {
	var rep AuthMigration2Report

	// 1) 找出所有带前导 + 的手机号，逐条判断是否冲突。
	type row struct {
		ID    uint64
		Phone string
	}
	var rows []row
	if err := gdb.Raw(`SELECT id, phone FROM users
		WHERE phone <> '' AND left(phone, 1) = '+' ORDER BY id`).Scan(&rows).Error; err != nil {
		return rep, fmt.Errorf("db: M35 读取带 + 的手机号失败: %w", err)
	}

	for _, r := range rows {
		normalized := r.Phone[1:]
		var taken int64
		if err := gdb.Raw(`SELECT count(*) FROM users WHERE id <> ? AND phone = ?`,
			r.ID, normalized).Scan(&taken).Error; err != nil {
			return rep, fmt.Errorf("db: M35 检查手机号冲突失败: %w", err)
		}
		if taken > 0 {
			rep.ConflictedPhones = append(rep.ConflictedPhones, PhoneConflict{
				UserID: r.ID, Phone: r.Phone, Conflict: normalized,
			})
			continue
		}
		rep.NormalizedPhones++
		if dryRun {
			continue
		}
		if err := gdb.Exec(`UPDATE users SET phone = ? WHERE id = ?`, normalized, r.ID).Error; err != nil {
			return rep, fmt.Errorf("db: M35 归一手机号失败（id=%d）: %w", r.ID, err)
		}
	}

	// 2) 清空用户自建 KEY 上残留的分组。
	//
	// 只清 kind='user'：托管密钥的分组同样不该存在（创建时已置空），
	// 但它的 kind 是 hosted —— 一并清掉更彻底，这里用 kind <> '' 之外的条件
	// 反而容易漏。直接按「非空即清」处理，两种 kind 都归零。
	var keyCount int64
	if err := gdb.Raw(`SELECT count(*) FROM api_keys WHERE "group" <> ''`).Scan(&keyCount).Error; err != nil {
		return rep, fmt.Errorf("db: M35 统计带分组的 KEY 失败: %w", err)
	}
	rep.ClearedKeyGroups = int(keyCount)
	if keyCount > 0 && !dryRun {
		if err := gdb.Exec(`UPDATE api_keys SET "group" = '' WHERE "group" <> ''`).Error; err != nil {
			return rep, fmt.Errorf("db: M35 清空 KEY 分组失败: %w", err)
		}
	}
	return rep, nil
}

// LogAuthMigration2 打印 M35 结果；无事可做时保持安静（每次启动都会跑）。
func LogAuthMigration2(rep AuthMigration2Report) {
	if rep.NormalizedPhones == 0 && rep.ClearedKeyGroups == 0 && len(rep.ConflictedPhones) == 0 {
		return
	}
	if len(rep.ConflictedPhones) > 0 {
		// 冲突必须显眼：它意味着「同一号码可能已经有两个账号」，
		// 需要人工确认哪个是真的，而不是让迁移静默跳过。
		items := make([]string, 0, len(rep.ConflictedPhones))
		for _, c := range rep.ConflictedPhones {
			items = append(items, fmt.Sprintf("uid=%d phone=%s conflicts=%s", c.UserID, c.Phone, c.Conflict))
		}
		logger.L().Warn("M35 手机号归一存在冲突，已保留原值待人工处理",
			zap.Strings("conflicts", items))
	}
	logger.L().Info("M35 认证数据迁移完成",
		zap.Int("normalized_phones", rep.NormalizedPhones),
		zap.Int("conflicted_phones", len(rep.ConflictedPhones)),
		zap.Int("cleared_key_groups", rep.ClearedKeyGroups))
}
