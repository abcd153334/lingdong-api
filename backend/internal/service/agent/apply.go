package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/channel"
	"lingdong-api/internal/service/lifecycle"
	"lingdong-api/internal/service/modelcfg"
	specsvc "lingdong-api/internal/service/spec"
)

var (
	ErrProposalNotFound = errors.New("agent: 提案不存在")
	ErrHashMismatch     = errors.New("agent: 提案内容与确认时不一致，请刷新")
	ErrNotApplyable     = errors.New("agent: 该提案当前状态不可应用")
	ErrStale            = errors.New("agent: 提案基于旧数据生成，请重新生成")
	// ErrDrift 回滚时实体已被改动（不能自动回滚）。
	ErrDrift = errors.New("agent: 提案应用后相关数据又被修改，无法自动回滚")
)

// ApplyResult 是 apply 的结果（前端据此刷新并展示"改了什么"）。
type ApplyResult struct {
	ProposalID uint64          `json:"proposal_id"`
	Ops        int             `json:"ops"`
	Diff       json.RawMessage `json:"diff"`
	// TouchedSpecs 是被改动的规格 slug（提交后需要失效注册表）。
	TouchedSpecs []string `json:"touched_specs,omitempty"`
	// Warnings 是**非阻断**告警（数据已生效，但首次调用可能失败）。
	//
	// 恒定下发数组（空为 []），前端直接取 .length。
	Warnings []string `json:"warnings"`
}

// applySink 收集「事务内产生、事务外仍需要」的副作用。
//
// 为什么单独一个结构而不是继续加参数：applyOp 已经有 6 个参数，再塞一个 warnings
// 就没人看得懂签名了；而试跑（trialApply）同样需要带出告警，正好共用同一份。
type applySink struct {
	TouchedSpecs []string
	Warnings     []string
}

// addNotes 把绑定预检的告警并入 sink（供 applyOp 在事务内调用）。
func (s *applySink) addNotes(notes []modelcfg.BindingNote) {
	for _, n := range notes {
		s.Warnings = append(s.Warnings, n.Message)
	}
}

