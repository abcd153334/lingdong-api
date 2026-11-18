package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
)

// 本文件是「终态矩阵」用例：一轮编排无论怎么结束（正常/上游错误/上限/守卫/
// 断线/panic），都必须恰好落一条 assistant 消息（含过程与 error 段）、
// finishRun 必达、事件以 done 收尾。此前只有 aborted 一条路径留痕，
// upstream_error / max_steps / loop_guard 的整轮过程刷新即丢。

// errStreamAdapter 是每次 StreamChat 都报错的假适配器（模拟上游故障）。
type errStreamAdapter struct{ name string }

func (f *errStreamAdapter) Name() string   { return f.name }
func (f *errStreamAdapter) Caps() []string { return []string{"text"} }
func (f *errStreamAdapter) Test(context.Context, *relay.ChannelConfig) error {
	return nil
}
func (f *errStreamAdapter) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("fake: 不使用非流式")
}
func (f *errStreamAdapter) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("fake: 不支持 embeddings")
}
func (f *errStreamAdapter) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("fake: 不支持 completions")
}
func (f *errStreamAdapter) Forward(context.Context, *relay.ChannelConfig, *relay.RelayRequest, string) (*relay.RawResponse, error) {
	return nil, errors.New("fake: 不支持透传")
}
func (f *errStreamAdapter) StreamChat(context.Context, *relay.ChannelConfig,
	*relay.RelayRequest, func(relay.SSEFrame) error) (*relay.StreamResult, error) {
	return nil, errors.New("fake upstream: 502 bad gateway")
}

// collector 收集事件供断言。
type collector struct{ evs []Event }

func (c *collector) emit(ev Event) error {
	c.evs = append(c.evs, ev)
	return nil
}

func (c *collector) types() []string {
	out := make([]string, 0, len(c.evs))
	for _, ev := range c.evs {
		out = append(out, ev.Type)
	}
	return out
}

func (c *collector) lastOf(typ string) *Event {
	for i := len(c.evs) - 1; i >= 0; i-- {
		if c.evs[i].Type == typ {
			return &c.evs[i]
		}
	}
	return nil
}

// runOnce 跑一轮并断言事件骨架以 done 收尾。
// 失败轮 Run 会返回 *RunError（既有契约：把原因交给调用方），这里只放行它。
func runOnce(t *testing.T, sess *model.AgentSession, msg *model.AgentMessage) *collector {
	t.Helper()
	c := &collector{}
	err := Run(context.Background(), RunInput{
		SessionID: sess.ID, MessageID: msg.ID, AdminID: 1, UserText: msg.Content,
	}, c.emit)
	var re *RunError
	if err != nil && !errors.As(err, &re) {
		t.Fatalf("Run 只应返回 *RunError（事件已自足）: %v", err)
	}
	if n := len(c.evs); n == 0 || c.evs[n-1].Type != "done" {
		t.Fatalf("事件序列必须以 done 收尾，实际: %v", c.types())
	}
	return c
}

// lastAssistantMsg 取会话最后一条 assistant 消息（不存在则 fatal）。
func lastAssistantMsg(t *testing.T, gdb *gorm.DB, sessionID uint64) model.AgentMessage {
	t.Helper()
	var m model.AgentMessage
	if err := gdb.Where("session_id = ? AND role = ?", sessionID, "assistant").
		Order("seq DESC").First(&m).Error; err != nil {
		t.Fatalf("应有一条 assistant 消息: %v", err)
	}
	return m
}

// partsItems 解析消息的 parts.items。
func partsItems(t *testing.T, m model.AgentMessage) []traceItem {
	t.Helper()
	if len(m.Parts) == 0 {
		return nil
	}
	var p struct {
		Items []traceItem `json:"items"`
	}
	if err := jsonUnmarshal(m.Parts, &p); err != nil {
		t.Fatalf("parts 解析失败: %v", err)
	}
	return p.Items
}

func errorItemOf(items []traceItem) *traceItem {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].K == traceError {
			return &items[i]
		}
	}
	return nil
}

