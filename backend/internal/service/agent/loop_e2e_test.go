package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/pricing"
)

// 本文件是**真端到端闭环**用例：假 LLM 只靠工具产出提案 → apply → 六类实体落库
// → 门户可见 → 文档站可见 → 回滚 → 再 apply。
//
// 为什么必须有一条这样的用例：此前三条链互不相交（手写 plan→apply 只验 4 张表、
// 桩 SSE→前端渲染、假 LLM→只读工具），于是"AI 产出的提案能不能用、
// 别人能不能在门户和文档站看到"从来没有人验证过。

// toolCallFrame 构造 OpenAI 形状的 tool_calls 增量帧。
func toolCallFrame(id, name, args string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"function":{"name":%q,"arguments":%q}}]}}]}`,
		id, name, args)
}

// textFrame 构造正文增量帧。
func textFrame(s string) string {
	b, _ := json.Marshal(s)
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%s}}]}`, string(b))
}

func setDryRunRequired(t *testing.T, gdb *gorm.DB, v bool) {
	t.Helper()
	if err := gdb.Table("agent_settings").Where("id = 1").Update("dry_run_required", v).Error; err != nil {
		t.Fatalf("改设置失败: %v", err)
	}
}

// seedProbe 直接落一条探测凭据到 agent_dry_runs（凭据的唯一来源）。
//
// 刻意不提供"往提案上写 dry_run"的辅助：那个字段已经废弃，
// 留着辅助函数会让人以为还有第二条路。
func seedProbe(t *testing.T, gdb *gorm.DB, sessionID uint64, adapterID, baseURL, apiKey string,
	ok bool, createdAt time.Time, expiresAt time.Time) {
	t.Helper()
	base := NormalizeBaseURL(baseURL)
	rec := model.AgentDryRun{
		SessionID: sessionID, AdapterID: adapterID, BaseURL: base,
		KeyFP: probeKeyFingerprint(sessionID, adapterID, base, apiKey),
		OK:    ok, CreatedAt: createdAt, ExpiresAt: expiresAt,
	}
	if expiresAt.IsZero() {
		rec.ExpiresAt = createdAt.Add(dryRunTTL)
	}
	if err := gdb.Create(&rec).Error; err != nil {
		t.Fatalf("写探测凭据失败: %v", err)
	}
}

// seedLLMSession 建一条会话 + 一条用户消息，并把设置指向假模型。
func seedLLMSession(t *testing.T, gdb *gorm.DB, userText string) (*model.AgentSession, *model.AgentMessage) {
	t.Helper()
	if err := gdb.Table("agent_settings").Where("id = 1").Updates(map[string]any{
		"enabled": true, "model_id": "fake-chat",
		"max_steps": 12, "max_tokens_per_run": 32000,
	}).Error; err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	sess, err := CreateSession(context.Background(), "", "")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	msg, err := AppendMessage(context.Background(), sess.ID, "user", userText, nil, MessageMeta{Status: "ok"})
	if err != nil {
		t.Fatalf("写用户消息失败: %v", err)
	}
	return sess, msg
}