// Apply 应用一条提案：抢占 → 校验 → 单事务执行 → 快照 → 审计。
//
// 事务边界是**整个 apply**：任何一个操作失败都整条回滚。
// 为什么不做"部分成功"：提案描述的是一个自洽的配置（规格 + 渠道 + 模型 + 绑定），
// 只落一半的结果通常是"模型存在但没有绑定"这种门户看不见、调用全失败的状态。
//
// 缓存失效（定价/规格注册表）必须在**提交之后**：那两处都会广播给其它副本，
// 而广播不可回滚 —— 事务内广播会让别的实例读到"未来状态"。
func Apply(ctx context.Context, proposalID uint64, planHash string, adminID uint64) (*ApplyResult, error) {
	var prop model.AgentProposal
	if err := db.DB.WithContext(ctx).First(&prop, proposalID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProposalNotFound
		}
		return nil, err
	}
	if prop.Kind != "change" {
		// unsupported 提案只是结论沉淀，没有可执行的操作。
		return nil, ErrNotApplyable
	}
	plan, err := DecodePlan(prop.Plan)
	if err != nil {
		return nil, err
	}
	// dry-run 闸门：只对"新建渠道 / 改上游地址"生效。
	//
	// 为什么不全量强制：纯改名/改价的提案如果也要求探测，运营会被迫为了改价
	// 先去点一次连通性测试 —— 那是流程负担，不是风险控制。
	//
	// 凭据的唯一来源是 agent_dry_runs（跨轮有效），**不再**读提案行的 dry_run 快照：
	// 那份快照只在"同一轮先探测再提交"时才会写，运营分两步做就永远是空的
	// —— 这正是"明明探过了、点应用还是要求探测"的根因。
	set, serr := Settings(ctx)
	if serr != nil {
		// 读不到设置时拒绝而不是放行：一次 DB 抖动就 fail-open，
		// 等于"没验证也能上线"，而这正是这道闸门存在的理由。
		return nil, fmt.Errorf("读取助手设置失败，暂不能应用：%w", serr)
	}
	if set.DryRunRequired {
		targets, terr := ProbeTargets(ctx, db.DB, prop.SessionID, plan)
		if terr != nil {
			return nil, terr
		}
		if bad := unsatisfied(targets); len(bad) > 0 {
			return nil, &DryRunRequiredError{Targets: bad}
		}
	}

	// 防御性重校验：规则收紧后，历史提案不能按旧规则执行。
	// 只跑**结构**规则（不查库）——语义由 applyOp 自己保证。
	if err := plan.ValidateStructure(ProposalKindChange); err != nil {
		return nil, fmt.Errorf("%w：%v", ErrNotApplyable, err)
	}
	// 幂等/防篡改：请求体里的 hash 必须与服务端存档一致。
	//
	// 空串**不再**放过：`if hash != "" && hash != want` 的写法让"不传 hash"
	// 等于跳过校验，防篡改形同虚设。确认框一定会带 hash，缺了就是调用方有问题。
	if strings.TrimSpace(planHash) == "" || strings.TrimSpace(planHash) != prop.PlanHash {
		return nil, ErrHashMismatch
	}

	snap := newSnapshot()
	res := &ApplyResult{ProposalID: proposalID}
	// sink 收集「事务内产生、事务外仍要用」的东西：试跑会回滚事务，但它活在 Go 栈上。
	sink := &applySink{}
	err = db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 抢占（第一句，锁住提案行）：双击/并发 apply 只有一个能继续。
		cas := tx.Model(&model.AgentProposal{}).
			Where("id = ? AND status = ?", proposalID, model.ProposalDraft).
			Update("status", model.ProposalApplying)
		if cas.Error != nil {
			return cas.Error
		}
		if cas.RowsAffected != 1 {
			return ErrNotApplyable
		}

		// 2) 校验版本（stale 判定）
		if err := checkBase(tx, plan, prop.BaseVersions); err != nil {
			return err
		}

		// 3) 逐 op 执行（按依赖顺序，不信任 AI 给的顺序）
		createdChannels := map[string]uint64{}
		for _, item := range plan.Ordered() {
			op := item.Op
			if err := applyOp(ctx, tx, &op, snap, createdChannels, sink, adminID); err != nil {
				// 用**原始下标**报位置：与提案 JSON 里的 operations[i] 对齐。
				return fmt.Errorf("第 %d 个操作失败：%w", item.Index+1, err)
			}
			res.Ops++
		}

		// 4) 快照（before 全量 + after_versions + 当时口径）——回滚靠它
		if snap.Basis == nil {
			snap.Basis = map[string]any{
				"currency": conf.SiteCurrency(),
				"scale":    money.Scale,
			}
		}
		snapJSON, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		// 5) 审计：与事务同生共死（资金/配置级操作不能用 RecordOrWarn）
		if err := audit.Record(tx, adminID, audit.ActionAgentApply, "proposal", fmt.Sprintf("%d", proposalID),
			map[string]any{"ops": res.Ops, "summary": plan.Summary}); err != nil {
			return err
		}
		now := time.Now()
		return tx.Model(&model.AgentProposal{}).Where("id = ?", proposalID).Updates(map[string]any{
			"status":     model.ProposalApplied,
			"snapshot":   model.JSON(snapJSON),
			"applied_by": adminID,
			"applied_at": now,
			"updated_at": now,
		}).Error
	})
	if err != nil {
		// 失败要落一个可解释的终态：卡在 applying 会让提案永远不可再操作。
		status := model.ProposalFailed
		switch {
		case errors.Is(err, ErrNotApplyable):
			status = "" // 状态没被我们改过，保持原样
		case errors.Is(err, ErrStale), errors.Is(err, ErrHashMismatch):
			status = model.ProposalStale
		}
		if status != "" {
			_ = db.DB.Model(&model.AgentProposal{}).Where("id = ?", proposalID).Updates(map[string]any{
				"status": status, "error": err.Error(), "updated_at": time.Now(),
			}).Error
		}
		return nil, err
	}

	// 事务成功后才落到结果上：sink 在回滚路径上不会污染 res。
	res.TouchedSpecs = sink.TouchedSpecs
	res.Warnings = sink.Warnings

	// 提交后：规格注册表失效 + 统一后置管线（定价失效 + 文档重算）。
	//
	// 顺序不能反：文档重算读的是 pricing 快照，先失效再重算才能看到新价。
	for _, slug := range res.TouchedSpecs {
		specsvc.Invalidate(slug)
		// 单条规格也要留痕：事务内不写审计（trialApply 会泄漏），只能在这里补。
		audit.RecordOrWarn(db.DB, adminID, audit.ActionAdapterSpecUpdate, "adapter_spec", slug,
			map[string]any{"via": "agent.apply", "proposal_id": proposalID})
	}
	lifecycle.AfterChange()
	// diff 直接复用提案存档里的那份：BuildDiff 要在**写库之前**算才有 before，
	// 提交后再算得到的 before 已经是改后值（before == after，确认框全是"无变化"）。
	res.Diff = json.RawMessage(prop.Diff)
	return res, nil
}

