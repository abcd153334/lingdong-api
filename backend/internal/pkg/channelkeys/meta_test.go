package channelkeys

import (
	"encoding/json"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件守两条容易在实现时写错、且错了不会报错的规则：
//   1. enabled=false 必须能落进 JSON（值类型 + omitempty 会静默丢掉 false）；
//   2. Reconcile 只保留仍然存在的 keyID（否则别名会在"删了又加"后复活）。

func boolPtr(v bool) *bool { return &v }

func TestMetaMarshalKeepsFalse(t *testing.T) {
	m := MetaMap{
		"id-disabled": {Alias: "备用", Enabled: boolPtr(false)},
		"id-alias":    {Alias: "主力"},
	}
	raw, err := MarshalMeta(m)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var back map[string]map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if v, ok := back["id-disabled"]["enabled"]; !ok || v != false {
		t.Fatalf("enabled=false 必须落进 JSON，实际 %v", back["id-disabled"])
	}
	if _, ok := back["id-alias"]["enabled"]; ok {
		t.Fatalf("未设置的 enabled 不应写进 JSON，实际 %v", back["id-alias"])
	}
	// 真实场景：从库里读回来仍然是禁用
	parsed := ParseMeta(raw)
	if alias, enabled := parsed.Effective("id-disabled"); enabled || alias != "备用" {
		t.Fatalf("读回后应为「备用/禁用」，实际 alias=%q enabled=%v", alias, enabled)
	}
}

func TestMetaDefaultsAndEffective(t *testing.T) {
	if _, enabled := (MetaMap)(nil).Effective("missing"); !enabled {
		t.Fatal("缺项必须按启用处理")
	}
	if got, err := MarshalMeta(nil); err != nil || got != nil {
		t.Fatalf("空表应写 NULL: %v %v", got, err)
	}
	// 非法 JSON 一律回落默认，不 panic
	if ParseMeta(model.JSON(`{bad`)) != nil {
		t.Fatal("非法属性列应回落 nil")
	}
	if ParseMeta(model.JSON(`{}`)) != nil {
		t.Fatal("空对象应回落 nil（不写空 map）")
	}
}

func TestReconcileKeepsSurvivors(t *testing.T) {
	old := MetaMap{
		"a": {Alias: "甲", Enabled: boolPtr(false)},
		"b": {Alias: "乙"},
		"c": {Alias: "丙"},
	}
	// 新池只剩 b 与 d：a/c 的属性必须被 GC，b 保留，d 取默认
	out := Reconcile(old, []string{"b", "d"})
	if len(out) != 1 {
		t.Fatalf("应只剩 b 一条，实际 %v", out)
	}
	if out["b"].Alias != "乙" {
		t.Fatalf("幸存 key 的别名应保留，实际 %v", out["b"])
	}
	if _, ok := out["a"]; ok {
		t.Fatal("已删除的 key 属性必须被 GC")
	}
	if _, ok := out["d"]; ok {
		t.Fatal("新 key 不应凭空出现在属性表里（缺项即默认）")
	}
	// 全删 → nil
	if Reconcile(old, nil) != nil {
		t.Fatal("池清空后属性表应为 nil")
	}
}

func TestEnabledIDs(t *testing.T) {
	m := MetaMap{"b": {Enabled: boolPtr(false)}}
	got := m.EnabledIDs([]string{"a", "b", "c"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("应返回启用的 a/c，实际 %v", got)
	}
}