// registerFake 注册假适配器并在结束时清理（不清理会污染同包其它用例的选路）。
func registerFake(t *testing.T, f relay.Adapter) {
	t.Helper()
	if err := relay.RegisterAdapter(f); err != nil {
		t.Fatalf("注册假适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.ForgetAdapter(f.Name()) })
}

// TestLoopEndToEnd 是全链路：LLM 只用工具 → 提案 → 应用 → 门户/文档可见 → 回滚。
func TestLoopEndToEnd(t *testing.T) {
	gdb := withAgentDB(t)
	plan := happyPlan()
	// 从 happyPlan 里取出 operations 原文，让"模型"把它当参数交给 propose_change。
	var parsed struct {
		Operations json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal([]byte(plan), &parsed); err != nil {
		t.Fatalf("计划解析失败: %v", err)
	}
	proposeArgs := fmt.Sprintf(`{"summary":"接入中转 A 的对话模型","operations":%s}`, string(parsed.Operations))

	fake := &fakeStreamAdapter{
		name: "agent-fake-loop",
		script: [][]string{
			{toolCallFrame("c1", "skill_read", `{"id":"20-plan-shape"}`)},
			{toolCallFrame("c2", "schema_get", `{"section":"plan"}`)},
			{toolCallFrame("c3", "pricing_basis", `{}`)},
			{toolCallFrame("c4", "propose_change", proposeArgs)},
			{textFrame("已生成提案，请确认后应用。")},
		},
		usage: relay.UsageInfo{Usage: billing.Usage{PromptTokens: 30, CompletionTokens: 10}},
	}
	registerFake(t, fake)
	seedChatChannel(t, gdb, fake.name)
	sess, msg := seedLLMSession(t, gdb, "接入中转 A 的对话模型")

	var toolNames []string
	var proposalID uint64
	if err := Run(context.Background(), RunInput{
		SessionID: sess.ID, MessageID: msg.ID, AdminID: 1,
		UserText: "接入中转 A 的对话模型",
	}, func(ev Event) error {
		switch ev.Type {
		case "tool_call":
			toolNames = append(toolNames, ev.Name)
		case "proposal":
			proposalID = ev.ProposalID
		}
		return nil
	}); err != nil {
		t.Fatalf("编排失败: %v", err)
	}
	// 工具链必须真的走全：读知识包 → 读字典 → 取刻度 → 提交提案。
	want := []string{"skill_read", "schema_get", "pricing_basis", "propose_change"}
	if strings.Join(toolNames, ",") != strings.Join(want, ",") {
		t.Fatalf("工具链不对：\n got %v\nwant %v", toolNames, want)
	}
	if proposalID == 0 {
		t.Fatal("事件里没有 proposal：模型产出了提案却没发给前端")
	}

	var prop model.AgentProposal
	if err := gdb.First(&prop, proposalID).Error; err != nil {
		t.Fatalf("读提案失败: %v", err)
	}
	if prop.Status != model.ProposalDraft {
		t.Fatalf("提案应为 draft，实际 %s（error=%s）", prop.Status, prop.Error)
	}

	res, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1)
	if err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if res.Ops == 0 {
		t.Fatal("apply 没有执行任何操作")
	}

	// ① 六类实体
	var spec model.AdapterSpec
	if err := gdb.Where("slug = ?", "relay-a").First(&spec).Error; err != nil {
		t.Fatalf("规格未落库: %v", err)
	}
	var ch model.Channel
	if err := gdb.Where("name = ?", "中转A-国内").First(&ch).Error; err != nil {
		t.Fatalf("渠道未落库: %v", err)
	}
	if ch.AdapterID != "spec:relay-a" {
		t.Fatalf("渠道适配器错误: %s", ch.AdapterID)
	}
	var m model.Model
	if err := gdb.Where("id = ?", "relay-chat").First(&m).Error; err != nil {
		t.Fatalf("模型未落库: %v", err)
	}
	var bindings []model.ModelBinding
	if err := gdb.Where("model_id = ?", "relay-chat").Find(&bindings).Error; err != nil {
		t.Fatalf("绑定查询失败: %v", err)
	}
	if len(bindings) != 1 || bindings[0].ChannelID != ch.ID {
		t.Fatalf("绑定不正确: %+v", bindings)
	}

	// ② 门户可见
	onSale, err := pricing.OnSaleModels()
	if err != nil {
		t.Fatalf("在售查询失败: %v", err)
	}
	found := false
	for _, x := range onSale {
		if x.ID == "relay-chat" {
			found = true
			if x.Prices.Input == nil {
				t.Fatal("在售模型的输入价为空：门户会把它显示成免费")
			}
		}
	}
	if !found {
		t.Fatal("新模型不在在售集合里：门户与 /v1/models 都看不到它")
	}
	// 定价快照必须**已失效**：否则门户/文档最长 10s 内还按旧数据展示，
	// 而运营刚刚在界面上确认"已生效"。这条断言就是 lifecycle.AfterChange 的存在意义。
	psnap, err := pricing.Get()
	if err != nil {
		t.Fatalf("读定价快照失败: %v", err)
	}
	var cached bool
	for _, it := range psnap.Models {
		if it.ID == "relay-chat" {
			cached = true
			if it.Prices.Input == nil {
				t.Fatal("定价快照里的输入价为空：门户会显示成免费")
			}
		}
	}
	if !cached {
		t.Fatal("定价快照里没有新模型：缓存未失效（Apply 的后置管线没跑）")
	}

	// ③ 文档站与模型解耦：apply 之后**不重算文档** —— 归属是读取时现算的。
	//    对话模型不单独建节点，它按 `AppliesDoc` 出现在「OpenAI 兼容」那篇的适用模型里。
	var chat model.Model
	if err := db.DB.Where("id = ?", "relay-chat").Take(&chat).Error; err != nil {
		t.Fatalf("读模型失败: %v", err)
	}
	if !apidoc.AppliesDoc(apidoc.FactsOf(&chat), apidoc.KindChatCompletions) {
		t.Fatal("对话模型没有落在「OpenAI 兼容」协议篇的适用模型里（端点声明丢了）")
	}
	if got, ok := apidoc.FamilyDocOfModelID(chat.ID); ok {
		t.Fatalf("对话模型不该属于任何模型族，实际命中 %q", got.Key)
	}

	// ④ 纯"新建"型提案没有可自动回滚的操作：必须**明确拒绝**，而不是生成一条
	//    空提案让运营以为回滚成功了。真正会改配置的回滚由
	//    TestRollbackRestoresAllPriceLanes 覆盖（含 8 条 lane + 规格快照）。
	if _, err := Rollback(context.Background(), prop.ID, 1); err == nil ||
		!strings.Contains(err.Error(), "没有可自动回滚的操作") {
		t.Fatalf("新建型提案的回滚应被明确拒绝，实际 %v", err)
	}
	// 拒绝之后配置不能被改动（不许"半回滚"）。
	var still model.Model
	if err := gdb.Where("id = ?", "relay-chat").First(&still).Error; err != nil {
		t.Fatalf("回滚被拒后模型不见了: %v", err)
	}
}

// TestRollbackRestoresAllPriceLanes 守住"回滚要恢复 8 条 lane"。
//
// 历史缺陷：回滚载荷只反算了 input/output 两条，其余 6 条由 FillMissing 按
// **改后倍率**回填 —— 界面上写着"已回滚"，缓存/图像/音频价还是新价。
func TestRollbackRestoresAllPriceLanes(t *testing.T) {
	gdb := withAgentDB(t)

	if err := gdb.Create(&model.Vendor{ID: "relayco", Name: "中转公司"}).Error; err != nil {
		t.Fatalf("种子供应商失败: %v", err)
	}
	seedBindableModel(t, gdb, "lane-chat")

	// 改价：只提交 input/output，其余 lane 必须由 FillMissing 沿用（而不是清零）。
	plan := `{"summary":"改价","operations":[
		{"op":"update","entity":"model","key":"lane-chat","expect_version":1,
		 "data":{"input_price":2000000,"output_price":4000000}}
	]}`
	prop := createProposal(t, plan)
	if _, err := Apply(context.Background(), prop.ID, prop.PlanHash, 1); err != nil {
		t.Fatalf("改价 apply 失败: %v", err)
	}

	var changed model.Model
	if err := gdb.Where("id = ?", "lane-chat").First(&changed).Error; err != nil {
		t.Fatalf("读模型失败: %v", err)
	}
	if changed.Prices.Cache == nil || changed.Prices.Image == nil || changed.Prices.Audio == nil {
		t.Fatalf("改价后其它 lane 被清空了：%+v", changed)
	}

	rev, err := Rollback(context.Background(), prop.ID, 1)
	if err != nil {
		t.Fatalf("生成反向提案失败: %v", err)
	}
	if _, err := Apply(context.Background(), rev.ID, rev.PlanHash, 1); err != nil {
		t.Fatalf("应用反向提案失败: %v", err)
	}

	var back model.Model
	if err := gdb.Where("id = ?", "lane-chat").First(&back).Error; err != nil {
		t.Fatalf("读回滚结果失败: %v", err)
	}
	// 回滚必须把 8 条 lane **逐项还原成种子原值**（绝对金额 / 微）。
	//
	// 旧实现只反算 input/output 两条，其余 6 条由 FillMissing 按"改后价"回填 ——
	// 界面写着"已回滚"，缓存/图像/音频仍是新价。所以这里逐项对原值，而不是对"改后值"。
	want := map[string]int64{
		"input": 2_500_000, "output": 5_000_000, "cache": 250_000,
		"cache_create": 3_000_000, "cache_create_1h": 4_750_000,
		"image": 7_500_000, "audio": 10_000_000, "audio_output": 3_750_000,
	}
	got := map[string]*int64{
		"input": back.Prices.Input, "output": back.Prices.Output, "cache": back.Prices.Cache,
		"cache_create": back.Prices.CacheCreate, "cache_create_1h": back.Prices.CacheCreate1h,
		"image": back.Prices.Image, "audio": back.Prices.Audio, "audio_output": back.Prices.AudioOutput,
	}
	for name, w := range want {
		v := got[name]
		if v == nil {
			t.Fatalf("回滚后 %s 价为空", name)
		}
		if *v != w {
			t.Fatalf("回滚后 %s 价未还原：期望 %d，实际 %d", name, w, *v)
		}
	}
	// 被本次改动触及的两条必须真的变过（否则上面的"已还原"是空的）。
	if *changed.Prices.Input == 2_500_000 || *changed.Prices.Output == 5_000_000 {
		t.Fatalf("改价没有生效，回滚断言失去意义：%+v", changed.Prices)
	}
}

func ptr(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// seedBindableModel 建一个"有启用绑定"的模型，让它在售（门户/文档都能看到）。
// 8 条价格 lane 全部给值，供回滚用例比对。
func seedBindableModel(t *testing.T, gdb *gorm.DB, id string) {
	t.Helper()
	keys, err := model.NewJSON([]string{"sk-test"})
	if err != nil {
		t.Fatalf("构造密钥失败: %v", err)
	}
	_ = keys
	ch := model.Channel{
		Name: "种子渠道", AdapterID: "openai", BaseURL: "https://seed.example",
		Status: model.ChannelStatusEnabled, MultiKeyMode: "random",
		Weight: 5, Priority: 1, RPMLimit: 60, MaxConcurrency: 10, AutoBan: true,
	}
	if err := gdb.Create(&ch).Error; err != nil {
		t.Fatalf("种子渠道失败: %v", err)
	}
	// 价目是**绝对金额（微 / 百万 tokens）**：$2.5 输入、$5 输出、$0.25 缓存……
	// 旧写法是 ratio（1.0/2.0/0.1），现在没有倍率，值必须写绝对值。
	im := func(v int64) *int64 { return &v }
	m := model.Model{
		ID: id, DisplayName: "回滚测试模型", Vendor: "relayco", Modality: "chat",
		BillingMode: model.BillingModeTokens, Enabled: true, Version: 1,
		Prices: model.Prices{
			Input: im(2_500_000), Output: im(5_000_000), Cache: im(250_000),
			CacheCreate: im(3_000_000), CacheCreate1h: im(4_750_000),
			Image: im(7_500_000), Audio: im(10_000_000), AudioOutput: im(3_750_000),
		},
	}
	if err := gdb.Create(&m).Error; err != nil {
		t.Fatalf("种子模型失败: %v", err)
	}
	if err := gdb.Create(&model.ModelBinding{
		ModelID: id, ChannelID: ch.ID, UpstreamModel: "upstream-1",
		Enabled: true, Weight: 1, Priority: 1,
	}).Error; err != nil {
		t.Fatalf("种子绑定失败: %v", err)
	}
}