// applySnapshot 是 apply 前的完整现场（回滚依据）。
//
// 存"完整 before 行"而不是"diff 的反向"：diff 会随实现变化，
// 而完整行永远能还原；代价只是几十 KB JSON。
type applySnapshot struct {
	Vendors map[string]json.RawMessage `json:"vendors,omitempty"`
	Specs   map[string]json.RawMessage `json:"specs,omitempty"`
	// SpecSlug 是 id → slug 的映射。
	//
	// 为什么需要它：反向提案只拿得到 op.ID（自增主键），而快照以 slug 为键。
	// 过去靠"取 map 里第一个 key"猜，多个规格时回滚会还原**另一个规格**。
	SpecSlug map[uint64]string          `json:"spec_slug,omitempty"`
	Channels map[string]json.RawMessage `json:"channels,omitempty"`
	Models   map[string]json.RawMessage `json:"models,omitempty"`
	Bindings map[string]json.RawMessage `json:"bindings,omitempty"`
	// AfterVersions 是应用后各实体的版本号（回滚时要求逐一相符，否则拒绝）。
	AfterVersions map[string]int `json:"after_versions,omitempty"`
	// Basis 是应用时的实体版本，回滚提示可据此判断"价格体系是否已变"。
	Basis map[string]any `json:"basis,omitempty"`
}

func newSnapshot() *applySnapshot {
	return &applySnapshot{
		Vendors: map[string]json.RawMessage{}, Specs: map[string]json.RawMessage{},
		Channels: map[string]json.RawMessage{}, Models: map[string]json.RawMessage{},
		Bindings: map[string]json.RawMessage{}, AfterVersions: map[string]int{},
		SpecSlug: map[uint64]string{},
	}
}

// checkBase 校验提案生成时的事实是否仍然成立（stale 判定）。
func checkBase(tx *gorm.DB, p *Plan, base model.JSON) error {
	// 结算币种变了 → 提案里写的金额单位已不是同一个东西（CNY 的 0.15 元 ≠ USD 的 0.15 元），
	// 按旧币种生成的价目与套餐必须重新填写。
	//
	// 这里不校验汇率：汇率只用于展示与上游成本折算，不进入任何扣费公式。
	if len(base) > 0 {
		var rec struct {
			Currency string `json:"currency"`
		}
		if err := json.Unmarshal(base, &rec); err == nil && rec.Currency != "" {
			if cur := conf.SiteCurrency(); cur != rec.Currency {
				return fmt.Errorf("%w（结算币种已从 %s 变为 %s）", ErrStale, rec.Currency, cur)
			}
		}
	}
	for _, op := range p.Operations {
		if op.Op != OpUpdate {
			continue
		}
		switch op.Entity {
		case EntityChannel:
			var v int
			if err := tx.Model(&model.Channel{}).Where("id = ?", op.ID).Select("version").Scan(&v).Error; err != nil {
				return err
			}
			if v != op.ExpectVersion {
				return fmt.Errorf("%w（渠道 %d 的版本已从 %d 变为 %d）", ErrStale, op.ID, op.ExpectVersion, v)
			}
		case EntityAdapterSpec:
			var v int
			if err := tx.Model(&model.AdapterSpec{}).Where("id = ?", op.ID).Select("version").Scan(&v).Error; err != nil {
				return err
			}
			if v != op.ExpectVersion {
				return fmt.Errorf("%w（规格 %d 的版本已从 %d 变为 %d）", ErrStale, op.ID, op.ExpectVersion, v)
			}
		case EntityModel:
			var v int
			if err := tx.Model(&model.Model{}).Where("id = ?", op.Key).Select("version").Scan(&v).Error; err != nil {
				return err
			}
			if v != op.ExpectVersion {
				return fmt.Errorf("%w（模型 %s 的版本已从 %d 变为 %d）", ErrStale, op.Key, op.ExpectVersion, v)
			}
		}
	}
	return nil
}

