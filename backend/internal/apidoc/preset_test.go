package apidoc

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// 本文件守「预设（调用方式）→ 所属文档的字段表」这条不变量。
//
// 预设最容易出的错是**指到一个不存在的字段**：右栏照样能发出去，上游静默忽略，
// 用户以为设了而没生效。所以判据只有一个：预设写进去的每个键，都必须是这篇文档
// 真的声明过的字段名。

// ① 每个预设的 Fields 与 Body 键都必须在这篇文档的字段表里。
func TestPresetFieldsExistInDoc(t *testing.T) {
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			declared := map[string]bool{}
			for _, fl := range e.Spec.Fields {
				declared[fl.Name] = true
			}
			for _, p := range e.Presets {
				names := append([]string{}, p.Fields...)
				names = append(names, p.Uploads...)
				for _, n := range names {
					if n == "" {
						t.Fatalf("族 %q 的预设 %q 有空字段名", f.Key, p.Key)
					}
					if !declared[n] {
						t.Fatalf("族 %q 的 %s 预设 %q 引用了未声明的字段 %q", f.Key, e.Doc, p.Key, n)
					}
				}
				for _, kv := range p.Body {
					if kv.K == "model" {
						continue
					}
					if !declared[kv.K] {
						t.Fatalf("族 %q 的 %s 预设 %q 的 body 里有未声明的键 %q", f.Key, e.Doc, p.Key, kv.K)
					}
				}
			}
		}
	}
}

// ② 预设 key 必须全局唯一（前端选中态与 e2e 断言都靠它）。
func TestPresetKeysUnique(t *testing.T) {
	seen := map[string]string{}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			for _, p := range e.Presets {
				if prev, ok := seen[p.Key]; ok {
					t.Fatalf("预设 key %q 重复（%s 与 %s/%s）", p.Key, prev, f.Key, e.Doc)
				}
				seen[p.Key] = f.Key + "/" + e.Doc.String()
				if strings.TrimSpace(p.Label) == "" {
					t.Fatalf("预设 %q 缺 label", p.Key)
				}
				if len(p.Fields) == 0 {
					t.Fatalf("预设 %q 没有声明它会写哪些字段", p.Key)
				}
			}
		}
	}
}

// ③ 两族的预设不得有相同的 body 键集（T5：复制粘贴的预设 = 复制粘贴的文档）。
func TestPresetsAreNotCopiedAcrossFamilies(t *testing.T) {
	seen := map[string]string{}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			for _, p := range e.Presets {
				keys := make([]string, 0, len(p.Body))
				for _, kv := range p.Body {
					// 键 + 取值一起算指纹：两族都写「文生图」是正常的，
					// 写成一模一样的报文才说明是复制粘贴。
					keys = append(keys, fmt.Sprintf("%s=%v", kv.K, kv.V))
				}
				sort.Strings(keys)
				fp := string(e.Doc.Kind) + "|" + strings.Join(keys, ",")
				if prev, ok := seen[fp]; ok && prev != f.Key {
					t.Fatalf("族 %q 与 %q 在 %s 上的预设有相同的 body 键集（%v）",
						f.Key, prev, e.Doc.Kind, keys)
				}
				seen[fp] = f.Key
			}
		}
	}
}

// ④ 预设里的占位符只能是已登记的那几个。
func TestPresetTokensKnown(t *testing.T) {
	known := map[string]bool{
		PresetModelToken: true, presetTokenSeconds: true, presetTokenResolution: true,
	}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			for _, p := range e.Presets {
				for _, kv := range p.Body {
					s, ok := kv.V.(string)
					if !ok || !strings.HasPrefix(s, "$") {
						continue
					}
					if !known[s] {
						t.Fatalf("预设 %q 用了未登记的占位符 %q（投影期不会解析它）", p.Key, s)
					}
				}
			}
		}
	}
}
