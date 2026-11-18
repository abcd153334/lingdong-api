package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/modelcfg"
)

// 本文件是提案的生命周期（不是 apply 本身，apply 在 apply.go）。

var (
	ErrSessionNotFound = errors.New("agent: 会话不存在")
	// ErrBusy 同一会话已有一轮在跑（串行执行，避免两轮同时改同一份配置）。
	ErrBusy = errors.New("agent: 该会话正在处理上一条消息")
)

/* ------------------------------ 提案创建 ------------------------------ */

// ProposalInput 是创建提案的入参（来自工具 propose_change）。
type ProposalInput struct {
	SessionID uint64
	MessageID uint64
	Kind      string
	Plan      json.RawMessage
}

// CreateProposal 校验并落库一条提案（**不写生产表**）。
//
// 校验顺序刻意是「结构 → 依赖事实 → 覆盖」：
// 结构错（JSON/字段名）最便宜，先挡住；依赖事实（重名/版本）其次；
// 覆盖（端点能不能被承接）最贵，放最后。
func CreateProposal(ctx context.Context, in ProposalInput) (*model.AgentProposal, error) {
	plan, err := DecodePlan(in.Plan)
	if err != nil {
		return nil, err
	}
	if in.Kind == "" {
		in.Kind = "change"
	}
	// 第一层：结构规则（不查库，错误能精确到 operations[i].<field>）。
	if err := plan.ValidateStructure(in.Kind); err != nil {
		return nil, err
	}
	// 第二层：语义规则 = 把写库路径真跑一遍再回滚。
	//
	// 为什么不是"再写一套校验"：渠道必填、价格符号、绑定完整性、适配器可解析、
	// vendor 存在、墓碑、端点覆盖——这些规则本来就在 modelcfg / channel / spec 里，
	// 抄一份到校验器必然分叉。试跑保证"提案说能落地"与"应用真的能落地"是同一件事。
	if in.Kind == "change" {
		// 试跑期的告警（模型无法被渠道承接等）在 create 时丢弃：提案此刻只是草稿，
		// 真正的出口是 validate_draft 与 apply —— 在那两处给 AI 反馈才有用。
		if _, err := trialApply(ctx, plan); err != nil {
			return nil, err
		}
	}
	hash, err := PlanHash(plan)
	if err != nil {
		return nil, err
	}
	diff, err := BuildDiff(db.DB, plan)
	if err != nil {
		return nil, err
	}
	// BaseVersions 记录"生成时的事实基线"：apply 前比对，防止用旧币种/旧版本覆盖。
	base := map[string]any{
		"currency":     conf.SiteCurrency(),
		"scale":        money.Scale,
		"plan_version": 1,
	}
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	// 提案不再携带探测快照：凭据的唯一来源是 agent_dry_runs（跨轮有效）。
	// 曾经在这里写 dry_run 的行为制造了一个"只在同轮有效"的副本，跨轮重提即丢。
	row := model.AgentProposal{
		SessionID:    in.SessionID,
		MessageID:    in.MessageID,
		Kind:         in.Kind,
		Status:       model.ProposalDraft,
		Plan:         model.JSON(planJSON),
		PlanHash:     hash,
		Diff:         model.JSON(diff),
		BaseVersions: model.JSON(baseJSON),
	}
	if err := db.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// GetProposal 取提案详情。
// ErrOutOfScope 表示提案里出现了调用方入口不允许的实体。
var ErrOutOfScope = errors.New("agent: 提案包含当前入口不允许的变更")

// EnforceScope 校验提案里的每个操作实体都在 allowed 内，返回 ErrOutOfScope 的包装。
//
// 为什么需要它：从「新建适配器」进弹窗时，AI 完全可以顺手生成一条改价或改别人渠道的
// 提案，而运营看到的是"帮我建个适配器"——点应用就真改了。这不是权限问题
// （管理员本来有权改价），而是**入口语义**问题：这个按钮不该顺手改别的东西。
//
// 空 allowed = 不限制：/admin/agent 页不传 scope，行为与以前完全一致。
// scope 由调用方（HTTP 请求）给出而不是存进提案：它是"这一次点击的语境"，
// 存进库会让历史提案在换入口后行为漂移。
func EnforceScope(prop *model.AgentProposal, allowed []string) error {
	if prop == nil {
		return ErrProposalNotFound
	}
	if len(allowed) == 0 {
		return nil
	}
	plan, err := DecodePlan(prop.Plan)
	if err != nil {
		return err
	}
	for _, op := range plan.Operations {
		ok := false
		for _, a := range allowed {
			if a == op.Entity {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%w：%s", ErrOutOfScope, op.Entity)
		}
	}
	return nil
}

func GetProposal(ctx context.Context, id uint64) (*model.AgentProposal, error) {
	var row model.AgentProposal
	if err := db.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProposalNotFound
		}
		return nil, err
	}
	return &row, nil
}

