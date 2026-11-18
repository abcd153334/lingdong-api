package db

import (
	"strings"
	"testing"
)

// TestLedgerMicro 锁死「额度 → 微」的换算锚。
//
// 换算只在这里改钱，写错一个系数就是全站余额错一个数量级。
// 参考点：8,857,058 额度 ≈ ¥127.5（旧刻度 1 CNY = 69444 额度）。
func TestLedgerMicro(t *testing.T) {
	cases := []struct {
		name  string
		quota int64
		fx    float64
		want  int64
	}{
		{"人民币站：6000 额度 = ¥0.0864", 6000, 7.2, 86_400},
		{"人民币站：69444 额度 ≈ ¥1", 69444, 7.2, 999_994},
		{"人民币站：8857058 额度 ≈ ¥127.54", 8_857_058, 7.2, 127_541_635},
		{"美元站：500000 额度 = $1", 500_000, 1, 1_000_000},
		{"0 额度 = 0 微", 0, 7.2, 0},
	}
	for _, c := range cases {
		if got := ledgerMicro(c.quota, c.fx); got != c.want {
			t.Errorf("%s：want %d, got %d", c.name, c.want, got)
		}
	}
	// 负数（欠费）必须原样保号，取绝对值就变成"白送"。
	if got := ledgerMicro(-1500, 7.2); got != -21_600 {
		t.Fatalf("负额度必须保持负号，got %d", got)
	}
}

// TestLedgerWhere 守「重跑不二次放大」。
//
// 判据写错的后果很贵：漏掉 `new = 0` 条件 → 重跑时把已迁移的值再乘 14.4，
// 余额凭空放大一个数量级，而且没有任何报错。
func TestLedgerWhere(t *testing.T) {
	p := ledgerPair{Table: "users", Old: "quota", New: "balance"}
	if got := ledgerWhere(p); got != "balance = 0 AND quota <> 0" {
		t.Fatalf("判据错: %s", got)
	}
}

// TestTransactionsExtraColumns 守一次真实事故：
// `balance_after` / `frozen_after` 没有改名，一直有旧值，无法用「新列 = 0」
// 判断是否迁过。第一版把她们当独立条目，条件 `balance_after = 0` 永不成立 ——
// 结果余额搬好了、流水金额搬好了，只有 balance_after 还停在旧额度上。
// 所以她们必须挤在同一条 UPDATE 里，随 `amount_quota → amount` 一起换算。
func TestTransactionsExtraColumns(t *testing.T) {
	var tx ledgerPair
	for _, p := range ledgerPairs {
		if p.Table == "transactions" {
			tx = p
		}
	}
	if tx.New != "amount" || tx.Old != "amount_quota" {
		t.Fatalf("transactions 的搬迁条目应挂在 amount 上，实际 %+v", tx)
	}
	if len(tx.AlsoConvert) != 2 {
		t.Fatalf("balance_after / frozen_after 必须随行换算，实际 %+v", tx.AlsoConvert)
	}
}

// TestLedgerExprUsesNumeric 守住「换算不许用 float 中间量」。
func TestLedgerExprUsesNumeric(t *testing.T) {
	got := ledgerExpr("quota")
	if !strings.Contains(got, "round(quota::numeric") {
		t.Fatalf("换算表达式错: %s", got)
	}
	if strings.Contains(got, "float") {
		t.Fatal("换算不能用 float 中间量（千万级额度会丢精度）")
	}
}
