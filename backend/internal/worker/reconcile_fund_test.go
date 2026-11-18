package worker

import (
	"testing"

	"lingdong-api/internal/service/reconcile"
)

// TestFundDiffCountsNegativeFrozen 锁死「负冻结必须进资金档」。
//
// 负冻结的语义是「用户可用余额没少，账上却记着还欠着」—— 凭空多冻了钱。
// 这条判据（reconcile.Users.NegativeFrozen）早就采集了，却既没进资金档也没进观察档，
// 采了没人消费：对账每天跑，天天看不到它。
func TestFundDiffCountsNegativeFrozen(t *testing.T) {
	var rep reconcile.Report
	if got := fundDiffOf(rep); got != 0 {
		t.Fatalf("空报告的资金差异应为 0，实际 %d", got)
	}

	rep.Users.NegativeFrozen = 1
	if got := fundDiffOf(rep); got == 0 {
		t.Fatal("负冻结必须计入资金档：它是「凭空多冻用户的钱」，不是观察项")
	}

	// 单独一条也要能被识别（防止将来重排字段时把它漏掉）。
	rep = reconcile.Report{}
	rep.Users.FrozenMismatch = 1
	if got := fundDiffOf(rep); got != 0 {
		t.Fatalf("冻结不平属于观察档，不应进资金档，实际 %d", got)
	}
}
