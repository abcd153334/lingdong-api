package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// TestActionsTableIntegrity 遍历常量表：表内动作必须全部通过校验（无表外/无效项）且不重复；
// 门户自助动作（key.*）必须**不在**可写集合内（§4.9.8 注：门户操作不写 admin_audit）。
func TestActionsTableIntegrity(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range AllowedActions {
		if a == "" {
			t.Fatal("动作表存在空值")
		}
		if seen[a] {
			t.Fatalf("动作表存在重复项: %q", a)
		}
		seen[a] = true
		if !Valid(a) {
			t.Fatalf("动作表内的动作未被校验函数接受: %q", a)
		}
		got, err := Normalize(a)
		if err != nil || got != a {
			t.Fatalf("Normalize(%q) = (%q, %v)，期望原样返回", a, got, err)
		}
	}
	for _, a := range PortalActions {
		if Valid(a) {
			t.Fatalf("门户自助动作 %q 不得进入 admin_audit 可写集合", a)
		}
		if _, err := Normalize(a); err == nil {
			t.Fatalf("门户自助动作 %q 不应被 Normalize 接受", a)
		}
	}
}

// TestRecordRejectsUnknownAction 断言表外动作被拒且不落库；权威动作原样落库（无别名兼容层）。
func TestRecordRejectsUnknownAction(t *testing.T) {
	gdb := withTestDB(t)

	if err := Record(gdb, 1, "not.an.action", "x", "1", nil); err == nil {
		t.Fatal("表外动作应被拒绝")
	}
	// 历史字面量（既非权威动作）同样必须被拒，不得静默收敛
	if err := Record(gdb, 1, "model.toggle", "model", "7", nil); err == nil {
		t.Fatal("非权威字面量 model.toggle 应被拒绝（调用方须使用权威常量）")
	}
	var n int64
	if err := gdb.Model(&model.AdminAudit{}).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("被拒动作不应落库，实际 %d 行", n)
	}

	// 权威常量原样落库
	if err := Record(gdb, 1, ActionModelBatchStatus, "model", "7", nil); err != nil {
		t.Fatalf("权威动作应入库: %v", err)
	}
	var row model.AdminAudit
	if err := gdb.First(&row).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if row.Action != ActionModelBatchStatus {
		t.Fatalf("权威动作未原样落库: got %q want %q", row.Action, ActionModelBatchStatus)
	}
}

// TestEveryActionConstantIsAllowed 遍历本包**源码里的全部 `Action*` 常量**，
// 断言每个都出现在 AllowedActions 或 PortalActions 里。
//
// 为什么必须扫源码而不是只看表：现有用例只校验「表内自洽」，于是
// 「新加了一个常量、忘了登记进表」在 CI 里完全看不出来 —— 而它的真实表现是
// 调用点拿到 ErrUnknownAction、被吞成一条 ERROR 日志，admin_audit 里
// 一条记录都没有（v7 的 9 个 docs.* 动作就这样漏了整整一轮）。
func TestEveryActionConstantIsAllowed(t *testing.T) {
	declared := actionConstantsOf(t)
	if len(declared) < 40 {
		t.Fatalf("只解析到 %d 个 Action 常量，解析器可能失效（路径或 AST 变了）", len(declared))
	}
	allowed := map[string]bool{}
	for _, a := range AllowedActions {
		allowed[a] = true
	}
	portal := map[string]bool{}
	for _, a := range PortalActions {
		portal[a] = true
	}
	for _, name := range declared {
		if !allowed[name] && !portal[name] {
			t.Errorf("常量 %s 既不在 AllowedActions 也不在 PortalActions：调用点会被 ErrUnknownAction 静默吞掉", name)
		}
	}
}

// actionConstantsOf 解析 actions.go 取所有字符串型 `Action*` 常量的**取值**。
func actionConstantsOf(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "actions.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 actions.go 失败: %v", err)
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Action") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("常量 %s 的取值无法解析: %v", n.Name, err)
				}
				out = append(out, v)
			}
		}
	}
	return out
}
