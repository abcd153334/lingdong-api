package modelcfg

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	specdsl "lingdong-api/internal/relay/spec"
	specsvc "lingdong-api/internal/service/spec"
)

// maxWeight 是权重/优先级的上限（与人工保存一致）。
//
// 不设上限时，AI 可以写 10^9 —— 同层其它渠道会被永远饿死，
// 而现象只是"某些渠道没流量了"，几乎无法归因。
const maxWeight = 1000

// BindingNote 是一条**非阻断**告警：绑定已经保存成功，但首次调用可能失败。
//
// 为什么不做成硬错误：绑定与渠道配置是两个页面，合理的操作顺序可能相反
// （先绑模型、后补渠道 model_overrides）。强行拦截会把「顺序问题」变成「配置死锁」。
// 但也不能静默：没有这条告警，运营只能等用户报障才知道模型用不了。
type BindingNote struct {
	// ChannelID 指向出问题的渠道（前端据此定位到抽屉里的那一行）。
	ChannelID uint64 `json:"channel_id"`
	// UpstreamModel 是这次提交的上游模型名（便于对照）。
	UpstreamModel string `json:"upstream_model"`
	// Message 是给运营看的可读原因，可能包含可直接粘贴的配置片段。
	Message string `json:"message"`
}

// channelRow 是校验期需要的渠道快照（一次查出，避免逐条回表）。
type channelRow struct {
	ID             uint64
	Name           string
	AdapterID      string
	Status         int
	AdapterOptions model.JSON
}

// NormalizeBindings 校验并归一绑定集合（**新建模型**路径：库里还没有该模型的状态）。
//
// 七条规则都是"写进去不报错、运行期才炸"的那一类：
// 渠道不存在/停用/模态不匹配 → 模型在目录里可见但每次调用 503；
// 同一渠道两条 → 撞唯一索引，报错里没有任何业务信息；
// 一条启用都没有 → 模型既不进目录也不报错，运营以为上线了。
func NormalizeBindings(tx *gorm.DB, modality string, items []Binding) ([]Binding, error) {
	out, _, err := normalizeBindings(tx, modality, nil, items)
	return out, err
}

// NormalizeReplace 是**替换已有模型绑定**的唯一校验入口（人工抽屉、AI apply 都走它）。
//
// 与 NormalizeBindings 的差别只有三处，都是「已有库内状态」才能判的：
//  1. 存量豁免：库里已存在的渠道不再重判「是否启用 / 适配器是否支持模态」——
//     渠道被自动封禁后，运营在模型页改个显示名都要保存，逐条重判会把配置页锁死；
//  2. 前后对比：只拦「本来能路由、这次被改成不能路由」的动作（P0-13 的静默不一致）；
//  3. 预检：对启用绑定跑一次适配器预检，产出非阻断告警（见 relay.ModelPreflighter）。
func NormalizeReplace(tx *gorm.DB, m *model.Model, items []Binding) ([]Binding, []BindingNote, error) {
	if m == nil {
		return nil, nil, fe("bindings", "模型不存在")
	}
	return normalizeBindings(tx, m.Modality, m, items)
}

