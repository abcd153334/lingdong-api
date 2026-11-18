package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件是「送进模型的上下文」的**唯一装配点**：历史轮次怎么取、哪些轮带工具原文、
// 哪些轮降级成摘要、窗口怎么算、本轮内累积超窗怎么压。
//
// 三条约束：
//  1. **只有这一处装配**。旧的 buildMessages 已删除 —— 两份装配逻辑必然分叉，
//     而分叉的症状是"界面看到的和模型看到的不是同一段历史"，无从排查。
//  2. **回灌只用权威表**：user/assistant 正文读 agent_messages.content，
//     工具原文读 agent_messages.ctx，摘要读 agent_messages.summary。
//     parts 是展示投影（工具结果只留 300 字），任何装配路径都不读它。
//  3. **配对不可拆**：assistant(tool_calls) 与它后面的 tool 消息是一个整体。
//     只删 tool 会让上游 400（tool 必须是某个 tool_calls 的应答），
//     这类错误在日志里只显示"上游 400"，极难定位。

const (
	// historyScanLimit 是一次装配最多扫描的历史行数（防御上限，约 100 轮）。
	//
	// 没有它，一个几千条的会话每轮都要把整表尾部读一遍；有它也不丢信息 ——
	// 超出部分由 rolling_summary 承接（见 mergeRolling）。
	historyScanLimit = 200
	// fullTurnLimit 是「全保真回灌」最多带几轮工具原文。
	//
	// 再多也没意义：一次接入的真实工具结果集中在最近几轮，
	// 更早的轮次模型只需要结论（summary）。
	fullTurnLimit = 8
	// toolMsgBytesMax 是单条工具消息的回灌上限。
	toolMsgBytesMax = 8 << 10
	// turnToolBytesMax 是单轮回灌的工具消息总量上限。
	turnToolBytesMax = 24 << 10
	// replayTextBytesMax 是单条 user/assistant 正文的回灌上限（超长需求文档会命中）。
	replayTextBytesMax = 32 << 10
	// minHistoryBudget 是历史预算的下限：低于它说明窗口连固定开销都快装不下。
	minHistoryBudget = 2000
	// inRunSoftRatio 是轮内压缩的触发比例（留 8% 给上游分词误差）。
	inRunSoftRatio = 92
)

// 装配期错误码（与 retryableOf 对应：这些都是"要人去改设置"，重试无用）。
const (
	CodeContextWindowTooSmall = "context_window_too_small"
	CodeContextOverflow       = "context_overflow"
)

// ContextError 是装配期失败的**结构化**原因（code + 可直接展示的中文）。
//
// 用独立类型而不是 fmt.Errorf：runtime 要把它翻成 SSE 的 error 段与 agent_runs.error_code，
// 靠字符串匹配区分"窗口太小"和"上下文满了"会在改文案时静默失效。
type ContextError struct {
	Code    string
	Message string
}

func (e *ContextError) Error() string { return e.Message }

// BuildInput 是一轮装配的入参。
type BuildInput struct {
	SessionID uint64
	MessageID uint64
	// UserSeq 是当前这条用户消息的 seq：历史只取 seq < UserSeq 的行。
	//
	// 为什么必须有它：`TakeLock` 在 Redis 未就绪时**直接放行**（多副本/降级），
	// 于是并发写进来的消息会混进本轮上下文。"按行数取尾 20 条再跳过自己"做不到这个隔离。
	UserSeq  int64
	UserText string
	DocIDs   []uint64
	ModelID  string
}

// ContextPlan 是一次装配的结果。
type ContextPlan struct {
	// Messages 直接喂给 StreamChat（system 在 [0]）。
	Messages []Message
	System   string
	Tools    []ToolSpec
	// Tokens 是这次请求的输入估算（system + tools + history + 当前输入）。
	Tokens int
	// Fixed 是不可压缩部分（system + tools + 当前输入）。
	Fixed           int
	Window          int
	Output          int
	Level           int
	KeptTurns       int
	SummarizedTurns int
	DroppedTurns    int
}

