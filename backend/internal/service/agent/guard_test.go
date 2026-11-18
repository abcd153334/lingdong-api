package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件守"失败分级"：参数写坏 ≠ 这条路走不通。
//
// 线上那次故障的形态：validate_draft 连错三次、每次只差一层括号，
// 第 3 次撞上 agent_loop_guard 整轮中止，运营看到"工具连续失败"，
// 于是换问法重来 —— 又写出一份同样大小的坏 JSON。

// toolMessageContents 取本轮回灌给模型的 tool 消息正文（按 seq 顺序）。
func toolMessageContents(t *testing.T, m model.AgentMessage) []string {
	t.Helper()
	if len(m.Ctx) == 0 {
		return nil
	}
	// ctx 的实际形状是 {"messages":[...]}（encodeCtx 的唯一输出）。
	var doc struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(m.Ctx, &doc); err != nil {
		t.Fatalf("ctx 解析失败: %v", err)
	}
	out := make([]string, 0, len(doc.Messages))
	for _, x := range doc.Messages {
		if x.Role == "tool" {
			out = append(out, x.Content)
		}
	}
	return out
}

// badArgs 生成第 i 份"语法写坏但内容不同"的参数。
//
// 必须各不相同：相同参数会在第 3 次被 repeated_call 拦下，测到的就不是
// "参数错"而是"防重复"（那条路径另有用例）。
func badArgs(i int) string {
	return `{"key":` + strings.Repeat(" ", i+1) + `}`
}

// TestGuardArgFaultTwiceKeepsRunning 守"语法错两次不中止，且第二次给出下一步"。
func TestGuardArgFaultTwiceKeepsRunning(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name: "agent-fake-arg2",
		script: [][]string{
			{toolCallFrame("c1", "doc_node_get", badArgs(0))},
			{toolCallFrame("c2", "doc_node_get", badArgs(1))},
			{textFrame("参数写坏了，我分块重写")},
		},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "读一下这份配置")

	c := runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	if it := errorItemOf(partsItems(t, m)); it != nil {
		t.Fatalf("语法错两次不该中止整轮，实际报了 %s", it.C)
	}
	if done := c.lastOf("done"); done == nil || done.Partial {
		t.Fatalf("这一轮应正常结束（非 partial）: %+v", done)
	}

	// 第一次不该带 advice（还不知道它改不改得对），第二次必须带。
	ctxs := toolMessageContents(t, m)
	if len(ctxs) < 2 {
		t.Fatalf("应有两次工具回执，实际 %d", len(ctxs))
	}
	if strings.Contains(ctxs[0], `"advice"`) {
		t.Fatalf("第一次失败就注入指令是噪音: %s", ctxs[0])
	}
	if !strings.Contains(ctxs[1], `"advice"`) {
		t.Fatalf("第二次失败必须注入分块指令: %s", ctxs[1])
	}
	if !strings.Contains(ctxs[1], "shape") {
		t.Fatalf("注入的指令应指向 mode=shape 分块: %s", ctxs[1])
	}
}

// TestGuardArgFaultFourTimesAborts 守"写坏第 4 次才停，且可重试"。
func TestGuardArgFaultFourTimesAborts(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name: "agent-fake-arg4",
		script: [][]string{
			{toolCallFrame("c1", "doc_node_get", badArgs(0))},
			{toolCallFrame("c2", "doc_node_get", badArgs(1))},
			{toolCallFrame("c3", "doc_node_get", badArgs(2))},
			{toolCallFrame("c4", "doc_node_get", badArgs(3))},
		},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "读一下这份配置")

	runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	it := errorItemOf(partsItems(t, m))
	if it == nil || it.C != "agent_tool_args_invalid" {
		t.Fatalf("写坏 4 次应报 agent_tool_args_invalid，实际 %+v", it)
	}
	if !it.R {
		t.Fatal("参数类失败改问法能救回来，必须 retryable=true")
	}
	if !strings.Contains(it.T, "分块") {
		t.Fatalf("中止文案必须给出下一步（分块提交）: %s", it.T)
	}
}

