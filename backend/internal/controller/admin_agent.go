package controller

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/agent"
	"lingdong-api/internal/service/audit"
	"lingdong-api/internal/service/iconref"
)

// 本文件是 /api/admin/agent/* 的管理端点（提案 / 会话 / 设置 / 文档 / 统计）。
//
// SSE 那条 `POST /sessions/:id/messages` 不在本文件：它要跑一整轮 LLM 编排，
// 与工具集同处一个文件更不容易走偏（见 admin_agent_stream.go）。
//
// 一条铁律：**这些端点自身不写生产表**。写生产表的唯一入口是 apply，
// 而 apply 在 service 层是单事务 + 审计 + 快照，控制器只负责鉴权与错误翻译。

/* ------------------------------ 下发视图 ------------------------------ */

// 本组视图不是"多此一举"。
//
// model 层只带 gorm tag、**没有 json tag**，直接序列化出去就是 PascalCase
// （`MaxSteps`）并且会把 `DeletedAt` 一起吐给前端；而前端契约是 snake_case。
// 字段名一错就是「开关永远显示关闭、步数输入框空白、更新时间 0001」这类
// **不报错的错**——桩件 e2e 用自己的假数据，永远测不出来，只有连真后端才看得见。
// 所以每个下发结构都必须在这里显式列字段（与其它管理端点同一口径）。

type agentSessionView struct {
	ID        uint64    `json:"id"`
	Title     string    `json:"title"`
	ModelID   string    `json:"model_id"`
	Status    int16     `json:"status"`
	LastSeq   int64     `json:"last_seq"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newAgentSessionView(m model.AgentSession) agentSessionView {
	return agentSessionView{
		ID: m.ID, Title: m.Title, ModelID: m.ModelID, Status: m.Status,
		LastSeq: m.LastSeq, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

type agentMessageView struct {
	ID        uint64     `json:"id"`
	SessionID uint64     `json:"session_id"`
	Seq       int64      `json:"seq"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Parts     model.JSON `json:"parts,omitempty"`
	Status    string     `json:"status"`
	TokensIn  int        `json:"tokens_in"`
	TokensOut int        `json:"tokens_out"`
	CreatedAt time.Time  `json:"created_at"`
}

type agentProposalView struct {
	ID        uint64     `json:"id"`
	SessionID uint64     `json:"session_id"`
	MessageID uint64     `json:"message_id"`
	Kind      string     `json:"kind"`
	Status    string     `json:"status"`
	Plan      model.JSON `json:"plan"`
	PlanHash  string     `json:"plan_hash"`
	Diff      model.JSON `json:"diff,omitempty"`
	Error     string     `json:"error,omitempty"`
	AppliedAt *time.Time `json:"applied_at"`
	CreatedAt time.Time  `json:"created_at"`
	// Probe 是"这条提案要不要先做连通性探测、现在满不满足"。
	// 放进来而不是让前端自己算：判定需要渠道现状与密钥指纹，前端两样都没有。
	// 算不出来时为 nil（omitempty），前端按"未知"渲染 —— 不要伪装成已满足。
	Probe *agent.ProbeStatus `json:"probe,omitempty"`
}

func newAgentProposalView(m model.AgentProposal) agentProposalView {
	return agentProposalView{
		ID: m.ID, SessionID: m.SessionID, MessageID: m.MessageID,
		Kind: m.Kind, Status: m.Status, Plan: m.Plan, PlanHash: m.PlanHash,
		Diff: m.Diff, Error: m.Error, AppliedAt: m.AppliedAt, CreatedAt: m.CreatedAt,
	}
}