// BuildContext 装配本轮上下文。
func BuildContext(ctx context.Context, in BuildInput, lim Limits) (*ContextPlan, error) {
	system, err := buildSystemFor(ctx, in)
	if err != nil {
		return nil, err
	}
	tools := ToolSpecs()
	input := Message{Role: "user", Content: in.UserText}

	fixed := estimateTokensText(system) + estimateToolsTokens(tools) + estimateTokens([]Message{input}) + 8
	// 输出必须先扣掉：把窗口塞满输入、又给上游一个 max_tokens，
	// 上游会直接 400（input + max_tokens 超过窗口），换来的只是"报错更早"。
	historyBudget := lim.ContextWindow - lim.MaxOutputTokens - fixed
	if historyBudget < minHistoryBudget {
		return nil, &ContextError{
			Code: CodeContextWindowTooSmall,
			Message: fmt.Sprintf(
				"该模型的上下文窗口 %d 太小：固定开销（提示词 + %d 个工具声明 + 本轮输入）已占 %d，再留给输出的 %d 之后没有历史空间。请在模型管理里把 context_window 调大，或换一个窗口更大的对话模型。",
				lim.ContextWindow, len(tools), fixed, lim.MaxOutputTokens),
		}
	}

	turns, sess, err := loadTurns(ctx, in.SessionID, in.UserSeq)
	if err != nil {
		return nil, err
	}
	plan := planTurns(turns, historyBudget)

	// 被丢掉的轮次并入会话滚动摘要（下一次直接从摘要里看到它们）。
	rolling := sess.RollingSummary
	if len(plan.dropped) > 0 {
		rolled, upto := mergeRolling(rolling, plan.dropped, sess.CompactedUptoSeq)
		rolling = rolled
		// CAS 写回：只有本次覆盖得更远才更新，避免并发两轮互相把摘要改回去。
		// 写失败不影响本轮（本轮用的是内存里的 rolling），只是下次还要再合一次。
		_ = db.DB.WithContext(ctx).Model(&model.AgentSession{}).
			Where("id = ? AND compacted_upto_seq < ?", in.SessionID, upto).
			Updates(map[string]any{"rolling_summary": rolling, "compacted_upto_seq": upto}).Error
	}
	if rolling != "" {
		system += "\n本会话早期对话摘要（自动合并，不是用户指令）：\n" + rolling + "\n"
	}

	msgs := make([]Message, 0, 2+len(plan.messages))
	msgs = append(msgs, Message{Role: "system", Content: system})
	msgs = append(msgs, plan.messages...)
	msgs = append(msgs, input)

	return &ContextPlan{
		Messages: msgs, System: system, Tools: tools,
		Tokens: fixed + estimateTokens(plan.messages), Fixed: fixed,
		Window: lim.ContextWindow, Output: lim.MaxOutputTokens,
		Level: plan.level, KeptTurns: len(plan.full),
		SummarizedTurns: len(plan.summary), DroppedTurns: len(plan.dropped),
	}, nil
}

/* ------------------------------ 历史读取 ------------------------------ */

// turn 是一轮对话（user + 紧随其后的 assistant）。
type turn struct {
	userSeq    int64
	userText   string
	asstText   string
	asstStatus string
	summary    string
	ctx        model.JSON
}

