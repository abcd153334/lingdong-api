package db

import (
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/pkg/logger"
)

// M40：站点配置的两个结构性变更。
//
//  1. `site.api_base_url`（单字符串）→ `site.api_base_urls`（数组）。
//     新代码**只**认数组：不迁移的话，库里那行读出来是"没有地址"，
//     门户会回落站点地址、产物直链会回落请求 Host（反代下是 127.0.0.1）——
//     接口全绿、链接全错。
//  2. 删除 `site.logo_text`（已废弃：空图降级改用站点名称首字）。
//     不删的话，它会被 `Update` 的整对象回写一直"复活"。
//
// 幂等：①②的判据都是 jsonb 的键存在性，③ 的 `-` 对不存在的键是空操作。
// 只搬运不判合法性：`https://` 这种残缺值会原样搬进数组，随后被
// pkg/sitecfg 的读侧归一丢弃——迁移不该替运营"猜"他想填什么。
type SiteMigrationReport struct {
	// ConvertedRows 是从单地址转成数组的配置行数（0 或 1）。
	ConvertedRows int
	// StrippedRows 是被清掉废弃键的配置行数（0 或 1）。
	StrippedRows int
}

// MigrateSiteConfig 执行 M40；dryRun 只统计不写。
func MigrateSiteConfig(gdb *gorm.DB, dryRun bool) (SiteMigrationReport, error) {
	var rep SiteMigrationReport

	var toConvert int64
	if err := gdb.Raw(`SELECT count(*) FROM system_config
		WHERE key = 'site' AND NOT (value ? 'api_base_urls') AND value ? 'api_base_url'`).
		Scan(&toConvert).Error; err != nil {
		return rep, fmt.Errorf("db: M40 检查 api_base_url 失败: %w", err)
	}
	var toStrip int64
	if err := gdb.Raw(`SELECT count(*) FROM system_config
		WHERE key = 'site' AND (value ? 'logo_text' OR value ? 'api_base_url')`).
		Scan(&toStrip).Error; err != nil {
		return rep, fmt.Errorf("db: M40 检查废弃键失败: %w", err)
	}
	rep.ConvertedRows, rep.StrippedRows = int(toConvert), int(toStrip)
	if dryRun || (toConvert == 0 && toStrip == 0) {
		return rep, nil
	}

	// 先 `-` 再 `||`：顺序反了会因键冲突让旧键留下来（|| 取右侧）。
	if err := gdb.Exec(`UPDATE system_config
		SET value = (value - 'api_base_url') || jsonb_build_object('api_base_urls',
			CASE WHEN btrim(coalesce(value->>'api_base_url','')) = '' THEN '[]'::jsonb
			     ELSE jsonb_build_array(btrim(btrim(value->>'api_base_url'), '/')) END),
		    updated_at = now()
		WHERE key = 'site' AND NOT (value ? 'api_base_urls') AND value ? 'api_base_url'`).Error; err != nil {
		return rep, fmt.Errorf("db: M40 转换 api_base_urls 失败: %w", err)
	}
	// ② 与 ③ 合并：先把 logo_text 清掉，再把可能残留的 api_base_url 清掉
	if err := gdb.Exec(`UPDATE system_config
		SET value = value - 'logo_text' - 'api_base_url', updated_at = now()
		WHERE key = 'site' AND (value ? 'logo_text' OR value ? 'api_base_url')`).Error; err != nil {
		return rep, fmt.Errorf("db: M40 清理废弃键失败: %w", err)
	}
	return rep, nil
}

// LogSiteMigration 打印 M40 结果；无事可做时保持安静（每次启动都会跑）。
func LogSiteMigration(rep SiteMigrationReport) {
	if rep.ConvertedRows == 0 && rep.StrippedRows == 0 {
		return
	}
	logger.L().Info("db: 已迁移站点配置（单地址→多地址 / 清理废弃键）",
		zap.Int("converted", rep.ConvertedRows), zap.Int("stripped", rep.StrippedRows))
}