// runStatusOf 取该会话最新 run 的状态（没有 run 返回 ""）。
func runStatusOf(t *testing.T, gdb *gorm.DB, sessionID uint64) string {
	t.Helper()
	var r model.AgentRun
	if err := gdb.Where("session_id = ?", sessionID).Order("id DESC").First(&r).Error; err != nil {
		return ""
	}
	return r.Status
}

func runErrorCodeOf(t *testing.T, gdb *gorm.DB, sessionID uint64) string {
	t.Helper()
	var r model.AgentRun
	if err := gdb.Where("session_id = ?", sessionID).Order("id DESC").First(&r).Error; err != nil {
		return ""
	}
	return r.ErrorCode
}

// jsonUnmarshal 是 model.JSON 的薄封装（避免测试里再 import model.JSON 的方法集）。
func jsonUnmarshal(raw model.JSON, v any) error {
	return json.Unmarshal([]byte(raw), v)
}

func TestFinalizeUpstreamErrorPersistsTrace(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &errStreamAdapter{name: "agent-fake-err"}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "随便说点什么")

	c := runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	if m.Status != "failed" {
		t.Fatalf("消息应 failed，实际 %q", m.Status)
	}
	it := errorItemOf(partsItems(t, m))
	if it == nil {
		t.Fatalf("parts 应含 error 段: %s", string(m.Parts))
	}
	if it.C != "upstream_error" || !it.R {
		t.Fatalf("error 段应为 upstream_error 且可重试: %+v", it)
	}
	ev := c.lastOf("error")
	if ev == nil || ev.Code != "upstream_error" || !ev.Retryable {
		t.Fatalf("应有 error 事件(upstream_error, retryable): %+v", ev)
	}
	if got := runStatusOf(t, gdb, sess.ID); got != "failed" {
		t.Fatalf("agent_runs 应 failed，实际 %q", got)
	}
	if code := runErrorCodeOf(t, gdb, sess.ID); code != "upstream_error" {
		t.Fatalf("agent_runs.error_code 应 upstream_error，实际 %q", code)
	}
}

func TestFinalizeMaxStepsKeepsNarration(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name: "agent-fake-maxsteps",
		script: [][]string{
			// 一步内：先叙述、再调工具 → 步数用尽，末段叙述必须保留（N2）。
			{textFrame("先看一下现有配置。"), toolCallFrame("c1", "vendor_list", `{}`)},
		},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "列出厂商")
	// 必须放在 seedLLMSession 之后：它会重置 max_steps/max_tokens。
	if err := gdb.Table("agent_settings").Where("id = 1").Update("max_steps", 1).Error; err != nil {
		t.Fatalf("改步数失败: %v", err)
	}
	t.Cleanup(func() {
		_ = gdb.Table("agent_settings").Where("id = 1").Update("max_steps", 12).Error
	})

	c := runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	if m.Status != "ok" {
		t.Fatalf("max_steps 消息状态应 ok，实际 %q", m.Status)
	}
	items := partsItems(t, m)
	it := errorItemOf(items)
	if it == nil || it.C != "max_steps" {
		t.Fatalf("parts 应含 max_steps error 段: %+v", items)
	}
	found := false
	for _, seg := range items {
		if seg.K == traceText && strings.Contains(seg.T, "先看一下现有配置") {
			found = true
		}
	}
	if !found {
		t.Fatalf("末段叙述不应被剔除（content 为空时 dropTrailing 必须关闭）: %+v", items)
	}
	done := c.lastOf("done")
	if done == nil || !done.Partial {
		t.Fatalf("max_steps 的 done 必须 partial=true: %+v", done)
	}
}

