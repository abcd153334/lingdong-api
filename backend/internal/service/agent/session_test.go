package agent

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/pricing"
)

// 本文件覆盖会话级的两件事：标题派生（侧栏导航的前提）与模型优先级
// （会话指定的模型必须真的被使用，否则模型菜单就是摆设）。

func TestAppendMessageDerivesSessionTitle(t *testing.T) {
	gdb := withAgentDB(t)
	sess, err := CreateSession(context.Background(), "", "")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	// 首行带 Markdown 标题符号：标题里不能出现 "## "。
	if _, err := AppendMessage(context.Background(), sess.ID, "user",
		"## 接入可灵视频\n文档在下面，先把规格建出来", nil, MessageMeta{Status: "ok"}); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	var got model.AgentSession
	if err := gdb.First(&got, sess.ID).Error; err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if got.Title != "接入可灵视频" {
		t.Fatalf("标题应由首条消息派生，实际 %q", got.Title)
	}
	// 第二条不得覆盖：标题是"这次对话讲什么"，不是"最后一句说了什么"。
	if _, err := AppendMessage(context.Background(), sess.ID, "user", "再帮我改个价", nil, MessageMeta{Status: "ok"}); err != nil {
		t.Fatalf("写第二条消息失败: %v", err)
	}
	if err := gdb.First(&got, sess.ID).Error; err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if got.Title != "接入可灵视频" {
		t.Fatalf("后续消息不应改写标题，实际 %q", got.Title)
	}
}

func TestUpdateSessionModelID(t *testing.T) {
	gdb := withAgentDB(t)
	sess, err := CreateSession(context.Background(), "会话", "")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	mid := "fake-chat"
	if err := UpdateSession(context.Background(), sess.ID, "", nil, &mid); err != nil {
		t.Fatalf("设置会话模型失败: %v", err)
	}
	var got model.AgentSession
	if err := gdb.First(&got, sess.ID).Error; err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if got.ModelID != "fake-chat" {
		t.Fatalf("会话模型应写入，实际 %q", got.ModelID)
	}
	// 传空串 = 回到全站默认（与"不传"必须区分开）。
	empty := ""
	if err := UpdateSession(context.Background(), sess.ID, "", nil, &empty); err != nil {
		t.Fatalf("清空会话模型失败: %v", err)
	}
	if err := gdb.First(&got, sess.ID).Error; err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if got.ModelID != "" {
		t.Fatalf("清空后应为空串，实际 %q", got.ModelID)
	}
}

// seedExtraChatModel 建第二个可路由的对话模型（用于验证优先级，而不是"恰好只有一个"）。
func seedExtraChatModel(t *testing.T, gdb *gorm.DB, id string) {
	t.Helper()
	var ch model.Channel
	if err := gdb.Where("name = ?", "假渠道").First(&ch).Error; err != nil {
		t.Fatalf("找不到假渠道: %v", err)
	}
	inPrice := int64(500000)
	if err := gdb.Create(&model.Model{
		ID: id, DisplayName: id, Vendor: "relayco", Modality: "chat",
		BillingMode: model.BillingModeTokens, Enabled: true,
		Prices: model.Prices{Input: &inPrice},
	}).Error; err != nil {
		t.Fatalf("插入第二个模型失败: %v", err)
	}
	if err := gdb.Create(&model.ModelBinding{
		ModelID: id, ChannelID: ch.ID, UpstreamModel: id,
		Enabled: true, Weight: 1, Priority: 1,
	}).Error; err != nil {
		t.Fatalf("插入第二个绑定失败: %v", err)
	}
	pricing.InvalidatePricingCache()
}

func TestRunPrefersSessionModel(t *testing.T) {
	gdb := withAgentDB(t)
	fake := &fakeStreamAdapter{
		name:   "agent-fake-prio",
		script: [][]string{{`{"choices":[{"delta":{"content":"好。"}}]}`}},
		usage:  relay.UsageInfo{Usage: billing.Usage{PromptTokens: 10, CompletionTokens: 2}},
	}
	if err := relay.RegisterAdapter(fake); err != nil {
		t.Fatalf("注册假适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.ForgetAdapter(fake.name) })
	seedChatChannel(t, gdb, fake.name)
	seedExtraChatModel(t, gdb, "other-chat")

	// 全站默认钉在 fake-chat，会话指定 other-chat：谁生效必须可判。
	// withAgentDB 已建好 id=1 的设置行（dry_run_required=false），这里只覆盖模型。
	if err := gdb.Model(&model.AgentSetting{}).Where("id = ?", 1).Updates(map[string]any{
		"enabled": true, "model_id": "fake-chat",
		"max_steps": 12, "max_tokens_per_run": 32000,
	}).Error; err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	sess, err := CreateSession(context.Background(), "", "other-chat")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	msg, err := AppendMessage(context.Background(), sess.ID, "user", "你好", nil, MessageMeta{Status: "ok"})
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}

	var usedModel string
	if err := Run(context.Background(), RunInput{
		SessionID: sess.ID, MessageID: msg.ID, AdminID: 1, UserText: "你好",
	}, func(ev Event) error {
		if ev.Type == "start" {
			usedModel = ev.Model
		}
		return nil
	}); err != nil {
		t.Fatalf("编排失败: %v", err)
	}
	if usedModel != "other-chat" {
		t.Fatalf("应按会话指定的模型运行，实际用了 %q", usedModel)
	}
}

func TestRunExplainsMissingSessionModel(t *testing.T) {
	gdb := withAgentDB(t)
	seedChatChannel(t, gdb, "agent-fake-missing")
	pricing.InvalidatePricingCache()

	sess, err := CreateSession(context.Background(), "", "deleted-model")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	msg, err := AppendMessage(context.Background(), sess.ID, "user", "你好", nil, MessageMeta{Status: "ok"})
	if err != nil {
		t.Fatalf("写消息失败: %v", err)
	}
	var got Event
	err = Run(context.Background(), RunInput{
		SessionID: sess.ID, MessageID: msg.ID, AdminID: 1, UserText: "你好",
	}, func(ev Event) error {
		if ev.Type == "error" {
			got = ev
		}
		return nil
	})
	if err == nil {
		t.Fatal("会话指定的模型不存在时必须拒绝，而不是悄悄用默认模型")
	}
	// 报错要能定位到"这个会话选的模型没了"，而不是笼统让人去改设置与渠道。
	if !strings.Contains(got.Message, "本会话指定的对话模型") {
		t.Fatalf("错误信息应指出是会话模型不可用，实际 %q", got.Message)
	}
}
