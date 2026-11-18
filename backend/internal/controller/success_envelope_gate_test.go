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

// gatewayJSONFiles 是允许直接写 `c.JSON(...)` 的文件：它们的响应形状由**协议**决定
// （OpenAI / Claude 兼容、multipart 与二进制），不是控制台契约。
//
// 清单按文件而不是按函数：这几个文件里每个 handler 都属于网关域，
// 按函数列会让每加一个网关端点都要动门禁，最后没人愿意维护。
// 现在是**空表**：协议面的响应全部走 internal/gateway（唯一写出口）。
//
// 这张表曾经豁免 5 个文件（file / gateway_catalog / media_task / task / video），
// 理由是"响应形状由协议决定"。豁免本身没错，但它让"每个网关文件自己拼信封"
// 变成合法状态 —— 于是同一个 /v1 前缀下并存四种成功形状、三种失败形状，
// 谁也不知道下一个端点该抄哪一份。收口到 gateway 后，豁免不再需要：
// 网关文件改调 gateway.OK / OKStatus / OKNewAPI / FailDef，一个 c.JSON 都不写。
//
// 保留这个空表而不是删掉机制：新端点出现时它仍会拦住"顺手写一个 c.JSON"。
var gatewayJSONFiles = map[string]bool{}

// TestNoBareSuccessJSON 守住「控制台 API 成功也带信封」。
//
// 事故：GET/POST /api/admin/icons 曾写成 `c.JSON(http.StatusOK, ...)`，而同一个
// handler 的失败分支是 response.FailDef（带 {code,message,error_code,data}）。
// 于是同一个接口有两种形状，前端 parseResponse 拿不到 `code` 就把 HTTP 200 判成失败
// —— 表现为「图标库永远加载失败、上传永远报错」，页面其余部分完全正常。
//
// 为什么用 AST 而不是打接口：e2e 全量 page.route 打桩，桩件返回的是 envelope(...)，
// **比真后端"更正确"**，永远测不出这个差异；单测又直接 mock 了 hook。
// 静态扫描是唯一能同时覆盖「现在这两处」与「以后新增的第 N 处」的手段
// （与 response_gate_test.go 同款理由）。
func TestNoBareSuccessJSON(t *testing.T) {
	var hits []string

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取 controller 目录失败: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if gatewayJSONFiles[name] {
			continue
		}
		path := filepath.Join(".", name)
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("解析 %s 失败: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "JSON" {
				return true
			}
			hits = append(hits, fset.Position(call.Pos()).String())
			return true
		})
	}

	if len(hits) > 0 {
		t.Fatalf("发现 %d 处裸 c.JSON（控制台接口不写信封）：\n  %s\n"+
			"改用 response.OK（成功）/ response.FailDef*（失败）。\n"+
			"若确属 /v1 网关协议形状，请把该文件加进 gatewayJSONFiles 并说明理由。",
			len(hits), strings.Join(hits, "\n  "))
	}
}