// applyOp 执行单条操作。
//
// **硬约束**：本函数不得出网、不得广播缓存、不得写审计、不得重算文档。
// 它在两个地方被调用——真 apply（事务内）与提案期试跑（事务内跑完即回滚）。
// 任何外部副作用都会在试跑时泄漏，所以这里只允许"读库 + 写库"。
func applyOp(ctx context.Context, tx *gorm.DB, op *Operation, snap *applySnapshot,
	createdChannels map[string]uint64, sink *applySink, owner uint64) error {
	switch op.Entity {

	case EntityVendor:
		if op.Op != OpCreate {
			return errors.New("供应商只支持新建；改名请人工在管理台操作")
		}
		var d VendorData
		if err := strictUnmarshal(op.Data, &d); err != nil {
			return err
		}
		return modelcfg.UpsertVendorTx(tx, op.Key, d.Name, d.Color, d.Icon)

	case EntityAdapterSpec:
		var d AdapterSpecData
		if err := strictUnmarshal(op.Data, &d); err != nil {
			return err
		}
		switch op.Op {
		case OpCreate:
			if _, err := specsvc.CreateTx(ctx, tx, specsvc.Input{
				Slug: op.Key, Label: d.Label, Vendor: d.Vendor, Spec: d.Spec,
				// 归属取"点确认的人"：AI 只是提案者，让它当 owner 等于没人负责。
				OwnerID: owner,
			}); err != nil {
				return err
			}
			sink.TouchedSpecs = append(sink.TouchedSpecs, op.Key)
		case OpUpdate:
			var row model.AdapterSpec
			if err := tx.First(&row, op.ID).Error; err != nil {
				return err
			}
			// 快照必须在**写库之前**取：写在后面拿到的就是改后值（before == after），
			// 回滚会把"改后"当成"改前"再写一遍，看起来成功、实际什么都没恢复。
			if err := snapshotSpec(tx, snap, op.ID, row.Slug); err != nil {
				return err
			}
			if _, err := specsvc.UpdateTx(ctx, tx, op.ID, specsvc.Input{
				Slug: row.Slug, Label: d.Label, Vendor: d.Vendor, Spec: d.Spec, Version: op.ExpectVersion,
			}); err != nil {
				return err
			}
			if err := recordSpecVersion(tx, snap, row.Slug); err != nil {
				return err
			}
			sink.TouchedSpecs = append(sink.TouchedSpecs, row.Slug)
		case OpDisable:
			var row model.AdapterSpec
			if err := tx.First(&row, op.ID).Error; err != nil {
				return err
			}
			if err := snapshotSpec(tx, snap, op.ID, row.Slug); err != nil {
				return err
			}
			if err := specsvc.SetStatusTx(ctx, tx, op.ID, model.SpecStatusDisabled); err != nil {
				return err
			}
			if err := recordSpecVersion(tx, snap, row.Slug); err != nil {
				return err
			}
			sink.TouchedSpecs = append(sink.TouchedSpecs, row.Slug)
		}
		return nil

	case EntityChannel:
		var d ChannelData
		if err := strictUnmarshal(op.Data, &d); err != nil {
			return err
		}
		switch op.Op {
		case OpCreate:
			req, err := createReqOf(d)
			if err != nil {
				return err
			}
			// base 传空串：这里只用返回值的 ID 记账，不渲染任何图片。
			item, err := channel.CreateTx(tx, req, "")
			if err != nil {
				return err
			}
			createdChannels[op.Key] = item.ID
		case OpUpdate:
			if err := snapshotChannel(tx, snap, op.ID); err != nil {
				return err
			}
			p, err := patchOf(d)
			if err != nil {
				return err
			}
			if _, err := channel.PatchTx(tx, op.ID, p, op.ExpectVersion, ""); err != nil {
				return err
			}
		case OpDisable:
			if err := snapshotChannel(tx, snap, op.ID); err != nil {
				return err
			}
			if _, err := channel.SetStatusTx(tx, op.ID, channel.StatusDisabled, ""); err != nil {
				return err
			}
		}
		return nil

	case EntityModel:
		var d ModelData
		if err := strictUnmarshal(op.Data, &d); err != nil {
			return err
		}
		switch op.Op {
		case OpCreate:
			if _, err := modelcfg.CreateTx(tx, modelInput(d, op.Key), nil); err != nil {
				return err
			}
			snap.AfterVersions["model:"+op.Key] = 1
		case OpUpdate:
			if err := snapshotModel(tx, snap, op.Key); err != nil {
				return err
			}
			if _, err := modelcfg.UpdateTx(tx, modelInput(d, op.Key), op.ExpectVersion); err != nil {
				return err
			}
			var v int
			_ = tx.Model(&model.Model{}).Where("id = ?", op.Key).Select("version").Scan(&v).Error
			snap.AfterVersions["model:"+op.Key] = v
		case OpDisable:
			if err := snapshotModel(tx, snap, op.Key); err != nil {
				return err
			}
			if err := modelcfg.SetEnabledTx(tx, op.Key, false); err != nil {
				return err
			}
		}
		return nil

	case EntityModelBindings:
		modelID := modelIDOf(*op)
		if op.Op == OpDisable {
			if err := snapshotBindings(tx, snap, modelID); err != nil {
				return err
			}
			if err := modelcfg.SetBindingEnabledTx(tx, modelID, op.ChannelID, false); err != nil {
				return err
			}
			return bumpModelVersion(tx, modelID, snap)
		}
		var d BindingData
		if err := strictUnmarshal(op.Data, &d); err != nil {
			return err
		}
		cur, err := modelcfg.GetTx(tx, modelID)
		if err != nil {
			return err
		}
		items, err := bindingItemsOf(d, createdChannels)
		if err != nil {
			return err
		}
		// 与人工抽屉同一条校验入口（含 upstream_model 必填、存量豁免、端点覆盖、预检告警）。
		nb, notes, err := modelcfg.NormalizeReplace(tx, cur, items)
		if err != nil {
			return err
		}
		sink.addNotes(notes)
		if err := snapshotBindings(tx, snap, modelID); err != nil {
			return err
		}
		if err := modelcfg.ReplaceBindingsTx(tx, modelID, nb); err != nil {
			return err
		}
		return bumpModelVersion(tx, modelID, snap)
	}
	return fmt.Errorf("未知实体 %q", op.Entity)
}

