package logs

import (
	"strings"
	"testing"
	"time"
)

// TestEscPreventsFormulaInjection 断言以 = + - @ / Tab / CR 起始的单元格被前置单引号（P2-11）。
func TestEscPreventsFormulaInjection(t *testing.T) {
	cases := map[string]string{
		"=cmd|' /C calc'!A0": "'=cmd|' /C calc'!A0",
		"+1+1":               "'+1+1",
		"-2+3":               "'-2+3",
		"@SUM(A1)":           "'@SUM(A1)",
		"\tx":                "'\tx",
		"\rx":                "\"'\rx\"",
		"normal":             "normal",
		"a,b":                "\"a,b\"",
		"a\"b":               "\"a\"\"b\"",
	}
	for in, want := range cases {
		if got := Esc(in); got != want {
			t.Fatalf("Esc(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLogsCSVNeutralizesFormulaInjection 断言日志导出中的公式载荷不再以 = 起始（不被解析为公式）。
func TestLogsCSVNeutralizesFormulaInjection(t *testing.T) {
	const payload = "=cmd|' /C calc'!A0"
	out := CSV([]Item{{CreatedAt: time.Unix(0, 0), Username: payload, Model: payload, Status: "success"}})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("导出应含表头 + 1 行数据，实际 %d 行", len(lines))
	}
	if !strings.Contains(lines[1], "'=cmd|' /C calc'!A0") {
		t.Fatalf("公式载荷未被前缀转义：%s", lines[1])
	}
}
