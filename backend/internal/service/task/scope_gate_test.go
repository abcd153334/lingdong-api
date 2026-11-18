package task_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoEmptyTaskScope 守「查任务必须显式限定归属」。
//
// 事故形态：`task.Get(ctx, id, task.Scope{})`。空 Scope 在过去等价于「不加任何
// 条件」，于是任何一个忘了传归属的新 handler 都能跨用户读别人的任务 ——
// 编译通过、测试不会红、日志里也看不出来。
//
// 现在空 Scope 会被运行时拒绝（ErrScopeRequired + 指标），但那是**兜底**；
// 本用例是静态防线：真的需要跨用户读的地方必须写 `task.AllScope()`，
// 让「我知道我在跨用户读」在评审时一眼可见。
func TestNoEmptyTaskScope(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("%v", err)
	}
	fset := token.NewFileSet()
	var bad []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", "testdata", ".git":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) > 0 {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Scope" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "task" {
				return true
			}
			bad = append(bad, fset.Position(lit.Pos()).String())
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("扫描失败: %v", walkErr)
	}
	if len(bad) > 0 {
		t.Fatalf("这些地方用了空 task.Scope{}（等于跨用户读，且没有显式声明意图）：\n  %s\n"+
			"确需跨用户（管理端/签名媒体路径）请改用 task.AllScope()", strings.Join(bad, "\n  "))
	}
}