// ListProposals 列出会话内提案（新的在前）。
func ListProposals(ctx context.Context, sessionID uint64) ([]model.AgentProposal, error) {
	var rows []model.AgentProposal
	q := db.DB.WithContext(ctx).Order("id DESC").Limit(50)
	if sessionID > 0 {
		q = q.Where("session_id = ?", sessionID)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// RejectProposal 人工忽略一条提案（可再次被 apply 的状态之外都能拒）。
func RejectProposal(ctx context.Context, id, adminID uint64) error {
	return db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.AgentProposal{}).
			// 只有 draft 可忽略：探测状态不是提案状态（它由 agent_dry_runs 现算），
			// 所以"已通过/未通过探测"都不再是独立状态。
			Where("id = ? AND status = ?", id, model.ProposalDraft).Update("status", model.ProposalRejected)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrNotApplyable
		}
		return audit.Record(tx, adminID, audit.ActionAgentProposalReject, "proposal",
			fmt.Sprintf("%d", id), nil)
	})
}

// Rollback 生成一条**反向提案**（不是魔法撤销）。
//
// 为什么不做直接回撤：回滚本身也是一次变更，同样需要人确认、需要 diff、需要可再回滚。
// 这里只负责把"反向操作"编译成一份普通 plan，后续走同一条 apply 管线。
func Rollback(ctx context.Context, id, adminID uint64) (*model.AgentProposal, error) {
	prop, err := GetProposal(ctx, id)
	if err != nil {
		return nil, err
	}
	if prop.Status != model.ProposalApplied {
		return nil, ErrNotApplyable
	}
	var snap applySnapshot
	if len(prop.Snapshot) == 0 {
		return nil, errors.New("agent: 提案缺少快照，无法生成回滚")
	}
	if err := json.Unmarshal(prop.Snapshot, &snap); err != nil {
		return nil, err
	}
	// 漂移检查：apply 之后又被人改过的实体不允许自动回滚（否则会覆盖新改动）。
	if err := checkDrift(ctx, &snap); err != nil {
		return nil, err
	}
	plan, err := DecodePlan(prop.Plan)
	if err != nil {
		return nil, err
	}
	reverse, err := buildReversePlan(plan, &snap)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(reverse)
	if err != nil {
		return nil, err
	}
	out, err := CreateProposal(ctx, ProposalInput{
		SessionID: prop.SessionID, MessageID: prop.MessageID, Kind: "change", Plan: raw,
	})
	if err != nil {
		return nil, err
	}
	// 回滚也是一次配置变更，必须留痕（过去 adminID 被丢弃，审计里查不到谁回滚的）。
	audit.RecordOrWarn(db.DB, adminID, audit.ActionAgentRollback, "proposal",
		fmt.Sprintf("%d", id), map[string]any{"reverse_proposal_id": out.ID})
	return out, nil
}

// checkDrift 校验 apply 后各实体版本是否仍是 after_versions（否则拒绝自动回滚）。
func checkDrift(ctx context.Context, snap *applySnapshot) error {
	for key, want := range snap.AfterVersions {
		parts := strings.SplitN(key, ":", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "channel":
			var v int
			_ = db.DB.WithContext(ctx).Model(&model.Channel{}).Where("id = ?", parts[1]).Select("version").Scan(&v).Error
			if v != want {
				return fmt.Errorf("%w（渠道 %s 已被改到版本 %d）", ErrDrift, parts[1], v)
			}
		case "model":
			var v int
			_ = db.DB.WithContext(ctx).Model(&model.Model{}).Where("id = ?", parts[1]).Select("version").Scan(&v).Error
			if v != want {
				return fmt.Errorf("%w（模型 %s 已被改到版本 %d）", ErrDrift, parts[1], v)
			}
		case "spec":
			var v int
			_ = db.DB.WithContext(ctx).Model(&model.AdapterSpec{}).Where("slug = ?", parts[1]).Select("version").Scan(&v).Error
			if v != want {
				return fmt.Errorf("%w（规格 %s 已被改到版本 %d）", ErrDrift, parts[1], v)
			}
		}
	}
	return nil
}