func TestFinalizeBudgetTooSmall(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{name: "agent-fake-tokens", script: [][]string{{textFrame("你好")}}}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "你好")
	// seedLLMSession 会重置 token 预算，必须在它之后写。
	if err := gdb.Table("agent_settings").Where("id = 1").Update("max_tokens_per_run", 1).Error; err != nil {
		t.Fatalf("改 token 预算失败: %v", err)
	}
	t.Cleanup(func() {
		_ = gdb.Table("agent_settings").Where("id = 1").Update("max_tokens_per_run", model.DefaultAgentMaxTokensPerRun).Error
	})

	c := runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	it := errorItemOf(partsItems(t, m))
	// 第 1 步就超预算 = 预算比"单次请求的固定开销"还小，属于**配置错**，
	// 不是"用完了"。两者必须分开报：前者要人去调设置，后者只需继续追问。
	if it == nil || it.C != "agent_budget_too_small" {
		t.Fatalf("parts 应含 agent_budget_too_small error 段: %s", string(m.Parts))
	}
	if !strings.Contains(it.T, "固定开销") {
		t.Fatalf("文案必须说清是固定开销超了（否则运营只会以为模型坏了）: %s", it.T)
	}
	done := c.lastOf("done")
	if done == nil || !done.Partial {
		t.Fatalf("预算类失败的 done 必须 partial=true（旧实现漏置，前端会当成正常完成）: %+v", done)
	}
}

func TestFinalizeLoopGuardAfterThreeFailures(t *testing.T) {
	gdb := withAgentDB(t)
	// 三次**不同参数**的业务失败：相同参数会先被防重复拦下（那是另一条路径，
	// 见 guard_test.go 的 TestGuardRepeatedCallDoesNotTripLoopGuard）。
	b1 := toolCallFrame("c1", "doc_node_get", `{"key":"不存在的组"}`)
	b2 := toolCallFrame("c2", "doc_node_get", `{"key":"也不存在"}`)
	b3 := toolCallFrame("c3", "doc_node_get", `{"key":"第三个不存在"}`)
	fake := &fakeStreamAdapter{
		name:   "agent-fake-guard",
		script: [][]string{{b1}, {b2}, {b3}},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "读一下配置")

	c := runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	it := errorItemOf(partsItems(t, m))
	if it == nil || it.C != "agent_loop_guard" {
		t.Fatalf("连续失败 3 次应触发 loop_guard error 段: %s", string(m.Parts))
	}
	done := c.lastOf("done")
	if done == nil || !done.Partial {
		t.Fatalf("loop_guard 的 done 必须 partial=true: %+v", done)
	}
}

func TestFinalizePreRunFailurePersistsReason(t *testing.T) {
	gdb := withAgentDB(t)
	sess, msg := seedLLMSession(t, gdb, "你好")
	// seedLLMSession 会把 enabled 置回 true，必须之后关。
	if err := gdb.Table("agent_settings").Where("id = 1").Update("enabled", false).Error; err != nil {
		t.Fatalf("关闭总闸失败: %v", err)
	}
	t.Cleanup(func() {
		_ = gdb.Table("agent_settings").Where("id = 1").Update("enabled", true).Error
	})

	runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	if m.Status != "failed" {
		t.Fatalf("前置失败也应落 failed 消息，实际 %q", m.Status)
	}
	it := errorItemOf(partsItems(t, m))
	if it == nil || it.C != "agent_disabled" {
		t.Fatalf("parts 应含 agent_disabled error 段: %s", string(m.Parts))
	}
	if got := runStatusOf(t, gdb, sess.ID); got != "" {
		t.Fatalf("前置失败不应产生 agent_runs 行，实际 %q", got)
	}
}

func TestFinalizePanicRecoversAndPersists(t *testing.T) {
	gdb := withAgentDB(t)
	toolImpls["panic_test"] = func(_ *toolEnv, _ json.RawMessage) (any, error) {
		panic("boom: 测试用 panic")
	}
	t.Cleanup(func() { delete(toolImpls, "panic_test") })

	fake := &fakeStreamAdapter{
		name: "agent-fake-panic",
		script: [][]string{
			{toolCallFrame("c1", "panic_test", `{}`)},
		},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "触发 panic")

	c := runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	if m.Status != "failed" {
		t.Fatalf("panic 轮应落 failed 消息，实际 %q", m.Status)
	}
	it := errorItemOf(partsItems(t, m))
	if it == nil || it.C != "internal_error" {
		t.Fatalf("parts 应含 internal_error error 段: %s", string(m.Parts))
	}
	if got := runStatusOf(t, gdb, sess.ID); got != "failed" {
		t.Fatalf("agent_runs 应 failed（不能卡 running），实际 %q", got)
	}
	if ev := c.lastOf("error"); ev == nil {
		t.Fatalf("panic 路径也应发 error 事件: %v", c.types())
	}
}

