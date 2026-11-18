package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// MigrationNameAgentContext 是「AI 助手运行参数列改名 + 旧默认值升级」的数据动作标识。
//
// 为什么必须有这一条：`agent_settings` 里"每日成本上限"曾经是三处三个名字 ——
// 列 `daily_quota_budget`、Go 字段 `DailyQuotaBudget`、对外 JSON `daily_amount_budget`。
// 前端按 JSON 名读写，于是**读回来是 undefined、保存被静默忽略**，
// 而页面还提示"已保存"。整改把三者统一成 `daily_amount_budget`，
// AutoMigrate 只会建新列（默认 0），旧列里的值留在原地 —— 这条迁移负责搬过去并删掉旧列。
//
// 另一件事是 `max_tokens_per_run` 的旧默认值 32000 → 2000000：
// 每一步都要重付整份 prompt（system + 21 个工具声明 ≈ 7–9k token），
// 32000 的实际效果是"第 3–4 步就被掐断"，运营看到的是"AI 做一半就停了"。
const MigrationNameAgentContext = "m31_agent_context"

// AgentContextReport 是一次迁移的结果（供启动日志与核对）。
type AgentContextReport struct {
	Skipped       bool  `json:"skipped"`
	DryRun        bool  `json:"dry_run"`
	CopiedBudget  int64 `json:"copied_budget"`   // 从旧列搬到新列的行数
	DroppedColumn bool  `json:"dropped_column"`  // 是否删掉了旧列
	ResetRunToken int64 `json:"reset_run_token"` // 旧默认值被升级的行数
}

// MigrateAgentContext 执行 m31（幂等）。
//
// 顺序不可颠倒：先搬值、再删列。反过来的话旧值会随列一起消失，
// 而"每日预算变成 0（不限）"这种后果是**放宽**限制 —— 不会报错，也不会有人发现。
func MigrateAgentContext(gdb *gorm.DB, dryRun bool) (AgentContextReport, error) {
	rep := AgentContextReport{DryRun: dryRun}
	if !dryRun {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", MigrationNameAgentContext).Count(&n).Error; err != nil {
			return rep, fmt.Errorf("db: 查询迁移标记失败: %w", err)
		}
		if n > 0 {
			rep.Skipped = true
			return rep, nil
		}
	}
	hasOld, err := columnExists(gdb, "agent_settings", "daily_quota_budget")
	if err != nil {
		return rep, err
	}
	// 统计要搬的行数（dry-run 也要给得出准确数字，否则"预演"没有意义）。
	if hasOld {
		if err := gdb.Raw(
			`SELECT count(*) FROM agent_settings WHERE daily_amount_budget = 0 AND daily_quota_budget <> 0`,
		).Scan(&rep.CopiedBudget).Error; err != nil {
			return rep, fmt.Errorf("db: 统计每日预算待搬迁行失败: %w", err)
		}
	}
	oldDefault := int64(0)
	if err := gdb.Model(&model.AgentSetting{}).
		Where("max_tokens_per_run = ?", 32000).Count(&oldDefault).Error; err != nil {
		return rep, fmt.Errorf("db: 统计旧 token 预算行失败: %w", err)
	}
	rep.ResetRunToken = oldDefault
	if dryRun {
		return rep, nil
	}

	return rep, gdb.Transaction(func(tx *gorm.DB) error {
		if hasOld {
			// 判据是"新列还是 0 且旧列非 0"：新列有值说明已经人工改过，绝不覆盖。
			if err := tx.Exec(
				`UPDATE agent_settings SET daily_amount_budget = daily_quota_budget
				 WHERE daily_amount_budget = 0 AND daily_quota_budget <> 0`,
			).Error; err != nil {
				return fmt.Errorf("db: 搬迁每日预算失败: %w", err)
			}
			// 旧列必须删掉：留着它就是第二个真相源 —— 下一个人会照它写代码，
			// 而它再也不会被任何路径更新（值永远停在迁移那一刻）。
			if err := tx.Exec(`ALTER TABLE agent_settings DROP COLUMN IF EXISTS daily_quota_budget`).Error; err != nil {
				return fmt.Errorf("db: 删除旧预算列失败: %w", err)
			}
			rep.DroppedColumn = true
		}
		// 旧默认值 = 用户从没改过。升级到新默认；被显式改成 32000 的极少，
		// 且本项目处于全新开发阶段（无存量用户数据），可接受。
		if err := tx.Exec(
			`UPDATE agent_settings SET max_tokens_per_run = ? WHERE max_tokens_per_run = 32000`,
			model.DefaultAgentMaxTokensPerRun,
		).Error; err != nil {
			return fmt.Errorf("db: 升级单轮 token 预算默认值失败: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: MigrationNameAgentContext, AppliedAt: time.Now(),
		}).Error
	})
}

// LogAgentContext 把迁移结果写进启动日志。
func LogAgentContext(rep AgentContextReport) {
	if rep.Skipped {
		return
	}
	if rep.DryRun {
		logger.L().Info("助手运行参数迁移（dry-run，未写库）",
			zap.Int64("copy_budget", rep.CopiedBudget), zap.Int64("reset_run_token", rep.ResetRunToken))
		return
	}
	logger.L().Info("助手运行参数迁移完成",
		zap.Int64("copy_budget", rep.CopiedBudget),
		zap.Bool("dropped_old_column", rep.DroppedColumn),
		zap.Int64("reset_run_token", rep.ResetRunToken))
}

// columnExists 判断某表是否有某列（不受 GORM 模型定义影响，看的是真实库结构）。
func columnExists(gdb *gorm.DB, table, column string) (bool, error) {
	var n int64
	err := gdb.Raw(
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = ? AND column_name = ?`, table, column,
	).Scan(&n).Error
	if err != nil {
		return false, fmt.Errorf("db: 读取表结构失败: %w", err)
	}
	return n > 0, nil
}
