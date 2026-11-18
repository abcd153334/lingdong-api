package gateway_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"lingdong-api/internal/pkg/errs"
)

// TestNoHandwrittenErrorCodes 守「对外错误码只能来自 pkg/errs」。
//
// 事故形态：handler 里写 `gateway.Fail(c, 400, "some_code", "…")`。这个码
//   - 不在 registry 里 ⇒ `errs.Get` 查不到，relay/preflight 把它当陌生错误；
//   - 不在文档码表里 ⇒ 接入方看到码却查不到含义；
//   - 不受「参考文档码表 ⊆ errs」门禁约束 ⇒ 改错字没人发现。
//
// 曾经的规模是 10 个（async_stream_conflict、prompt_too_long、upstream_timeout…），
// 全部散在 media_task.go 与 relay.go 里。收口之后本用例是**唯一**防线：
// 新增端点时若又"顺手写一个字符串码"，CI 直接红，并打印出应该补到哪。
func TestNoHandwrittenErrorCodes(t *testing.T) {
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
			// 只扫我们自己的代码；new-api / node_modules 不在 backend 下，但仍然显式跳过。
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
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Fail" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "gateway" {
				return true
			}
			// gateway.Fail(c, status, code, msg)：第 3 个参数是码。
			if len(call.Args) < 3 {
				return true
			}
			lit, ok := call.Args[2].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true // 变量码（由调用方保证）不在静态扫描范围
			}
			code, _ := strconv.Unquote(lit.Value)
			if _, exists := errs.Get(code); !exists {
				pos := fset.Position(lit.Pos())
				bad = append(bad, pos.String()+" → "+code)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("扫描失败: %v", walkErr)
	}
	if len(bad) > 0 {
		t.Fatalf("这些错误码没有登记进 pkg/errs（先加常量+Def+registry，再改成 gateway.FailDef）：\n  %s",
			strings.Join(bad, "\n  "))
	}
}
