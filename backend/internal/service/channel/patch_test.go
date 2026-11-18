package channel

import (
	"context"
	"encoding/json"
	"testing"

	"lingdong-api/internal/model"
)

// Patch 的语义是「只改提到的字段」。这条语义有两个易错点：
//  1. 用零值当"没提交" —— 0 是合法的限流值（不限流），会被当成没提，于是永远改不成 0；
//  2. adapter_config 整体替换 —— 只想改 auth_header 会顺手抹掉 path_overrides。
//
// 两者都不会报错，只会让配置悄悄变成另一种东西。

func TestPatchMergeKeepsUnmentionedFields(t *testing.T) {
	cur := &model.Channel{
		Name: "老名字", AdapterID: "spec:relay-a", BaseURL: "https://api.a.example",
		MultiKeyMode: ModeRandom, Weight: 5, Priority: 1, RPMLimit: 60, MaxConcurrency: 10,
		AutoBan: true, Status: model.ChannelStatusEnabled,
	}
	req, err := Patch{Name: strPtr("新名字")}.Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败: %v", err)
	}
	if req.Name != "新名字" {
		t.Fatalf("名字未生效: %s", req.Name)
	}
	// 改名不能顺带重置限流/并发/权重 —— 这正是历史上"只改个名字取消限流"的成因。
	if req.RPMLimit != 60 || req.MaxConcurrency != 10 || req.Weight != 5 || !req.AutoBan {
		t.Fatalf("未提交的字段被重置了: rpm=%d 并发=%d 权重=%d auto_ban=%v",
			req.RPMLimit, req.MaxConcurrency, req.Weight, req.AutoBan)
	}
}

func TestPatchMergeWritesExplicitZero(t *testing.T) {
	cur := &model.Channel{
		Name: "渠道", AdapterID: "spec:relay-a", BaseURL: "https://api.a.example",
		MultiKeyMode: ModeRandom, Weight: 5, Priority: 1, RPMLimit: 60, MaxConcurrency: 10,
		AutoBan: true,
	}
	zero := 0
	req, err := Patch{RPMLimit: &zero, MaxConcurrency: &zero}.Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败: %v", err)
	}
	if req.RPMLimit != 0 || req.MaxConcurrency != 0 {
		t.Fatalf("显式 0 应真的写 0（不限流/不限并发），实际 rpm=%d 并发=%d", req.RPMLimit, req.MaxConcurrency)
	}
}

func TestPatchMergeAdapterConfigIsIncremental(t *testing.T) {
	cur := &model.Channel{
		Name: "渠道", AdapterID: "spec:relay-a", BaseURL: "https://api.a.example",
		MultiKeyMode: ModeRandom, Weight: 5, Priority: 1,
		AdapterConfig: model.JSON(`{"auth_header":"authorization","auth_prefix":"Bearer ","path_overrides":{"/v1/chat/completions":"/chat"}}`),
	}
	patch := json.RawMessage(`{"auth_header":"x-api-key","path_overrides":{"/v1/embeddings":"/embed"}}`)
	req, err := Patch{AdapterConfig: patch}.Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(req.AdapterConfig, &got); err != nil {
		t.Fatalf("合并结果不是 JSON: %v（%s）", err, req.AdapterConfig)
	}
	if got["auth_header"] != "x-api-key" {
		t.Fatalf("auth_header 未更新: %v", got["auth_header"])
	}
	// 没提交的 auth_prefix 必须保留：整体替换会把它抹掉，而鉴权头前缀没了等于所有请求 401。
	if got["auth_prefix"] != "Bearer " {
		t.Fatalf("未提交的 auth_prefix 被抹掉了: %v", got["auth_prefix"])
	}
	po, _ := got["path_overrides"].(map[string]any)
	if len(po) != 2 {
		t.Fatalf("path_overrides 应按键合并（保留旧键），实际 %v", po)
	}
	if po["/v1/embeddings"] != "/embed" || po["/v1/chat/completions"] != "/chat" {
		t.Fatalf("path_overrides 合并结果不对: %v", po)
	}
}