// modelIDOf 取绑定操作指向的模型 ID。
//
// disable 把模型 ID 放在 op.Model（同一条操作还要指定 channel_id），
// replace 放在 op.Key（提案内的引用名就是模型 ID）。
// 这两处写反过一次：回滚"停用绑定"时 Key 为空 → 快照取不到 → 整条操作被静默跳过，
// 界面上却显示"回滚成功"。
func modelIDOf(op Operation) string {
	if op.Op == OpDisable {
		return op.Model
	}
	return op.Key
}

// bumpModelVersion 在绑定变更后给模型 version+1（绑定也是模型状态的一部分）。
func bumpModelVersion(tx *gorm.DB, modelID string, snap *applySnapshot) error {
	res := tx.Model(&model.Model{}).Where("id = ?", modelID).Updates(map[string]any{
		"version": gorm.Expr("version + 1"), "updated_at": time.Now(),
	})
	if res.Error != nil {
		return res.Error
	}
	// 目标不存在时不能静默继续：AfterVersions 记 0 会让之后的漂移检查恒报 drift，
	// 运营看到的是"数据又被改过"，而真实原因是模型根本不在。
	if res.RowsAffected == 0 {
		return modelcfg.ErrNotFound
	}
	var v int
	_ = tx.Model(&model.Model{}).Where("id = ?", modelID).Select("version").Scan(&v).Error
	snap.AfterVersions["model:"+modelID] = v
	return nil
}

