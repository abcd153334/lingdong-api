// Package audit 记录管理面写操作审计（v2.0 §4.2.10）。
package audit

import (
	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// Record 写入一条 admin_audit 记录；detail 会序列化为 JSONB。
// action 必须先经 Normalize 收敛到权威枚举，表外动作一律拒绝入库（防止动作枚举漂移）。
// RecordOrWarn 写入审计并在失败时输出 Error 日志（不静默吞错）。
// 资金类操作应改用 Record + 同一事务（失败即回滚），不得使用本函数。
func RecordOrWarn(gdb *gorm.DB, adminID uint64, action, targetType, targetID string, detail any) {
	if err := Record(gdb, adminID, action, targetType, targetID, detail); err != nil {
		logger.L().Error("审计写入失败",
			zap.Bool("audit_write_failed", true),
			zap.Uint64("admin_id", adminID),
			zap.String("action", action),
			zap.String("target_type", targetType),
			zap.String("target_id", targetID),
			zap.Error(err))
	}
}

func Record(gdb *gorm.DB, adminID uint64, action, targetType, targetID string, detail any) error {
	normalized, err := Normalize(action)
	if err != nil {
		return err
	}
	j, err := model.NewJSON(detail)
	if err != nil {
		return err
	}
	row := model.AdminAudit{
		AdminID:    adminID,
		Action:     normalized,
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     j,
	}
	return gdb.Create(&row).Error
}