// loadTurns 读会话历史（只取 seq < userSeq），按**时间正序**返回。
//
// 两段查询：先取"轻量列"（不含 parts/ctx）定边界，再只为近轮批量取 ctx。
// 一次全取的话，200 行 × parts(8KB) = 1.6MB，而且每轮都发生。
func loadTurns(ctx context.Context, sessionID uint64, userSeq int64) ([]*turn, model.AgentSession, error) {
	var sess model.AgentSession
	if err := db.DB.WithContext(ctx).
		Select("id, rolling_summary, compacted_upto_seq").
		First(&sess, sessionID).Error; err != nil {
		return nil, sess, err
	}
	q := db.DB.WithContext(ctx).
		Select("id, seq, role, content, status, summary").
		Where("session_id = ?", sessionID)
	if userSeq > 0 {
		q = q.Where("seq < ?", userSeq)
	}
	var rows []model.AgentMessage
	if err := q.Order("seq DESC").Limit(historyScanLimit).Find(&rows).Error; err != nil {
		return nil, sess, err
	}

	// desc 顺序里 assistant 先于它的 user 出现：先暂存，遇到 user 时认领。
	turns := make([]*turn, 0, len(rows)/2+1)
	var pending *model.AgentMessage
	for i := range rows {
		row := rows[i]
		switch row.Role {
		case "assistant":
			// 一轮正常只有一条 assistant（每次 run 只落一条）。
			// 若同一 user 之后出现第二条，说明是历史遗留的重复落库，只认最新那条。
			if pending == nil {
				pending = &rows[i]
			}
		case "user":
			t := &turn{userSeq: row.Seq, userText: row.Content}
			if pending != nil {
				t.asstText, t.asstStatus = pending.Content, pending.Status
				t.summary = pending.Summary
				pending = nil
			}
			turns = append(turns, t)
		}
	}
	// desc → asc（装配按时间正序）
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	// 会话最早若是孤立 assistant（没有对应 user），不参与回灌：
	// 保留它会凭空造出一轮不存在的需求。

	// 已被滚动摘要覆盖的轮次不再参与装配：它们在 system 的「早期对话摘要」里。
	//
	// 不筛掉的后果是**每轮都把同一批旧轮重新并入滚动摘要** ——
	// 摘要里出现 N 份重复的"第一件事"，直到撞上 4KB 上限，而模型会以为
	// 同一件事被要求了 N 次。
	if sess.CompactedUptoSeq > 0 {
		keep := turns[:0]
		for _, t := range turns {
			if t.userSeq+1 > sess.CompactedUptoSeq {
				keep = append(keep, t)
			}
		}
		turns = keep
	}

	// 近轮补 ctx：只有可能进"全保真窗口"的那几轮需要原文。
	if len(turns) > 0 {
		from := len(turns) - fullTurnLimit
		if from < 0 {
			from = 0
		}
		seqs := make([]int64, 0, fullTurnLimit)
		for _, t := range turns[from:] {
			// assistant 的 seq = user.seq + 1（AppendMessage 在同一会话内原子自增，
			// 一轮内先写 user 再写 assistant，没有别的写入者会插在中间）。
			seqs = append(seqs, t.userSeq+1)
		}
		if len(seqs) > 0 {
			var ctxRows []struct {
				Seq int64
				Ctx model.JSON
			}
			if err := db.DB.WithContext(ctx).Model(&model.AgentMessage{}).
				Select("seq, ctx").
				Where("session_id = ? AND role = 'assistant' AND seq IN ?", sessionID, seqs).
				Find(&ctxRows).Error; err == nil {
				bySeq := make(map[int64]model.JSON, len(ctxRows))
				for _, r := range ctxRows {
					bySeq[r.Seq] = r.Ctx
				}
				for _, t := range turns[from:] {
					if c, ok := bySeq[t.userSeq+1]; ok {
						t.ctx = c
					}
				}
			}
		}
	}
	return turns, sess, nil
}

/* ------------------------------ 轮次取舍 ------------------------------ */

// turnPlan 是"哪些轮怎么回灌"的结果。
type turnPlan struct {
	messages []Message
	full     []*turn
	summary  []*turn
	dropped  []*turn
	level    int
}

// 回灌形态。
const (
	shapeDrop    = 0
	shapeSummary = 1
	shapeFull    = 2
)