// buildReversePlan 把原 plan 编译成反向 plan（只覆盖能做安全反向的实体）。
func buildReversePlan(p *Plan, snap *applySnapshot) (*Plan, error) {
	out := &Plan{Summary: "回滚：" + p.Summary}
	// 反向操作统一以「快照还原」表达：update 回到 before；create **不反向**。
	//
	// 为什么 create 一律跳过：平台没有 delete，能做的只有"停用"，而停用一个刚建好的
	// 渠道/规格会把它引用的模型一起打成不可用 —— 那不是回滚，是制造第二起事故。
	// 这类提案在下面会被判成"没有可自动回滚的操作"，由人工处理。
	for _, op := range p.Operations {
		switch op.Entity {
		case EntityChannel:
			if op.Op == OpCreate {
				// create 的渠道 id 在 apply 时才知道：从快照 after_versions 反查太脆，
				// 这里要求人工在管理台确认该渠道的处置方式。
				continue
			}
			if raw, ok := snap.Channels[fmt.Sprintf("%d", op.ID)]; ok {
				var before model.Channel
				if err := json.Unmarshal(raw, &before); err != nil {
					return nil, err
				}
				data, err := json.Marshal(ChannelData{
					Name: before.Name, AdapterID: before.AdapterID, BaseURL: before.BaseURL,
					Groups: groupsOf(before.Groups), MultiKeyMode: before.MultiKeyMode,
					Weight: &before.Weight, Priority: &before.Priority,
					RPMLimit: &before.RPMLimit, MaxConcurrency: &before.MaxConcurrency,
					AutoBan: &before.AutoBan,
				})
				if err != nil {
					return nil, err
				}
				out.Operations = append(out.Operations, Operation{
					Op: OpUpdate, Entity: EntityChannel, ID: op.ID,
					ExpectVersion: snap.AfterVersions[fmt.Sprintf("channel:%d", op.ID)],
					Data:          data,
				})
			}
		case EntityAdapterSpec:
			// create 没有 before：规格建了就留着（停用它等于让引用它的渠道全 503，
			// 那不是"回滚"，是制造第二起事故）。与渠道 create 的处理一致。
			if op.Op == OpCreate {
				continue
			}
			// 只认 apply 时记下的 id→slug：过去"取 map 里第一个 key"在多个规格时
			// 会把回滚还原到**另一个规格**上，而且不报错。
			slug, ok := snap.SpecSlug[op.ID]
			if !ok {
				return nil, fmt.Errorf("%w：提案快照里没有规格 %d 的 before（无法安全回滚）", ErrNotApplyable, op.ID)
			}
			raw, ok := snap.Specs[slug]
			if !ok {
				return nil, fmt.Errorf("%w：提案快照里没有规格 %s 的 before", ErrNotApplyable, slug)
			}
			var before model.AdapterSpec
			if err := json.Unmarshal(raw, &before); err != nil {
				return nil, err
			}
			data, err := json.Marshal(AdapterSpecData{
				Label: before.Label, Vendor: before.Vendor, Spec: json.RawMessage(before.Spec),
			})
			if err != nil {
				return nil, err
			}
			out.Operations = append(out.Operations, Operation{
				Op: OpUpdate, Entity: EntityAdapterSpec, ID: op.ID,
				ExpectVersion: snap.AfterVersions["spec:"+before.Slug],
				Data:          data,
			})
		case EntityModel:
			raw, ok := snap.Models[op.Key]
			if !ok {
				continue
			}
			var before model.Model
			if err := json.Unmarshal(raw, &before); err != nil {
				return nil, err
			}
			data, err := json.Marshal(modelDataOf(&before))
			if err != nil {
				return nil, err
			}
			out.Operations = append(out.Operations, Operation{
				Op: OpUpdate, Entity: EntityModel, Key: op.Key,
				ExpectVersion: snap.AfterVersions["model:"+op.Key],
				Data:          data,
			})
		case EntityModelBindings:
			raw, ok := snap.Bindings[modelIDOf(op)]
			if !ok {
				continue
			}
			var rows []model.ModelBinding
			if err := json.Unmarshal(raw, &rows); err != nil {
				return nil, err
			}
			// 快照为空 = 这次是"新建模型后第一次绑"（之前没有绑定）。
			// 回滚不能表达"清空绑定"（平台不允许没有绑定的模型进目录），
			// 也没有"删除"这个 op —— 跳过而不是编一个空集合去过校验。
			if len(rows) == 0 {
				continue
			}
			items := make([]BindingItem, 0, len(rows))
			for _, r := range rows {
				id := r.ChannelID
				w, p, e := r.Weight, r.Priority, r.Enabled
				items = append(items, BindingItem{
					ChannelID: id, UpstreamModel: r.UpstreamModel, Weight: &w, Priority: &p, Enabled: &e,
				})
			}
			data, err := json.Marshal(BindingData{Bindings: items})
			if err != nil {
				return nil, err
			}
			out.Operations = append(out.Operations, Operation{
				Op: OpReplace, Entity: EntityModelBindings, Key: op.Key, Data: data,
			})
		}
	}
	if len(out.Operations) == 0 {
		return nil, errors.New("agent: 该提案没有可自动回滚的操作（新建类实体请人工处理）")
	}
	return out, nil
}

