package db

import (
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/uploadcfg"
)

// M38：把 `uploads` 从「一个全局 max_size_mb」迁到「按类型上限 + 后缀表 + 压缩」。
//
// 为什么必须迁：新代码读的是 limits/types/compress。留着 max_size_mb 会有两套体积口径
// ——全局 50MB 与按类 20/100/200 同时存在，保存成功但没人说得清哪个生效。
//
// 迁移策略（只做等值搬运，不改运营已调的数值）：
//   - limits.image = 旧 max_size_mb（保留运营已经调过的数），其余四类取出厂值；
//   - types / compress 缺就补出厂值，已有就原样保留；
//   - 删掉 max_size_mb。
//
// 幂等判据：有 limits 且没有 max_size_mb → 直接跳过。
type UploadsCfgMigrationReport struct {
	// Migrated 表示本次是否真的改写了配置行。
	Migrated bool
	// OldMaxMB 是搬运到图片类的旧全局上限（0 表示没取到）。
	OldMaxMB int64
}

// MigrateUploadsConfig 把 uploads 配置迁到新形状；dryRun 只判断不写。
func MigrateUploadsConfig(gdb *gorm.DB, dryRun bool) (UploadsCfgMigrationReport, error) {
	var rep UploadsCfgMigrationReport

	var raw string
	if err := gdb.Raw(`SELECT value::text FROM system_config WHERE key = 'uploads'`).Scan(&raw).Error; err != nil {
		return rep, fmt.Errorf("db: M38 读取 uploads 配置失败: %w", err)
	}
	if raw == "" {
		return rep, nil // 新库还没 seed；seed 会直接写新形状
	}

	var cur map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &cur); err != nil {
		// 配置本身坏了：交给保存期去修。这里不覆盖，免得把运营的其它配置一并冲掉。
		return rep, fmt.Errorf("db: M38 uploads 配置不是合法 JSON: %w", err)
	}
	_, hasLimits := cur["limits"]
	maxRaw, hasMax := cur["max_size_mb"]
	if hasLimits && !hasMax {
		return rep, nil
	}

	next := uploadcfg.Defaults()
	// 逐项覆盖「运营可能已经改过」的字段；解析失败就保留出厂值。
	overlay := func(key string, dst any) {
		if v, ok := cur[key]; ok {
			_ = json.Unmarshal(v, dst)
		}
	}
	overlay("enabled", &next.Enabled)
	overlay("daily_limit_per_key", &next.DailyLimitPerKey)
	overlay("retention_hours", &next.RetentionHours)
	overlay("user_total_mb", &next.UserTotalMB)
	overlay("concurrency", &next.Concurrency)
	overlay("types", &next.Types)
	overlay("limits", &next.Limits)
	overlay("compress", &next.Compress)

	if hasMax {
		var mb int64
		if err := json.Unmarshal(maxRaw, &mb); err == nil && mb >= 1 && mb <= 10240 {
			if next.Limits == nil {
				// `"limits": null` 会让 overlay 把 map 置成 nil，直接赋值会 panic。
				next.Limits = map[string]int64{}
			}
			next.Limits[uploadcfg.KindImage] = mb
			rep.OldMaxMB = mb
		}
	}
	// 补齐缺失的类别：旧配置里 limits 只可能是「运营手写过一部分」，
	// 缺一类就让 Validate 失败会连带把其它四类已经调好的数字一起丢掉。
	base := uploadcfg.Defaults().Limits
	for _, k := range uploadcfg.Kinds {
		if next.Limits[k] <= 0 {
			next.Limits[k] = base[k]
		}
	}
	if err := next.Validate(); err != nil {
		// 拼出来的配置不合法（历史脏值）→ 整份换出厂值，宁可回到可用状态。
		logger.L().Warn("db: M38 uploads 迁移结果不合法，已回退出厂值",
			zap.Error(err), zap.String("raw", raw))
		next = uploadcfg.Defaults()
		rep.OldMaxMB = 0
	}

	rep.Migrated = true
	if dryRun {
		return rep, nil
	}
	blob, err := json.Marshal(next)
	if err != nil {
		return rep, fmt.Errorf("db: M38 序列化 uploads 配置失败: %w", err)
	}
	if err := gdb.Exec(`UPDATE system_config SET value = ?, updated_at = now() WHERE key = 'uploads'`,
		string(blob)).Error; err != nil {
		return rep, fmt.Errorf("db: M38 写回 uploads 配置失败: %w", err)
	}
	return rep, nil
}

// LogUploadsMigration 打印 M38 结果；无事可做时保持安静（每次启动都会跑）。
func LogUploadsMigration(rep UploadsCfgMigrationReport) {
	if !rep.Migrated {
		return
	}
	logger.L().Info("db: uploads 配置已迁移为按类型上限",
		zap.Int64("image_limit_mb", rep.OldMaxMB))
}
