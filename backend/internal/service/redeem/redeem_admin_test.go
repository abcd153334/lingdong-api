package redeem

import (
	"context"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestStatusOf 覆盖状态派生的唯一实现：已用 > 已过期 > 可用。
func TestStatusOf(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	uid := uint64(7)

	if got := StatusOf(model.RedeemCode{}, now); got != StatusAvailable {
		t.Fatalf("无到期时间应为可用: %s", got)
	}
	if got := StatusOf(model.RedeemCode{ExpiresAt: &future}, now); got != StatusAvailable {
		t.Fatalf("未到期应为可用: %s", got)
	}
	if got := StatusOf(model.RedeemCode{ExpiresAt: &past}, now); got != StatusExpired {
		t.Fatalf("已过到期时间应为过期: %s", got)
	}
	// 已用优先于过期：两者同时命中仍判「已用」。
	if got := StatusOf(model.RedeemCode{UsedBy: &uid, ExpiresAt: &past}, now); got != StatusUsed {
		t.Fatalf("已用应优先于过期: %s", got)
	}
}

// TestGenerateAndList 覆盖批量生成（格式/条数/落库）与按批次+状态筛选、分页。
func TestGenerateAndList(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	exp := time.Now().Add(48 * time.Hour)
	codes, err := svc.Generate(ctx, GenerateInput{Batch: "batch-A", Amount: 5000, Count: 3, ExpiresAt: &exp})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(codes) != 3 {
		t.Fatalf("生成条数不符: %d", len(codes))
	}
	// 回码必须与落库逐条一致：运营直接把这批码发出去，少一条或多一条都要立刻发现。
	codeSet := map[string]bool{}
	for _, c := range codes {
		if codeSet[c] {
			t.Fatalf("返回列表里出现重复码: %s", c)
		}
		codeSet[c] = true
	}

	var rows []model.RedeemCode
	if err := gdb.Where("batch = ?", "batch-A").Find(&rows).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("落库条数不符: %d", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if !strings.HasPrefix(r.Code, "LD-") || len(r.Code) != 27 {
			t.Fatalf("码格式不符: %s", r.Code)
		}
		if r.Code != strings.ToUpper(r.Code) {
			t.Fatalf("码应全大写: %s", r.Code)
		}
		if r.Amount != 5000 || r.Batch != "batch-A" || r.ExpiresAt == nil {
			t.Fatalf("字段落库不符: %+v", r)
		}
		if seen[r.Code] {
			t.Fatalf("同批次出现重复码: %s", r.Code)
		}
		seen[r.Code] = true
		if !codeSet[r.Code] {
			t.Fatalf("落库的码 %s 不在 Generate 的返回列表里", r.Code)
		}
	}

	// 追加第二批：可用 2 条 + 已用 1 条
	if _, err := svc.Generate(ctx, GenerateInput{Batch: "batch-B", Amount: 100, Count: 2}); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	uid := uint64(9)
	seedCode(t, gdb, "USED-IN-B-BATCH", 100, nil, &uid)
	var used model.RedeemCode
	if err := gdb.Where("code = ?", "USED-IN-B-BATCH").First(&used).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if err := gdb.Model(&model.RedeemCode{}).Where("id = ?", used.ID).Update("batch", "batch-B").Error; err != nil {
		t.Fatalf("改批次失败: %v", err)
	}

	items, total, err := svc.List(ctx, ListInput{Batch: "batch-B", Status: StatusAvailable, Offset: 0, Limit: 10})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("批次+可用筛选不符: total=%d len=%d", total, len(items))
	}
	for _, it := range items {
		if it.Status != StatusAvailable {
			t.Fatalf("状态不符: %s", it.Status)
		}
	}

	// 不分批次的已用筛选：命中 USED-IN-B-BATCH 一条
	usedItems, usedTotal, err := svc.List(ctx, ListInput{Status: StatusUsed, Offset: 0, Limit: 10})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if usedTotal != 1 || usedItems[0].Code != "USED-IN-B-BATCH" || usedItems[0].UsedBy == nil {
		t.Fatalf("已用筛选不符: total=%d items=%+v", usedTotal, usedItems)
	}

	// 过期筛选：造一条已过期
	past := time.Now().Add(-time.Hour)
	seedCode(t, gdb, "EXPIRED-ONE-XX", 100, &past, nil)
	expItems, expTotal, err := svc.List(ctx, ListInput{Status: StatusExpired, Offset: 0, Limit: 10})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if expTotal != 1 || expItems[0].Code != "EXPIRED-ONE-XX" || expItems[0].Status != StatusExpired {
		t.Fatalf("过期筛选不符: total=%d items=%+v", expTotal, expItems)
	}

	// 分页：batch-A 共 3 条，取第 2 页每页 2 → 1 条
	pItems, pTotal, err := svc.List(ctx, ListInput{Batch: "batch-A", Offset: 2, Limit: 2})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if pTotal != 3 || len(pItems) != 1 {
		t.Fatalf("分页不符: total=%d len=%d", pTotal, len(pItems))
	}
}