// normalizeBindings 是两条入口的共享实现。m 为 nil 表示新建（没有库内状态）。
func normalizeBindings(tx *gorm.DB, modality string, m *model.Model, items []Binding) ([]Binding, []BindingNote, error) {
	// 重复渠道的处理按路径分：
	//   - 新建（m == nil，AI 提案 / 建模型带绑定）：报错。重复说明 plan 写错了，
	//     静默去重会让模型永远学不会；
	//   - 替换（m != nil，人工整页 PUT）：后出现的覆盖先出现的。整页回传的语义是
	//     「最后一次编辑生效」，硬拒会让前端某些回传顺序直接 400。
	seen := make(map[uint64]int, len(items))
	out := make([]Binding, 0, len(items))
	for i := range items {
		b := items[i]
		b.UpstreamModel = strings.TrimSpace(b.UpstreamModel)
		if b.UpstreamModel == "" {
			return nil, nil, fe(fmt.Sprintf("bindings[%d].upstream_model", i), "必须填写上游模型名")
		}
		if b.ChannelID == 0 {
			return nil, nil, fe(fmt.Sprintf("bindings[%d].channel_id", i), "必须指定渠道")
		}
		if b.Weight <= 0 {
			b.Weight = 1
		}
		if b.Priority <= 0 {
			b.Priority = 1
		}
		if b.Weight > maxWeight || b.Priority > maxWeight {
			return nil, nil, fe(fmt.Sprintf("bindings[%d]", i), "权重与优先级必须在 1..%d 之间", maxWeight)
		}
		if prev, dup := seen[b.ChannelID]; dup {
			if m == nil {
				return nil, nil, fe(fmt.Sprintf("bindings[%d].channel_id", i),
					"与第 %d 项指向同一个渠道（同一模型在同一渠道上只能有一条绑定）", prev+1)
			}
			out[prev] = b
			continue
		}
		seen[b.ChannelID] = len(out)
		out = append(out, b)
	}

	// ② 清空绑定：在售模型不能清（清了就是"启用 + 零可用绑定"，目录里消失、列表里仍显示已启用）。
	if len(out) == 0 {
		if m == nil {
			return nil, nil, ErrNoEnabledBinding
		}
		if m.Enabled {
			before, err := CountUsableBindings(tx, m.ID)
			if err != nil {
				return nil, nil, err
			}
			if before > 0 {
				return nil, nil, fe("bindings", "该模型目前在售：至少保留一条「启用绑定 + 启用渠道」，或先停用该模型再调整")
			}
		}
		return out, []BindingNote{}, nil
	}

	// ③ 一次查出全部渠道（含 adapter_options，供预检解析；该列不含密钥）。
	ids := make([]uint64, 0, len(out))
	for _, b := range out {
		ids = append(ids, b.ChannelID)
	}
	var chans []channelRow
	if err := tx.Model(&model.Channel{}).
		Select("id", "name", "adapter_id", "status", "adapter_options").
		Where("id IN ?", ids).Scan(&chans).Error; err != nil {
		return nil, nil, err
	}
	byID := make(map[uint64]channelRow, len(chans))
	for _, ch := range chans {
		byID[ch.ID] = ch
	}

	// ④ 存量集合（新建模型时为空，于是全部走严格判定）。
	existing := make(map[uint64]bool, len(out))
	if m != nil {
		var old []uint64
		if err := tx.Model(&model.ModelBinding{}).Where("model_id = ?", m.ID).
			Pluck("channel_id", &old).Error; err != nil {
			return nil, nil, err
		}
		for _, id := range old {
			existing[id] = true
		}
	}

	notes := make([]BindingNote, 0, 2)
	hasEnabled := false
	for i := range out {
		b := out[i]
		ch, ok := byID[b.ChannelID]
		if !ok {
			return nil, nil, fe(fmt.Sprintf("bindings[%d].channel_id", i), "渠道 #%d 不存在或已删除", b.ChannelID)
		}
		if !existing[b.ChannelID] {
			if ch.Status != model.ChannelStatusEnabled {
				return nil, nil, fe(fmt.Sprintf("bindings[%d].channel_id", i),
					"渠道「%s」已停用，不能作为绑定目标", ch.Name)
			}
			if !adapterSatisfiesModality(tx, ch.AdapterID, modality) {
				return nil, nil, fe(fmt.Sprintf("bindings[%d].channel_id", i),
					"渠道「%s」的适配器 %s 不支持模态 %s", ch.Name, ch.AdapterID, modality)
			}
		}
		// 存量绑定：渠道状态与模态都不重判（见函数注释的豁免说明）。
		if b.Enabled {
			hasEnabled = true
		}
		// 预检只对「会真正路由」的绑定做：禁用绑定或停用渠道不产生告警噪音。
		if m != nil && b.Enabled && ch.Status == model.ChannelStatusEnabled {
			if pf, supported := relay.PreflightModel(ch.AdapterID, m.ID, []byte(ch.AdapterOptions)); supported {
				switch {
				case !pf.OK:
					notes = append(notes, BindingNote{
						ChannelID: b.ChannelID, UpstreamModel: b.UpstreamModel,
						Message: "该渠道可能无法承接模型 " + m.ID + "：" + pf.Reason,
					})
				case pf.Note != "":
					notes = append(notes, BindingNote{
						ChannelID: b.ChannelID, UpstreamModel: b.UpstreamModel,
						Message: pf.Note,
					})
				}
			}
		}
	}

	// ⑤ 新建模型必须有至少一条启用绑定（否则建出来就是不可用）。
	if m == nil && !hasEnabled {
		return nil, nil, ErrNoEnabledBinding
	}

	// ⑥ 在售模型的「可路由」不能被这次保存改写成「不可路由」。
	if m != nil && m.Enabled && !hasEnabled {
		before, err := CountUsableBindings(tx, m.ID)
		if err != nil {
			return nil, nil, err
		}
		if before > 0 {
			return nil, nil, fe("bindings", "该模型目前在售：至少保留一条「启用绑定 + 启用渠道」，或先停用该模型再调整")
		}
	}

	// ⑦ 端点覆盖：声明的每个端点都要有启用绑定能承接。
	if m != nil {
		if err := checkCoverageOf(tx, byID, out, decodeStrings(m.Endpoints), false); err != nil {
			return nil, nil, err
		}
	}
	return out, notes, nil
}

