package channel

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/relay"
)

// OptionsAuditReport 是一次启动期渠道路径配置体检的结果。
type OptionsAuditReport struct {
	// Scanned 是检查过的渠道数（有非空 adapter_options 的）。
	Scanned int `json:"scanned"`
	// Invalid 是解析不通过的渠道 ID。
	Invalid []uint64 `json:"invalid"`
}

// AuditAdapterOptions 在启动时检查全部渠道的私有配置。
//
// 为什么需要它：`DisallowUnknownFields` 只在**保存期**与**请求期**生效。
// 删掉一个字段后，存量配置不会自己变脏也不会被复查，
// 直到第一个真实请求才炸 —— 而那时表现是「该渠道所有模型 503」（2026-11-04）。
//
// 为什么只告警不阻断：一条脏配置不该让整个服务起不来。阻断把「某个渠道不可用」
// 放大成「全站不可用」，恢复还得改库；而告警 + 渠道列表可见，运维能从容处理。
func AuditAdapterOptions(gdb *gorm.DB) (OptionsAuditReport, error) {
	var rep OptionsAuditReport
	if !gdb.Migrator().HasTable("channels") {
		return rep, nil
	}
	type row struct {
		ID        uint64
		AdapterID string
		Options   string
	}
	var rows []row
	const q = `SELECT id, adapter_id, adapter_options::text AS options
	             FROM channels
	            WHERE adapter_options IS NOT NULL
	              AND adapter_options::text NOT IN ('', 'null', '{}')`
	if err := gdb.Raw(q).Scan(&rows).Error; err != nil {
		return rep, fmt.Errorf("channel: 读取渠道私有配置失败: %w", err)
	}
	for _, r := range rows {
		rep.Scanned++
		// 不支持的适配器（openai 等）本来就不该有私有配置，属于历史脏数据，
		// 同样报出来 —— 静默忽略会让运营以为那份配置在生效。
		if err := relay.ValidateAdapterOptions(r.AdapterID, []byte(r.Options)); err != nil {
			rep.Invalid = append(rep.Invalid, r.ID)
			logger.L().Error("渠道私有配置无效（该渠道的请求会在组装阶段失败）",
				zap.Uint64("channel_id", r.ID),
				zap.String("adapter_id", r.AdapterID),
				zap.Error(err))
		}
	}
	return rep, nil
}

// LogOptionsAudit 汇总体检结果（单个渠道的详情已在 AuditAdapterOptions 里逐条打过）。
func LogOptionsAudit(rep OptionsAuditReport) {
	if len(rep.Invalid) == 0 {
		return
	}
	logger.L().Warn("渠道私有配置体检发现无效项",
		zap.Int("scanned", rep.Scanned),
		zap.Uint64s("invalid_channel_ids", rep.Invalid))
}
