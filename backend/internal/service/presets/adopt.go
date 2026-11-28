package presets

import (
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	channelsvc "lingdong-api/internal/service/channel"
)

// adoptMarker 是「认领存量行」的一次性标记。
const adoptMarker = "m66_preset_adopt"

// Adopt 把**已存在的运营渠道/模型**认领为内置（只补 preset_group，不改业务字段）。
//
// 为什么需要它：老库升级时 presets.Seed 走"命中标记/同名"路径，通常只复用、不新建；
// 而火山方舟那组在老版本就写过标记，Seed 直接跳过 —— 结果内置标记永远为空，
// 「恢复出厂」在最需要它的老库上没有对象。
//
// 匹配规则与 resolveGroupChannel 对齐：adapter + 归一化 base + 名称全等，且只认
// preset_group 为空的行；模型必须**确实绑在这条渠道上**才认领（避免把同名无关模型标成内置）。
func Adopt(gdb *gorm.DB) error {
	if !conf.EnvConfig().BuiltinPresets {
		return nil
	}
	var applied model.DataMigration
	err := gdb.Where("name = ?", adoptMarker).First(&applied).Error
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return fmt.Errorf("读取内置认领标记失败: %w", err)
	}

	return gdb.Transaction(func(tx *gorm.DB) error {
		channels, models := 0, 0
		for _, g := range presetGroups() {
			var ch model.Channel
			err := tx.Where("preset_group = ?", g.RowGroup()).Order("id").First(&ch).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				base := channelsvc.NormalizeBaseURL(g.Channel.BaseURL)
				err = tx.Where("adapter_id = ? AND name = ? AND preset_group = ''",
					g.Channel.AdapterID, g.Channel.Name).Order("id").First(&ch).Error
				if err == nil && channelsvc.NormalizeBaseURL(ch.BaseURL) != base {
					err = gorm.ErrRecordNotFound
				}
			}
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return fmt.Errorf("查询待认领渠道失败(%s): %w", g.MarkerKey, err)
			}
			if ch.PresetGroup == "" {
				if err := tx.Model(&model.Channel{}).Where("id = ?", ch.ID).
					Update("preset_group", g.RowGroup()).Error; err != nil {
					return fmt.Errorf("认领渠道 #%d 失败: %w", ch.ID, err)
				}
				channels++
			}
			for _, m := range g.Models {
				var n int64
				if err := tx.Model(&model.ModelBinding{}).
					Where("model_id = ? AND channel_id = ?", m.ID, ch.ID).
					Count(&n).Error; err != nil {
					return fmt.Errorf("检查模型 %s 绑定失败: %w", m.ID, err)
				}
				if n == 0 {
					continue // 不在本渠道上：不是本组的模型
				}
				res := tx.Model(&model.Model{}).
					Where("id = ? AND preset_group = ''", m.ID).
					Update("preset_group", g.RowGroup())
				if res.Error != nil {
					return fmt.Errorf("认领模型 %s 失败: %w", m.ID, res.Error)
				}
				if res.RowsAffected > 0 {
					models++
				}
			}
		}
		if err := tx.Create(&model.DataMigration{
			Name:      adoptMarker,
			AppliedAt: time.Now().UTC(),
		}).Error; err != nil {
			return fmt.Errorf("写入内置认领标记失败: %w", err)
		}
		logger.L().Info("内置预设认领完成", zap.Int("channels", channels), zap.Int("models", models))
		return nil
	})
}