func TestFinalizeDisconnectAbortedNoEmptyMessage(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{name: "agent-fake-cancel", script: [][]string{{textFrame("x")}}}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "你好")

	// 已取消的请求 ctx 模拟客户端断线：run 应 aborted 收尾；什么都没生成时不留空消息。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &collector{}
	if err := Run(ctx, RunInput{
		SessionID: sess.ID, MessageID: msg.ID, AdminID: 1, UserText: msg.Content,
	}, c.emit); err != nil {
		t.Fatalf("断线路径不应返回错误: %v", err)
	}
	done := c.lastOf("done")
	if done == nil || !done.Partial {
		t.Fatalf("断线应以 done(partial=true) 收尾: %+v", done)
	}
	var cnt int64
	gdb.Model(&model.AgentMessage{}).Where("session_id = ? AND role = ?", sess.ID, "assistant").Count(&cnt)
	if cnt != 0 {
		t.Fatalf("无内容无过程的断线不应落空消息，实际 %d 条", cnt)
	}
	if got := runStatusOf(t, gdb, sess.ID); got != "aborted" {
		t.Fatalf("agent_runs 应 aborted，实际 %q", got)
	}
}

func TestMessagesReturnsLatestWindow(t *testing.T) {
	withAgentDB(t)
	sess, err := CreateSession(context.Background(), "", "")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := AppendMessage(context.Background(), sess.ID, "user", strings.Repeat("m", i), nil, MessageMeta{Status: "ok"}); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
	}
	rows, err := Messages(context.Background(), sess.ID, 0, 3)
	if err != nil {
		t.Fatalf("Messages 失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("应取最新 3 条，实际 %d", len(rows))
	}
	// 顺序必须仍是 seq ASC（前端按顺序建轮，不再排序）。
	if rows[0].Seq >= rows[1].Seq || rows[1].Seq >= rows[2].Seq {
		t.Fatalf("窗口内应 ASC：(%d,%d,%d)", rows[0].Seq, rows[1].Seq, rows[2].Seq)
	}
	if rows[2].Content != "mmmmm" {
		t.Fatalf("最后一条应是最新的第 5 条，实际 %q", rows[2].Content)
	}
	// 续传语义不变。
	tail, err := Messages(context.Background(), sess.ID, rows[1].Seq, 10)
	if err != nil {
		t.Fatalf("Messages(afterSeq) 失败: %v", err)
	}
	if len(tail) != 1 || tail[0].Seq != rows[2].Seq {
		t.Fatalf("afterSeq 续传应只返回更新的 1 条: %+v", tail)
	}
}

func TestAppendErrorPartBuildsSegment(t *testing.T) {
	if got := appendErrorPart(nil, "upstream_error", "上游挂了", true); len(got) == 0 {
		t.Fatalf("nil parts 也应产出 error 段")
	}
	var p struct {
		Items []traceItem `json:"items"`
	}
	if err := jsonUnmarshal(appendErrorPart(nil, "upstream_error", "上游挂了", true), &p); err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if len(p.Items) != 1 || p.Items[0].K != traceError || p.Items[0].C != "upstream_error" || !p.Items[0].R {
		t.Fatalf("error 段结构错误: %+v", p.Items)
	}
	// 已有 parts 时追加在尾部，不覆盖原段。
	base := buildParts(&toolEnv{Trace: []traceItem{{K: traceText, T: "hello"}}}, false)
	if err := jsonUnmarshal(appendErrorPart(base, "max_steps", "步数用尽", true), &p); err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if len(p.Items) != 2 || p.Items[0].K != traceText || p.Items[1].K != traceError {
		t.Fatalf("error 段应追加在尾部: %+v", p.Items)
	}
}
