package redeem

import (
	"strings"
	"testing"
	"time"
)

// TestRedeemCSVNeutralizesFormulaInjection 断言兑换码导出（批次名可自由填写）不再以 = 起始。
func TestRedeemCSVNeutralizesFormulaInjection(t *testing.T) {
	const payload = "=cmd|' /C calc'!A0"
	out := CSV([]Item{{Code: "ABCD2345", Batch: payload, Amount: 100, Status: "unused", CreatedAt: time.Unix(0, 0)}})
	if !strings.Contains(out, "'=cmd|' /C calc'!A0") {
		t.Fatalf("兑换码导出未对公式载荷做前缀转义：%s", out)
	}
}