func groupsOf(raw model.JSON) []string {
	var g []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &g)
	}
	return g
}

// modelDataOf 把模型行转成提案载荷（回滚用）。
//
// 实现只有一句：modelcfg.Data 是"行 → 载荷"的唯一换算点（8 条 lane + spec_prices）。
// 过去这里自己反算，只补了 input/output 两条 lane —— 回滚后 cache/image/audio
// 仍是"改后价"，而界面上写着"已回滚"。
func modelDataOf(m *model.Model) ModelData {
	in := modelcfg.Data(m)
	bm := in.BillingMode
	tags := in.Tags
	return ModelData{
		DisplayName: in.DisplayName, Vendor: in.Vendor, Modality: in.Modality,
		BillingMode: &bm, Description: in.Description, Enabled: in.Enabled,
		Endpoints: in.Endpoints, Capabilities: in.Capabilities,
		Tags:               &tags,
		SpecPrices:         in.SpecPrices,
		ContextWindow:      in.ContextWindow,
		MaxOutputTokens:    in.MaxOutputTokens,
		CallPrice:          in.Prices.Call,
		InputPrice:         in.Prices.Input,
		OutputPrice:        in.Prices.Output,
		CachePrice:         in.Prices.Cache,
		CacheCreatePrice:   in.Prices.CacheCreate,
		CacheCreatePrice1h: in.Prices.CacheCreate1h,
		ImagePrice:         in.Prices.Image,
		AudioPrice:         in.Prices.Audio,
		AudioOutputPrice:   in.Prices.AudioOutput,
	}
}

/* ------------------------------ 会话与设置 ------------------------------ */

// CreateSession 新建会话。
//
// 标题**留空**而不是塞一个占位串：占位串会让所有新会话在侧栏里长得一模一样，
// 而聊天式界面的侧栏就是导航。留空后由首条用户消息派生（见 AppendMessage），
// 前端在派生之前显示「新对话」。
func CreateSession(ctx context.Context, title, modelID string) (*model.AgentSession, error) {
	row := model.AgentSession{Title: strings.TrimSpace(title), ModelID: strings.TrimSpace(modelID), Status: 1}
	if err := db.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// ListSessions 列出会话（page 为 0 时返回最近 50 条全量，与其它管理列表的分页口径一致）。
func ListSessions(ctx context.Context, page, size int) ([]model.AgentSession, int64, error) {
	q := db.DB.WithContext(ctx).Model(&model.AgentSession{})
	var total int64
	limit := 50
	if page > 0 {
		if err := q.Count(&total).Error; err != nil {
			return nil, 0, err
		}
		if size <= 0 {
			size = 20
		}
		if size > 100 {
			size = 100
		}
		limit = size
	}
	var rows []model.AgentSession
	sel := q.Order("status ASC, id DESC").Limit(limit)
	if page > 0 {
		// 夹紧规则与 HTTP 层共用一份（pagination.OffsetOf），不在这里再写 (page-1)*size。
		sel = sel.Offset(pagination.OffsetOf(page, size))
	}
	if err := sel.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	if page == 0 {
		total = int64(len(rows))
	}
	return rows, total, nil
}

// GetSession 取会话。
func GetSession(ctx context.Context, id uint64) (*model.AgentSession, error) {
	var row model.AgentSession
	if err := db.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, err
	}
	return &row, nil
}