// CheckCoverage 校验"模型声明的端点"能否被至少一个启用绑定承接。
//
// 这是唯一实现：提案期的覆盖校验也走这里（试跑），不再另写一份规则。
func CheckCoverage(tx *gorm.DB, items []Binding, endpoints []string) error {
	if len(endpoints) == 0 {
		// 模型进入"有绑定"状态就是它要被放到线上：此时端点为空意味着
		// 文档里给不出任何一节、/v1/models 也说不出接入路径，而模型广场
		// 上它照样可点。必须当场拦住，否则就是"列得出来、调不通"。
		return fe("endpoints", "必须声明该模型支持的端点（可用端点见 schema_get）")
	}
	ids := make([]uint64, 0, len(items))
	for _, b := range items {
		if b.Enabled {
			ids = append(ids, b.ChannelID)
		}
	}
	var chans []channelRow
	if len(ids) > 0 {
		if err := tx.Model(&model.Channel{}).
			Select("id", "name", "adapter_id", "status").
			Where("id IN ?", ids).Scan(&chans).Error; err != nil {
			return err
		}
	}
	byID := make(map[uint64]channelRow, len(chans))
	for _, ch := range chans {
		byID[ch.ID] = ch
	}
	return checkCoverageOf(tx, byID, items, endpoints, true)
}

// checkCoverageOf 用已查出的渠道快照做端点覆盖判定（避免逐条回表）。
//
// strictUnknown 决定「无法识别的端点写法」是报错还是跳过：
//   - 新建模型（strict=true）：端点是这次提交的一部分，写错就该当场改 ——
//     AI 尤其需要一个明确的报错，否则它会反复复制同一个错别名；
//   - 替换绑定（strict=false）：端点是**模型已有的**配置，历史别名/运营备注很常见，
//     硬拒会让「只改个权重」都保存不了。真正不支持的端点会在运行期被模态/端点闸门拦住。
func checkCoverageOf(tx *gorm.DB, byID map[uint64]channelRow, items []Binding, endpoints []string, strictUnknown bool) error {
	if len(endpoints) == 0 {
		// 空端点 = 这个模型没有任何接口，却即将挂上渠道。文档里给不出一节、
		// /v1/models 说不出接入路径，而模型广场照样可点 —— 必须当场拦住。
		return fe("endpoints", "必须声明该模型支持的端点（可用端点见 schema_get）")
	}
	adapters := make([]string, 0, len(items))
	for _, b := range items {
		if !b.Enabled {
			continue
		}
		if ch, ok := byID[b.ChannelID]; ok {
			adapters = append(adapters, ch.AdapterID)
		}
	}
	// 一条启用绑定都没有 → 端点覆盖无从谈起（那是「模型整体不可路由」，
	// 由「在售模型不能改成零可用」那条规则负责），这里不能重复报同一个问题：
	// 非在售模型允许全停用，报「端点没承接」只会让人以为要改 endpoints。
	if len(adapters) == 0 {
		return nil
	}
	for _, key := range endpoints {
		if _, ok := apidoc.KindOfEndpointKey(key); !ok {
			if strictUnknown {
				return fe("endpoints", "未知端点 %q（可用端点见 schema_get）", key)
			}
			continue
		}
		covered := false
		for _, id := range adapters {
			if adapterCovers(tx, id, key) {
				covered = true
				break
			}
		}
		if !covered {
			return fe("endpoints", "声明的端点 %q 没有任何启用绑定能承接（会出现在文档里但调用必然失败）", key)
		}
	}
	return nil
}

// adapterSatisfiesModality 判断渠道适配器能否服务该模态。
//
// 规格适配器必须用**事务内**的规格定义判（注册表里还没有它）：
// 否则"新建规格 → 新建渠道 → 新建模型 → 建绑定"这条最常见的提案
// 会在最后一步被判成"适配器不支持该模态"。
func adapterSatisfiesModality(tx *gorm.DB, adapterID, modality string) bool {
	slug, isSpec := specsvc.SlugOf(adapterID)
	if !isSpec {
		return relay.AdapterSatisfiesModality(adapterID, modality)
	}
	var row model.AdapterSpec
	if err := tx.Where("slug = ?", slug).Limit(1).Find(&row).Error; err != nil || row.ID == 0 {
		return false
	}
	sp, err := specdsl.Decode(row.Spec)
	if err != nil {
		return false
	}
	return relay.AdapterSatisfiesModalityCaps(sp.Caps(), modality, sp.SupportsEndpointKey)
}

