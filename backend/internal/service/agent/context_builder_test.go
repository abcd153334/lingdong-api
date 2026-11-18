package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// seedTurn 写一轮对话（user + assistant），assistant 可带 ctx 与摘要。
func seedTurn(t *testing.T, gdb *gorm.DB, sessID uint64, userText, asstText, status, summary string, ctx model.JSON) int64 {
	t.Helper()
	um, err := AppendMessage(context.Background(), sessID, "user", userText, nil, MessageMeta{})
	if err != nil {
		t.Fatalf("写用户消息失败: %v", err)
	}
	if _, err := AppendMessage(context.Background(), sessID, "assistant", asstText, nil, MessageMeta{
		Status: status, Summary: summary, Ctx: ctx,
	}); err != nil {
		t.Fatalf("写助手消息失败: %v", err)
	}
	return um.Seq
}

// toolCtx 造一段 ctx：一次 assistant(tool_calls) + 两条 tool 结果。
func toolCtx(t *testing.T, result string) model.JSON {
	t.Helper()
	msgs := []Message{
		{
			Role: "assistant",
			ToolCalls: []wireToolCall{
				{ID: "c1", Type: "function", Function: wireFunctionCall{Name: "docs_read", Arguments: `{"doc_id":1}`}},
				{ID: "c2", Type: "function", Function: wireFunctionCall{Name: "schema_get", Arguments: `{"section":"all"}`}},
			},
		},
		{Role: "tool", ToolCallID: "c1", Name: "docs_read", Content: result},
		{Role: "tool", ToolCallID: "c2", Name: "schema_get", Content: `{"ok":true}`},
	}
	buf, err := json.Marshal(map[string]any{"messages": msgs})
	if err != nil {
		t.Fatalf("造 ctx 失败: %v", err)
	}
	return model.JSON(buf)
}

// newSession 建一个空会话，返回 id。
func newSession(t *testing.T, gdb *gorm.DB, title string) uint64 {
	t.Helper()
	row := model.AgentSession{Title: title, Status: 1}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	return row.ID
}

// rolesOf 抽出消息的角色序列（断言形状用）。
func rolesOf(msgs []Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

// assertToolPairs 校验：每条 tool 消息都能找到前一条声明了它的 assistant(tool_calls)。
//
// 这是上游最容易 400 的一处：`tool must be a response to a preceding message with tool_calls`。
// 装配与轮内压缩都必须保住这个不变量。
func assertToolPairs(t *testing.T, msgs []Message) {
	t.Helper()
	open := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case "tool":
			if !open[m.ToolCallID] {
				t.Fatalf("第 %d 条 tool 消息(role=%s, id=%s) 找不到来源 assistant(tool_calls)；角色序列=%v",
					i, m.Role, m.ToolCallID, rolesOf(msgs))
			}
		}
	}
}