// ErrSessionBusy 会话里还有 applying 的提案（软删会把它变成"无处可回的变更"）。
var ErrSessionBusy = errors.New("agent: 该会话有正在应用的提案")

// UpdateSession 改名 / 归档。
// UpdateSession 改名 / 归档 / 指定本会话使用的对话模型。
//
// modelID 传 nil 表示"不改"；传空串表示"清空，回到全站默认"——两者语义不同，
// 所以这里用指针而不是字符串（否则"不传"和"清空"无法区分）。
func UpdateSession(ctx context.Context, id uint64, title string, status *int16, modelID *string) error {
	updates := map[string]any{"updated_at": time.Now()}
	if t := strings.TrimSpace(title); t != "" {
		if r := []rune(t); len(r) > 128 {
			t = string(r[:128])
		}
		updates["title"] = t
	}
	if modelID != nil {
		updates["model_id"] = strings.TrimSpace(*modelID)
	}
	if status != nil {
		if *status != 1 && *status != 2 {
			return errors.New("agent: 会话状态只能是 1（活跃）或 2（归档）")
		}
		updates["status"] = *status
	}
	res := db.DB.WithContext(ctx).Model(&model.AgentSession{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 改 title 与改 status 都可能是"值没变"，此时 RowsAffected 也是 0；
		// 先确认行存在再决定是不是真的不存在。
		if _, err := GetSession(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// DeleteSession 软删会话（有 applying 提案时拒绝）。
func DeleteSession(ctx context.Context, id uint64) error {
	return db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&model.AgentProposal{}).
			Where("session_id = ? AND status = ?", id, model.ProposalApplying).
			Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrSessionBusy
		}
		res := tx.Delete(&model.AgentSession{}, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrSessionNotFound
		}
		// 提问行跟着会话一起硬删：它是运行期状态，没有历史价值，
		// 留着只会在"任一时刻只有一张可答卡片"的判据里变成噪声。
		if err := tx.Where("session_id = ?", id).Delete(&model.AgentAsk{}).Error; err != nil {
			return err
		}
		return nil
	})
}

// SessionStats 是一段时间内的成本与运行统计（管理台头部展示）。
type SessionStats struct {
	Days        int   `json:"days"`
	Runs        int64 `json:"runs"`
	Failed      int64 `json:"failed"`
	EstAmount   int64 `json:"est_amount"`
	TokensIn    int64 `json:"tokens_in"`
	TokensOut   int64 `json:"tokens_out"`
	Proposals   int64 `json:"proposals"`
	Applied     int64 `json:"applied"`
	AvgLatency  int64 `json:"avg_latency_ms"`
	DailyBudget int64 `json:"daily_budget"`
	TodaySpend  int64 `json:"today_spend"`
}