// TestDeleteBatch 覆盖：仅删未使用、跳过已用、不存在 id 幂等、空集返回 0。
func TestDeleteBatch(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	if _, err := svc.Generate(ctx, GenerateInput{Batch: "del", Amount: 10, Count: 2}); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	var rows []model.RedeemCode
	if err := gdb.Where("batch = ?", "del").Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	uid := uint64(3)
	if err := gdb.Model(&model.RedeemCode{}).Where("id = ?", rows[0].ID).
		Updates(map[string]any{"used_by": uid, "used_at": time.Now()}).Error; err != nil {
		t.Fatalf("标记已用失败: %v", err)
	}

	deleted, skipped, err := svc.DeleteBatch(ctx, []uint64{rows[0].ID, rows[1].ID, 999999})
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if deleted != 1 || skipped != 2 {
		t.Fatalf("删除计数不符: deleted=%d skipped=%d", deleted, skipped)
	}

	// 已用码未被软删除
	var left model.RedeemCode
	if err := gdb.Where("id = ?", rows[0].ID).First(&left).Error; err != nil {
		t.Fatalf("已用码不应被删除: %v", err)
	}
	// 未用码应软删除（默认作用域查不到，Unscoped 查得到且 DeletedAt 非空）
	var gone model.RedeemCode
	if err := gdb.Where("id = ?", rows[1].ID).First(&gone).Error; err == nil {
		t.Fatalf("未用码应被软删除")
	}
	var unscoped model.RedeemCode
	if err := gdb.Unscoped().Where("id = ?", rows[1].ID).First(&unscoped).Error; err != nil {
		t.Fatalf("软删除行应仍可追溯: %v", err)
	}
	if !unscoped.DeletedAt.Valid {
		t.Fatalf("deleted_at 应已写入")
	}

	// 幂等：重复删除同一批 → 全部跳过
	deleted, skipped, err = svc.DeleteBatch(ctx, []uint64{rows[1].ID, 999999})
	if err != nil || deleted != 0 || skipped != 2 {
		t.Fatalf("重复删除应全部跳过: deleted=%d skipped=%d err=%v", deleted, skipped, err)
	}

	// 空集：不报错、不产生变更
	if deleted, skipped, err = svc.DeleteBatch(ctx, nil); err != nil || deleted != 0 || skipped != 0 {
		t.Fatalf("空集应为空操作: deleted=%d skipped=%d err=%v", deleted, skipped, err)
	}
}

// TestCSV 覆盖导出表头与字段转义（批次含逗号）。
func TestCSV(t *testing.T) {
	now := time.Now()
	items := []Item{
		{ID: 1, Code: "LD-AAA", Batch: "a,b", Amount: 100, Status: StatusAvailable, CreatedAt: now},
	}
	out := CSV(items)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应为表头+1 行: %q", out)
	}
	if lines[0] != "Code,Batch,Amount,Status,UsedBy,UsedAt,ExpiresAt,CreatedAt" {
		t.Fatalf("表头不符: %s", lines[0])
	}
	if !strings.HasPrefix(lines[1], `LD-AAA,"a,b",100,available,,,`) {
		t.Fatalf("字段转义/取值不符: %s", lines[1])
	}
}
