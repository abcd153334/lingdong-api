package task

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// 本文件验证 sort 片段真的到达 SQL：同一批数据按两种排序各查一次，顺序必须不同。
//
// 只断言"handler 把字符串传进来了"是不够的 —— `Order(orderBy)` 写成常量
// `Order("id DESC")` 一样能编译、一样返回 200，只是永远不生效。

// TestListSortReachesSQL 默认 id DESC 与显式 created_at ASC 必须给出相反顺序。
func TestListSortReachesSQL(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// 插入顺序与 created_at 同向：id 越大越新，默认 id DESC 就是"新的在前"。
	for i := 0; i < 3; i++ {
		row := &model.Task{
			UserID: 1, Model: "m", Modality: "video", Status: StatusQueued, Kind: KindVideo,
			CreatedAt: base.Add(time.Duration(i) * time.Hour),
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造任务行失败: %v", err)
		}
	}
	scope := Scope{UserID: 1}

	def, total, err := List(ctx, scope, ListFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("默认列表失败: %v", err)
	}
	if total != 3 || len(def) != 3 {
		t.Fatalf("默认列表条数不符: total=%d len=%d", total, len(def))
	}
	if def[0].CreatedAt.Before(def[2].CreatedAt) {
		t.Fatalf("默认应按 id 倒序（新的在前）: %v", taskTimes(def))
	}

	asc, _, err := List(ctx, scope, ListFilter{Page: 1, PageSize: 10, OrderBy: "created_at ASC, id ASC"})
	if err != nil {
		t.Fatalf("created_at 升序失败: %v", err)
	}
	if !asc[0].CreatedAt.Before(asc[2].CreatedAt) {
		t.Fatalf("created_at ASC 应把最早的排最前: %v", taskTimes(asc))
	}
	if asc[0].ID != def[2].ID {
		t.Fatalf("两种排序首条应互为对方末条：asc[0]=%d def[2]=%d", asc[0].ID, def[2].ID)
	}
}

// TestListCursorIgnoresSort 固定"游标分页与自定义排序互斥"这条约定。
//
// `id < after` 只有在按 id 单调倒序时才成立。如果哪天有人把 OrderBy 也接进
// 游标分支（看起来"顺便统一一下"很自然），翻页就会漏行/重复行，而**第一页永远正常**。
// 所以这里断言：给了 After 就一定是 id DESC，传什么 OrderBy 都不影响。
func TestListCursorIgnoresSort(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)

	rows := make([]model.Task, 0, 3)
	for i := 0; i < 3; i++ {
		row := &model.Task{
			UserID: 2, Model: "m", Modality: "video", Status: StatusQueued, Kind: KindVideo,
			CreatedAt: base.Add(time.Duration(i) * time.Hour),
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造任务行失败: %v", err)
		}
		rows = append(rows, *row)
	}
	// 从最大 id 之前取：只应拿到 id 更小的那些，且仍是 id 倒序。
	newest := rows[2].ID

	got, _, err := List(ctx, Scope{UserID: 2}, ListFilter{
		After: newest, PageSize: 10, OrderBy: "created_at ASC, id ASC",
	})
	if err != nil {
		t.Fatalf("游标分页失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("游标应取到 id 更小的 2 条，实际 %d", len(got))
	}
	if got[0].ID != rows[1].ID || got[1].ID != rows[0].ID {
		t.Fatalf("游标分页必须按 id 倒序（OrderBy 被忽略）：得到 %d,%d", got[0].ID, got[1].ID)
	}
}

// taskTimes 给失败信息一组可读的时间序列。
func taskTimes(rows []model.Task) []time.Time {
	out := make([]time.Time, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.CreatedAt)
	}
	return out
}
