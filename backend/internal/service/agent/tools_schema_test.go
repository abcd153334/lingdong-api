package agent

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"lingdong-api/internal/db"
)

// 本文件锁死"工具声明与实际实现不能分叉"。
//
// 本次故障的成因就在这一层：schema_get 的 enum 里写了 plan，
// 而 switch 里没有 plan 分支 —— 提示词让模型调它，模型永远拿不到字段表。
// 当时之所以没人发现，是因为**一个工具都没有测试**。

func TestSchemaSectionNamesAllImplemented(t *testing.T) {
	for _, name := range schemaSectionNames {
		switch name {
		case "all", "plan", "spec_dsl", "docs_text":
			continue
		default:
			if _, ok := schemaReadSections[name]; !ok {
				t.Errorf("section %q 在 enum 里但没有实现", name)
			}
		}
	}
	// 反向：实现里有的，enum 也必须声明（否则模型永远调不到）。
	for name := range schemaReadSections {
		found := false
		for _, n := range schemaSectionNames {
			if n == name {
				found = true
			}
		}
		if !found {
			t.Errorf("section %q 已实现但没写进 enum", name)
		}
	}
}

func TestToolSpecsAndImplsAgree(t *testing.T) {
	specs := ToolSpecs()
	declared := make([]string, 0, len(specs))
	for _, s := range specs {
		declared = append(declared, s.Function.Name)
		if len(s.Function.Parameters) == 0 {
			t.Errorf("工具 %s 没有参数 schema", s.Function.Name)
		}
		if !json.Valid(s.Function.Parameters) {
			t.Errorf("工具 %s 的参数 schema 不是合法 JSON", s.Function.Name)
		}
	}
	impls := make([]string, 0, len(toolImpls))
	for name := range toolImpls {
		impls = append(impls, name)
	}
	sort.Strings(declared)
	sort.Strings(impls)
	if !reflect.DeepEqual(declared, impls) {
		t.Fatalf("声明与实现不一致：\n声明 %v\n实现 %v", declared, impls)
	}
}

// TestDraftParamsMatchProposeItems：两个工具必须共用同一份 operations schema，
// 否则模型按 propose 的形状写预演参数会被判错。
func TestDraftParamsMatchProposeItems(t *testing.T) {
	var draft, propose map[string]any
	if err := json.Unmarshal(draftParams(), &draft); err != nil {
		t.Fatalf("draftParams 不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal(proposeChangeParams(), &propose); err != nil {
		t.Fatalf("proposeChangeParams 不是合法 JSON: %v", err)
	}
	pick := func(m map[string]any) any {
		props, _ := m["properties"].(map[string]any)
		ops, _ := props["operations"].(map[string]any)
		return ops["items"]
	}
	if !reflect.DeepEqual(pick(draft), pick(propose)) {
		t.Fatal("validate_draft 与 propose_change 的 operations.items 不一致")
	}
}

// TestSchemaGetReflectionSectionsNeedNoDB：字段手册是纯反射产物，
// 不该因为"库里还没有模型"而查不到（schema.Build 会读库）。
func TestSchemaGetReflectionSectionsNeedNoDB(t *testing.T) {
	prev := db.DB
	db.DB = nil
	defer func() { db.DB = prev }()

	for _, args := range []string{
		`{"section":"plan"}`,
		`{"section":"plan","entity":"adapter_spec"}`,
		`{"section":"spec_dsl"}`,
		`{"section":"spec_dsl","part":"endpoint"}`,
	} {
		res, err := toolSchemaGet(nil, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s 不应返回 Go error: %v", args, err)
		}
		m, ok := res.(map[string]any)
		if !ok || m["ok"] != true {
			t.Fatalf("%s 应成功，实际 %v", args, res)
		}
	}
}

func TestSchemaGetUnknownSectionListsAllowed(t *testing.T) {
	res, _ := toolSchemaGet(nil, json.RawMessage(`{"section":"nope"}`))
	m, _ := res.(map[string]any)
	if m["ok"] != false || m["field"] != "section" {
		t.Fatalf("未知 section 应报 field=section，实际 %v", res)
	}
	if got := toAnyStrings(m["allowed"]); len(got) != len(schemaSectionNames) {
		t.Fatalf("应回全部可选 section，实际 %v", m["allowed"])
	}
}

func TestSchemaGetPlanEntityFilter(t *testing.T) {
	res, _ := toolSchemaGet(nil, json.RawMessage(`{"section":"plan","entity":"adapter_spec"}`))
	m, _ := res.(map[string]any)
	plan, _ := m["plan"].(map[string]any)
	if plan["entity"] != EntityAdapterSpec {
		t.Fatalf("应只回 adapter_spec，实际 %v", plan)
	}
	if _, ok := plan["fields"]; !ok {
		t.Fatal("应带 fields")
	}
	// 单实体必须是全量的子集：多回等于把上下文又撑爆。
	full, _ := PlanDictionary("")
	allEntities, _ := full["entities"].(map[string]any)
	if _, ok := allEntities[EntityAdapterSpec]; !ok {
		t.Fatal("全量字典里应有 adapter_spec")
	}
}

func TestSchemaGetPlanUnknownEntity(t *testing.T) {
	res, _ := toolSchemaGet(nil, json.RawMessage(`{"section":"plan","entity":"spec"}`))
	m, _ := res.(map[string]any)
	if m["ok"] != false || m["field"] != "entity" {
		t.Fatalf("旧命名 spec 应被拒并指向 entity，实际 %v", res)
	}
}