func TestPatchMergeAdapterConfigNullClears(t *testing.T) {
	cur := &model.Channel{
		Name: "渠道", AdapterID: "spec:relay-a", BaseURL: "https://api.a.example",
		MultiKeyMode: ModeRandom, AdapterConfig: model.JSON(`{"auth_header":"authorization"}`),
	}
	req, err := Patch{AdapterConfig: json.RawMessage(`null`)}.Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败: %v", err)
	}
	// 显式 null = 清空；与"没提交"必须能区分。
	if string(req.AdapterConfig) != "null" {
		t.Fatalf("显式 null 应原样透传（清空），实际 %s", req.AdapterConfig)
	}
}

// TestPatchMergeKeepsIconWhenOmitted 图标与名字同类：没提到就不许动。
//
// UpdateTx 是「非 nil 才写」的增量更新，看起来不复制也行；但 Merge 的契约是
// "产出完整快照"，一旦哪天 UpdateTx 改成全列写（模型配置那边就是全列写），
// 漏复制就会把图标静默清空，且没有任何报错。
func TestPatchMergeKeepsIconWhenOmitted(t *testing.T) {
	cur := &model.Channel{
		Name: "渠道", AdapterID: "spec:relay-a", BaseURL: "https://api.a.example",
		Icon: "builtin:openai-gpt", MultiKeyMode: ModeRandom,
	}
	req, err := Patch{Name: strPtr("新名字")}.Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败: %v", err)
	}
	if req.Icon == nil || *req.Icon != "builtin:openai-gpt" {
		t.Fatalf("改名把图标丢了: %v", req.Icon)
	}
}

// TestPatchMergeIconsExplicitEmpty 显式传空串 = 清空图标（回落到首字占位）。
// 这条同时守住"空串是合法值"：Normalize("") 必须返回 "" + nil，
// 否则用户会永远清不掉已设的图标。
func TestPatchMergeIconsExplicitEmpty(t *testing.T) {
	cur := &model.Channel{
		Name: "渠道", AdapterID: "spec:relay-a", BaseURL: "https://api.a.example",
		Icon: "builtin:openai-gpt", MultiKeyMode: ModeRandom,
	}
	empty := ""
	req, err := Patch{Icon: &empty}.Merge(cur)
	if err != nil {
		t.Fatalf("Merge 失败: %v", err)
	}
	if req.Icon == nil || *req.Icon != "" {
		t.Fatalf("显式空串应清空图标，实际 %v", req.Icon)
	}
}

// TestPatchMergeRejectsBadIcon 非法 token 必须在 Merge 后的规范化阶段就被挡下。
// 放进库里的后果不是"图标不显示"——前端会把整个 src 指向奇怪的东西。
func TestPatchMergeRejectsBadIcon(t *testing.T) {
	cur := &model.Channel{Name: "渠道", AdapterID: "spec:relay-a", BaseURL: "https://a.example"}
	for _, bad := range []string{"data:image/png;base64,AAA", "javascript:alert(1)", "builtin:../etc/passwd"} {
		b := bad
		req, err := Patch{Icon: &b}.Merge(cur)
		if err != nil {
			t.Fatalf("Merge 失败: %v", err)
		}
		if err := req.resolveIcon(context.Background()); err == nil {
			t.Fatalf("非法图标 %q 应被拒绝", bad)
		}
	}
}

// TestIconEmptyIsValid 空串是合法值（清空图标），不能被 Normalize 判成非法。
func TestIconEmptyIsValid(t *testing.T) {
	empty := ""
	req, err := Patch{Icon: &empty}.Merge(&model.Channel{Name: "渠道"})
	if err != nil {
		t.Fatalf("Merge 失败: %v", err)
	}
	if err := req.resolveIcon(context.Background()); err != nil {
		t.Fatalf("空图标必须合法（用于清空），实际 %v", err)
	}
	if req.Icon == nil || *req.Icon != "" {
		t.Fatalf("空图标归一后应仍为空串，实际 %v", req.Icon)
	}
}

func strPtr(s string) *string { return &s }
