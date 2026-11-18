package adapters

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestDuomiOverrideKeysMatchStruct 锁住「结构体字段 ↔ 允许子键」的一致性。
//
// 只冻结结构体没用：真正会让运营保存失败的是 duomiOverrideKeyOrder 与 json tag
// 不同步（例如给结构体加了字段，却忘了加进 order，前端就永远写不出这个键）。
func TestDuomiOverrideKeysMatchStruct(t *testing.T) {
	typ := reflect.TypeOf(duomiOverride{})
	tags := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		tags = append(tags, name)
	}
	if len(tags) != len(duomiOverrideKeyOrder) {
		t.Fatalf("允许子键数量与结构体不一致：order=%v struct=%v", duomiOverrideKeyOrder, tags)
	}
	for i, name := range tags {
		if duomiOverrideKeyOrder[i] != name {
			t.Fatalf("第 %d 个子键不一致：order=%q struct=%q（顺序也参与生成物）", i, duomiOverrideKeyOrder[i], name)
		}
	}
}

// TestGeneratedOptionsSchemaIsUpToDate 是跨端契约的硬门禁。
//
// 改了 options_schema.go 却忘了重新生成前端常量时，这里红；
// 没有它，漂移只会在线上第一个请求暴露（2026-11-04 fixed_tier 事故）。
func TestGeneratedOptionsSchemaIsUpToDate(t *testing.T) {
	want, err := RenderOptionsSchemaTS()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	// internal/relay/adapters → 仓库根 → web/src/lib
	path := filepath.Join("..", "..", "..", "..", "web", "src", "lib", "adapterOptions.gen.ts")
	got, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("前端目录不存在（后端独立构建），跳过生成物比对")
	}
	if err != nil {
		t.Fatalf("读取生成物失败: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("前端常量已过期：请在 backend 目录运行 go run ./cmd/dump-adapter-options 并提交")
	}
}

// TestDefaultMetadataAllowIsSubsetOfKnown 保证「默认放行」里的键都是已知键。
//
// 默认清单里出现一个未知键时，后端会因为 metadata_allow 里的未知名整段拒绝，
// 而前端仍显示勾选状态 —— 又一类"界面看着对、保存就 400"。
func TestDefaultMetadataAllowIsSubsetOfKnown(t *testing.T) {
	for _, k := range duomiDefaultMetadataAllow {
		if !duomiMetadataKnown(k) {
			t.Fatalf("默认放行清单含未知键 %q", k)
		}
	}
}
