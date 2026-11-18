package controller

import (
	"encoding/json"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// 这组测试守的是一个**不会报错的错**：model 层只带 gorm tag，直接序列化出去
// 就是 PascalCase（`MaxSteps`）且带上 `DeletedAt`；前端契约是 snake_case。
// 字段名错了页面不会崩，只会「开关永远显示关闭、步数输入框空白」。
//
// 桩件 e2e 用自己的假数据，测不出这一层；连真后端才能发现，而那时已经上线了。
// 所以这里在编译期就把字段名钉死。

func marshalKeys(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	return m
}

func requireKeys(t *testing.T, v any, want ...string) map[string]json.RawMessage {
	t.Helper()
	got := marshalKeys(t, v)
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("缺少字段 %q，实际字段: %v", k, keysOf(got))
		}
	}
	for _, bad := range []string{"ID", "Title", "ModelID", "Status", "LastSeq", "DeletedAt", "Enabled", "MaxSteps"} {
		if _, ok := got[bad]; ok {
			t.Fatalf("泄漏了 PascalCase/内部字段 %q，实际字段: %v", bad, keysOf(got))
		}
	}
	return got
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAgentSessionViewIsSnakeCase(t *testing.T) {
	now := time.Now()
	v := newAgentSessionView(model.AgentSession{ID: 7, Title: "t", ModelID: "m", Status: 1, LastSeq: 3,
		CreatedAt: now, UpdatedAt: now})
	requireKeys(t, v, "id", "title", "model_id", "status", "last_seq", "created_at", "updated_at")
}

func TestAgentProposalViewIsSnakeCase(t *testing.T) {
	v := newAgentProposalView(model.AgentProposal{ID: 1, SessionID: 2, MessageID: 3, Kind: "change",
		Status: model.ProposalDraft, Plan: model.JSON(`{"ops":[]}`), PlanHash: "h"})
	got := requireKeys(t, v, "id", "session_id", "message_id", "kind", "status", "plan", "plan_hash", "created_at")
	// plan 必须是对象而不是 base64：model.JSON 一旦只实现 MarshalJSON，
	// 这里会变成字符串，前端 ops 统计直接读到 0。
	var plan map[string]any
	if err := json.Unmarshal(got["plan"], &plan); err != nil {
		t.Fatalf("plan 不是 JSON 对象: %v", err)
	}
	if _, ok := plan["ops"]; !ok {
		t.Fatalf("plan 内容被改写: %s", got["plan"])
	}
	// applied_at 必须恒在（前端据 null 判断"未应用"），不能 omitempty 掉。
	if _, ok := got["applied_at"]; !ok {
		t.Fatal("applied_at 缺失")
	}
}

func TestAgentSettingViewIsSnakeCaseAndHidesZeroTime(t *testing.T) {
	v := newAgentSettingItemView(model.AgentSetting{ID: 1, Enabled: true, MaxSteps: 12,
		MaxTokensPerRun: 32000, DryRunRequired: true})
	got := requireKeys(t, v, "id", "enabled", "model_id", "max_steps", "max_tokens_per_run",
		"daily_amount_budget", "dry_run_required")
	// 从未保存过时下发 0001-01-01 会让人以为设置被改过。
	if _, ok := got["updated_at"]; ok {
		t.Fatal("零值 updated_at 不应下发")
	}

	saved := newAgentSettingItemView(model.AgentSetting{ID: 1, UpdatedAt: time.Now()})
	if _, ok := marshalKeys(t, saved)["updated_at"]; !ok {
		t.Fatal("保存后必须下发 updated_at")
	}
}

func TestAgentDocItemViewIsSnakeCase(t *testing.T) {
	v := newAgentDocItemView(model.AgentDoc{ID: 2, Title: "doc", SourceType: "paste",
		SizeBytes: 10, CreatedAt: time.Now()})
	requireKeys(t, v, "id", "title", "source_type", "size_bytes", "created_at")
}
