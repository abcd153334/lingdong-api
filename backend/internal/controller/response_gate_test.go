package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBareResponseFail 守住「4xx/5xx 必须带 error_code」这条契约。
//
// 为什么用 AST 而不是打接口：
//   - 27 个调用点里有 5 个是错误映射函数（respondSpecErr / respondAgentErr /
//     respondDocErr / parseChannelReq / AdminDocGroupSave 的前置快照），它们
//     没有独立路由入口，httptest 与 e2e 都打不到；
//   - e2e 里 35/36 个 spec 用 page.route 打桩，桩返回什么码由测试自己决定，
//     永远测不出后端带不带码；
//   - 静态扫描能同时覆盖「现在的 27 处」与「以后新增的第 28 处」。
//
// 唯一被允许的例外是网关线格式（FailOpenAI*）：/v1 的响应体是 OpenAI/Claude
// 形状，本来就没有 error_code 字段，那是协议决定的，不属于本契约。
func TestNoBareResponseFail(t *testing.T) {
	roots := []string{".", "../relay"}
	var hits []string

	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "response" {
					return true
				}
				// 只匹配 response.Fail：FailOpenAI* 是网关线格式，不受此约束。
				if sel.Sel.Name == "Fail" {
					pos := fset.Position(call.Pos())
					hits = append(hits, pos.String())
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s 失败: %v", root, err)
		}
	}

	if len(hits) > 0 {
		t.Fatalf("发现 %d 处 response.Fail（不写 error_code）：\n  %s\n"+
			"改用 response.FailDef（固定文案）或 response.FailDefMsg（自定义文案但保留码）。",
			len(hits), strings.Join(hits, "\n  "))
	}
}
