package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/db"
)

// 本文件覆盖 validate_draft 的**唯一职责**：它是 propose_change 的预演，
// 两条路径必须给出同一套规则、同一套字段定位。
//
// 这条契约过去是不成立的（validate_draft 自己调 channel.ValidateDraft /
// specdsl.Validate，entity 名还叫 spec），症状是"预演通过、提交被拒"。

func execToolMap(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("参数序列化失败: %v", err)
	}
	res := ExecTool(nil, ToolCall{Name: name, Arguments: string(raw)})
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("工具结果应为对象，实际 %T", res)
	}
	return m
}

// draftArgs 拼 validate_draft 的参数。
func draftArgs(ops ...map[string]any) map[string]any {
	return map[string]any{"operations": ops}
}

func TestValidateDraftUnknownSlugFieldNamesKey(t *testing.T) {
	res := execToolMap(t, "validate_draft", draftArgs(map[string]any{
		"op": "create", "entity": "adapter_spec", "key": "autodl-indextts",
		"data": map[string]any{
			"label": "AutoDL IndexTTS",
			// 本次故障的原文：slug 写在 data 顶层
			"slug": "autodl-indextts",
			"spec": map[string]any{"slug": "autodl-indextts", "label": "L", "endpoints": map[string]any{}},
		},
	}))
	if res["ok"] != false {
		t.Fatalf("应失败，实际 %v", res)
	}
	if res["code"] != "plan_invalid" {
		t.Fatalf("code 应为 plan_invalid，实际 %v", res["code"])
	}
	if res["field"] != "data" {
		t.Fatalf("field 应为 data，实际 %v", res["field"])
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, `不认识的字段 "slug"`) || !strings.Contains(msg, "operations[].key") {
		t.Fatalf("文案应指路到 operations[].key：%s", msg)
	}
}

func TestValidateDraftRejectsSingleModelWithoutBindings(t *testing.T) {
	// 跨操作规则只有 propose 有；预演若漏了它，"预演通过、提交被拒"就会复现。
	res := execToolMap(t, "validate_draft", draftArgs(map[string]any{
		"op": "create", "entity": "model", "key": "kling-v3",
		"data": map[string]any{
			"display_name": "可灵 v3", "vendor": "kuaishou",
			"modality": "chat", "billing_mode": "tokens",
		},
	}))
	if res["ok"] != false {
		t.Fatalf("只建模型不建绑定应失败，实际 %v", res)
	}
	msg, _ := res["message"].(string)
	if !strings.Contains(msg, "model_bindings") {
		t.Fatalf("应提示缺 model_bindings：%s", msg)
	}
}

func TestValidateDraftTooManyOperations(t *testing.T) {
	ops := make([]map[string]any, 0, maxDraftOps+1)
	for i := 0; i <= maxDraftOps; i++ {
		ops = append(ops, map[string]any{
			"op": "create", "entity": "vendor", "key": "v", "data": map[string]any{"name": "v"},
		})
	}
	res := execToolMap(t, "validate_draft", draftArgs(ops...))
	if res["ok"] != false || res["code"] != "invalid_arguments" {
		t.Fatalf("超过上限应报 invalid_arguments，实际 %v", res)
	}
}

func TestValidateDraftEmptyOperations(t *testing.T) {
	res := execToolMap(t, "validate_draft", map[string]any{"operations": []any{}})
	if res["ok"] != false || res["code"] != "invalid_arguments" {
		t.Fatalf("空 operations 应报 invalid_arguments，实际 %v", res)
	}
}

// TestValidateDraftFieldsMatchPropose 是"单管线"的可执行定义：
// 同一份坏草稿，两条路径必须给出相同的 field 与 message。
func TestValidateDraftFieldsMatchPropose(t *testing.T) {
	cases := []struct {
		name string
		op   map[string]any
	}{
		{"未知字段", map[string]any{
			"op": "create", "entity": "adapter_spec", "key": "s1",
			"data": map[string]any{"label": "L", "slug": "s1", "spec": map[string]any{}},
		}},
		{"缺 key", map[string]any{
			"op": "create", "entity": "vendor", "data": map[string]any{"name": "v"},
		}},
		{"未知实体", map[string]any{
			"op": "create", "entity": "spec", "key": "x", "data": map[string]any{},
		}},
		{"update 缺版本", map[string]any{
			"op": "update", "entity": "channel", "id": 3, "data": map[string]any{"name": "c"},
		}},
		{"派生字段", map[string]any{
			"op": "create", "entity": "model", "key": "m",
			"data": map[string]any{"display_name": "m", "vendor": "v", "modality": "chat", "billing_mode": "tokens", "is_free": true},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toolRes := execToolMap(t, "validate_draft", draftArgs(c.op))
			if toolRes["ok"] != false {
				t.Fatalf("预演应失败，实际 %v", toolRes)
			}
			planJSON, _ := json.Marshal(map[string]any{"summary": "t", "operations": []any{c.op}})
			_, perr := CreateProposal(context.Background(), ProposalInput{Kind: "change", Plan: planJSON})
			ve, ok := perr.(*ValidationError)
			if !ok {
				t.Fatalf("提案应返回 ValidationError，实际 %T(%v)", perr, perr)
			}
			if toolRes["field"] != ve.Field {
				t.Fatalf("field 不一致：预演 %v / 提案 %v", toolRes["field"], ve.Field)
			}
			if idx, _ := toolRes["index"].(int); idx != ve.Index {
				t.Fatalf("index 不一致：预演 %v / 提案 %d", toolRes["index"], ve.Index)
			}
		})
	}
}

// TestValidateDraftDBErrorIsNotToolInternal：预演依赖真库，
// 库不可用时必须说"环境问题"，而不是让模型以为草稿写错了反复重写。
func TestValidateDraftDBErrorIsNotToolInternal(t *testing.T) {
	prev := db.DB
	db.DB = nil
	defer func() { db.DB = prev }()

	res := execToolMap(t, "validate_draft", draftArgs(map[string]any{
		"op": "create", "entity": "vendor", "key": "acme",
		"data": map[string]any{"name": "acme"},
	}))
	if res["ok"] != false || res["code"] != "db_error" {
		t.Fatalf("应为 db_error，实际 %v", res)
	}
}

func TestValidateDraftAcceptsProposeShapedFragment(t *testing.T) {
	withAgentDB(t)
	res := execToolMap(t, "validate_draft", draftArgs(map[string]any{
		"op": "create", "entity": "vendor", "key": "acme",
		"data": map[string]any{"name": "acme"},
	}))
	if res["ok"] != true {
		t.Fatalf("合法草稿应通过，实际 %v", res)
	}
	if n, _ := res["operations"].(int); n != 1 {
		t.Fatalf("operations 应为 1，实际 %v", res["operations"])
	}
	ents, _ := res["entities"].([]string)
	if len(ents) != 1 || ents[0] != EntityVendor {
		t.Fatalf("entities 应为 [vendor]，实际 %v", res["entities"])
	}
}