type agentSettingItemView struct {
	ID       uint64 `json:"id"`
	Enabled  bool   `json:"enabled"`
	ModelID  string `json:"model_id"`
	MaxSteps int    `json:"max_steps"`
	// MaxTokensPerRun 是整轮 token 成本闸（0 = 不限）。
	MaxTokensPerRun int `json:"max_tokens_per_run"`
	// ContextWindow / MaxOutputTokens 是 0 表示"用内置默认"，不是"不限"。
	ContextWindow     int        `json:"context_window"`
	MaxOutputTokens   int        `json:"max_output_tokens"`
	DailyAmountBudget int64      `json:"daily_amount_budget"`
	DryRunRequired    bool       `json:"dry_run_required"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
}

func newAgentSettingItemView(m model.AgentSetting) agentSettingItemView {
	v := agentSettingItemView{
		ID: m.ID, Enabled: m.Enabled, ModelID: m.ModelID,
		MaxSteps: m.MaxSteps, MaxTokensPerRun: m.MaxTokensPerRun,
		ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
		DailyAmountBudget: m.DailyAmountBudget, DryRunRequired: m.DryRunRequired,
	}
	// 单行设置从未保存过时 Settings() 返回的是内存默认值，UpdatedAt 是零值。
	// 下发 `0001-01-01T00:00:00Z` 会让人以为设置被改过，宁可不给。
	if !m.UpdatedAt.IsZero() {
		t := m.UpdatedAt
		v.UpdatedAt = &t
	}
	return v
}

type agentDocItemView struct {
	ID         uint64    `json:"id"`
	Title      string    `json:"title"`
	SourceType string    `json:"source_type"`
	SourceURL  string    `json:"source_url,omitempty"`
	SizeBytes  int       `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
}

func newAgentDocItemView(m model.AgentDoc) agentDocItemView {
	return agentDocItemView{
		ID: m.ID, Title: m.Title, SourceType: m.SourceType,
		SourceURL: m.SourceURL, SizeBytes: m.SizeBytes, CreatedAt: m.CreatedAt,
	}
}

/* ------------------------------ 会话 ------------------------------ */

// ListAdminAgentSessions 处理 GET /api/admin/agent/sessions。
func ListAdminAgentSessions(c *gin.Context) {
	// page 缺省/0 = 全量（服务层按 50 条截断）；非法值必须 400，
	// 否则 page=abc 会静默走全量分支 —— 与模型/渠道两处同一口径。
	p, all, err := pagination.ParseOptional(c)
	if err != nil {
		response.FailDefMsg(c, errs.InvalidParam, err.Error())
		return
	}
	page := p.Page
	if all {
		page = 0
	}
	rows, total, err := agent.ListSessions(c.Request.Context(), page, p.PageSize)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]agentSessionView, 0, len(rows))
	for i := range rows {
		items = append(items, newAgentSessionView(rows[i]))
	}
	resp := gin.H{"items": items, "total": total}
	if page > 0 {
		// 全量分支不回显页码：前端据此保持「未分页」的展示，与改造前一致。
		resp["page"] = page
		resp["page_size"] = p.PageSize
	}
	response.OK(c, resp)
}