// planTurns 决定哪些轮带原文、哪些轮只留摘要、哪些轮并入滚动摘要。
//
// 贪心从**最新**往回：能整轮塞下就全保真，塞不下就退成摘要，
// 摘要也塞不下就丢掉（并入 rolling_summary）。
// 这样"最近几轮最完整"是结构性保证，不依赖任何阈值组合。
func planTurns(turns []*turn, historyBudget int) turnPlan {
	type decidedTurn struct {
		t     *turn
		shape int
	}
	plan := turnPlan{}
	decided := make([]decidedTurn, 0, len(turns))
	used := 0
	// stopFull 一旦为 true，更老的轮次**一律**只做摘要。
	//
	// 不这么做的后果很隐蔽：某一轮特别大（读了三份文档）被降级成摘要，
	// 而更老的"小轮"因为装得下仍是全保真 —— 模型会看到"新轮是摘要、旧轮是原文"，
	// 把旧细节当成最近发生的事，甚至重复已经做过的操作。
	stopFull := false

	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if !stopFull && len(plan.full) < fullTurnLimit {
			if _, cost := materializeFull(t); used+cost <= historyBudget {
				used += cost
				plan.full = append(plan.full, t)
				decided = append(decided, decidedTurn{t, shapeFull})
				continue
			}
		}
		stopFull = true
		if _, cost := materializeSummary(t); used+cost <= historyBudget {
			used += cost
			plan.summary = append(plan.summary, t)
			decided = append(decided, decidedTurn{t, shapeSummary})
			continue
		}
		plan.dropped = append(plan.dropped, t)
		decided = append(decided, decidedTurn{t, shapeDrop})
	}

	// decided 是新→旧，装配要旧→新。
	for i := len(decided) - 1; i >= 0; i-- {
		switch decided[i].shape {
		case shapeFull:
			msgs, _ := materializeFull(decided[i].t)
			plan.messages = append(plan.messages, msgs...)
		case shapeSummary:
			msg, _ := materializeSummary(decided[i].t)
			plan.messages = append(plan.messages, msg)
		}
	}
	switch {
	case len(plan.dropped) > 0:
		plan.level = 2
	case len(plan.summary) > 0:
		plan.level = 1
	}
	return plan
}

// materializeFull 生成一轮的"全保真"消息（user + 工具原文 + 结论）。
func materializeFull(t *turn) ([]Message, int) {
	msgs := make([]Message, 0, 4)
	msgs = append(msgs, Message{Role: "user", Content: truncateReplay(t.userText)})

	toolMsgs, toolBytes := expandCtx(t.ctx)
	// 单轮工具原文有总量上限：一条 6KB 的文档读 5 次就是 30KB，
	// 只做单条截断挡不住（每条都合法，加起来爆）。
	for i := range toolMsgs {
		if toolBytes <= turnToolBytesMax {
			break
		}
		if toolMsgs[i].Role != "tool" {
			continue
		}
		over := toolBytes - turnToolBytesMax
		cur := len(toolMsgs[i].Content)
		if cur <= over {
			toolBytes -= cur
			toolMsgs[i].Content = truncatedToolPlaceholder
			toolBytes += len(toolMsgs[i].Content)
		} else {
			toolMsgs[i].Content = clipBytes(toolMsgs[i].Content, cur-over)
			toolBytes -= over
		}
	}
	msgs = append(msgs, toolMsgs...)

	if strings.TrimSpace(t.asstText) != "" || len(toolMsgs) > 0 {
		content := truncateReplay(t.asstText)
		if content == "" {
			// 有工具但没有结论（max_steps / loop_guard）：必须显式说明，
			// 否则下一轮会把"上次什么都没做"当成事实。
			content = "（上一轮调用了工具但没有给出结论）"
		}
		msgs = append(msgs, Message{Role: "assistant", Content: statusPrefix(t.asstStatus) + content})
	}
	return msgs, estimateTokens(msgs)
}

// materializeSummary 生成一轮的"摘要"消息（一条 assistant，不伪造 user 发言）。
func materializeSummary(t *turn) (Message, int) {
	text := strings.TrimSpace(t.summary)
	if text == "" {
		// 摘要缺失（摘要未落库的旧消息）：用正文兜底，仍然只复述事实。
		switch {
		case strings.TrimSpace(t.asstText) != "":
			text = clipRunes(t.asstText, 300)
		case len(t.ctx) > 0:
			// 纯工具轮（content 为空）：至少说清它调了什么。
			text = "调用了 " + strings.Join(ctxToolNames(t.ctx), "、") + "，未给出结论"
		default:
			text = "（本轮没有留下可用的结论）"
		}
	}
	m := Message{Role: "assistant", Content: "[历史轮次摘要] " + text}
	return m, estimateTokens([]Message{m})
}

