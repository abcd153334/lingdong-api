package modelcfg

import (
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// 本文件是「单条绑定」的唯一写实现（PUT/DELETE /admin/models/:id/bindings/:channelId）。
//
// 为什么不复用整组替换：整组接口要求调用方先读出全部渠道、改一处、再整组回传，
// 两名管理员同时改同一模型的不同渠道时会互相覆盖（后写者把对方的渠道恢复成旧值）；
// 而且整组是硬删重建，created_at 与自增 id 全变，任何基于它的 CAS 立即失效。
//
// 但**校验不另写一份**：两个函数都是「合并/剔除出目标集合 → NormalizeReplace →
// 只写/只删那一行」。NormalizeReplace 已经是唯一校验入口（人工抽屉、AI apply 都走它），
// 单条路径只是把"写"缩小到一行。

// UpsertBindingTx 新增或更新一条绑定。
//
// 顺序不能换：先合并再校验，最后才落库。校验里「在售模型不能被改成零可用」
// 依赖库内当前状态（CountUsableBindings），先写再校验会把这个判据算错。
func UpsertBindingTx(tx *gorm.DB, m *model.Model, b Binding) ([]BindingNote, error) {
	if m == nil {
		return nil, fe("bindings", "模型不存在")
	}
	cur, err := Bindings(tx, m.ID)
	if err != nil {
		return nil, err
	}
	merged := make([]Binding, 0, len(cur)+1)
	replaced := false
	for _, x := range cur {
		if x.ChannelID == b.ChannelID {
			merged = append(merged, b)
			replaced = true
			continue
		}
		merged = append(merged, x)
	}
	if !replaced {
		merged = append(merged, b)
	}

	out, notes, err := NormalizeReplace(tx, m, merged)
	if err != nil {
		return nil, err
	}
	var target *Binding
	for i := range out {
		if out[i].ChannelID == b.ChannelID {
			target = &out[i]
			break
		}
	}
	if target == nil {
		return nil, fe("bindings", "渠道 #%d 未通过校验", b.ChannelID)
	}
	if err := upsertBindingRow(tx, m.ID, *target); err != nil {
		return nil, err
	}
	return notes, TouchTx(tx, m.ID)
}

// DeleteBindingTx 解除模型与某渠道的绑定。
//
// 先校验"删掉之后还合不合法"再删：NormalizeReplace 的「在售不能零可用」用的是
// 删除**之前**的可用数，顺序反过来（先删后校验）那个判据就会读成 0 而放行 ——
// 结果是模型在目录里显示在售、每次调用 503。
func DeleteBindingTx(tx *gorm.DB, m *model.Model, channelID uint64) error {
	if m == nil {
		return fe("bindings", "模型不存在")
	}
	cur, err := Bindings(tx, m.ID)
	if err != nil {
		return err
	}
	remaining := make([]Binding, 0, len(cur))
	found := false
	for _, x := range cur {
		if x.ChannelID == channelID {
			found = true
			continue
		}
		remaining = append(remaining, x)
	}
	if !found {
		return ErrBindingNotFound
	}
	if _, _, err := NormalizeReplace(tx, m, remaining); err != nil {
		return err
	}
	// 必须硬删：idx_model_channel 不含 deleted_at，软删行会永久占键，
	// 之后再绑同一对渠道必然 23505。
	if err := tx.Unscoped().
		Where("model_id = ? AND channel_id = ?", m.ID, channelID).
		Delete(&model.ModelBinding{}).Error; err != nil {
		return err
	}
	return TouchTx(tx, m.ID)
}

// upsertBindingRow 只写目标渠道那一行（其余渠道一个字节都不动）。
//
// 先 UPDATE 再看 RowsAffected 决定是否 INSERT，而不是 GORM 的
// Clauses(OnConflict)：后者不会带 updated_at，且 on conflict 的目标列名在
// 部分索引/软删语义下容易写错。
//
// 用 map 落库：`enabled` 带 default:true，结构体零值 false 会被列默认值吃掉。
func upsertBindingRow(tx *gorm.DB, modelID string, b Binding) error {
	now := time.Now()
	res := tx.Model(&model.ModelBinding{}).
		Where("model_id = ? AND channel_id = ?", modelID, b.ChannelID).
		Updates(map[string]any{
			"upstream_model": b.UpstreamModel,
			"weight":         b.Weight,
			"priority":       b.Priority,
			"enabled":        b.Enabled,
			"updated_at":     now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		return nil
	}
	return tx.Table("model_bindings").Create(map[string]any{
		"model_id":       modelID,
		"channel_id":     b.ChannelID,
		"upstream_model": b.UpstreamModel,
		"weight":         b.Weight,
		"priority":       b.Priority,
		"enabled":        b.Enabled,
		"created_at":     now,
		"updated_at":     now,
	}).Error
}
