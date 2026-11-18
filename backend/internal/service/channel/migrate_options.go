package channel

import (
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/relay"
)

const (
	// legacyDuomiOptionKey 是 11-02 从 duomiOverride 删除的字段。
	//
	// 存量 JSONB 里残留它时，parseDuomiOptions 的 DisallowUnknownFields 会让
	// **整段配置**解析失败 —— 表现不是「这个键不生效」，而是该渠道绑定的一切模型
	// 在组装请求前就 503（2026-11-04 多米渠道 12 个模型全挂即此）。
	// 迁移只删这一个键，不重排、不改写其它配置。
	legacyDuomiOptionKey = "fixed_tier"

	// channelOptionsBackupTable 是一次性备份表，供误删时回滚。
	channelOptionsBackupTable = "channels_backup_20261104"
)

// ChannelOptionsReport 是一次配置清理的结果（供启动日志核对）。
type ChannelOptionsReport struct {
	// BackedUp 表示本次是否新建了备份表（已存在则为 false）。
	BackedUp bool `json:"backed_up"`
	// Scanned 是扫描到的 duomi 渠道数（有非空 adapter_options）。
	Scanned int `json:"scanned"`
	// Cleaned 是被清理的渠道数。
	Cleaned int `json:"cleaned"`
	// Failed 是无法解析或清理后仍不合法、需要人工处理的渠道 ID。
	Failed []uint64 `json:"failed"`
}

// MigrateChannelOptions 清理渠道私有配置中的已删除字段（幂等）。
//
// 为什么放在 service/channel 而不是 db 包：迁移要用 relay.ValidateAdapterOptions
// 复验，而 relay 已经 import db，放在 db 包会形成 import 循环。
//
// 为什么必须复验才写回：只按 JSON 删键无法证明"改完真的能跑"。复验用的是
// 运行期同一个 parser（parseDuomiOptions），解析不通过就保留原值并报错，
// 让人工介入 —— 静默写回一份仍然不合法（甚至更糟）的配置比不改更危险。
func MigrateChannelOptions(gdb *gorm.DB, dryRun bool) (ChannelOptionsReport, error) {
	var rep ChannelOptionsReport
	if !gdb.Migrator().HasTable("channels") {
		return rep, nil
	}

	if !dryRun {
		backedUp, err := ensureChannelOptionsBackup(gdb)
		if err != nil {
			return rep, err
		}
		rep.BackedUp = backedUp
	}

	type optRow struct {
		ID        uint64
		AdapterID string
		Options   string
	}
	var rows []optRow
	const q = `SELECT id, adapter_id, adapter_options::text AS options
	             FROM channels
	            WHERE adapter_id = 'duomiapi'
	              AND adapter_options IS NOT NULL
	              AND adapter_options::text NOT IN ('', 'null', '{}')`
	if err := gdb.Raw(q).Scan(&rows).Error; err != nil {
		return rep, fmt.Errorf("channel: 读取渠道私有配置失败: %w", err)
	}

	for _, r := range rows {
		rep.Scanned++
		cleaned, changed, err := pruneLegacyDuomiKeys([]byte(r.Options))
		if err != nil {
			rep.Failed = append(rep.Failed, r.ID)
			logger.L().Error("渠道私有配置无法解析，需人工处理",
				zap.Uint64("channel_id", r.ID), zap.Error(err))
			continue
		}
		if !changed {
			continue
		}
		if err := relay.ValidateAdapterOptions(r.AdapterID, cleaned); err != nil {
			rep.Failed = append(rep.Failed, r.ID)
			logger.L().Error("清理后的渠道私有配置仍未通过校验，已跳过",
				zap.Uint64("channel_id", r.ID), zap.Error(err))
			continue
		}
		if dryRun {
			rep.Cleaned++
			continue
		}
		// 不改 updated_at：它的语义是「运营最后一次编辑」，迁移不是编辑。
		if err := gdb.Exec(`UPDATE channels SET adapter_options = CAST(? AS jsonb) WHERE id = ?`,
			string(cleaned), r.ID).Error; err != nil {
			return rep, fmt.Errorf("channel: 写回渠道 %d 失败: %w", r.ID, err)
		}
		rep.Cleaned++
	}
	return rep, nil
}

// ensureChannelOptionsBackup 建一次性备份表；已存在则复用（幂等）。
func ensureChannelOptionsBackup(gdb *gorm.DB) (bool, error) {
	var exists bool
	if err := gdb.Raw(`SELECT to_regclass(?) IS NOT NULL`, channelOptionsBackupTable).
		Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("channel: 检查备份表失败: %w", err)
	}
	if exists {
		return false, nil
	}
	// 备份失败必须中止整次迁移：没有回滚点在先，就不该动生产配置。
	if err := gdb.Exec(`CREATE TABLE ` + channelOptionsBackupTable + ` AS SELECT * FROM channels`).Error; err != nil {
		return false, fmt.Errorf("channel: 备份 channels 失败（未做任何清理）: %w", err)
	}
	return true, nil
}

// pruneLegacyDuomiKeys 删除 model_overrides.*.fixed_tier，其余内容原样保留。
//
// 顶层与单个覆盖项都用 json.RawMessage 承载，避免把不认识的字段（以及将来新增的
// 字段）重新序列化成另一种形状 —— 迁移的职责只有"删一个已知的废弃键"。
func pruneLegacyDuomiKeys(raw []byte) ([]byte, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	overridesRaw, ok := doc["model_overrides"]
	if !ok {
		return raw, false, nil
	}
	var overrides map[string]map[string]json.RawMessage
	if err := json.Unmarshal(overridesRaw, &overrides); err != nil {
		return nil, false, err
	}
	changed := false
	for id, ov := range overrides {
		if _, has := ov[legacyDuomiOptionKey]; !has {
			continue
		}
		delete(ov, legacyDuomiOptionKey)
		overrides[id] = ov
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	nextOverrides, err := json.Marshal(overrides)
	if err != nil {
		return nil, false, err
	}
	doc["model_overrides"] = nextOverrides
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// LogChannelOptions 记录迁移结果。失败项必须打进日志（否则"12 个模型全挂"会重演）。
func LogChannelOptions(rep ChannelOptionsReport) {
	if rep.Cleaned == 0 && len(rep.Failed) == 0 && !rep.BackedUp {
		return
	}
	logger.L().Info("渠道私有配置清理完成",
		zap.Bool("backed_up", rep.BackedUp),
		zap.Int("scanned", rep.Scanned),
		zap.Int("cleaned", rep.Cleaned),
		zap.Uint64s("failed_channel_ids", rep.Failed))
}