// adapterCovers 判断某适配器能否承接端点。
//
// 规格适配器必须查**传入的连接**：同一提案里"建规格 + 建渠道 + 建模型"时，
// 规格尚未提交，relay 的注册表里没有它，用 relay.SupportsEndpointOn 会误判。
func adapterCovers(tx *gorm.DB, adapterID, key string) bool {
	if slug, ok := specsvc.SlugOf(adapterID); ok {
		var row model.AdapterSpec
		if err := tx.Where("slug = ?", slug).Limit(1).Find(&row).Error; err != nil || row.ID == 0 {
			return false
		}
		sp, err := specdsl.Decode(row.Spec)
		if err != nil {
			return false
		}
		return sp.SupportsEndpointKey(key)
	}
	// 内置适配器走与人工绑定校验同一个判定（能力矩阵 ∩ 端点实现）。
	pr := relay.ProbeEndpointPath(key)
	if !pr.Known {
		return false
	}
	return relay.AdapterSatisfiesProbe(adapterID, pr)
}

// ReplaceBindingsTx 用给定集合整体替换模型的绑定。
//
// **必须硬删**（Unscoped）：`idx_model_channel` 是 (model_id, channel_id) 上的
// 普通唯一索引，不含 deleted_at 条件 —— 软删的行仍然占着键，随后插入同一对
// 必然 23505。历史上这条让"替换绑定"与"回滚提案"100% 失败。
func ReplaceBindingsTx(tx *gorm.DB, modelID string, items []Binding) error {
	if err := tx.Unscoped().Where("model_id = ?", modelID).
		Delete(&model.ModelBinding{}).Error; err != nil {
		return err
	}
	now := time.Now()
	for _, b := range items {
		// 用 map 落库：GORM 对带 default 的列会丢 false/0。
		row := map[string]any{
			"model_id":       modelID,
			"channel_id":     b.ChannelID,
			"upstream_model": b.UpstreamModel,
			"weight":         b.Weight,
			"priority":       b.Priority,
			"enabled":        b.Enabled,
			"created_at":     now,
			"updated_at":     now,
		}
		if err := tx.Table("model_bindings").Create(row).Error; err != nil {
			return err
		}
	}
	return nil
}

// CountUsableBindings 统计「绑定启用且渠道启用」的条数 —— 模型能否被路由的硬条件。
//
// 为什么必须同时看两张表：绑定表的 enabled 只表达"运营勾了这条"，
// 真正决定能不能路由的是渠道状态。只数绑定会把「渠道已停用」的模型算成可用，
// 于是启用成功、目录显示在售、调用必然 503（这就是 P0-13 的原始症状）。
//
// 两个 deleted_at 都要判，且含义不同：
//   - `b.deleted_at IS NULL`：绑定行是软删表，删过的行还在库里；
//   - `c.deleted_at IS NULL`：渠道被**软删**时 `channels` 行仍在，不加这一条
//     这个 JOIN 会把它算成"可用渠道"——模型显示可路由、实际永远选不中。
//     只有被硬删的渠道才靠 JOIN 自然消失。
func CountUsableBindings(tx *gorm.DB, modelID string) (int64, error) {
	var n int64
	err := tx.Table("model_bindings AS b").
		Joins("JOIN channels AS c ON c.id = b.channel_id AND c.deleted_at IS NULL").
		Where("b.model_id = ? AND b.enabled = ? AND b.deleted_at IS NULL AND c.status = ?",
			modelID, true, model.ChannelStatusEnabled).
		Count(&n).Error
	return n, err
}

// SetBindingEnabledTx 启停单条绑定。
func SetBindingEnabledTx(tx *gorm.DB, modelID string, channelID uint64, enabled bool) error {
	res := tx.Model(&model.ModelBinding{}).
		Where("model_id = ? AND channel_id = ?", modelID, channelID).
		Updates(map[string]any{"enabled": enabled, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fe("bindings", "模型 %s 没有绑定到渠道 #%d", modelID, channelID)
	}
	return nil
}

// Bindings 读取模型的当前绑定（按渠道路径稳定排序）。
func Bindings(tx *gorm.DB, modelID string) ([]Binding, error) {
	var rows []model.ModelBinding
	if err := tx.Where("model_id = ?", modelID).
		Order("channel_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Binding, 0, len(rows))
	for _, r := range rows {
		out = append(out, Binding{
			ChannelID: r.ChannelID, UpstreamModel: r.UpstreamModel,
			Weight: r.Weight, Priority: r.Priority, Enabled: r.Enabled,
		})
	}
	return out, nil
}