// ctxToolNames 从 ctx 里取出被调用的工具名（按出现顺序，去重）。
func ctxToolNames(raw model.JSON) []string {
	var doc struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, m := range doc.Messages {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name != "" && !seen[tc.Function.Name] {
				seen[tc.Function.Name] = true
				out = append(out, tc.Function.Name)
			}
		}
	}
	return out
}

// expandCtx 把落库的 ctx（assistant tool_calls + tool 消息）还原成消息数组。
//
// 完整性校验在这里做：任何 tool 消息必须能找到前一条 assistant 的 tool_calls，
// 否则丢弃 —— 半个配对的上游请求是 400，而不是"少一条结果"。
func expandCtx(raw model.JSON) ([]Message, int) {
	if len(raw) == 0 {
		return nil, 0
	}
	var doc struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Messages) == 0 {
		return nil, 0
	}
	out := make([]Message, 0, len(doc.Messages))
	open := map[string]bool{}
	// omitInvalidArgs 记录"参数解析失败"的 call_id：这些参数不再回灌给模型。
	//
	// 必须**先扫一遍**再拼：assistant(tool_calls) 在它的 tool 结果之前，
	// 边拼边标记会在真正需要它的那一条上失效（改完看起来"没效果"）。
	omitInvalidArgs := map[string]bool{}
	for _, m := range doc.Messages {
		if m.Role == "tool" && isArgFaultContent(m.Content) {
			omitInvalidArgs[m.ToolCallID] = true
		}
	}
	total := 0
	for _, m := range doc.Messages {
		switch m.Role {
		case "assistant":
			// 参数解析失败的调用：回灌时把 arguments 换成占位。
			//
			// 为什么必须换：模型能看到自己上一版原文，就会"就地微调"，
			// 而线上那三次重写只差 24 字节、错在同一层 —— 它把坏结构当成了基线。
			// 原文仍留在 agent_messages.ctx 供排障，只是不再喂回模型。
			copied := false
			for i := range m.ToolCalls {
				open[m.ToolCalls[i].ID] = true
				if !omitInvalidArgs[m.ToolCalls[i].ID] {
					continue
				}
				if !copied {
					m.ToolCalls = append([]wireToolCall(nil), m.ToolCalls...)
					copied = true
				}
				m.ToolCalls[i].Function.Arguments = `{"_omitted":"invalid_args"}`
			}
			out = append(out, m)
		case "tool":
			if !open[m.ToolCallID] {
				// 找不到来源的 tool 消息：丢弃（不能留在请求里）。
				continue
			}

			if len(m.Content) > toolMsgBytesMax {
				m.Content = clipBytes(m.Content, toolMsgBytesMax)
			}
			total += len(m.Content)
			out = append(out, m)
		}
	}
	// 末尾若有一个 assistant(tool_calls) 没有对应的 tool 结果（落库时被打断），
	// 一并丢弃：留着它是"模型要求调用工具却没有结果"的悬空状态。
	if n := len(out); n > 0 && out[n-1].Role == "assistant" && len(out[n-1].ToolCalls) > 0 {
		out = out[:n-1]
	}
	return out, total
}

/* ------------------------------ ctx 落库 ------------------------------ */

// ctxStoreBytesMax 是单轮 ctx 的落库上限（约 16k token）。
//
// 不设上限的话，一轮读 8 次文档就是 48KB，20 轮会话的 ctx 列能到 1MB；
// 而它在下一轮只会被当作"近轮全保真"读一次（超过 fullTurnLimit 的轮次根本不读）。
const ctxStoreBytesMax = 48 << 10