// TestGuardRepeatedCallDoesNotTripLoopGuard 守"防重复不算失败"。
//
// 早前 repeated_call 也返回 ok:false 并计入 FailStreak：同一调用被拒两次
// 就把该工具推到 3，整轮中止 —— 而它一次都没真的执行过。
func TestGuardRepeatedCallDoesNotTripLoopGuard(t *testing.T) {
	gdb := withAgentDB(t)
	same := toolCallFrame("c", "schema_get", `{"section":"plan","entity":"model"}`)
	fake := &fakeStreamAdapter{
		name:   "agent-fake-repeat",
		script: [][]string{{same}, {same}, {same}, {same}, {textFrame("好的")}},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "看看有哪些文档")

	runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	if it := errorItemOf(partsItems(t, m)); it != nil {
		t.Fatalf("重复调用不该触发守卫，实际报了 %s：%s", it.C, it.T)
	}
	var repeated int
	for _, c := range toolMessageContents(t, m) {
		if strings.Contains(c, `"repeated_call"`) {
			repeated++
		}
	}
	if repeated < 2 {
		t.Fatalf("应至少有两次被防重复拦下（否则用例没测到目标路径），实际 %d", repeated)
	}
}

// TestGuardSuccessResetsBoth 守"成功一次就清零"。
//
// 不清零的后果：一轮里零散失败三次（中间都成功过）也会中止，
// 而那三次失败说明的是"模型偶尔填错"，不是"这条路不通"。
func TestGuardSuccessResetsBoth(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name: "agent-fake-reset",
		// 成功必须发生在**同一个工具**上：别的工具成功不该清零它的计数
		// （否则"换个工具试一下"就能把死路洗白）。seedChatChannel 会建 fake-chat。
		script: [][]string{
			{toolCallFrame("c1", "model_get", `{"id":"不存在1"}`)},
			{toolCallFrame("c2", "model_get", `{"id":"不存在2"}`)},
			{toolCallFrame("c3", "model_get", `{"id":"fake-chat"}`)},
			{toolCallFrame("c4", "model_get", `{"id":"不存在3"}`)},
			{toolCallFrame("c5", "model_get", `{"id":"不存在4"}`)},
			{textFrame("没找到")},
		},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "找一个不存在的模型")

	runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	if it := errorItemOf(partsItems(t, m)); it != nil {
		t.Fatalf("中途成功过就不该累计到 3 次而中止，实际 %s", it.C)
	}
}

// TestGuardBusinessFailureNamesTool 守"业务失败仍按 3 次中止，并点名工具"。
//
// 只给一句"工具连续失败"的话，运营无法判断是哪个环节、该换什么问法。
func TestGuardBusinessFailureNamesTool(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name: "agent-fake-biz",
		script: [][]string{
			{toolCallFrame("c1", "doc_node_get", `{"key":"a1"}`)},
			{toolCallFrame("c2", "doc_node_get", `{"key":"a2"}`)},
			{toolCallFrame("c3", "doc_node_get", `{"key":"a3"}`)},
		},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "读文档")

	runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	it := errorItemOf(partsItems(t, m))
	if it == nil || it.C != "agent_loop_guard" {
		t.Fatalf("业务失败 3 次应报 agent_loop_guard，实际 %+v", it)
	}
	if !strings.Contains(it.T, "doc_node_get") {
		t.Fatalf("文案必须点名工具（运营据此换问法），实际 %q", it.T)
	}
	if it.R {
		t.Fatal("loop_guard 不该可重试：重试=重发同一条消息，会再烧一遍同一条死路")
	}
}

// TestGuardRepeatTripsToStopSpinning 守"卡住要停，但不许当成失败".
//
// 这是补上 Repeat 计数器的直接原因：真实日志里出现过 12 步全是 repeated_call
// （模型原样再要同一个结果），既没有失败也没有产出，一路烧到步数耗尽。
// 所以"被防重复拦下"不计失败，但连续三次就中止。
func TestGuardRepeatTripsToStopSpinning(t *testing.T) {
	gdb := withAgentDB(t)
	same := toolCallFrame("c", "schema_get", `{"section":"plan","entity":"model"}`)
	fake := &fakeStreamAdapter{
		name:   "agent-fake-stuck",
		script: [][]string{{same}, {same}, {same}, {same}, {same}},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "再读一次字段表")

	runOnce(t, sess, msg)

	m := lastAssistantMsg(t, gdb, sess.ID)
	it := errorItemOf(partsItems(t, m))
	if it == nil || it.C != "agent_loop_guard" {
		t.Fatalf("重复同一调用 3 次应中止（否则空转到步数耗尽），实际 %+v", it)
	}
	if !strings.Contains(it.T, "重复") {
		t.Fatalf("文案要区分卡住与失败（处置完全不同），实际 %q", it.T)
	}
}
