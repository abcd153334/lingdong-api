package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// 邀请返利表从「一行两受益人」改造成「一行一受益人」。
//
// # 为什么重建而不是加列
//
// 旧表一行装两个人的钱（invitee_amount / inviter_amount），共用一个 transferred_at。
// AutoMigrate 只会**加**列：加完新列后，同一张表里同时存在两套金额语义，
// 写错列没有任何约束拦得住 —— 那就是把双真相源写进数据库。所以必须整表换掉。
//
// # 判据与动作
//
//	表不存在                      → skip
//	已有 beneficiary_id/role/amount → skip（幂等）
//	行数 = 0                      → DROP（没有任何资金证据可丢）
//	行数 > 0                      → RENAME 为 affiliate_rewards_legacy_<日期>，打 WARN
//
// 表在全新项目里是空的（旧实现从未发放成功过），所以正常路径就是 DROP；
// 非空分支只是防止「有人在旧库里真的发过钱」，那批行必须留档人工核对，
// 绝不能静默删掉。
type AffiliateTableReport struct {
	// Skipped 为 true 表示无需处理（表不存在，或已是新结构）。
	Skipped bool `json:"skipped"`
	// Dropped 为 true 表示空表被直接删除，交给 AutoMigrate 重建。
	Dropped bool `json:"dropped"`
	// Renamed 非空表示旧表被改名留档（非空表）。
	Renamed string `json:"renamed,omitempty"`
	// Rows 是改造前的行数（仅用于日志）。
	Rows int64 `json:"rows"`
}

// MigrateAffiliateRewardsV2 把 affiliate_rewards 重建成「一行一受益人」。
//
// 必须在 AutoMigrate(models()...) **之前**执行：AutoMigrate 会按新 struct 建表，
// 若先建后改，新表反而会被旧表挡住（同名表已存在 → 只加列）。
func MigrateAffiliateRewardsV2(gdb *gorm.DB) (AffiliateTableReport, error) {
	var rep AffiliateTableReport
	m := gdb.Migrator()
	if !m.HasTable("affiliate_rewards") {
		rep.Skipped = true
		return rep, nil
	}
	// 三个新列同时存在才算「已改造」：只判一个会在改造中途留下半新半旧的表。
	if m.HasColumn("affiliate_rewards", "beneficiary_id") &&
		m.HasColumn("affiliate_rewards", "role") &&
		m.HasColumn("affiliate_rewards", "amount") {
		rep.Skipped = true
		return rep, nil
	}
	if err := gdb.Table("affiliate_rewards").Count(&rep.Rows).Error; err != nil {
		return rep, fmt.Errorf("db: 统计邀请返利行数失败: %w", err)
	}
	if rep.Rows == 0 {
		if err := m.DropTable("affiliate_rewards"); err != nil {
			return rep, fmt.Errorf("db: 删除旧邀请返利表失败: %w", err)
		}
		rep.Dropped = true
		return rep, nil
	}
	// 非空：留档而不是删。同一自然日内重复启动时加序号，避免第二次改名失败。
	legacy := "affiliate_rewards_legacy_" + time.Now().Format("20060102")
	for i := 2; m.HasTable(legacy); i++ {
		legacy = fmt.Sprintf("affiliate_rewards_legacy_%s_%d", time.Now().Format("20060102"), i)
	}
	if err := m.RenameTable("affiliate_rewards", legacy); err != nil {
		return rep, fmt.Errorf("db: 旧邀请返利表改名失败: %w", err)
	}
	rep.Renamed = legacy
	return rep, nil
}

// LogAffiliateTable 把表改造结果写进启动日志。
func LogAffiliateTable(rep AffiliateTableReport) {
	switch {
	case rep.Dropped:
		logger.L().Warn("邀请返利表已重建为「一行一受益人」（旧表为空，已删除）")
	case rep.Renamed != "":
		logger.L().Warn("邀请返利表已重建为「一行一受益人」（旧表非空，已改名留档，需人工核对）",
			zap.String("legacy_table", rep.Renamed), zap.Int64("rows", rep.Rows))
	}
}