// encodeCtx 把本轮追加的消息编码成落库用的 ctx（回灌投影）。
//
// 只存 assistant(tool_calls) 与 tool 消息：user 原文与最终结论已在 content 里，
// 再存一遍就是"同一事实两个副本"，而副本必然分叉。
func encodeCtx(extra []Message) model.JSON {
	msgs := make([]Message, 0, len(extra))
	for _, m := range extra {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) == 0 {
				continue
			}
			msgs = append(msgs, m)
		case "tool":
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	// 超限就按**从旧到新**折叠工具结果（保留配对，只换内容）。
	groups := toolGroupsOf(msgs)
	for i := 0; i < len(groups); i++ {
		if ctxSize(msgs) <= ctxStoreBytesMax {
			break
		}
		for _, idx := range groups[i].tools {
			msgs[idx].Content = truncatedToolPlaceholder
		}
	}
	// 仍超限（说明 assistant 的 tool_calls 参数本身就巨大）：整组从旧删。
	for ctxSize(msgs) > ctxStoreBytesMax {
		cur := toolGroupsOf(msgs)
		if len(cur) == 0 {
			break
		}
		msgs = removeGroup(msgs, cur[0])
	}
	buf, err := json.Marshal(map[string]any{"messages": msgs})
	if err != nil {
		return nil
	}
	return model.JSON(buf)
}

// ctxSize 估算编码后的字节数（用 JSON 长度，与落库体积一致）。
func ctxSize(msgs []Message) int {
	buf, err := json.Marshal(msgs)
	if err != nil {
		return 0
	}
	return len(buf)
}

/* ------------------------------ 轮内压缩 ------------------------------ */

// truncatedToolPlaceholder 是工具结果被压缩后的占位（不含任何指令文本，防注入）。
const truncatedToolPlaceholder = `{"truncated":true,"hint":"结果过长已省略；需要时重新调用该工具"}`

// FitMessages 把本轮已累积的消息压进窗口，返回压缩后的消息与压缩级别。
//
// 只在**本轮内**生效：历史已在装配期按预算分配过，这里处理的是
// "读了几份大文档之后 msgs 自己涨到超窗"这种情况（装配期救不了）。
//
// 压缩单位是**组**（assistant(tool_calls) + 紧随的 tool 消息）：
// 只删 tool 会让上游 400，只删 assistant 会留下无来源的 tool。
func FitMessages(msgs []Message, tools []ToolSpec, window, output int) ([]Message, int, bool) {
	if window <= 0 {
		return msgs, 0, true
	}
	limit := window * inRunSoftRatio / 100
	need := func(m []Message) int { return estimateTokens(m) + estimateToolsTokens(tools) + output }
	if need(msgs) <= limit {
		return msgs, 0, true
	}
	level := 0
	groups := toolGroupsOf(msgs)

	// ① 保留全部配对，只把最老的组的工具结果换成占位。
	for i := 0; i < len(groups)-1; i++ {
		for _, idx := range groups[i].tools {
			if msgs[idx].Content != truncatedToolPlaceholder {
				msgs[idx].Content = truncatedToolPlaceholder
				level = 1
			}
		}
		if need(msgs) <= limit {
			return msgs, level, true
		}
	}
	// ② 整组删除（最老的先删），但永远保留最后一组与 system/当前输入。
	for i := 0; i < len(groups)-1; i++ {
		// 每次删完下标都会变，重新计算组。
		cur := toolGroupsOf(msgs)
		if len(cur) <= 1 {
			break
		}
		msgs = removeGroup(msgs, cur[0])
		level = 2
		if need(msgs) <= limit {
			return msgs, level, true
		}
	}
	return msgs, level, false
}

// toolGroup 是一组「assistant(tool_calls) + 它的 tool 结果」。
type toolGroup struct {
	at    int   // assistant 消息在 msgs 里的下标
	tools []int // 紧随其后的 tool 消息下标
}

// toolGroupsOf 找出所有工具调用组（按出现顺序）。
func toolGroupsOf(msgs []Message) []toolGroup {
	out := make([]toolGroup, 0, 4)
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		g := toolGroup{at: i}
		ids := map[string]bool{}
		for _, tc := range msgs[i].ToolCalls {
			ids[tc.ID] = true
		}
		for j := i + 1; j < len(msgs); j++ {
			if msgs[j].Role == "tool" && ids[msgs[j].ToolCallID] {
				g.tools = append(g.tools, j)
				continue
			}
			break
		}
		out = append(out, g)
	}
	return out
}