// Stats 聚合最近 days 天的运行与提案数据。
func Stats(ctx context.Context, days int) (*SessionStats, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	since := time.Now().AddDate(0, 0, -days)
	out := &SessionStats{Days: days}
	// 一次查询取全部计数：分五条 SQL 会让"页面上这几个数字"来自五个不同时刻。
	row := struct {
		Runs      int64
		Failed    int64
		QuotaEst  int64
		TokensIn  int64
		TokensOut int64
		AvgLat    int64
	}{}
	err := db.DB.WithContext(ctx).Model(&model.AgentRun{}).
		Where("created_at >= ?", since).
		Select(`count(*) AS runs,
			COALESCE(sum(CASE WHEN status IN ('failed','aborted') THEN 1 ELSE 0 END),0) AS failed,
			COALESCE(sum(quota_est),0) AS quota_est,
			COALESCE(sum(tokens_in),0) AS tokens_in,
			COALESCE(sum(tokens_out),0) AS tokens_out,
			-- 必须显式转 bigint：PostgreSQL 的 avg() 返回 numeric，平均耗时一旦不是整数
			-- （100ms 与 201ms → 150.5）扫进 int64 就报类型转换失败 —— 而 stats 把这个
			-- 错误翻成 500。症状极隐蔽：空库时 avg 是 0 能过，**有了真实运行记录才 500**，
			-- 桩件 e2e 与"刚上线还没跑过"都会漏掉。ROUND 而不是截断：耗时展示不该凭空少 1ms。
			COALESCE(round(avg(latency_ms))::bigint,0) AS avg_lat`).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	out.Runs, out.Failed = row.Runs, row.Failed
	out.EstAmount, out.TokensIn, out.TokensOut, out.AvgLatency = row.QuotaEst, row.TokensIn, row.TokensOut, row.AvgLat
	if err := db.DB.WithContext(ctx).Model(&model.AgentProposal{}).
		Where("created_at >= ?", since).Count(&out.Proposals).Error; err != nil {
		return nil, err
	}
	if err := db.DB.WithContext(ctx).Model(&model.AgentProposal{}).
		Where("created_at >= ? AND status = ?", since, model.ProposalApplied).
		Count(&out.Applied).Error; err != nil {
		return nil, err
	}
	set, err := Settings(ctx)
	if err != nil {
		return nil, err
	}
	out.DailyBudget = set.DailyAmountBudget
	if out.TodaySpend, err = DailySpend(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// Messages 返回会话消息。
//
// 两种取法（语义都保持 seq ASC 返回，前端无需再排）：
//   - afterSeq > 0：续传，只取更新的（断线续传用）；
//   - afterSeq == 0：首屏，取**最新** limit 条再反转 —— 按 ASC Limit 取的是最早的
//     100 条，长会话的最新几轮在界面上直接消失，而模型侧用的是尾部（context_builder 的 loadTurns），
//     界面比上下文还旧就是双标。
func Messages(ctx context.Context, sessionID uint64, afterSeq int64, limit int) ([]model.AgentMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	// 显式列白名单：`ctx` 是回灌用的原文（单轮可达数十 KB），
	// 用它渲染列表会让首屏多读几 MB，而且前端一个字段都用不到。
	var rows []model.AgentMessage
	q := func() *gorm.DB {
		return db.DB.WithContext(ctx).Select(
			"id, session_id, seq, role, content, parts, status, tokens_in, tokens_out, created_at",
		).Where("session_id = ?", sessionID)
	}
	if afterSeq > 0 {
		err := q().Where("seq > ?", afterSeq).Order("seq ASC").Limit(limit).Find(&rows).Error
		return rows, err
	}
	if err := q().Order("seq DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	slices.Reverse(rows)
	return rows, nil
}

// MessageMeta 是消息的计量与上下文投影（与正文一起落库，走同一次写入）。
//
// 为什么不做事后 UPDATE：那会变成两条写入路径，第二条随时可能被漏掉
// （`tokens_in/out` 就是这么变成死列的：列早就加了，从没人写）。
type MessageMeta struct {
	// TokensIn / TokensOut 是该轮 run 的累计用量（无 run 的消息留 0）。
	TokensIn  int
	TokensOut int
	// Ctx 是回灌投影：本轮 assistant(tool_calls) + tool 消息原文（见 context_builder）。
	Ctx model.JSON
	// Summary 是该轮的规则摘要（回灌与界面展示共用）。
	Summary string
	// Status 是消息终态（ok / aborted / failed），空 = ok。
	Status string
}

// AppendMessage 追加一条消息并推进会话游标（seq 单调）。
func AppendMessage(ctx context.Context, sessionID uint64, role, content string, parts model.JSON,
	meta MessageMeta) (*model.AgentMessage, error) {
	if meta.Status == "" {
		meta.Status = "ok"
	}
	var out model.AgentMessage
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sess model.AgentSession
		if err := tx.Select("id", "title").First(&sess, sessionID).Error; err != nil {
			return ErrSessionNotFound
		}
		// 原子自增：过去是 `seq = LastSeq + 1` 再 UPDATE，Redis 锁失效（未就绪时放行）
		// 的并发写会读到同一个 LastSeq → 两条消息同 seq → 历史顺序错乱且无报错。
		var seq int64
		if err := tx.Raw(
			`UPDATE agent_sessions SET last_seq = last_seq + 1 WHERE id = ? RETURNING last_seq`,
			sessionID,
		).Scan(&seq).Error; err != nil {
			return err
		}
		row := model.AgentMessage{
			SessionID: sessionID, Seq: seq, Role: role, Content: content, Parts: parts,
			Status: meta.Status, TokensIn: meta.TokensIn, TokensOut: meta.TokensOut,
			Ctx: meta.Ctx, Summary: meta.Summary,
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		updates := map[string]any{"updated_at": time.Now()}
		// 首条用户消息顺手派生标题（侧栏就是导航，全是「未命名会话」等于没有导航）。
		if role == "user" && strings.TrimSpace(sess.Title) == "" {
			if t := deriveSessionTitle(content); t != "" {
				updates["title"] = t
			}
		}
		if err := tx.Model(&model.AgentSession{}).Where("id = ?", sessionID).
			Updates(updates).Error; err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// deriveSessionTitle 从首条用户消息取一个标题（首行、去列表符号、24 字）。
func deriveSessionTitle(content string) string {
	s := strings.TrimSpace(content)
	if s == "" {
		return ""
	}
	// 只取首行：运营经常把整份需求贴进来，标题里塞进换行会让侧栏项撑高。
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(strings.TrimLeft(s, "#>-*· 　"))
	s = strings.Join(strings.Fields(s), " ")
	// URL 起头（"https://…"）做标题毫无辨识度，退化成域名更易扫读。
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			s = u.Host
		}
	}
	return clipRunes(s, 24)
}

// Settings 返回单行设置（不存在时返回默认值）。
func Settings(ctx context.Context) (*model.AgentSetting, error) {
	var row model.AgentSetting
	err := db.DB.WithContext(ctx).Where("id = ?", 1).Limit(1).Find(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		// 默认值只在这里用常量表达：散落的字面量 32000/12 曾是"设置页显示 32000、
		// 代码里默认 32000、模型级又是另一套"的根源，改一处必然漏两处。
		row = model.AgentSetting{
			ID: 1, Enabled: true,
			MaxSteps:        model.DefaultAgentMaxSteps,
			MaxTokensPerRun: model.DefaultAgentMaxTokensPerRun,
			// ContextWindow / MaxOutputTokens 留 0：0 的语义就是"用内置默认"，
			// 这样将来调默认值时不必改库里的行。
			DryRunRequired: true,
		}
	}
	return &row, nil
}

// SaveSettings 保存单行设置。
//
// 校验在这里**再跑一次**（controller 已跑过）：SaveSettings 是唯一的落库入口，
// 将来任何新调用方（脚本/迁移/另一个端点）都不可能绕过区间规则。
func SaveSettings(ctx context.Context, in model.AgentSetting) error {
	if err := ValidateSettings(&in); err != nil {
		return err
	}
	in.ID = 1
	in.UpdatedAt = time.Now()
	return db.DB.WithContext(ctx).Save(&in).Error
}

// DailySpend 返回今日已花的估算额度（预算闸用）。
//
// "今日"必须按**本地零点**算。`time.Truncate(24h)` 截的是零时刻起算的绝对时长，
// 在东八区得到的是 08:00 的边界 —— 结果是"今天"实际统计了昨天 08:00 到今天的量，
// 且每天上午 8 点预算额度自己变一次。这类边界错不会报错，只会让人觉得
// "预算时紧时松"。
func DailySpend(ctx context.Context) (int64, error) {
	var sum int64
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	err := db.DB.WithContext(ctx).Model(&model.AgentRun{}).
		Where("created_at >= ?", start).Select("COALESCE(sum(quota_est),0)").Scan(&sum).Error
	return sum, err
}
