package audit

import (
	"strings"
	"testing"
)

// TestAuditCSVNeutralizesFormulaInjection 断言审计导出的单元格不再以 = 起始（不被解析为公式）。
func TestAuditCSVNeutralizesFormulaInjection(t *testing.T) {
	const payload = "=cmd|' /C calc'!A0"
	out := CSV([]Item{{Actor: payload, Action: ActionUserUpdate, TargetType: payload}})
	if !strings.Contains(out, "'=cmd|' /C calc'!A0") {
		t.Fatalf("审计导出未对公式载荷做前缀转义：%s", out)
	}
}
