package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/channel"
)

// 本文件覆盖"一轮编排"的骨架：LLM 流式解析 → 工具循环 → 事件序列 → 成本记账。
//
// 用假适配器而不是假 HTTP 上游：要验的是**编排逻辑**（分片聚合、循环退出、
// 事件顺序、记账），不是 relay 的 HTTP 细节 —— 后者已经有自己的测试。
// 假适配器经 relay.RegisterAdapter 真注册，走的选路/密钥/配置链路与生产一致。

// fakeStreamAdapter 是按脚本吐帧的适配器。
type fakeStreamAdapter struct {
	name string
	// script 是每次 StreamChat 调用的帧脚本（切片长度 = 调用次数）。
	script [][]string
	calls  int
	usage  relay.UsageInfo
}

func (f *fakeStreamAdapter) Name() string   { return f.name }
func (f *fakeStreamAdapter) Caps() []string { return []string{"text"} }
func (f *fakeStreamAdapter) Test(context.Context, *relay.ChannelConfig) error {
	return nil
}
func (f *fakeStreamAdapter) Chat(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("fake: 不使用非流式")
}
func (f *fakeStreamAdapter) Embeddings(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("fake: 不支持 embeddings")
}
func (f *fakeStreamAdapter) Completions(context.Context, *relay.ChannelConfig, *relay.RelayRequest) (*relay.RelayResponse, error) {
	return nil, errors.New("fake: 不支持 completions")
}
func (f *fakeStreamAdapter) Forward(context.Context, *relay.ChannelConfig, *relay.RelayRequest, string) (*relay.RawResponse, error) {
	return nil, errors.New("fake: 不支持透传")
}

func (f *fakeStreamAdapter) StreamChat(_ context.Context, _ *relay.ChannelConfig,
	_ *relay.RelayRequest, onFrame func(relay.SSEFrame) error) (*relay.StreamResult, error) {

	idx := f.calls
	f.calls++
	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	frames := f.script[idx]
	for _, fr := range frames {
		if err := onFrame(relay.SSEFrame{Data: []byte(fr)}); err != nil {
			return nil, err
		}
	}
	return &relay.StreamResult{StatusCode: 200, Usage: f.usage, Frames: len(frames)}, nil
}

// seedChatChannel 建"模型 + 绑定 + 渠道"，让选路真的能选出东西。
func seedChatChannel(t *testing.T, gdb *gorm.DB, adapterID string) {
	t.Helper()
	keys, err := channel.EncryptKeys([]string{"sk-test"})
	if err != nil {
		t.Fatalf("加密密钥失败: %v", err)
	}
	groups, err := model.NewJSON([]string{"default"})
	if err != nil {
		t.Fatalf("构造分组失败: %v", err)
	}
	ch := model.Channel{
		Name: "假渠道", AdapterID: adapterID, BaseURL: "https://fake.example",
		Keys: keys, Groups: groups, Status: model.ChannelStatusEnabled,
		MultiKeyMode: channel.ModeRandom, Weight: 1, Priority: 1,
	}
	if err := gdb.Create(&ch).Error; err != nil {
		t.Fatalf("插入渠道失败: %v", err)
	}
	inPrice := int64(500000)
	m := model.Model{
		ID: "fake-chat", DisplayName: "假对话", Vendor: "relayco",
		Modality: "chat", BillingMode: model.BillingModeTokens, Enabled: true,
		Prices: model.Prices{Input: &inPrice},
	}
	if err := gdb.Create(&m).Error; err != nil {
		t.Fatalf("插入模型失败: %v", err)
	}
	b := model.ModelBinding{
		ModelID: "fake-chat", ChannelID: ch.ID, UpstreamModel: "fake-upstream",
		Enabled: true, Weight: 1, Priority: 1,
	}
	if err := gdb.Create(&b).Error; err != nil {
		t.Fatalf("插入绑定失败: %v", err)
	}
}

func TestToolAggregatorMergesByIndex(t *testing.T) {
	agg := newToolAggregator()
	// 两帧拼一次调用 + 另一帧是同一轮里的第二次同名调用（换 index）。
	for _, frame := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"schema_get","arguments":"{\"sec"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"tion\":\"all\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"schema_get","arguments":"{}"}}]}}]}`,
	} {
		if _, _, err := agg.feed([]byte(frame)); err != nil {
			t.Fatalf("解析帧失败: %v", err)
		}
	}
	calls := agg.result()
	if len(calls) != 2 {
		t.Fatalf("应按 index 得到 2 次调用，实际 %d：%+v", len(calls), calls)
	}
	// 按 index 而不是按 name 聚合：同名两次调用的参数顺序不能串。
	if calls[0].ID != "call_a" || calls[0].Arguments != `{"section":"all"}` {
		t.Fatalf("第一次调用合并错误：%+v", calls[0])
	}
	if calls[1].ID != "call_b" || calls[1].Arguments != "{}" {
		t.Fatalf("第二次调用合并错误：%+v", calls[1])
	}
}