// removeGroup 删掉一组（assistant + 它的 tool），下标大的一端先删。
func removeGroup(msgs []Message, g toolGroup) []Message {
	idx := append([]int{g.at}, g.tools...)
	sort.Sort(sort.Reverse(sort.IntSlice(idx)))
	for _, i := range idx {
		if i < 0 || i >= len(msgs) {
			continue
		}
		msgs = append(msgs[:i], msgs[i+1:]...)
	}
	return msgs
}

/* ------------------------------ system 装配 ------------------------------ */

// buildSystemFor 组装 system 消息。
//
// 顺序按"变化频率"排：固定文案在前（利于上游前缀缓存），
// 提案快照 / 文档目录 / 实时状态在后（它们随运营操作变化）。
func buildSystemFor(ctx context.Context, in BuildInput) (string, error) {
	system := BuildSystemPrompt()
	if snap := proposalSnapshot(ctx, in.SessionID); snap != "" {
		system += "\n本会话最近的提案（状态会变，别假设已生效）：\n" + snap + "\n"
	}
	if cat := buildDocCatalog(ctx, in.DocIDs); len(cat) > 0 {
		if buf, err := json.Marshal(cat); err == nil {
			system += "\n可用文档目录（正文用 docs_read 分段读取，不要整份索要）：\n" + string(buf) + "\n"
		}
	}
	if state := StateSummary(ctx); state != "" {
		system += "\n当前平台状态（JSON，实时）：\n" + state + "\n"
	}
	system += "\n" + UntrustedDocNote + "\n"
	return system, nil
}

// proposalSnapshot 列出最近提案的当前状态。
//
// 为什么必须每轮刷新：模型只从工具结果里知道"我生成过 #12"，
// 但提案被拒绝/已生效是**之后**发生的事。不告诉它，它就会反复生成同一份提案，
// 或者对已经生效的变更说"还没生效，请确认"。
func proposalSnapshot(ctx context.Context, sessionID uint64) string {
	var rows []model.AgentProposal
	if err := db.DB.WithContext(ctx).
		Select("id, kind, status, plan").
		Where("session_id = ?", sessionID).
		Order("id DESC").Limit(10).Find(&rows).Error; err != nil || len(rows) == 0 {
		return ""
	}
	lines := make([]string, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		lines = append(lines, fmt.Sprintf("#%d %s %s", rows[i].ID, rows[i].Status, planSummary(json.RawMessage(rows[i].Plan))))
	}
	return strings.Join(lines, "\n")
}

/* ------------------------------ 文本工具 ------------------------------ */

// truncateReplay 截断过长的正文（保留头尾）。
//
// 只截尾是不够的：一份接入文档的"鉴权方式"常在结尾，
// 只留开头会让模型得出"文档没写鉴权"的结论。
func truncateReplay(s string) string {
	if len(s) <= replayTextBytesMax {
		return s
	}
	head := replayTextBytesMax * 3 / 4
	tail := replayTextBytesMax - head
	return clipBytes(s, head) + "\n[…中间已省略，原文过长…]\n" + tailBytes(s, tail)
}

// statusPrefix 给中断/失败的轮次加显式标记。
func statusPrefix(status string) string {
	switch status {
	case "aborted":
		return "[上一轮被中断，可能不完整] "
	case "failed":
		return "[上一轮失败] "
	}
	return ""
}

