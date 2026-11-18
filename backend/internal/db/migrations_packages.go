package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// MigrationNamePackagesPrice 是「套餐售价回填到 price_cent」的数据动作标识。
//
// 为什么必须有这一条：M28 把计费单位从「额度」换成「微」时，`packages` 表新增了
// `price_cent` / `bonus` 两列（`AutoMigrate` 加列，默认 0），但**没有回填**。
// 旧列 `price_cny_cent`（分）里还留着真实售价，新列却是 0。
//
// 症状不是"价格显示成 0"这么温和 —— `order.Create` 的套餐分支判 `p.PriceCent <= 0`
// 直接回 400 `invalid_param`，于是**所有套餐都买不了**，而错误码指向"参数非法"，
// 运营只会以为前端传错了。这条迁移把旧列的金额搬到新列，让套餐恢复可购买。
//
// 换算：两列单位都是「分」，直接搬，不乘不除。这是刻意的 ——
// 任何 ×100 / ÷100 都是又一个"量纲锚"，正是这次整改要消灭的东西。
const MigrationNamePackagesPrice = "m29_packages_price_cent"

// PackagesPriceRow 是单行回填结果（供报告与核对表使用）。
type PackagesPriceRow struct {
	ID      uint64 `json:"id"`
	Name    string `json:"name"`
	OldCent int64  `json:"old_price_cent"`
	NewCent int64  `json:"new_price_cent"`
}

// PackagesPriceReport 是一次回填的完整结果。
type PackagesPriceReport struct {
	Skipped  bool               `json:"skipped"` // true = 已迁移过或无需迁移
	DryRun   bool               `json:"dry_run"`
	Rows     []PackagesPriceRow `json:"rows"`
	Warnings []string           `json:"warnings,omitempty"`
}

// legacyPackageRow 是回填所需的存量行（旧列可能是 NULL，故用指针）。
type legacyPackageRow struct {
	ID           uint64
	Name         string
	PriceCent    int64
	PriceCnyCent *int64
	Bonus        int64
	BonusQuota   *int64
}

// planPackagesPrice 是**纯函数**：给定存量行，算出该写什么、该告警什么。
//
// 抽出来是为了能不起数据库就跑测试 —— 这段逻辑直接决定「用户能不能买到套餐」，
// 不该只靠一次手工 dry-run 来验证。
func planPackagesPrice(rows []legacyPackageRow) (updates []PackagesPriceRow, warnings []string) {
	for _, r := range rows {
		// 售价与赠送是两件事，判据必须各自独立：把赠送告警挂在「售价已定」的分支里，
		// 就会漏掉「售价早就手工补过、但赠送还没补」这种最需要提醒的行。
		switch {
		case r.PriceCent > 0:
			// 新列已有值：运营已经手工定价过，绝不覆盖。
		case r.PriceCnyCent != nil && *r.PriceCnyCent > 0:
			updates = append(updates, PackagesPriceRow{
				ID: r.ID, Name: r.Name, OldCent: r.PriceCent, NewCent: *r.PriceCnyCent,
			})
		default:
			// 两列都为 0/空：这是"从没定过价"的套餐，迁移无能为力，必须让人来处理。
			warnings = append(warnings, fmt.Sprintf(
				"套餐 %d(%s) 售价为空（新旧列都是 0），下单会被拒（invalid_param），需要人工定价",
				r.ID, r.Name))
		}
		if r.BonusQuota != nil && *r.BonusQuota > 0 && r.Bonus == 0 {
			// 旧「赠送额度」与「微」不是同一量纲（1 额度 ≈ 1/500000 美元），
			// 自动折算会造出一个没人看得懂的数字 —— 交给人确认比猜更安全。
			warnings = append(warnings, fmt.Sprintf(
				"套餐 %d(%s) 的旧赠送额度 %d 未回填（量纲不同：额度 vs 微），请确认赠送金额后手工填写",
				r.ID, r.Name, *r.BonusQuota))
		}
	}
	return updates, warnings
}

// MigratePackagesPrice 把存量套餐售价从 `price_cny_cent` 回填到 `price_cent`，幂等。
//
// 幂等策略：
//  1. data_migrations 标记（跑过就不再跑）；
//  2. 逐行判据：只处理 `price_cent = 0` 且旧列有正值的行，已有值的不覆盖。
func MigratePackagesPrice(gdb *gorm.DB, dryRun bool) (PackagesPriceReport, error) {
	rep := PackagesPriceReport{DryRun: dryRun}
	if !gdb.Migrator().HasTable(&model.Package{}) {
		rep.Skipped = true
		return rep, nil
	}
	// 全新库（没有旧列）= 没有需要搬迁的存量，直接返回。
	// 这一步让迁移对「新建环境 + 种子数据」完全无副作用。
	if !gdb.Migrator().HasColumn(&model.Package{}, "price_cny_cent") {
		rep.Skipped = true
		return rep, nil
	}
	if !dryRun {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", MigrationNamePackagesPrice).Count(&n).Error; err != nil {
			return rep, fmt.Errorf("db: 查询迁移标记失败: %w", err)
		}
		if n > 0 {
			rep.Skipped = true
			return rep, nil
		}
	}

	var rows []legacyPackageRow
	if err := gdb.Table("packages").
		Select("id, name, price_cent, price_cny_cent, bonus, bonus_quota").
		Where("deleted_at IS NULL").
		Order("id ASC").
		Scan(&rows).Error; err != nil {
		return rep, fmt.Errorf("db: 读取套餐失败: %w", err)
	}

	updates, warnings := planPackagesPrice(rows)
	rep.Rows = updates
	rep.Warnings = warnings

	if dryRun {
		return rep, nil
	}
	return rep, gdb.Transaction(func(tx *gorm.DB) error {
		for _, u := range updates {
			// 带 `price_cent = 0` 条件：并发下有人刚手工定价时不会被旧值覆盖。
			if err := tx.Table("packages").
				Where("id = ? AND price_cent = 0", u.ID).
				Updates(map[string]any{
					"price_cent": u.NewCent, "updated_at": time.Now(),
				}).Error; err != nil {
				return fmt.Errorf("db: 回填套餐 %d 失败: %w", u.ID, err)
			}
		}
		return tx.Create(&model.DataMigration{
			Name: MigrationNamePackagesPrice, AppliedAt: time.Now(),
		}).Error
	})
}

// LogPackagesPrice 把回填结果写进启动日志（被改了什么必须留痕）。
func LogPackagesPrice(rep PackagesPriceReport) {
	switch {
	case rep.Skipped:
		return
	case len(rep.Rows) == 0 && len(rep.Warnings) == 0:
		return
	}
	if rep.DryRun {
		logger.L().Info("套餐售价回填（dry-run，未写库）",
			zap.Int("rows", len(rep.Rows)), zap.Int("warnings", len(rep.Warnings)))
		return
	}
	logger.L().Info("套餐售价回填完成",
		zap.Int("rows", len(rep.Rows)), zap.Int("warnings", len(rep.Warnings)))
	for _, r := range rep.Rows {
		logger.L().Info("套餐售价已回填",
			zap.Uint64("package_id", r.ID), zap.String("name", r.Name),
			zap.Int64("old_price_cent", r.OldCent), zap.Int64("new_price_cent", r.NewCent))
	}
	for _, w := range rep.Warnings {
		logger.L().Warn("套餐售价回填告警", zap.String("detail", w))
	}
}
