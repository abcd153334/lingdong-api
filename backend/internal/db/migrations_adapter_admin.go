package db

import (
	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// MigrationNameAdapterAdmin 是「适配器管理」的数据动作标识。
//
// 两件事：
//  1. 给 adapter_specs 加 owner_id（谁建的），供「我的」分组展示；
//  2. 删掉 docs_id —— 它从来没有写入点，也没有读取点（全仓只在模型定义里出现），
//     是一条会让人以为"规格能追溯到上游文档"的死列。
//
// 为什么删而不是补写入点：真要追溯「这份规格照哪份文档写的」，答案是提案里的
// doc_ids（agent_proposals.plan），那是**一次生成过程**的属性，不是规格的属性；
// 规格可能被人工改过十次，挂在它身上的 doc_id 只会误导。
const MigrationNameAdapterAdmin = "m32_adapter_admin"

// AdapterAdminReport 是一次迁移的结果（供启动日志核对）。
type AdapterAdminReport struct {
	// DroppedDocsID 表示 docs_id 列是否被删除（已删过则为 false）。
	DroppedDocsID bool `json:"dropped_docs_id"`
	// BackfilledOwner 是被回填 owner 的行数（全新库恒为 0）。
	BackfilledOwner int64 `json:"backfilled_owner"`
}

// MigrateAdapterAdmin 执行 m32（幂等）。
func MigrateAdapterAdmin(gdb *gorm.DB, dryRun bool) (AdapterAdminReport, error) {
	var rep AdapterAdminReport
	if !gdb.Migrator().HasTable("adapter_specs") {
		return rep, nil
	}
	if dryRun {
		rep.DroppedDocsID = gdb.Migrator().HasColumn("adapter_specs", "docs_id")
		return rep, nil
	}
	// 先回填再删列：顺序反过来的话，"这批规格是谁建的"这条信息就永久丢了。
	if gdb.Migrator().HasColumn("adapter_specs", "docs_id") {
		// docs_id <> 0 的行说明历史上确实被外部脚本写过；把归属留成 0（系统预置），
		// 只做计数，供上线时人工确认一次。全新库这里恒为 0。
		var n int64
		if err := gdb.Raw(`SELECT count(*) FROM adapter_specs WHERE docs_id <> 0`).Scan(&n).Error; err != nil {
			return rep, err
		}
		rep.BackfilledOwner = n
		if err := gdb.Exec(`ALTER TABLE adapter_specs DROP COLUMN IF EXISTS docs_id`).Error; err != nil {
			return rep, err
		}
		rep.DroppedDocsID = true
	}
	return rep, nil
}

// LogAdapterAdmin 记录迁移结果（删列是一次性动作，必须留痕）。
func LogAdapterAdmin(rep AdapterAdminReport) {
	if !rep.DroppedDocsID && rep.BackfilledOwner == 0 {
		return
	}
	logger.L().Info("适配器管理迁移完成",
		zap.Bool("dropped_docs_id", rep.DroppedDocsID),
		zap.Int64("rows_with_docs_id", rep.BackfilledOwner))
}