// CreateAdminAgentSession 处理 POST /api/admin/agent/sessions。
func CreateAdminAgentSession(c *gin.Context) {
	var req struct {
		Title   string `json:"title"`
		ModelID string `json:"model_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	row, err := agent.CreateSession(c.Request.Context(), req.Title, req.ModelID)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, gin.H{"item": newAgentSessionView(*row)})
}

// UpdateAdminAgentSession 处理 PATCH /api/admin/agent/sessions/:id（改名 / 归档 / 换模型）。
func UpdateAdminAgentSession(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	var req struct {
		Title   string  `json:"title"`
		Status  *int16  `json:"status"`
		ModelID *string `json:"model_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	// model_id 是**会话级**的"这次对话用哪个模型"：空串表示回到全站默认。
	if err := agent.UpdateSession(c.Request.Context(), id, req.Title, req.Status, req.ModelID); err != nil {
		respondAgentErr(c, err)
		return
	}
	response.OK(c, nil)
}

// DeleteAdminAgentSession 处理 DELETE /api/admin/agent/sessions/:id。
func DeleteAdminAgentSession(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	if err := agent.DeleteSession(c.Request.Context(), id); err != nil {
		respondAgentErr(c, err)
		return
	}
	response.OK(c, nil)
}

// ListAdminAgentMessages 处理 GET /api/admin/agent/sessions/:id/messages（断线续传用）。
//
// after_seq 是为了续传：断线后前端只需要"我没看到的那几条"，
// 靠时间戳排序会在同一毫秒的两条消息上错序，所以续传键必须是单调的 seq。
func ListAdminAgentMessages(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(strings.TrimSpace(c.Query("after_seq")), 10, 64)
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	rows, err := agent.Messages(c.Request.Context(), id, after, limit)
	if err != nil {
		respondAgentErr(c, err)
		return
	}
	items := make([]agentMessageView, 0, len(rows))
	for i := range rows {
		m := rows[i]
		items = append(items, agentMessageView{
			ID: m.ID, SessionID: m.SessionID, Seq: m.Seq, Role: m.Role,
			Content: m.Content, Parts: m.Parts, Status: m.Status,
			TokensIn: m.TokensIn, TokensOut: m.TokensOut, CreatedAt: m.CreatedAt,
		})
	}
	// 正在等待回答的问题一并下发：刷新/换标签页后仍能看到那张卡片并作答。
	// 没有它的话，"等回答"这个状态只存在于一条 SSE 连接里，刷新就消失，
	// 而库里那一轮还在等 —— 运营只能干等到超时。
	body := gin.H{"items": items, "pending_ask": nil}
	if cur, err := agent.PendingAsk(c.Request.Context(), id); err == nil && cur != nil {
		body["pending_ask"] = gin.H{
			"id": cur.ID, "ask": agent.AskViewOf(cur),
		}
	}
	response.OK(c, body)
}

/* ------------------------------ 提案 ------------------------------ */

// GetAdminAgentProposal 处理 GET /api/admin/agent/proposals/:id。
//
// 同时下发 diff_stats：前端要显示"新增 3 / 修改 1 / 删除 0"，
// 让前端自己数 JSON 里的数组长度会在 diff 结构变化时静默变成 0。
func GetAdminAgentProposal(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	row, err := agent.GetProposal(c.Request.Context(), id)
	if err != nil {
		respondAgentErr(c, err)
		return
	}
	view := newAgentProposalView(*row)
	if plan, perr := agent.DecodePlan(row.Plan); perr == nil {
		if st, serr := agent.ProbeStatusFor(c.Request.Context(), db.DB, row.SessionID, plan); serr == nil {
			view.Probe = &st
		} else {
			logger.L().Warn("计算提案探测状态失败（卡片将不显示探测状态）",
				zap.Uint64("proposal_id", row.ID), zap.Error(serr))
		}
	}
	added, changed, removed := agent.DiffStats(json.RawMessage(row.Diff))
	response.OK(c, gin.H{
		"item": view,
		"diff_stats": gin.H{
			"added": added, "changed": changed, "removed": removed,
		},
	})
}

// ListAdminAgentProposals 处理 GET /api/admin/agent/proposals?session_id=。
func ListAdminAgentProposals(c *gin.Context) {
	sid, _ := strconv.ParseUint(strings.TrimSpace(c.Query("session_id")), 10, 64)
	rows, err := agent.ListProposals(c.Request.Context(), sid)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]agentProposalView, 0, len(rows))
	for i := range rows {
		items = append(items, newAgentProposalView(rows[i]))
	}
	// 整页只查两次库（channels / agent_dry_runs），与提案条数无关。
	// 失败只降级（不显示探测状态），不能让列表 500 —— 看列表比看那一枚徽标重要。
	if sts, err := agent.ProbeStatusBatch(c.Request.Context(), db.DB, rows); err != nil {
		logger.L().Warn("批量计算提案探测状态失败（卡片将不显示探测状态）", zap.Error(err))
	} else {
		for i := range items {
			if st, ok := sts[items[i].ID]; ok {
				st := st
				items[i].Probe = &st
			}
		}
	}
	response.OK(c, gin.H{"items": items})
}

