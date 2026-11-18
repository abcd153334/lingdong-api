package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestPlanSchemaMatchesStructs 守「字段名只有一个来源」。
//
// tool schema 是反射生成的，但反射很容易在边界上漏东西（json.RawMessage 被当成
// []uint8、指针字段被当成必填）。这条用例做双向比对：结构体有的 schema 必须有，
// schema 有的结构体必须能解释。
func TestPlanSchemaMatchesStructs(t *testing.T) {
	var op map[string]any
	if err := json.Unmarshal(planOperationsSchema(), &op); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
	props, _ := op["properties"].(map[string]any)
	data, _ := props["data"].(map[string]any)
	oneOf, _ := data["oneOf"].([]any)
	if len(oneOf) != 5 {
		t.Fatalf("data 应有 5 个实体载荷，实际 %d 个", len(oneOf))
	}

	fromSchema := map[string]bool{}
	for _, v := range oneOf {
		obj, _ := v.(map[string]any)
		fields, _ := obj["properties"].(map[string]any)
		for k := range fields {
			fromSchema[k] = true
		}
	}

	// 与 planOperationsSchema 的 5 个结构体逐一对应。
	types := []reflect.Type{
		reflect.TypeOf(VendorData{}), reflect.TypeOf(AdapterSpecData{}),
		reflect.TypeOf(ChannelData{}), reflect.TypeOf(ModelData{}),
		reflect.TypeOf(BindingData{}),
	}
	fromStructs := map[string]bool{}
	for _, t0 := range types {
		for i := 0; i < t0.NumField(); i++ {
			name, _, _ := jsonName(t0.Field(i))
			if name == "" || name == "-" {
				continue
			}
			fromStructs[name] = true
		}
	}
	for k := range fromStructs {
		if !fromSchema[k] {
			t.Errorf("结构体字段 %q 不在 schema 里：模型按 schema 写会被 strictUnmarshal 拒绝", k)
		}
	}
	for k := range fromSchema {
		if !fromStructs[k] {
			t.Errorf("schema 里的 %q 不在任何结构体里：模型照着写必然报 unknown field", k)
		}
	}
}

// TestPlanSchemaEnumsMatchConstants 守 op/entity 枚举与常量同步。
func TestPlanSchemaEnumsMatchConstants(t *testing.T) {
	var op map[string]any
	_ = json.Unmarshal(planOperationsSchema(), &op)
	props := op["properties"].(map[string]any)

	check := func(field string, want []string) {
		spec := props[field].(map[string]any)
		raw, _ := spec["enum"].([]any)
		got := make([]string, 0, len(raw))
		for _, v := range raw {
			got = append(got, v.(string))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s 枚举不同步：schema=%v 常量=%v", field, got, want)
		}
	}
	check("op", []string{OpCreate, OpUpdate, OpDisable, OpReplace})
	check("entity", []string{EntityVendor, EntityAdapterSpec, EntityChannel, EntityModel, EntityModelBindings})
}

// TestPlanFieldNotesOnlyKnownFields 守"字段说明不能指向不存在的字段"。
//
// 说明表是手写的（只放"看名字猜不出来"的那些），写错键不会报错，
// 只会让模型永远看不到那条说明。
func TestPlanFieldNotesOnlyKnownFields(t *testing.T) {
	// 实体清单必须与 `planOperationsSchema()` 的 data 列表**逐个对应**：
	// 漏一个就会出现「说明写得好好的，但这个实体根本没被生成进 schema」，
	// 而测试只在 planFieldNotes 里找实体，永远发现不了。
	known := map[string]map[string]bool{
		"vendor":         tagsOf(reflect.TypeOf(VendorData{})),
		"adapter_spec":   tagsOf(reflect.TypeOf(AdapterSpecData{})),
		"channel":        tagsOf(reflect.TypeOf(ChannelData{})),
		"model":          tagsOf(reflect.TypeOf(ModelData{})),
		"model_bindings": tagsOf(reflect.TypeOf(BindingData{})),
	}
	for entity, notes := range planFieldNotes {
		set, ok := known[entity]
		if !ok {
			t.Fatalf("planFieldNotes 里的实体 %q 不存在", entity)
		}
		for field := range notes {
			if !set[field] {
				t.Errorf("字段说明 %s.%s 指向不存在的字段", entity, field)
			}
		}
	}
}

func tagsOf(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := jsonName(t.Field(i))
		if name != "" {
			out[name] = true
		}
	}
	return out
}

// TestSkillPackIntegrity 守知识包的可读性与索引预算。
func TestSkillPackIntegrity(t *testing.T) {
	ids := SkillIDs()
	if len(ids) == 0 {
		t.Fatal("知识包为空（go:embed 没生效？）")
	}
	idx := SkillIndex()
	if len(idx) > skillIndexMaxBytes {
		t.Fatalf("索引 %d 字节超过预算 %d", len(idx), skillIndexMaxBytes)
	}
	for _, id := range ids {
		if !strings.Contains(idx, id) {
			t.Errorf("索引里缺少 %s：模型看不到就永远不会读它", id)
		}
		text, _, _, ok := ReadSkill(id, 0, 0)
		if !ok || strings.TrimSpace(text) == "" {
			t.Errorf("知识包 %s 读不出正文", id)
		}
		// 正文第一段必须是标题（front-matter 被正确剥离）。
		if !strings.HasPrefix(strings.TrimSpace(text), "#") {
			t.Errorf("知识包 %s 的 front-matter 没被剥离：%q", id, firstLine(text))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
