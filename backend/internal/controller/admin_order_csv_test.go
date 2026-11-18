package controller

import (
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/service/order"
)

// TestOrderCSVColumns 锁死订单导出的列契约。
//
// 为什么需要这个用例：表头是**对账脚本的接口**。少一列不会报错，只会让运维把
// 「下单时倍率」读成「套餐」，或者把金额读成币种 —— 而且这种错要在 Excel 里
// 人工发现。改表头必须同时改这里、`web/e2e/admin-orders.spec.ts` 的桩件、
// 以及 `docs/充值套餐与额度口径.md` §5。
func TestOrderCSVColumns(t *testing.T) {
	paidAt := time.Date(2026, 10, 24, 10, 30, 0, 0, time.UTC)
	items := []order.View{{
		ID: 1, UserID: 7, OutTradeNo: "LD2026102400000000001",
		AmountCent: 7200, Currency: "CNY", Credited: 79_200_000,
		PackageName: "标准包", Status: "paid",
		PayChannel: "易支付-微信支付", PayChannelID: "epay", PayProtocol: "epay",
		PayMethod: "wechat", CreatedAt: paidAt, PaidAt: &paidAt,
	}}
	csv := orderCSV(items, map[uint64]string{7: "alice"})
	lines := strings.Split(strings.TrimRight(csv, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 1 行表头 + 1 行数据，实际 %d 行", len(lines))
	}

	header := strings.Split(lines[0], ",")
	row := strings.Split(lines[1], ",")
	if len(header) != len(row) {
		t.Fatalf("表头 %d 列 ≠ 数据 %d 列（对账脚本会错列）", len(header), len(row))
	}

	// 列顺序是契约：对账脚本按索引取列。
	// 「额度 / 比例 / 倍率」三列已合并成「到账金额」一列 —— 它们本来就是同一个数的三种写法，
	// 分开存只会让对账时先乘回去。
	want := []string{"订单号", "用户ID", "用户名", "金额", "币种", "到账金额", "套餐"}
	for i, w := range want {
		if i >= len(header) || header[i] != w {
			t.Fatalf("第 %d 列表头应为 %q，实际 %q（完整表头：%s）", i+1, w, header[i], lines[0])
		}
	}

	// 到账金额按两位小数输出（微 → 十进制）：79,200,000 微 = 79.20。
	if got := row[5]; got != "79.20" {
		t.Fatalf("到账金额应输出两位小数 79.20，实际 %q", got)
	}
	if got := row[3]; got != "72.00" {
		t.Fatalf("金额列（分→元）应为 72.00，实际 %q", got)
	}
}