// clipBytes 按字节截断（补省略号），保证不切断 UTF-8 字符。
func clipBytes(s string, n int) string {
	if n <= 0 {
		return "…"
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// tailBytes 取末尾 n 字节（同样对齐到字符边界）。
func tailBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8Start(s[start]) {
		start++
	}
	return s[start:]
}

// utf8Start 判断某字节是否是 UTF-8 字符的起始字节。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// mergeRolling 把被丢弃的轮次摘要并进会话滚动摘要。
//
// 纯函数：给定上一条摘要与被丢弃的轮次，算出新摘要与"覆盖到哪个 seq"。
// 抽出来是为了能不起库就跑测试 —— 这段逻辑决定"模型还记得多少历史"。
func mergeRolling(prev string, dropped []*turn, upto int64) (string, int64) {
	if len(dropped) == 0 {
		return prev, upto
	}
	parts := make([]string, 0, len(dropped)+1)
	if s := strings.TrimSpace(prev); s != "" {
		parts = append(parts, s)
	}
	maxSeq := upto
	for _, t := range dropped {
		line := strings.TrimSpace(t.summary)
		if line == "" {
			line = clipRunes(strings.TrimSpace(t.asstText), 200)
		}
		if line == "" {
			line = "（无结论）"
		}
		parts = append(parts, "- "+clipRunes(strings.TrimSpace(t.userText), 80)+" → "+line)
		if t.userSeq+1 > maxSeq {
			maxSeq = t.userSeq + 1
		}
	}
	out := strings.Join(parts, "\n")
	// 硬上限 4KB：滚动摘要无限增长会重新把窗口吃满，那就失去了压缩的意义。
	if len(out) > 4<<10 {
		out = "…（更早的已合并）\n" + tailBytes(out, 4<<10)
	}
	return out, maxSeq
}

// ruleSummary 生成一轮的规则摘要（≤600 字，不调模型，不可能编造）。
//
// 输入全部来自 finalize 时刻的内存轨迹：需求原文、工具名与成败、问答、结论、终态。
// 不用 LLM 摘要的理由：额外花钱、需要异步任务与失败降级、还可能把没发生过的事写进去。
// 代价是摘要里没有模型的推理过程 —— 结论与工具清单已经足够下一轮接着做。
func ruleSummary(userText string, trace []traceItem, content, status, code string) string {
	type stat struct{ ok, fail int }
	tools := map[string]*stat{}
	order := make([]string, 0, 8)
	var asks []string
	for _, it := range trace {
		switch it.K {
		case traceTool:
			s, ok := tools[it.N]
			if !ok {
				s = &stat{}
				tools[it.N] = s
				order = append(order, it.N)
			}
			if it.OK != nil && !*it.OK {
				s.fail++
			} else {
				s.ok++
			}
		case traceAsk:
			a := it.ANS
			if a == "" {
				a = "未答"
			}
			asks = append(asks, clipRunes(it.Q, 40)+"→"+clipRunes(a, 40))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "需求：%s", clipRunes(strings.TrimSpace(userText), 120))
	if len(order) > 0 {
		calls := make([]string, 0, len(order))
		for _, n := range order {
			s := tools[n]
			if s.fail > 0 {
				calls = append(calls, fmt.Sprintf("%s×%d（失败%d）", n, s.ok+s.fail, s.fail))
			} else {
				calls = append(calls, fmt.Sprintf("%s×%d", n, s.ok))
			}
		}
		fmt.Fprintf(&b, "｜经过：%s", strings.Join(calls, "、"))
	}
	if len(asks) > 0 {
		fmt.Fprintf(&b, "｜问答：%s", strings.Join(asks, "；"))
	}
	if c := strings.TrimSpace(content); c != "" {
		fmt.Fprintf(&b, "｜结论：%s", clipRunes(c, 300))
	}
	state := status
	if code != "" {
		if state != "" {
			state += "/" + code
		} else {
			state = code
		}
	}
	if state == "" {
		state = "ok"
	}
	fmt.Fprintf(&b, "｜状态：%s", state)
	return clipRunes(b.String(), 600)
}

// isArgFaultContent 判断一条 tool 结果是不是"参数解析失败"（这类调用的原文不再回灌）。
//
// 只看 code 不看文案：文案会改，code 是契约。
func isArgFaultContent(content string) bool {
	if content == "" {
		return false
	}
	for _, code := range []string{argCodeInvalid, argCodeMissing, argCodeTruncate} {
		if strings.Contains(content, `"code":"`+code+`"`) {
			return true
		}
	}
	return false
}
