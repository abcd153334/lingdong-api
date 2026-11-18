package adapters_test

// 本文件是**对外错误文案**的门禁：调用方看到的每一句话都不能暴露平台内部结构。
//
// 为什么用 AST 扫源码而不是跑一遍适配器：坏文案分布在 140 多处参数校验分支里，
// 构造 140 个请求才能覆盖；而「哪句话会被返回给调用方」是**静态可判**的。
// 一旦有人新写一句「上游只支持 720p」，这是唯一能在 CI 里当场拦下的地方。
//
// 为什么只扫 CallerParamError.Msg：这是**唯一**以 4xx 原文回给调用方的类型
// （见 relay.CallerParamError.Error）。其它 error 经 outcome 归因后只剩通用措辞，
// 里面写「上游」是运维信息，不该被这条规则误伤。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// bannedInCallerText 是调用方不能看到的词。
//
// 「渠道」「适配器」「上游」会直接说明平台背后还有一层转售；
// 「运营」会让调用方以为要找某个后台同事改配置（他改不了，也没有那个权限）。
// 英文只收 upstream / adapter：channel 是音频接口的合法参数名，
// provider 在 OAuth 语境里是正常词，收进来只会逼出豁免。
var bannedInCallerText = []string{"上游", "渠道", "适配器", "运营", "供应商", "upstream", "adapter"}

// minCallerTexts 是期望扫到的文案条数下限。
//
// 没有它，解析逻辑一旦失效（包名改了、类型名改了）测试会「零命中即通过」，
// 这个门禁就变成了摆设。数字取实测值的八成左右，新增校验时上调。
const minCallerTexts = 100

func TestCallerErrorsAreNeutral(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列举源码失败：%v", err)
	}
	// relay 包自己也可能构造 CallerParamError（如通用请求体解析）。
	relayPaths, err := filepath.Glob("../*.go")
	if err != nil {
		t.Fatalf("列举 relay 源码失败：%v", err)
	}
	paths = append(paths, relayPaths...)

	fset := token.NewFileSet()
	checked := 0
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", p, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok || !isCallerParamError(cl.Type) {
				return true
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Msg" {
					continue
				}
				checked++
				for _, lit := range stringLiterals(kv.Value) {
					for _, w := range bannedInCallerText {
						if strings.Contains(strings.ToLower(lit), strings.ToLower(w)) {
							t.Errorf("%s:%d 对调用方的错误文案含 %q：%q（改写成「本模型/该参数」的说法）",
								p, fset.Position(kv.Pos()).Line, w, lit)
						}
					}
				}
			}
			return true
		})
	}
	if checked < minCallerTexts {
		t.Fatalf("只扫到 %d 条 CallerParamError 文案（期望 ≥ %d）：门禁解析可能已失效", checked, minCallerTexts)
	}
}

// isCallerParamError 判断复合字面量类型是不是 CallerParamError（含 relay. 前缀写法）。
func isCallerParamError(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "CallerParamError"
	case *ast.SelectorExpr:
		return t.Sel.Name == "CallerParamError"
	case *ast.StarExpr:
		return isCallerParamError(t.X)
	}
	return false
}

// stringLiterals 收集表达式里可以被静态判定为文案的字符串常量。
//
// 只收常量而不要求整个表达式都是常量：`field + " 不能为空"` 这种拼接里，
// 中文部分照样可能写「上游」，而 `field` 是运行时才知道的字段名，没必要因此放弃检查。
// 完全由标识符构成的（`Msg: msg`）返回空 —— 那种文案在别处构造，本规则不覆盖。
func stringLiterals(e ast.Expr) []string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return nil
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			// 反引号原文（含换行）用 Unquote 也能处理；真解不出来就按原文比对。
			s = strings.Trim(v.Value, "`\"")
		}
		return []string{s}
	case *ast.BinaryExpr:
		return append(stringLiterals(v.X), stringLiterals(v.Y)...)
	case *ast.ParenExpr:
		return stringLiterals(v.X)
	case *ast.CallExpr:
		var out []string
		for _, a := range v.Args {
			out = append(out, stringLiterals(a)...)
		}
		return out
	}
	return nil
}
