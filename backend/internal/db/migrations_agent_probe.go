package db

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// M34：删掉 proposals 上的探测快照列（dry_run）。
//
// 为什么必须显式删：
//  1. 探测凭据的唯一真相源是 agent_dry_runs（跨轮有效、按会话+适配器+地址+密钥指纹判定）。
//     agent_proposals.dry_run 是**同轮快照**，只在"这一轮里先探测再提交"时才有值，
//     运营分两步做就永远是空的 —— 这正是"明明探过了、点应用还是要求探测"的根因。
//     两个来源并存时，闸门读哪一个都会有一半场景是错的。
//  2. GORM 的 AutoMigrate 只加不删：光删掉 struct 字段，列会一直留在库里，
//     下一个人看到它就会以为还有第二条路（注释曾写着"闸门只看这里"）。
//
// 先删字段再删列：只要 struct 里还有该字段，每次启动 AutoMigrate 都会把列补回来
// （与 model_docs 的反复建删同一个坑）。字段已在 model.AgentProposal 中删除。
//
// 不写 data_migrations 标记：本迁移只做幂等的 DDL 存在性检查，
// 重复执行是 no-op（与 M32/M33 同口径）。
func MigrateAgentProbe(gdb *gorm.DB, dryRun bool) (AgentProbeReport, error) {
	var rep AgentProbeReport

	var hasCol bool
	if err := gdb.Raw(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'agent_proposals' AND column_name = 'dry_run')`).Scan(&hasCol).Error; err != nil {
		return rep, fmt.Errorf("db: M34 检查 agent_proposals.dry_run 失败: %w", err)
	}
	rep.DroppedProposalDryRun = hasCol
	if hasCol && !dryRun {
		if err := gdb.Exec(`ALTER TABLE agent_proposals DROP COLUMN IF EXISTS dry_run`).Error; err != nil {
			return rep, fmt.Errorf("db: M34 删除 agent_proposals.dry_run 失败: %w", err)
		}
	}
	return rep, nil
}

// AgentProbeReport 是 M34 的执行结果（日志与迁移自检用）。
type AgentProbeReport struct {
	DroppedProposalDryRun bool
}

// LogAgentProbe 打印 M34 结果；无事可做时保持安静（每次启动都会跑）。
func LogAgentProbe(rep AgentProbeReport) {
	if !rep.DroppedProposalDryRun {
		return
	}
	logger.L().Info("M34 探测凭据迁移完成",
		zap.Bool("dropped_proposal_dry_run", rep.DroppedProposalDryRun))
}
