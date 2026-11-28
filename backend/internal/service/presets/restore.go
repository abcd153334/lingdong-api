package presets

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	channelsvc "lingdong-api/internal/service/channel"
	"lingdong-api/internal/service/modelcfg"
	"lingdong-api/internal/service/pricing"
)

// 恢复出厂：把「内置标记命中」的行显式拉回出厂值。
//
// 与 Seed 的分工必须分清：
//   - Seed 只在缺失时补、绝不覆盖运营改动（版本升级也只补不改）；
//   - Restore 是**显式破坏性动作**：覆盖名称/地址/官网/价目/绑定，但**绝不动**
//     密钥、余额、健康快照与主键；默认保留当前启停，勾选后才回到停用。
//
// 实现上 dry-run 用"事务 + 回滚"取得精确计数：复制一套只读模拟逻辑迟早与写路径
// 分叉，而计数恰恰是运营点确认前唯一能看的依据。

// RestoreOptions 是恢复出厂的入参。
type RestoreOptions struct {
	// Group 为空 = 全部组；可传 slug（duomi）、行标记（builtin.duomi）或 marker。
	Group       string `json:"group"`
	DryRun      bool   `json:"dry_run"`
	KeepEnabled bool   `json:"keep_enabled"`
}

// RestoreCounts 是一个对象的三态计数。
type RestoreCounts struct {
	Reused  int `json:"reused"`
	Created int `json:"created"`
	Skipped int `json:"skipped"`
}

// RestoreGroupResult 是一组的恢复预览/结果。
type RestoreGroupResult struct {
	Group string `json:"group"`
	// Label 是本组渠道的出厂名（给人看的）。只做展示，不参与任何匹配 ——
	// slug 是机器标识，运营在弹窗里认不出「builtin.metaso-2」是哪一条。
	Label    string        `json:"label"`
	Channels RestoreCounts `json:"channels"`
	Models   RestoreCounts `json:"models"`
	Bindings RestoreCounts `json:"bindings"`
	Warnings []string      `json:"warnings,omitempty"`
}

// RestoreResult 是 API 响应。
type RestoreResult struct {
	DryRun bool                 `json:"dry_run"`
	Groups []RestoreGroupResult `json:"groups"`
}

// ErrUnknownPresetGroup 请求了不存在的组。
var ErrUnknownPresetGroup = errors.New("presets: 未知的内置预设组")

// ErrPresetsDisabled 是「内置预设功能被关掉」时恢复出厂的拒绝理由。
//
// 存在的意义：`LD_BUILTIN_PRESETS=0` 只有**一个**含义 —— 这台实例不要出厂预设。
// 如果恢复出厂绕过它，同一次部署里就会同时存在两种事实（启动不写、点一下又写），
// 而且绕过 Seed 建出来的行不带 marker，等于绕过版本管理。
var ErrPresetsDisabled = errors.New("presets: 内置预设已关闭（LD_BUILTIN_PRESETS）")

// errDryRun 只在事务内部用于回滚，不会外泄。
var errDryRun = errors.New("presets: dry-run rollback")

// Restore 恢复出厂（group 为空 = 全部）。
func Restore(gdb *gorm.DB, opts RestoreOptions) (*RestoreResult, error) {
	if !conf.EnvConfig().BuiltinPresets {
		return nil, ErrPresetsDisabled
	}
	selected, err := selectRestoreGroups(opts.Group)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{DryRun: opts.DryRun}
	err = gdb.Transaction(func(tx *gorm.DB) error {
		for _, g := range selected {
			gr, rerr := restoreGroup(tx, g, opts.KeepEnabled)
			if rerr != nil {
				return rerr
			}
			res.Groups = append(res.Groups, gr)
		}
		if opts.DryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return nil, err
	}
	if !opts.DryRun {
		// 恢复会改渠道状态/价目/绑定，目录必须立刻重建；不能等后台 10s 轮询。
		pricing.InvalidatePricingCache()
	}
	return res, nil
}

func selectRestoreGroups(group string) ([]presetGroup, error) {
	all := presetGroups()
	g := strings.TrimSpace(group)
	if g == "" {
		return all, nil
	}
	for _, it := range all {
		if g == it.MarkerKey || g == it.RowGroup() || g == strings.TrimPrefix(it.MarkerKey, "builtin_presets.") {
			return []presetGroup{it}, nil
		}
	}
	return nil, ErrUnknownPresetGroup
}

func restoreGroup(tx *gorm.DB, g presetGroup, keepEnabled bool) (RestoreGroupResult, error) {
	out := RestoreGroupResult{Group: g.RowGroup(), Label: g.Channel.Name}
	warn := func(format string, args ...any) {
		out.Warnings = append(out.Warnings, fmt.Sprintf(format, args...))
	}

	vendors, err := upsertPresetVendors(tx, g)
	if err != nil {
		return out, err
	}
	ch, err := restoreChannel(tx, g, keepEnabled, &out, warn)
	if err != nil {
		return out, err
	}
	if ch == nil {
		return out, nil // 渠道恢复失败（同名冲突等）：不再建绑定，避免挂到错误渠道
	}
	for _, m := range g.Models {
		restoreModel(tx, g, m, ch.ID, vendors, keepEnabled, &out, warn)
	}
	return out, nil
}

func restoreChannel(tx *gorm.DB, g presetGroup, keepEnabled bool, out *RestoreGroupResult, warn func(string, ...any)) (*model.Channel, error) {
	var ch model.Channel
	err := tx.Unscoped().Where("preset_group = ?", g.RowGroup()).Order("id").First(&ch).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		item, cerr := channelsvc.CreateFactoryTx(tx, channelReqOf(g.Channel))
		if cerr != nil {
			warn("渠道新建失败：%v", cerr)
			out.Channels.Skipped++
			return nil, nil
		}
		if err := tx.Model(&model.Channel{}).Where("id = ?", item.ID).
			Update("preset_group", g.RowGroup()).Error; err != nil {
			return nil, err
		}
		if err := tx.First(&ch, item.ID).Error; err != nil {
			return nil, err
		}
		out.Channels.Created++
	case err != nil:
		return nil, err
	default:
		if ch.DeletedAt.Valid {
			if uerr := tx.Unscoped().Model(&model.Channel{}).Where("id = ?", ch.ID).
				Update("deleted_at", nil).Error; uerr != nil {
				warn("渠道 #%d 无法解除删除（可能同名占用）：%v", ch.ID, uerr)
				out.Channels.Skipped++
				return nil, nil
			}
		}
		if _, uerr := channelsvc.UpdateTx(tx, ch.ID, channelReqOf(g.Channel), 0, ""); uerr != nil {
			// 单条失败不中断整组：其余模型/绑定仍应恢复，但必须留痕。
			warn("渠道 #%d 恢复失败：%v", ch.ID, uerr)
			out.Channels.Skipped++
		} else {
			out.Channels.Reused++
		}
	}
	if !keepEnabled {
		if err := tx.Model(&model.Channel{}).Where("id = ?", ch.ID).
			Updates(map[string]any{
				"status":  model.ChannelStatusDisabled,
				"version": gorm.Expr("version + 1"),
			}).Error; err != nil {
			return nil, err
		}
	}
	return &ch, nil
}