// TestBuildContextGolden3Turns 是上下文装配的黄金用例。
//
// 三轮：① 正常轮（2 次工具调用）② 只有工具、content 为空（max_steps 截断）
// ③ 问答轮（ask_user 的结果在 ctx 里）。断言：
//   - system 在首位、当前用户输入在末位；
//   - tool 与 tool_calls 严格成对；
//   - 空结论的轮次显式标注（否则下一轮会把"上次没做事"当事实）；
//   - 问答只出现一次（既读 agent_asks 又回灌 ctx 就会出现两遍）。
func TestBuildContextGolden3Turns(t *testing.T) {
	gdb := withAgentDB(t)
	sessID := newSession(t, gdb, "黄金用例")

	seedTurn(t, gdb, sessID, "帮我把可灵接进来", "好的，我先读文档。", "ok",
		"需求：接入可灵｜经过：docs_read×1、schema_get×1｜结论：已了解端点与鉴权", toolCtx(t, "文档正文：POST /v1/videos"))
	seedTurn(t, gdb, sessID, "继续", "", "ok", "需求：继续｜经过：docs_read×2｜状态：max_steps", toolCtx(t, "第二段文档"))
	cur := seedTurn(t, gdb, sessID, "音色怎么填", "用 voice 字段。", "ok", "需求：音色｜结论：用 voice", nil)

	// 当前这一轮的用户消息（assistant 尚未落库，模拟"正在跑"）。
	um, err := AppendMessage(context.Background(), sessID, "user", "那参考音频呢", nil, MessageMeta{})
	if err != nil {
		t.Fatalf("写当前消息失败: %v", err)
	}
	_ = cur

	plan, err := BuildContext(context.Background(), BuildInput{
		SessionID: sessID, MessageID: um.ID, UserSeq: um.Seq, UserText: um.Content,
	}, Limits{ContextWindow: 2_000_000, MaxOutputTokens: 8192})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	msgs := plan.Messages
	if len(msgs) < 4 {
		t.Fatalf("消息太少：%v", rolesOf(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("首条必须是 system，实际 %s", msgs[0].Role)
	}
	if last := msgs[len(msgs)-1]; last.Role != "user" || last.Content != "那参考音频呢" {
		t.Fatalf("末条必须是当前用户输入，实际 %+v", last)
	}
	assertToolPairs(t, msgs)

	joined := ""
	for _, m := range msgs {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "上一轮调用了工具但没有给出结论") {
		t.Fatalf("空结论轮必须显式标注: %s", joined)
	}
	if n := strings.Count(joined, "文档正文：POST /v1/videos"); n != 1 {
		t.Fatalf("工具结果应只出现一次，实际 %d 次", n)
	}
	if plan.KeptTurns != 3 {
		t.Fatalf("默认 2M 窗口下三轮都应全保真，实际 %d（摘要 %d / 丢弃 %d）",
			plan.KeptTurns, plan.SummarizedTurns, plan.DroppedTurns)
	}
	if plan.Level != 0 {
		t.Fatalf("没触发任何降级，level 应为 0，实际 %d", plan.Level)
	}
}

// TestBuildContextSeqUpperBound 锁死"历史只取 seq < 当前"。
//
// 锁在 Redis 未就绪时会直接放行，并发写进来的消息必须被物理隔离掉。
func TestBuildContextSeqUpperBound(t *testing.T) {
	gdb := withAgentDB(t)
	sessID := newSession(t, gdb, "并发隔离")
	seedTurn(t, gdb, sessID, "第一轮", "答复一", "ok", "", nil)
	curSeq := seedTurn(t, gdb, sessID, "第二轮", "答复二", "ok", "", nil)
	// 模拟"别人在我之后写进来的消息"。
	if _, err := AppendMessage(context.Background(), sessID, "user", "并发插入的干扰消息", nil, MessageMeta{}); err != nil {
		t.Fatalf("写干扰消息失败: %v", err)
	}

	plan, err := BuildContext(context.Background(), BuildInput{
		SessionID: sessID, UserSeq: curSeq, UserText: "第二轮",
	}, Limits{ContextWindow: 2_000_000, MaxOutputTokens: 8192})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	for _, m := range plan.Messages {
		if strings.Contains(m.Content, "并发插入的干扰消息") {
			t.Fatalf("seq >= 当前消息的历史不得进入上下文: %+v", plan.Messages)
		}
	}
}

// TestBuildContextSummarizesWhenTight 锁死 L1：窗口紧时旧轮退成摘要（保留结论）。
func TestBuildContextSummarizesWhenTight(t *testing.T) {
	gdb := withAgentDB(t)
	sessID := newSession(t, gdb, "紧窗口")
	big := strings.Repeat("文档正文片段。", 1200) // ≈12KB（≈5.6k token）
	for i := 0; i < 6; i++ {
		seedTurn(t, gdb, sessID, "第"+string(rune('一'+i))+"轮需求", "第"+string(rune('一'+i))+"轮结论",
			"ok", "需求：第"+string(rune('一'+i))+"轮｜结论：结论"+string(rune('一'+i)), toolCtx(t, big))
	}
	um, err := AppendMessage(context.Background(), sessID, "user", "继续", nil, MessageMeta{})
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	plan, err := BuildContext(context.Background(), BuildInput{
		SessionID: sessID, UserSeq: um.Seq, UserText: "继续",
		// 24k 窗口：扣掉固定开销（≈7.3k）与输出（8k）后历史只剩 ≈8.5k，
		// 而 6 轮各带一条被截到 8KB 的工具结果（≈1.8k token/轮）装不下。
	}, Limits{ContextWindow: 24_000, MaxOutputTokens: 8192})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if plan.Level == 0 {
		t.Fatalf("窗口 24k 装不下 6 轮带工具原文的历史，必须降级，实际 level=0（fixed=%d tokens=%d kept=%d）",
			plan.Fixed, plan.Tokens, plan.KeptTurns)
	}
	if plan.SummarizedTurns == 0 && plan.DroppedTurns == 0 {
		t.Fatalf("降级必须体现为摘要或丢弃，实际 kept=%d", plan.KeptTurns)
	}
	assertToolPairs(t, plan.Messages)
	// 摘要形态必须是"明确标注的历史摘要"，不能伪装成用户发言。
	hasSummary := false
	for _, m := range plan.Messages {
		if strings.HasPrefix(m.Content, "[历史轮次摘要]") {
			hasSummary = true
		}
		if m.Role == "user" && strings.Contains(m.Content, "历史轮次摘要") {
			t.Fatalf("摘要不得伪造为用户消息: %+v", m)
		}
	}
	if plan.SummarizedTurns > 0 && !hasSummary && plan.DroppedTurns == 0 {
		t.Fatalf("有摘要轮却找不到摘要消息")
	}
}

// TestBuildContextWindowTooSmall 锁死"窗口装不下固定开销"时的可操作报错。
func TestBuildContextWindowTooSmall(t *testing.T) {
	gdb := withAgentDB(t)
	sessID := newSession(t, gdb, "小窗口")
	um, err := AppendMessage(context.Background(), sessID, "user", "你好", nil, MessageMeta{})
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	_, err = BuildContext(context.Background(), BuildInput{
		SessionID: sessID, UserSeq: um.Seq, UserText: "你好",
	}, Limits{ContextWindow: 12_000, MaxOutputTokens: 8_192})
	if err == nil {
		t.Fatal("窗口 12k 必须报错（固定开销 + 输出已经超了）")
	}
	ce, ok := err.(*ContextError)
	if !ok || ce.Code != CodeContextWindowTooSmall {
		t.Fatalf("错误码应为 %s，实际 %T %v", CodeContextWindowTooSmall, err, err)
	}
	// 文案必须给出数字与"该改哪里"，否则运营只能猜。
	if !strings.Contains(ce.Message, "context_window") {
		t.Fatalf("文案要指向具体设置项: %s", ce.Message)
	}
}

// TestFitMessagesDropsWholeGroup 锁死轮内压缩的配对不变量。
//
// 只删 tool 会让上游 400；只删 assistant 会留下无来源的 tool。
func TestFitMessagesDropsWholeGroup(t *testing.T) {
	mk := func() []Message {
		return []Message{
			{Role: "system", Content: strings.Repeat("系统提示", 400)},
			{Role: "user", Content: "读文档"},
			{
				Role: "assistant",
				ToolCalls: []wireToolCall{
					{ID: "a1", Type: "function", Function: wireFunctionCall{Name: "docs_read", Arguments: "{}"}},
				},
			},
			{Role: "tool", ToolCallID: "a1", Name: "docs_read", Content: strings.Repeat("旧结果", 5000)},
			{
				Role: "assistant",
				ToolCalls: []wireToolCall{
					{ID: "a2", Type: "function", Function: wireFunctionCall{Name: "docs_read", Arguments: `{"offset":6000}`}},
				},
			},
			{Role: "tool", ToolCallID: "a2", Name: "docs_read", Content: strings.Repeat("新结果", 100)},
		}
	}
	window := 10_000
	out, level, ok := FitMessages(mk(), nil, window, 2048)
	if !ok {
		t.Fatalf("压缩后应装得下")
	}
	if level == 0 {
		t.Fatalf("明显超窗时必须压缩（level>0）")
	}
	assertToolPairs(t, out)
	// 最后一组必须是原文：模型正要基于它继续推理。
	last := out[len(out)-1]
	if last.Role != "tool" || last.Content == truncatedToolPlaceholder {
		t.Fatalf("最后一组必须保留原文，实际 %+v", last)
	}
	// system 与当前 user 不能被删。
	if out[0].Role != "system" || out[1].Role != "user" {
		t.Fatalf("压缩不得删 system/user: %v", rolesOf(out))
	}
}

// TestEncodeCtxExcludesText 锁死 ctx 只存"工具那一段"。
//
// user 原文与最终结论已在 agent_messages.content 里，再存一份就是两个副本。
func TestEncodeCtxExcludesText(t *testing.T) {
	extra := []Message{
		{Role: "assistant", Content: "我先查一下", ToolCalls: []wireToolCall{
			{ID: "c1", Type: "function", Function: wireFunctionCall{Name: "docs_read", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "c1", Name: "docs_read", Content: `{"ok":true}`},
		{Role: "assistant", Content: "查到了，结论是 X"}, // 最终正文：不入 ctx
	}
	raw := encodeCtx(extra)
	if len(raw) == 0 {
		t.Fatal("有工具调用时 ctx 不应为空")
	}
	var doc struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("ctx 不是合法 JSON: %v", err)
	}
	if len(doc.Messages) != 2 {
		t.Fatalf("ctx 只应含 tool_calls 与 tool 两条，实际 %d", len(doc.Messages))
	}
	if strings.Contains(string(raw), "查到了，结论是 X") {
		t.Fatalf("最终正文不得重复存进 ctx: %s", string(raw))
	}
	// 没有工具调用的一轮：ctx 必须为空（否则列里全是空壳 JSON）。
	if got := encodeCtx([]Message{{Role: "assistant", Content: "纯文本答复"}}); len(got) != 0 {
		t.Fatalf("无工具调用时 ctx 应为空，实际 %s", string(got))
	}
}

// TestRuleSummaryNoFabrication 锁死摘要"只复述发生过的事"。
func TestRuleSummaryNoFabrication(t *testing.T) {
	okTrue := true
	okFalse := false
	trace := []traceItem{
		{K: traceText, T: "我先读文档"},
		{K: traceTool, N: "docs_read", OK: &okTrue, A: "{}", B: "..."},
		{K: traceTool, N: "docs_read", OK: &okFalse, A: "{}", B: "..."},
		{K: traceAsk, Q: "用哪个分组？", ANS: "默认分组", ST: AskAnswered},
		{K: traceProposal, ID: 7, T: "接入可灵"},
	}
	got := ruleSummary("帮我把可灵接进来", trace, "已生成提案，等待确认。", "ok", "")

	for _, want := range []string{"帮我把可灵接进来", "docs_read×2", "失败1", "用哪个分组", "默认分组", "已生成提案", "状态：ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("摘要缺少 %q：%s", want, got)
		}
	}
	// 不能凭空出现没发生过的实体名（这类幻觉会被下一轮当成事实）。
	for _, bad := range []string{"openai", "anthropic", "已生效", "已应用"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Fatalf("摘要出现了未发生的事实 %q：%s", bad, got)
		}
	}
	if len([]rune(got)) > 600 {
		t.Fatalf("摘要超过 600 字上限：%d", len([]rune(got)))
	}
}

// TestMergeRollingKeepsCoverage 锁死滚动摘要的边界与覆盖点。
func TestMergeRollingKeepsCoverage(t *testing.T) {
	dropped := []*turn{
		{userSeq: 10, userText: "第一件事", asstText: "结论一", summary: "需求：第一件事｜结论：结论一"},
		{userSeq: 20, userText: "第二件事", asstText: "结论二", summary: ""},
	}
	out, upto := mergeRolling("旧摘要", dropped, 5)
	if upto != 21 {
		t.Fatalf("覆盖点应推进到最新一轮的 assistant seq（21），实际 %d", upto)
	}
	if !strings.Contains(out, "旧摘要") || !strings.Contains(out, "第一件事") || !strings.Contains(out, "结论二") {
		t.Fatalf("旧摘要与两轮内容都要在: %s", out)
	}
	// 超长必须被截断（否则压缩失去意义）。
	prev := strings.Repeat("很长的历史摘要", 1000)
	capped, _ := mergeRolling(prev, dropped, 0)
	if len(capped) > 4<<10+64 {
		t.Fatalf("滚动摘要必须封顶 4KB，实际 %d 字节", len(capped))
	}
}

// TestBuildContextCompactsOnce 锁死滚动摘要的两条不变量：
//  1. `compacted_upto_seq` 单调不减（只有覆盖得更远才写回，CAS 保证不回退）；
//  2. **同一轮只进摘要一次** —— 不筛 `compacted_upto_seq` 时，每轮装配都会把
//     同一批旧轮重新并进去，摘要里出现 N 份重复的同一件事，直到撞 4KB 上限。
//     模型看到重复条目会以为被反复要求过。
func TestBuildContextCompactsOnce(t *testing.T) {
	gdb := withAgentDB(t)
	sessID := newSession(t, gdb, "去重")
	// 80 轮、每轮摘要 ≈300 字：远超 24k 窗口的历史预算，于是会一路丢到 L2。
	summary := "需求：" + strings.Repeat("很早以前的一次接入", 25)
	for i := 0; i < 80; i++ {
		// 每轮一个唯一标记，才能判断"有没有被写进摘要两次"。
		seedTurn(t, gdb, sessID, fmt.Sprintf("轮次-%03d", i), "结论", "ok", summary, nil)
	}
	lim := Limits{ContextWindow: 24_000, MaxOutputTokens: 8192}
	call := func(text string) model.AgentSession {
		um, err := AppendMessage(context.Background(), sessID, "user", text, nil, MessageMeta{})
		if err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
		if _, err := BuildContext(context.Background(), BuildInput{
			SessionID: sessID, UserSeq: um.Seq, UserText: text,
		}, lim); err != nil {
			t.Fatalf("装配失败: %v", err)
		}
		var sess model.AgentSession
		if err := gdb.First(&sess, sessID).Error; err != nil {
			t.Fatalf("读会话失败: %v", err)
		}
		return sess
	}
	first := call("继续一")
	if first.CompactedUptoSeq == 0 || first.RollingSummary == "" {
		t.Fatalf("发生过丢弃后滚动摘要与覆盖点都必须推进（upto=%d len=%d）",
			first.CompactedUptoSeq, len(first.RollingSummary))
	}
	second := call("继续二")
	if second.CompactedUptoSeq < first.CompactedUptoSeq {
		t.Fatalf("覆盖点只能前进：%d → %d", first.CompactedUptoSeq, second.CompactedUptoSeq)
	}
	// 唯一标记在摘要里最多出现一次 = 没有被重复并入。
	for i := 0; i < 80; i++ {
		mark := fmt.Sprintf("轮次-%03d", i)
		n := strings.Count(second.RollingSummary, mark)
		if n > 1 {
			t.Fatalf("%s 在滚动摘要里出现 %d 次（同一轮被重复并入）", mark, n)
		}
	}
}