// ApplyAdminAgentProposal 处理 POST /api/admin/agent/proposals/:id/apply。
//
// 必须带 `confirm:true` 与 `plan_hash`：
// plan_hash 防的是"UI 显示的是 A、实际提交的是 B"（前端 bug 或中途被改过）；
// confirm 防的是误触——这是一个会改线上配置的按钮。
func ApplyAdminAgentProposal(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	var req struct {
		PlanHash string `json:"plan_hash"`
		Confirm  bool   `json:"confirm"`
		// Scope 是**入口的自我约束**，由前端按"从哪个按钮进来"给出：
		// 「新建适配器」弹窗传 ["adapter_spec"]，独立助手页不传。
		// 服务端只校验、不存储 —— 见 agent.EnforceScope 的注释。
		Scope []string `json:"scope"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	if !req.Confirm {
		response.FailDef(c, errs.InvalidParam.WithMessage("请确认后再次提交"))
		return
	}
	// 越界一律拒绝，**不做裁剪**：静默丢掉渠道操作会让管理员以为自己批准的那份
	// diff 就是最终生效的，实际改的是另一份 —— 那比拒绝危险得多。
	if len(req.Scope) > 0 {
		prop, perr := agent.GetProposal(c.Request.Context(), id)
		if perr != nil {
			respondAgentErr(c, perr)
			return
		}
		if serr := agent.EnforceScope(prop, req.Scope); serr != nil {
			respondAgentErr(c, serr)
			return
		}
	}
	adminID := c.GetUint64("user_id")
	res, err := agent.Apply(c.Request.Context(), id, req.PlanHash, adminID)
	if err != nil {
		respondAgentErr(c, err)
		return
	}
	response.OK(c, gin.H{
		"proposal_id":   res.ProposalID,
		"ops":           res.Ops,
		"diff":          res.Diff,
		"added":         diffCount(res.Diff),
		"touched_specs": res.TouchedSpecs,
	})
}

// RollbackAdminAgentProposal 处理 POST /api/admin/agent/proposals/:id/rollback。
//
// 回滚**不是魔法撤销**：它生成一条反向提案，仍然要人工确认后再 apply 一次。
// 这样"谁在什么时候把配置改回去了"同样有据可查，且回滚本身也能再被回滚。
func RollbackAdminAgentProposal(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || !req.Confirm {
		response.FailDef(c, errs.InvalidParam.WithMessage("请确认后再次提交"))
		return
	}
	row, err := agent.Rollback(c.Request.Context(), id, c.GetUint64("user_id"))
	if err != nil {
		respondAgentErr(c, err)
		return
	}
	response.OK(c, gin.H{"item": newAgentProposalView(*row), "plan_hash": row.PlanHash})
}

// RejectAdminAgentProposal 处理 POST /api/admin/agent/proposals/:id/reject。
func RejectAdminAgentProposal(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	if err := agent.RejectProposal(c.Request.Context(), id, c.GetUint64("user_id")); err != nil {
		respondAgentErr(c, err)
		return
	}
	response.OK(c, nil)
}

/* ------------------------------ 设置与统计 ------------------------------ */

// agentChatModelView 是「可选对话模型」的下发形状。
//
// 只给 id / 展示名 / 厂商 / 是否默认：模型菜单要的是"能认出来是哪一个"，
// 把 ratio、endpoints 这些计费字段一起下发等于在管理台里开了第二份价目表。
type agentChatModelView struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	VendorName  string `json:"vendor_name"`
	// IconURL / VendorColor 是识别信息的一部分（与模型管理、模型广场同一份来源）：
	// 同名的「gpt-4o」在多渠道下只看名字分不出是官方还是中转。
	IconURL     string `json:"icon_url"`
	VendorColor string `json:"vendor_color"`
	IsDefault   bool   `json:"is_default"`
	// Tags 是营销标签（与模型广场同一份快照，已过滤到期项）。
	Tags []model.ModelTagView `json:"tags"`
}

// agentSettingsView 是下发给前端的设置视图。
//
// `available=false` 时必须带 reason：AI 编排依赖一个可用的对话模型，
// 没有时前端要能明说"请先配置对话模型"，而不是让运营面对一个点不动的输入框。
type agentSettingsView struct {
	Item      agentSettingItemView `json:"item"`
	Available bool                 `json:"available"`
	Reason    string               `json:"reason,omitempty"`
	Hint      string               `json:"hint,omitempty"`
	// ModelID 是「这一轮实际会用的模型」（设置未指定时是系统默认），
	// 供前端显示"当前用的是哪个模型"——与 Item.ModelID（可能为空）分开。
	ModelID string `json:"effective_model_id"`
	// TodaySpend 是今日已花估算额度（预算闸的分子）。
	TodaySpend int64 `json:"today_spend"`
	// Limits 是各字段的合法区间：前端用它渲染 min/max，不自己写一份常量。
	// 两处各写一份时，"页面标红但后端接受"这种故障会长期存在。
	Limits agent.LimitRanges `json:"limits"`
	// ChatModels 是输入框「模型」菜单的候选（modality=chat 且可用）。
	// 只收 chat：把 embeddings/rerank 放进菜单，选中后每轮都会在"不支持工具"上失败，
	// 而报错发生在运营敲完需求之后。
	ChatModels []agentChatModelView `json:"chat_models"`
}

// GetAdminAgentSettings 处理 GET /api/admin/agent/settings。
func GetAdminAgentSettings(c *gin.Context) {
	ctx := c.Request.Context()
	row, err := agent.Settings(ctx)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	view := agentSettingsView{Item: newAgentSettingItemView(*row), Available: true, Limits: agent.Ranges()}
	if id, ok := agent.ResolveChatModel(row.ModelID); ok {
		view.ModelID = id
	} else {
		view.Available = false
		view.Reason = "agent_model_unavailable"
		view.Hint = "请先在模型管理配置一个对话模型并绑定到可用渠道"
	}
	defaultModel := view.ModelID
	base := siteurl.Base(c)
	view.ChatModels = make([]agentChatModelView, 0, 8)
	for _, m := range agent.ChatModelCandidates() {
		view.ChatModels = append(view.ChatModels, agentChatModelView{
			ID: m.ID, DisplayName: m.DisplayName, VendorName: m.VendorName,
			IconURL: iconref.URL(base, m.Icon), VendorColor: m.VendorColor,
			IsDefault: m.ID == defaultModel,
			Tags:      m.Tags,
		})
	}
	if view.TodaySpend, err = agent.DailySpend(ctx); err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, view)
}

// SaveAdminAgentSettings 处理 PUT /api/admin/agent/settings。
func SaveAdminAgentSettings(c *gin.Context) {
	var req struct {
		Enabled           *bool   `json:"enabled"`
		ModelID           *string `json:"model_id"`
		MaxSteps          *int    `json:"max_steps"`
		MaxTokensPerRun   *int    `json:"max_tokens_per_run"`
		ContextWindow     *int    `json:"context_window"`
		MaxOutputTokens   *int    `json:"max_output_tokens"`
		DailyAmountBudget *int64  `json:"daily_amount_budget"`
		DryRunRequired    *bool   `json:"dry_run_required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	cur, err := agent.Settings(c.Request.Context())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 逐字段覆盖：未传的字段保持原值，避免"只改步数"把开关一起重置成 false。
	//
	// 区间校验**全部**交给 agent.ValidateSettings（唯一规则处），
	// 这里不再逐字段判 < > —— 以前是六段 if，放宽区间时必然漏改其中一段，
	// 表现就是"某个字段永远只能是旧范围"。
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.ModelID != nil {
		cur.ModelID = strings.TrimSpace(*req.ModelID)
	}
	if req.MaxSteps != nil {
		cur.MaxSteps = *req.MaxSteps
	}
	if req.MaxTokensPerRun != nil {
		cur.MaxTokensPerRun = *req.MaxTokensPerRun
	}
	if req.ContextWindow != nil {
		cur.ContextWindow = *req.ContextWindow
	}
	if req.MaxOutputTokens != nil {
		cur.MaxOutputTokens = *req.MaxOutputTokens
	}
	if req.DailyAmountBudget != nil {
		cur.DailyAmountBudget = *req.DailyAmountBudget
	}
	if req.DryRunRequired != nil {
		cur.DryRunRequired = *req.DryRunRequired
	}
	if err := agent.ValidateSettings(cur); err != nil {
		// 校验错误自带字段名与具体数字，直接展示；再包一层通用文案等于把信息丢掉。
		response.FailDef(c, errs.InvalidParam.WithMessage(err.Error()))
		return
	}
	if err := agent.SaveSettings(c.Request.Context(), *cur); err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	// 审计必须记全量：成本相关的字段（预算/窗口/输出）改动如果不留痕，
	// 事后"为什么这一轮突然被拦"就只能靠猜。
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAgentSettings, "agent_setting", "1",
		gin.H{
			"enabled": cur.Enabled, "model_id": cur.ModelID, "max_steps": cur.MaxSteps,
			"max_tokens_per_run": cur.MaxTokensPerRun,
			"context_window":     cur.ContextWindow, "max_output_tokens": cur.MaxOutputTokens,
			"daily_amount_budget": cur.DailyAmountBudget, "dry_run_required": cur.DryRunRequired,
		})
	response.OK(c, gin.H{"item": newAgentSettingItemView(*cur), "limits": agent.Ranges()})
}