// createReqOf 把提案载荷转成**新建**渠道的入参。
//
// 出厂值取 channel.Defaults()：与人工保存同一个来源。
// 过去这里自己写了一份 1/1/0/0，导致"AI 建的渠道"默认不限额也不限并发。
func createReqOf(d ChannelData) (channel.Req, error) {
	req := channel.Defaults()
	req.Name = d.Name
	req.AdapterID = d.AdapterID
	req.BaseURL = d.BaseURL
	req.MultiKeyMode = d.MultiKeyMode
	req.Groups = d.Groups
	req.Keys = d.Keys
	req.Icon = d.Icon
	if d.Weight != nil {
		req.Weight = *d.Weight
	}
	if d.Priority != nil {
		req.Priority = *d.Priority
	}
	if d.RPMLimit != nil {
		req.RPMLimit = *d.RPMLimit
	}
	if d.MaxConcurrency != nil {
		req.MaxConcurrency = *d.MaxConcurrency
	}
	if d.AutoBan != nil {
		req.AutoBan = *d.AutoBan
	}
	if d.AdapterConfig != nil {
		raw, err := json.Marshal(d.AdapterConfig)
		if err != nil {
			return channel.Req{}, err
		}
		req.AdapterConfig = raw
	}
	return req, nil
}

// patchOf 把提案载荷转成**增量更新**入参：只填提案里真的出现的字段。
//
// 这是"只改名字不会把限流重置"的实现点。注意 0 与 nil 的区别：
// `rpm_limit: 0` 是合法的"不限流"，必须写进去；没提这个字段则不写。
func patchOf(d ChannelData) (channel.Patch, error) {
	var p channel.Patch
	if d.Name != "" {
		p.Name = &d.Name
	}
	if d.AdapterID != "" {
		p.AdapterID = &d.AdapterID
	}
	if d.BaseURL != "" {
		p.BaseURL = &d.BaseURL
	}
	if d.MultiKeyMode != "" {
		p.MultiKeyMode = &d.MultiKeyMode
	}
	if len(d.Groups) > 0 {
		p.Groups = &d.Groups
	}
	if len(d.Keys) > 0 {
		p.Keys = &d.Keys
	}
	p.Weight = d.Weight
	p.Priority = d.Priority
	p.RPMLimit = d.RPMLimit
	p.MaxConcurrency = d.MaxConcurrency
	p.AutoBan = d.AutoBan
	p.Icon = d.Icon
	if d.AdapterConfig != nil {
		raw, err := json.Marshal(d.AdapterConfig)
		if err != nil {
			return channel.Patch{}, err
		}
		p.AdapterConfig = raw
	}
	return p, nil
}

// modelInput 把提案载荷转成 modelcfg 入参（价格 9 条 lane 一一对应，单位微）。
func modelInput(d ModelData, id string) modelcfg.Input {
	in := modelcfg.Input{
		ID:          id,
		DisplayName: d.DisplayName,
		Description: d.Description,
		Icon:        d.Icon,
		Vendor:      d.Vendor,
		Modality:    d.Modality,
		Enabled:     d.Enabled,
		Prices: model.Prices{
			Input: d.InputPrice, Output: d.OutputPrice,
			Cache: d.CachePrice, CacheCreate: d.CacheCreatePrice,
			CacheCreate1h: d.CacheCreatePrice1h,
			Image:         d.ImagePrice, Audio: d.AudioPrice, AudioOutput: d.AudioOutputPrice,
			Call: d.CallPrice,
		},
		SpecPrices:      d.SpecPrices,
		ContextWindow:   d.ContextWindow,
		MaxOutputTokens: d.MaxOutputTokens,
	}
	// nil = 提案没提（沿用库内）；非 nil 才写。
	if d.BillingMode != nil {
		in.BillingMode = *d.BillingMode
		in.BillingModeSet = true
	}
	if d.Endpoints != nil {
		in.EndpointsSet = true
		in.Endpoints = d.Endpoints
	}
	if d.Capabilities != nil {
		in.CapabilitiesSet = true
		in.Capabilities = d.Capabilities
	}
	if d.Tags != nil {
		in.TagsSet = true
		in.Tags = *d.Tags
	}
	return in
}