func TestToolAggregatorHandlesNonOpenAIFrame(t *testing.T) {
	agg := newToolAggregator()
	// 透传型上游可能吐非 OpenAI 形状：必须返回错误让调用方忽略这一帧，而不是 panic。
	if _, _, err := agg.feed([]byte(`{"foo":"bar"}`)); err != nil {
		t.Fatalf("结构不同但合法 JSON 不应报错: %v", err)
	}
	if _, _, err := agg.feed([]byte(`not json`)); err == nil {
		t.Fatal("非 JSON 帧应报错（由调用方忽略）")
	}
	if len(agg.result()) != 0 {
		t.Fatal("没有 tool_calls 时结果应为空")
	}
}

func TestRunToolLoopAndAccounting(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name: "agent-fake",
		script: [][]string{
			// 第 1 步：模型先查平台 schema（一次工具调用，参数分两帧到达）。
			{
				`{"choices":[{"delta":{"content":"我先查一下平台支持的端点。"}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"schema_get","arguments":"{\"section\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"endpoints\"}"}}]}}]}`,
			},
			// 第 2 步：给出最终答复（无工具调用 → 循环结束）。
			{`{"choices":[{"delta":{"content":"平台支持 chat/completions。"}}]}`},
		},
		usage: relay.UsageInfo{Usage: billing.Usage{PromptTokens: 120, CompletionTokens: 30}},
	}
	if err := relay.RegisterAdapter(fake); err != nil {
		t.Fatalf("注册假适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.ForgetAdapter(fake.name) })
	seedChatChannel(t, gdb, fake.name)

	sess, err := CreateSession(context.Background(), "测试会话", "")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	userMsg, err := AppendMessage(context.Background(), sess.ID, "user", "支持哪些端点？", nil, MessageMeta{Status: "ok"})
	if err != nil {
		t.Fatalf("写用户消息失败: %v", err)
	}

	var types []string
	var deltas []string
	var toolCalls, toolResults []Event
	err = Run(context.Background(), RunInput{
		SessionID: sess.ID, MessageID: userMsg.ID, AdminID: 1,
		UserText: "支持哪些端点？", ModelID: "fake-chat",
	}, func(ev Event) error {
		types = append(types, ev.Type)
		switch ev.Type {
		case "delta":
			deltas = append(deltas, ev.Text)
		case "tool_call":
			toolCalls = append(toolCalls, ev)
		case "tool_result":
			toolResults = append(toolResults, ev)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("编排失败: %v", err)
	}

	// step 事件是每步开头一条：前端要靠它把"这步说了什么 + 调了什么"归组。
	want := []string{"start", "step", "delta", "tool_call", "tool_result", "step", "delta", "usage", "done"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("事件顺序不对：\n got %v\nwant %v", types, want)
	}
	if !strings.Contains(strings.Join(deltas, ""), "chat/completions") {
		t.Fatalf("增量文本未累加：%v", deltas)
	}
	if len(toolCalls) != 1 || toolCalls[0].CallID != "call_1" || toolCalls[0].Step != 1 {
		t.Fatalf("tool_call 必须带 call_id 与步号：%+v", toolCalls)
	}
	if len(toolResults) != 1 || toolResults[0].CallID != "call_1" {
		t.Fatalf("tool_result 必须回同一个 call_id（否则前端只能按名字猜）：%+v", toolResults)
	}

	// 两轮都算：120+30 与 120+30（每步一次调用）。
	var run model.AgentRun
	if err := gdb.Order("id DESC").First(&run).Error; err != nil {
		t.Fatalf("读取 run 失败: %v", err)
	}
	if run.Status != "ok" {
		t.Fatalf("run 状态应为 ok，实际 %s（error_code=%s）", run.Status, run.ErrorCode)
	}
	if run.Steps != 2 {
		t.Fatalf("应记录 2 步，实际 %d", run.Steps)
	}
	if run.TokensIn != 240 || run.TokensOut != 60 {
		t.Fatalf("token 记账不对：in=%d out=%d", run.TokensIn, run.TokensOut)
	}
	if run.QuotaEst <= 0 {
		t.Fatalf("有价格时必须算出成本，实际 %d", run.QuotaEst)
	}

	// 助手消息必须落库（含工具轨迹）：断线/事后复盘都靠它。
	var msgs []model.AgentMessage
	if err := gdb.Where("session_id = ? AND role = ?", sess.ID, "assistant").Find(&msgs).Error; err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应有 1 条助手消息，实际 %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "chat/completions") {
		t.Fatalf("助手消息内容不对：%q", msgs[0].Content)
	}
	if !strings.Contains(string(msgs[0].Parts), "schema_get") {
		t.Fatalf("消息轨迹应记录调过的工具：%s", msgs[0].Parts)
	}
	// items 是有序轨迹：前端靠它还原"说了一句 → 调工具 → 再说"。
	// 只存 tools[] 的话刷新后只能得到一串工具名，过程与顺序全丢。
	if !strings.Contains(string(msgs[0].Parts), `"items"`) {
		t.Fatalf("消息轨迹必须是 items 有序段：%s", msgs[0].Parts)
	}
	// 末段正文（= 最终答复）不能进轨迹，否则刷新后正文会出现两遍。
	if strings.Contains(string(msgs[0].Parts), "平台支持 chat/completions") {
		t.Fatalf("最终答复不应重复出现在轨迹里：%s", msgs[0].Parts)
	}
	// 中间叙述要留下（模型在第 1 步说过的话是过程的一部分）。
	if !strings.Contains(string(msgs[0].Parts), "我先查一下平台支持的端点") {
		t.Fatalf("中间叙述应进轨迹：%s", msgs[0].Parts)
	}
}

func TestRunRejectsWhenDisabled(t *testing.T) {
	gdb := withAgentDB(t)
	// 必须用 map 落 enabled=false：该列带 `default:true`，用结构体 Create 时
	// GORM 会把 false 当零值丢掉并回填 true —— 于是"关掉总闸"的用例根本关不掉。
	if err := gdb.Model(&model.AgentSetting{}).Where("id = ?", 1).Updates(map[string]any{
		"enabled": false, "max_steps": 12, "max_tokens_per_run": 32000,
	}).Error; err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}
	var got Event
	err := Run(context.Background(), RunInput{SessionID: 1, MessageID: 1, UserText: "x"},
		func(ev Event) error {
			if ev.Type == "error" {
				got = ev
			}
			return nil
		})
	if err == nil {
		t.Fatal("总闸关闭时必须拒绝")
	}
	if got.Code != "agent_disabled" {
		t.Fatalf("应发 agent_disabled 事件，实际 %q", got.Code)
	}
}

// TestRunSeparatesReasoningFromAnswer 守住"思考不是回答"这条线。
//
// 思维链如果混进 content，会有三个后果：落库的助手消息里多出一整段自问自答、
// 前端折叠块拿不到内容、重发时模型把上一轮的内部推理当成自己说过的话。
func TestRunSeparatesReasoningFromAnswer(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name: "agent-fake-reason",
		script: [][]string{
			{
				`{"choices":[{"delta":{"reasoning_content":"先确认端点，"}}]}`,
				`{"choices":[{"delta":{"reasoning_content":"再看计价格。"}}]}`,
				`{"choices":[{"delta":{"content":"平台支持 chat。"}}]}`,
			},
		},
		usage: relay.UsageInfo{Usage: billing.Usage{PromptTokens: 20, CompletionTokens: 8}},
	}
	if err := relay.RegisterAdapter(fake); err != nil {
		t.Fatalf("注册假适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.ForgetAdapter(fake.name) })
	seedChatChannel(t, gdb, fake.name)

	sess, err := CreateSession(context.Background(), "", "")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	msg, err := AppendMessage(context.Background(), sess.ID, "user", "支持哪些端点？", nil, MessageMeta{Status: "ok"})
	if err != nil {
		t.Fatalf("写用户消息失败: %v", err)
	}

	var types []string
	var reasoning, answer string
	if err := Run(context.Background(), RunInput{
		SessionID: sess.ID, MessageID: msg.ID, AdminID: 1, UserText: "支持哪些端点？",
	}, func(ev Event) error {
		types = append(types, ev.Type)
		switch ev.Type {
		case "reasoning":
			reasoning += ev.Text
		case "delta":
			answer += ev.Text
		}
		return nil
	}); err != nil {
		t.Fatalf("编排失败: %v", err)
	}

	want := []string{"start", "step", "reasoning", "reasoning", "delta", "usage", "done"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("事件顺序不对（思维链必须与正文分流）：\n got %v\nwant %v", types, want)
	}
	if reasoning != "先确认端点，再看计价格。" {
		t.Fatalf("思维链增量未累加：%q", reasoning)
	}
	if strings.TrimSpace(answer) != "平台支持 chat。" {
		t.Fatalf("正文不该掺入思维链：%q", answer)
	}

	var saved model.AgentMessage
	if err := gdb.Where("session_id = ? AND role = ?", sess.ID, "assistant").First(&saved).Error; err != nil {
		t.Fatalf("读助手消息失败: %v", err)
	}
	if strings.Contains(saved.Content, "先确认端点") {
		t.Fatalf("落库正文不应包含思维链：%q", saved.Content)
	}
	// 思维链要进轨迹：前端"默认收起"的那块内容只能来自这里（刷新后也是）。
	// 注意 jsonb 往返后键值间会多一个空格（PG 会规范化），所以只断言值本身。
	if !strings.Contains(string(saved.Parts), `"reason"`) {
		t.Fatalf("轨迹应记录思维链：%s", saved.Parts)
	}
}