// GetAdminAgentStats 处理 GET /api/admin/agent/stats?days=7。
func GetAdminAgentStats(c *gin.Context) {
	days, _ := strconv.Atoi(strings.TrimSpace(c.Query("days")))
	st, err := agent.Stats(c.Request.Context(), days)
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	response.OK(c, st)
}

/* ------------------------------ 文档副本 ------------------------------ */

// CreateAdminAgentDoc 处理 POST /api/admin/agent/docs。
//
// 返回 `duplicate:true` 表示同一份内容已存在（复用已有行）：
// 这是提示而不是错误——运营重复粘贴同一份文档时，多落一行会让
// docs_read 的目录里出现三条同名条目，模型每次都只读第一条。
func CreateAdminAgentDoc(c *gin.Context) {
	var req struct {
		Title      string `json:"title"`
		SourceType string `json:"source_type"`
		Content    string `json:"content"`
		URL        string `json:"url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FailDef(c, errs.InvalidParam)
		return
	}
	doc, created, err := agent.CreateDoc(c.Request.Context(), c.GetUint64("user_id"), agent.DocInput{
		Title: req.Title, SourceType: req.SourceType, Content: req.Content, URL: req.URL,
	})
	if err != nil {
		respondAgentErr(c, err)
		return
	}
	if created {
		audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAgentDocCreate, "agent_doc",
			strconv.FormatUint(doc.ID, 10), gin.H{"title": doc.Title, "size": doc.SizeBytes})
	}
	response.OK(c, gin.H{
		"item": newAgentDocItemView(*doc), "duplicate": !created,
		"sections": agent.DocOutline(doc),
	})
}

// ListAdminAgentDocs 处理 GET /api/admin/agent/docs（目录，不含正文）。
func ListAdminAgentDocs(c *gin.Context) {
	rows, err := agent.ListDocs(c.Request.Context())
	if err != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for i := range rows {
		items = append(items, gin.H{
			"id": rows[i].ID, "title": rows[i].Title,
			"source_type": rows[i].SourceType, "source_url": rows[i].SourceURL,
			"size_bytes": rows[i].SizeBytes, "created_at": rows[i].CreatedAt,
		})
	}
	response.OK(c, gin.H{"items": items, "limits": agentDocLimits()})
}

// PreviewAdminAgentDoc 处理 GET /api/admin/agent/docs/:id/preview（前 8KB + 目录）。
func PreviewAdminAgentDoc(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	pv, err := agent.PreviewDoc(c.Request.Context(), id)
	if err != nil {
		respondAgentErr(c, err)
		return
	}
	response.OK(c, pv)
}

// DeleteAdminAgentDoc 处理 DELETE /api/admin/agent/docs/:id。
func DeleteAdminAgentDoc(c *gin.Context) {
	id, ok := agentID(c)
	if !ok {
		return
	}
	if err := agent.DeleteDoc(c.Request.Context(), id); err != nil {
		respondAgentErr(c, err)
		return
	}
	audit.RecordOrWarn(db.DB, c.GetUint64("user_id"), audit.ActionAgentDocDelete, "agent_doc",
		strconv.FormatUint(id, 10), nil)
	response.OK(c, nil)
}

// agentDocLimits 下发文档上限（前端在提交前就要拦住，不要等 413）。
func agentDocLimits() gin.H {
	return gin.H{
		"max_bytes":       agent.MaxDocBytes,
		"max_fetch_bytes": agent.MaxDocFetchBytes,
		"preview_bytes":   agent.DocPreviewBytes,
	}
}

/* ------------------------------ 助手 ------------------------------ */

func agentID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id == 0 {
		response.FailDef(c, errs.InvalidParam)
		return 0, false
	}
	return id, true
}

// diffCount 取 diff 里的"有变更的实体数"（响应体里的 added 字段）。
func diffCount(diff json.RawMessage) int {
	added, changed, removed := agent.DiffStats(diff)
	return added + changed + removed
}

// respondAgentErr 把 agent 服务层错误翻译成对外错误码。
//
// 提案相关的错误**必须分开**：运营看到之后的处置动作完全不同
// （刷新页面 / 重新生成 / 别再点了 / 去手工核对 / 改需求），
// 合成一个"提案不可用"会让人只能反复点同一个按钮。
func respondAgentErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, agent.ErrSessionNotFound), errors.Is(err, agent.ErrProposalNotFound),
		errors.Is(err, agent.ErrDocNotFound):
		response.FailDef(c, errs.NotFound)
	case errors.Is(err, agent.ErrBusy), errors.Is(err, agent.ErrSessionBusy):
		response.FailDef(c, errs.AgentBusy)
	case errors.Is(err, agent.ErrHashMismatch):
		response.FailDef(c, errs.AgentProposalConflict)
	case errors.Is(err, agent.ErrNotApplyable):
		response.FailDef(c, errs.AgentProposalState)
	case errors.Is(err, agent.ErrOutOfScope):
		response.FailDef(c, errs.AgentOutOfScope)
	case errors.Is(err, agent.ErrStale):
		response.FailDef(c, errs.AgentProposalStale)
	case errors.Is(err, agent.ErrDrift):
		response.FailDef(c, errs.AgentProposalDrift)
	case errors.Is(err, agent.ErrDryRunRequired):
		// 409：状态可修复（先做一次探测），不是参数错。
		// data 里带"缺哪个地址、为什么"：只给一句话，运营只能反复点同一个按钮。
		data := gin.H{"targets": []agent.ProbeTarget{}}
		var de *agent.DryRunRequiredError
		if errors.As(err, &de) {
			data["targets"] = de.Targets
		}
		response.FailDefData(c, errs.AgentProbeRequired, data)
	case errors.Is(err, agent.ErrDocEmpty):
		response.FailDef(c, errs.AgentDocEmpty)
	case errors.Is(err, agent.ErrDocTooLarge):
		response.FailDef(c, errs.AgentDocTooLarge)
	case errors.Is(err, agent.ErrDocTypeUnsupported), errors.Is(err, agent.ErrDocFetchFailed):
		response.FailDefMsg(c, errs.InvalidParam, err.Error())
	case errors.Is(err, agent.ErrDocBinary):
		// 415：不是"参数写错"，而是"这类内容读不出正文"，语义比 400 更准。
		response.FailDefMsg(c, errs.AgentDocBinary,
			"这份内容看起来是二进制文件（PDF/Office 压缩包等），当前只能读取文本；请提供文本/Markdown/JSON 版本")
	default:
		response.FailDefMsg(c, errs.InvalidParam, err.Error())
	}
}