// bindingItemsOf 把提案的绑定项转成 modelcfg 的绑定（channel_key 在这里解析成真实 ID）。
func bindingItemsOf(d BindingData, createdChannels map[string]uint64) ([]modelcfg.Binding, error) {
	out := make([]modelcfg.Binding, 0, len(d.Bindings))
	for i, b := range d.Bindings {
		chID := b.ChannelID
		if b.ChannelKey != "" {
			id, ok := createdChannels[b.ChannelKey]
			if !ok {
				return nil, fmt.Errorf("绑定第 %d 项引用的渠道 %q 不在本次创建的渠道里", i+1, b.ChannelKey)
			}
			chID = id
		}
		w, pr, en := 1, 1, true
		if b.Weight != nil {
			w = *b.Weight
		}
		if b.Priority != nil {
			pr = *b.Priority
		}
		if b.Enabled != nil {
			en = *b.Enabled
		}
		out = append(out, modelcfg.Binding{
			ChannelID: chID, UpstreamModel: b.UpstreamModel,
			Weight: w, Priority: pr, Enabled: en,
		})
	}
	return out, nil
}

func decodeEndpoints(raw model.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

/* ------------------------------ 快照辅助 ------------------------------ */

// snapshotSpec 记录规格的 before 行（create 没有 before）。
//
// 版本号不在这里猜（+1 是假设而不是事实）：写库后由 recordSpecVersion 读真值。
// 猜错会让反向提案的 expect_version 永远对不上，回滚直接报 stale。
func snapshotSpec(tx *gorm.DB, snap *applySnapshot, id uint64, slug string) error {
	if id == 0 {
		return nil // create：没有 before
	}
	var row model.AdapterSpec
	if err := tx.First(&row, id).Error; err != nil {
		return err
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	snap.Specs[slug] = b
	snap.SpecSlug[id] = slug
	return nil
}

// recordSpecVersion 在规格写库后记下真实版本（回滚的 CAS 依据）。
func recordSpecVersion(tx *gorm.DB, snap *applySnapshot, slug string) error {
	var v int
	if err := tx.Model(&model.AdapterSpec{}).Where("slug = ?", slug).
		Select("version").Scan(&v).Error; err != nil {
		return err
	}
	snap.AfterVersions["spec:"+slug] = v
	return nil
}

func snapshotChannel(tx *gorm.DB, snap *applySnapshot, id uint64) error {
	var row model.Channel
	if err := tx.First(&row, id).Error; err != nil {
		return err
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	snap.Channels[fmt.Sprintf("%d", id)] = b
	snap.AfterVersions[fmt.Sprintf("channel:%d", id)] = row.Version + 1
	return nil
}

func snapshotModel(tx *gorm.DB, snap *applySnapshot, id string) error {
	var row model.Model
	if err := tx.First(&row, "id = ?", id).Error; err != nil {
		return err
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	snap.Models[id] = b
	snap.AfterVersions["model:"+id] = row.Version + 1
	return nil
}

func snapshotBindings(tx *gorm.DB, snap *applySnapshot, modelID string) error {
	var rows []model.ModelBinding
	if err := tx.Where("model_id = ?", modelID).Find(&rows).Error; err != nil {
		return err
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	snap.Bindings[modelID] = b
	return nil
}

/* ------------------------------ diff ------------------------------ */

// BuildDiff 生成字段级差异（展示用；金额字段同时给出额度与折算价）。
//
// 为什么展示时要折算：运营是按"元/张、元/秒"思考的，看到 56000 额度
// 没有任何直觉；而只显示元又有精度问题。两者都给，谁都不用猜。
func BuildDiff(gdb *gorm.DB, p *Plan) (json.RawMessage, error) {
	items := make([]map[string]any, 0, len(p.Operations))
	added, changed, removed := 0, 0, 0
	for _, op := range p.Operations {
		item := map[string]any{"op": op.Op, "entity": op.Entity}
		if op.Key != "" {
			item["key"] = op.Key
		}
		if op.ID != 0 {
			item["id"] = op.ID
		}
		// before 只有人看得懂才有意义：运营判断"要不要点确认"靠的是
		// 「rpm 从 60 变成 0」这种对比，而不是只有 after。
		if before := beforeOf(gdb, op); before != nil {
			item["before"] = before
		}
		var data any
		if len(op.Data) > 0 {
			_ = json.Unmarshal(op.Data, &data)
		}
		if data != nil {
			item["after"] = data
		}
		switch op.Op {
		case OpCreate:
			added++
		case OpUpdate, OpReplace:
			changed++
		case OpDisable:
			removed++
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return entityOrder[items[i]["entity"].(string)] < entityOrder[items[j]["entity"].(string)]
	})
	// stats 由**这里**写入：前端读 `diff.stats` 显示"新增/修改/停用"，
	// 而过去的 diff 只有 items —— 于是确认框里的三个数字恒为 0（静默假信息）。
	out, err := json.Marshal(map[string]any{
		"summary": p.Summary,
		"stats":   map[string]int{"added": added, "changed": changed, "removed": removed},
		"items":   items,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// beforeOf 读取被改动实体的当前值（create 没有 before）。
//
// 失败一律返回 nil：diff 是展示用，不能因为它读不到就让提案生成失败。
func beforeOf(gdb *gorm.DB, op Operation) any {
	if gdb == nil || op.Op == OpCreate {
		return nil
	}
	switch op.Entity {
	case EntityChannel:
		var row model.Channel
		if err := gdb.Where("id = ?", op.ID).Limit(1).Find(&row).Error; err != nil || row.ID == 0 {
			return nil
		}
		return map[string]any{
			"name": row.Name, "adapter_id": row.AdapterID, "base_url": row.BaseURL,
			"groups": decodeEndpoints(row.Groups), "multi_key_mode": row.MultiKeyMode,
			"weight": row.Weight, "priority": row.Priority,
			"rpm_limit": row.RPMLimit, "max_concurrency": row.MaxConcurrency,
			"auto_ban": row.AutoBan, "status": row.Status,
		}
	case EntityModel:
		row, err := modelcfg.GetTx(gdb, op.Key)
		if err != nil {
			return nil
		}
		return modelcfg.Data(row)
	case EntityAdapterSpec:
		var row model.AdapterSpec
		if err := gdb.Where("id = ?", op.ID).Limit(1).Find(&row).Error; err != nil || row.ID == 0 {
			return nil
		}
		return map[string]any{
			"label": row.Label, "vendor": row.Vendor, "status": row.Status, "version": row.Version,
		}
	case EntityModelBindings:
		modelID := modelIDOf(op)
		var rows []model.ModelBinding
		if err := gdb.Where("model_id = ?", modelID).Find(&rows).Error; err != nil {
			return nil
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"channel_id": r.ChannelID, "upstream_model": r.UpstreamModel,
				"weight": r.Weight, "priority": r.Priority, "enabled": r.Enabled,
			})
		}
		return out
	}
	return nil
}

// DiffStats 统计新增/修改/停用数量（提案卡片上的一行摘要）。
func DiffStats(diff json.RawMessage) (added, changed, removed int) {
	var rec struct {
		Items []struct {
			Op string `json:"op"`
		} `json:"items"`
	}
	if err := json.Unmarshal(diff, &rec); err != nil {
		return
	}
	for _, it := range rec.Items {
		switch it.Op {
		case OpCreate:
			added++
		case OpUpdate, OpReplace:
			changed++
		case OpDisable:
			removed++
		}
	}
	return
}

// RecoverApplying 把卡在 applying 的提案落成失败终态。
//
// 没有它的话，进程在 apply 中途崩溃会让提案永久停在 applying ——
// 界面上既不能重试也不能重新生成，只能改库。
func RecoverApplying(ctx context.Context, olderThan time.Duration) int64 {
	if olderThan <= 0 {
		olderThan = 5 * time.Minute
	}
	res := db.DB.WithContext(ctx).Model(&model.AgentProposal{}).
		Where("status = ? AND updated_at < ?", model.ProposalApplying, time.Now().Add(-olderThan)).
		Updates(map[string]any{
			"status": model.ProposalFailed, "error": "进程中断，请重新生成提案", "updated_at": time.Now(),
		})
	if res.Error != nil {
		return 0
	}
	return res.RowsAffected
}