func restoreModel(tx *gorm.DB, g presetGroup, m presetModel, channelID uint64, vendors map[string]string, keepEnabled bool, out *RestoreGroupResult, warn func(string, ...any)) {
	var row model.Model
	err := tx.Unscoped().Where("id = ?", m.ID).First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		enabled := false
		in := presetModelInput(m, vendors, g.Vendor.ID, &enabled)
		if _, cerr := modelcfg.CreateTx(tx, in, nil); cerr != nil {
			warn("模型 %s 新建失败：%v", m.ID, cerr)
			out.Models.Skipped++
			return
		}
		if err := tx.Model(&model.Model{}).Where("id = ?", m.ID).
			Update("preset_group", g.RowGroup()).Error; err != nil {
			warn("模型 %s 标记失败：%v", m.ID, err)
			out.Models.Skipped++
			return
		}
		out.Models.Created++
	case err != nil:
		warn("模型 %s 读取失败：%v", m.ID, err)
		out.Models.Skipped++
		return
	default:
		switch {
		case row.PresetGroup == "":
			// 运营自建（未认领）：恢复出厂不碰它，也不给它建绑定。
			out.Models.Skipped++
			return
		case row.PresetGroup != g.RowGroup():
			// 同 ID 模型归另一个组（如 duomi 与方舟共享 seedance）：只补本渠道绑定。
		default:
			if row.DeletedAt.Valid {
				if uerr := tx.Unscoped().Model(&model.Model{}).Where("id = ?", m.ID).
					Update("deleted_at", nil).Error; uerr != nil {
					warn("模型 %s 无法解除删除：%v", m.ID, uerr)
					out.Models.Skipped++
					return
				}
			}
			var enabled *bool
			if !keepEnabled {
				f := false
				enabled = &f
			}
			in := presetModelInput(m, vendors, g.Vendor.ID, enabled)
			if _, uerr := modelcfg.UpdateTx(tx, in, 0); uerr != nil {
				warn("模型 %s 恢复失败：%v", m.ID, uerr)
				out.Models.Skipped++
			} else {
				out.Models.Reused++
			}
		}
	}
	restoreBinding(tx, m, channelID, keepEnabled, out, warn)
}

func restoreBinding(tx *gorm.DB, m presetModel, channelID uint64, keepEnabled bool, out *RestoreGroupResult, warn func(string, ...any)) {
	weight, priority := m.BindingWeight, m.BindingPriority
	if weight <= 0 {
		weight = 1
	}
	if priority <= 0 {
		priority = 1
	}
	upstream := strings.TrimSpace(m.BindingUpstream)
	if upstream == "" {
		upstream = m.ID
	}
	var b model.ModelBinding
	err := tx.Unscoped().Where("model_id = ? AND channel_id = ?", m.ID, channelID).First(&b).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := addDisabledBinding(tx, m.ID, channelID, upstream, weight, priority); err != nil {
			warn("绑定 %s 新建失败：%v", m.ID, err)
			out.Bindings.Skipped++
			return
		}
		out.Bindings.Created++
	case err != nil:
		warn("绑定 %s 读取失败：%v", m.ID, err)
		out.Bindings.Skipped++
	default:
		if b.DeletedAt.Valid {
			if uerr := tx.Unscoped().Model(&model.ModelBinding{}).Where("id = ?", b.ID).
				Update("deleted_at", nil).Error; uerr != nil {
				warn("绑定 %s 无法解除删除：%v", m.ID, uerr)
				out.Bindings.Skipped++
				return
			}
		}
		updates := map[string]any{
			"upstream_model": upstream,
			"weight":         weight,
			"priority":       priority,
		}
		if !keepEnabled {
			updates["enabled"] = false
		}
		if uerr := tx.Model(&model.ModelBinding{}).Where("id = ?", b.ID).
			Updates(updates).Error; uerr != nil {
			warn("绑定 %s 恢复失败：%v", m.ID, uerr)
			out.Bindings.Skipped++
			return
		}
		out.Bindings.Reused++
	}
}
