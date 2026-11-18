package agent

import (
	"reflect"
	"sort"
	"testing"

	specdsl "lingdong-api/internal/relay/spec"
)

// 本文件锁死"字段字典只能来自反射"这条铁律。
//
// 漂移的代价已知：知识包写的字段表过期时不会报错，只会让模型写出被拒的规格
// （本次故障的直接成因）。因此字典与结构体必须逐字段对齐，且缺 note 即失败。

func TestPlanDictionaryMatchesStructs(t *testing.T) {
	for _, e := range planEntityList {
		dict, ok := PlanDictionary(e.name)
		if !ok {
			t.Fatalf("实体 %s 查不到", e.name)
		}
		fields, _ := dict["fields"].(map[string]any)
		want := map[string]bool{}
		for i := 0; i < e.typ.NumField(); i++ {
			name, omit, ptr := jsonName(e.typ.Field(i))
			if name == "" || name == "-" {
				continue
			}
			want[name] = true
			item, _ := fields[name].(map[string]any)
			if item == nil {
				t.Fatalf("%s.%s 不在字典里", e.name, name)
			}
			_, hasReq := item["required"]
			if hasReq != (!ptr && !omit) {
				t.Errorf("%s.%s 的 required 与结构体不一致（指针=%v omitempty=%v 字典=%v）",
					e.name, name, ptr, omit, hasReq)
			}
		}
		if len(fields) != len(want) {
			names := make([]string, 0, len(fields))
			for k := range fields {
				if !want[k] {
					names = append(names, k)
				}
			}
			sort.Strings(names)
			t.Errorf("%s 有多余字段：%v", e.name, names)
		}
	}
}

func TestEveryEntityFieldHasNote(t *testing.T) {
	for _, e := range planEntityList {
		dict, _ := PlanDictionary(e.name)
		fields, _ := dict["fields"].(map[string]any)
		for name, raw := range fields {
			item, _ := raw.(map[string]any)
			if note, _ := item["note"].(string); note == "" {
				t.Errorf("%s.%s 缺少 note（模型只能靠猜）", e.name, name)
			}
		}
	}
}

func TestEverySpecFieldHasNote(t *testing.T) {
	for _, s := range specStructs {
		fields := specFields(s.key, s.typ)
		for name, raw := range fields {
			item, _ := raw.(map[string]any)
			if note, _ := item["note"].(string); note == "" {
				t.Errorf("spec_dsl %s.%s 缺少 note", s.key, name)
			}
		}
	}
}

func TestSpecDictionaryCoversSpecStructs(t *testing.T) {
	dict, ok := SpecDictionary("all")
	if !ok {
		t.Fatal("all 应合法")
	}
	structsOut, _ := dict["structures"].(map[string]any)
	if len(structsOut) != len(specStructs) {
		t.Fatalf("结构体数量不一致：字典 %d / 列表 %d", len(structsOut), len(specStructs))
	}
	for _, s := range specStructs {
		entry, _ := structsOut[s.key].(map[string]any)
		if entry == nil {
			t.Fatalf("缺少结构 %s", s.key)
		}
		fields, _ := entry["fields"].(map[string]any)
		for i := 0; i < s.typ.NumField(); i++ {
			name, _, _ := jsonName(s.typ.Field(i))
			if name == "" || name == "-" {
				continue
			}
			if _, ok := fields[name]; !ok {
				t.Errorf("%s.%s 不在字典里", s.key, name)
			}
		}
	}
}

func TestSpecDictionaryParts(t *testing.T) {
	for _, p := range specParts {
		if _, ok := SpecDictionary(p); !ok {
			t.Fatalf("part %s 应合法", p)
		}
	}
	if _, ok := SpecDictionary("nope"); ok {
		t.Fatal("未知 part 应返回 false")
	}
	dict, _ := SpecDictionary("endpoint")
	if _, has := dict["structures"]; !has {
		t.Fatal("endpoint 应回 structures")
	}
	structsOut, _ := dict["structures"].(map[string]any)
	if len(structsOut) != 1 {
		t.Fatalf("part=endpoint 只应回一层，实际 %d", len(structsOut))
	}
	if _, has := dict["limits"]; has {
		t.Fatal("part=endpoint 不应回 limits")
	}
	lim, _ := SpecDictionary("limits")
	if _, has := lim["structures"]; has {
		t.Fatal("part=limits 不应回 structures")
	}
}

func TestSpecDictionaryLimitsMatchConstants(t *testing.T) {
	dict, _ := SpecDictionary("limits")
	limits, _ := dict["limits"].(map[string]any)
	want := map[string]int{
		"max_endpoints":       specdsl.MaxEndpoints,
		"max_const_keys":      specdsl.MaxConstKeys,
		"max_copy_keys":       specdsl.MaxCopyKeys,
		"max_arrays":          specdsl.MaxArrays,
		"max_array_items":     specdsl.MaxArrayItems,
		"max_static_query":    specdsl.MaxStaticQuery,
		"max_path_len":        specdsl.MaxPathLen,
		"max_spec_bytes":      specdsl.MaxSpecBytes,
		"max_slug_len":        specdsl.MaxSlugLen,
		"max_label_runes":     specdsl.MaxLabelRunes,
		"max_json_path_depth": specdsl.MaxJSONPathDeep,
	}
	for k, v := range want {
		got, ok := limits[k].(int)
		if !ok || got != v {
			t.Errorf("limits.%s 应为 %d，实际 %v", k, v, limits[k])
		}
	}
}

func TestSpecDictionaryEnumsMatchConstants(t *testing.T) {
	dict, _ := SpecDictionary("enums")
	enums, _ := dict["enums"].(map[string]any)

	if got, _ := enums["slug_pattern"].(string); got != specdsl.SlugPattern {
		t.Errorf("slug_pattern 应为 %s，实际 %v", specdsl.SlugPattern, enums["slug_pattern"])
	}
	if !reflect.DeepEqual(enums["status_states"], anySlice(specdsl.StatusStates)) &&
		!reflect.DeepEqual(toAnyStrings(enums["status_states"]), specdsl.StatusStates) {
		t.Errorf("status_states 与常量不一致：%v", enums["status_states"])
	}
	if got := toAnyStrings(enums["reserved_const_keys"]); len(got) != len(specdsl.ReservedConstKeys()) {
		t.Errorf("reserved_const_keys 数量不一致：%d", len(got))
	}
	if got := toAnyStrings(enums["auth_headers"]); len(got) != len(specdsl.AuthHeaders()) {
		t.Errorf("auth_headers 数量不一致：%d", len(got))
	}
	// 排序是为了稳定输出（模型对"同一个问题两次回答不同"很敏感）。
	headers := toAnyStrings(enums["auth_headers"])
	if !sort.StringsAreSorted(headers) {
		t.Errorf("auth_headers 应有序：%v", headers)
	}
}

func anySlice(in []string) any {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func toAnyStrings(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, x := range s {
			if str, ok := x.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}
