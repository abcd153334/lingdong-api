package order

import (
	"context"
	"strconv"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// 本文件只验证一件事：**sort 片段真的改变了 SQL 的执行顺序**。
//
// 为什么不能只靠"handler 传了字符串"来证明：
//
//	把 OrderBy 抄错一个字母（`Order(f.OrderBy)` 写成 `Order("id DESC")`）
//	编译一样过、接口一样 200、文档一样好看 —— 只是顺序永远不变。
//	真实接通与否，只有让同一批数据按两种排序各查一次、再比对顺序才能确认。

// TestOrderListSortReachesSQL 默认（id DESC）与显式 created_at ASC 顺序必须**相反**。
func TestOrderListSortReachesSQL(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	expire := time.Now().Add(time.Hour)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// 插入顺序与 created_at 同向：id 越大 = 越新。默认 id DESC 恰好是"新的在前"，
	// 于是只要显式排序生效，结果就一定是插入倒序。
	for i := 0; i < 3; i++ {
		o := &model.Order{
			UserID: 1, OutTradeNo: "SORT-" + strconv.Itoa(i),
			AmountCent: int64(100 + i), Credited: int64(100 + i),
			Status: "pending", ExpireAt: &expire, CreatedAt: base.Add(time.Duration(i) * time.Hour),
		}
		if err := gdb.Create(o).Error; err != nil {
			t.Fatalf("造订单行失败: %v", err)
		}
	}
	svc := New(gdb)

	def, total, err := svc.List(ctx, ListFilter{UserID: 1, Limit: 10})
	if err != nil {
		t.Fatalf("默认列表失败: %v", err)
	}
	if total != 3 || len(def) != 3 {
		t.Fatalf("默认列表条数不符: total=%d len=%d", total, len(def))
	}
	if def[0].CreatedAt.Before(def[2].CreatedAt) {
		t.Fatalf("默认应按 id 倒序（新的在前），实际 %v", times(def))
	}

	asc, _, err := svc.List(ctx, ListFilter{UserID: 1, Limit: 10, OrderBy: "created_at ASC, id ASC"})
	if err != nil {
		t.Fatalf("created_at 升序失败: %v", err)
	}
	if !asc[0].CreatedAt.Before(asc[2].CreatedAt) {
		t.Fatalf("created_at ASC 应把最早的排在最前，实际 %v", times(asc))
	}
	if asc[0].ID != def[2].ID {
		t.Fatalf("两种排序的首条应分别是对方的最末条：asc[0]=%d def[2]=%d", asc[0].ID, def[2].ID)
	}
}

// TestOrderListSortTieBreaksOnID 保证"排序值相同的行"也有稳定次序。
//
// 这是分页正确性的前提：三行 created_at 完全相同时，Postgres 不保证相对顺序，
// 而分页是 OFFSET/LIMIT —— 没有 id 次序键时，翻页会重复或漏掉行，
// 且只在数据量和同值情况凑巧时才出现，几乎无法在生产日志里看出来。
func TestOrderListSortTieBreaksOnID(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	expire := time.Now().Add(time.Hour)
	same := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		o := &model.Order{
			UserID: 2, OutTradeNo: "TIE-" + strconv.Itoa(i),
			AmountCent: 100, Credited: 100,
			Status: "pending", ExpireAt: &expire, CreatedAt: same,
		}
		if err := gdb.Create(o).Error; err != nil {
			t.Fatalf("造订单行失败: %v", err)
		}
	}
	svc := New(gdb)

	seen := map[uint64]bool{}
	for page := 0; page < 2; page++ {
		items, _, err := svc.List(ctx, ListFilter{
			UserID: 2, Offset: page * 2, Limit: 2, OrderBy: "created_at DESC, id DESC",
		})
		if err != nil {
			t.Fatalf("第 %d 页失败: %v", page, err)
		}
		for _, it := range items {
			if seen[it.ID] {
				t.Fatalf("第 %d 页出现重复行 id=%d（缺 id 次序键）", page, it.ID)
			}
			seen[it.ID] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("两页合计应覆盖 3 条，实际 %d", len(seen))
	}
}

// TestTransactionsSortReachesSQL 流水按金额排序必须真的按 amount 排。
//
// 用 amount 而不是 created_at：金额是"业务上想排、但列名与对外字段名一致"
// 的少数情况之一，能同时证明列名映射没写错。
func TestTransactionsSortReachesSQL(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)

	// 金额刻意与插入顺序错开（10, 30, 20）：这样 id DESC 得到 20,30,10，
	// amount ASC 得到 10,20,30，两种顺序**首条就不同**。
	// 若金额恰好随 id 递增，两种排序会给出同一个结果，用例就永远绿 —
	// 哪怕 OrderBy 根本没进 SQL。
	for i, amt := range []int64{10, 30, 20} {
		tx := &model.Transaction{
			UserID: 3, Type: "recharge", Amount: amt, BalanceAfter: amt,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := gdb.Create(tx).Error; err != nil {
			t.Fatalf("造流水行失败: %v", err)
		}
	}
	svc := New(gdb)

	def, total, err := svc.Transactions(ctx, 3, "", 0, 10, "")
	if err != nil {
		t.Fatalf("默认流水失败: %v", err)
	}
	if total != 3 || len(def) != 3 {
		t.Fatalf("默认流水条数不符: total=%d len=%d", total, len(def))
	}
	if def[0].Amount != 20 || def[2].Amount != 10 {
		t.Fatalf("默认应按 id 倒序: %d,%d,%d", def[0].Amount, def[1].Amount, def[2].Amount)
	}

	asc, _, err := svc.Transactions(ctx, 3, "", 0, 10, "amount ASC, id ASC")
	if err != nil {
		t.Fatalf("金额升序失败: %v", err)
	}
	if asc[0].Amount != 10 || asc[2].Amount != 30 {
		t.Fatalf("amount ASC 应按金额升序: %d,%d,%d", asc[0].Amount, asc[1].Amount, asc[2].Amount)
	}
}

// times 给失败信息一组可读的时间序列（只看 ID 看不出"顺序反了还是没变"）。
func times(vs []View) []time.Time {
	out := make([]time.Time, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.CreatedAt)
	}
	return out
}
