package apicontract

import (
	"strings"
	"testing"
)

// TestSortParamIsGenerated 封死"手写 sort 参数"这条路。
//
// 规则只有两条，但缺一条就会回到"两处列表各自漂移"的老路：
//
//	声明了 Sortable → 必须**恰好一个** sort 参数，且枚举由 Sortable 派生；
//	没声明 Sortable → **不许**出现 sort 参数（手写的 sort 一定没有白名单支撑，
//	服务端会 400，而文档承诺可用）。
//
// 为什么"恰好一个"也要查：手写 + 自动派生会并存成两行，表格上看着像两个
// 不同的参数，接入方会以为可以传两次。
func TestSortParamIsGenerated(t *testing.T) {
	for _, e := range Rules() {
		d := e.Doc()
		var got []Param
		for _, p := range d.Params {
			if p.Name == sortParamName {
				got = append(got, p)
			}
		}

		if len(d.Sortable) == 0 {
			if len(got) != 0 {
				t.Errorf("%s：没有声明 Sortable，却出现了 sort 参数（只能是手写的）", e.Key)
			}
			continue
		}

		if len(got) != 1 {
			t.Errorf("%s：Sortable 有 %d 个字段，sort 参数却有 %d 个（应为 1）",
				e.Key, len(d.Sortable), len(got))
			continue
		}
		p := got[0]
		if p.In != InQuery {
			t.Errorf("%s：sort 必须在 query，实际 %q", e.Key, p.In)
		}
		if p.Required != ReqOptional {
			t.Errorf("%s：sort 必须是可选，实际 %q", e.Key, p.Required)
		}
		if p.Type != TypeString {
			t.Errorf("%s：sort 必须是 string，实际 %q", e.Key, p.Type)
		}
		// 枚举必须逐字等于 [f, -f] × n，顺序也一致：文档里的顺序就是
		// 试运行下拉框的顺序，字段少的时候才好在列表里找。
		want := make([]string, 0, len(d.Sortable)*2)
		for _, f := range d.Sortable {
			want = append(want, f, "-"+f)
		}
		if len(p.Enum) != len(want) {
			t.Fatalf("%s：sort 枚举 %v，期望 %v", e.Key, p.Enum, want)
		}
		for i := range want {
			if p.Enum[i] != want[i] {
				t.Fatalf("%s：sort 枚举第 %d 项 %q，期望 %q", e.Key, i, p.Enum[i], want[i])
			}
		}
		// 生成路径绕过 normalize，数组可能是 nil -> JSON 变 null。
		if p.Constraints == nil || p.Enum == nil {
			t.Errorf("%s：sort 参数的 Enum/Constraints 必须是非 nil 空安全切片", e.Key)
		}
	}
}

// TestSortableFieldsAreClean 保证 Sortable 里的字段名能直接拼进枚举。
//
// 三类脏值各自的后果都很难看：
//
//	空串 → 枚举里出现 "" 与 "-"，试运行能选到一个必然 400 的值；
//	带前导 - → 枚举里出现 "--amount"，用户永远选不中合法的降序值；
//	带空格 → 枚举看起来一样、实际传过去带空格，400 里还看不出区别。
func TestSortableFieldsAreClean(t *testing.T) {
	for _, e := range Rules() {
		seen := make(map[string]bool, len(e.Doc().Sortable))
		for _, f := range e.Doc().Sortable {
			if f == "" {
				t.Errorf("%s：Sortable 里有空串", e.Key)
				continue
			}
			if strings.TrimSpace(f) != f {
				t.Errorf("%s：Sortable 字段 %q 带首尾空白", e.Key, f)
			}
			if strings.HasPrefix(f, "-") {
				t.Errorf("%s：Sortable 字段 %q 不该带前导 -（方向由请求决定）", e.Key, f)
			}
			if seen[f] {
				t.Errorf("%s：Sortable 字段 %q 重复", e.Key, f)
			}
			seen[f] = true
		}
	}
}
