package task

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 本文件锁死「空作用域不再等于全表」。
//
// 回归的越权路径：`task.Get(ctx, id, task.Scope{})` 在过去等价于「不加任何条件」。
// 任何一个忘了传归属的新 handler 都能读到别人的任务 —— 编译通过、测试不红，
// 只在上线后被用户发现。现在零值被拒绝，跨用户读必须显式写 AllScope()。

// TestScopeBounded 是纯函数：只有显式声明才算限定了归属。
func TestScopeBounded(t *testing.T) {
	cases := []struct {
		name  string
		scope Scope
		want  bool
	}{
		{"零值（危险默认）", Scope{}, false},
		{"显式全表", AllScope(), true},
		{"按用户", Scope{UserID: 7}, true},
		{"按 KEY", Scope{KeyID: 9}, true},
		{"用户 + KEY", Scope{UserID: 7, KeyID: 9}, true},
	}
	for _, c := range cases {
		if got := c.scope.bounded(); got != c.want {
			t.Fatalf("%s：bounded()=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestZeroScopeRejectedWithoutDB 不需要数据库：闸门在任何查询之前。
func TestZeroScopeRejectedWithoutDB(t *testing.T) {
	ctx := context.Background()
	ref := "task_" + strings.Repeat("0", 26) // 26 位 ULID 长度的合法对外号

	if _, err := Get(ctx, 1, Scope{}); !errors.Is(err, ErrScopeRequired) {
		t.Fatalf("Get 的空作用域应被拒绝，实际 %v", err)
	}
	if _, err := GetByRef(ctx, ref, Scope{}); !errors.Is(err, ErrScopeRequired) {
		t.Fatalf("GetByRef 的空作用域应被拒绝，实际 %v", err)
	}
	if _, _, err := List(ctx, Scope{}, ListFilter{}); !errors.Is(err, ErrScopeRequired) {
		t.Fatalf("List 的空作用域应被拒绝，实际 %v", err)
	}
	if _, err := ListByRefs(ctx, []string{ref}, Scope{}); !errors.Is(err, ErrScopeRequired) {
		t.Fatalf("ListByRefs 的空作用域应被拒绝，实际 %v", err)
	}
}
