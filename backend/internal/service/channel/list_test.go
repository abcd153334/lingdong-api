package channel

import (
	"errors"
	"fmt"
	"testing"

	"lingdong-api/internal/model"
)

// TestListPagination 断言分页下推的边界：总数、页内条数、越界页。
func TestListPagination(t *testing.T) {
	gdb := withChannelDB(t)
	for i := 1; i <= 25; i++ {
		c := model.Channel{
			Name:      fmt.Sprintf("ch-%02d", i),
			AdapterID: "openai",
			Weight:    i,
			Priority:  i % 3,
			Status:    1,
			AutoBan:   true,
		}
		if err := gdb.Create(&c).Error; err != nil {
			t.Fatalf("造渠道失败: %v", err)
		}
	}

	items, total, err := List(ListQuery{Page: 1, Size: 10}, "")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 25 {
		t.Fatalf("总数应为 25（不是页内条数），实际 %d", total)
	}
	if len(items) != 10 {
		t.Fatalf("第 1 页应为 10 条，实际 %d", len(items))
	}
	if items[0].Name != "ch-01" {
		t.Fatalf("默认按 id 升序，首条应为 ch-01，实际 %s", items[0].Name)
	}

	items, total, err = List(ListQuery{Page: 3, Size: 10}, "")
	if err != nil {
		t.Fatalf("查询第 3 页失败: %v", err)
	}
	if total != 25 || len(items) != 5 {
		t.Fatalf("第 3 页应为 5/25，实际 %d/%d", len(items), total)
	}

	// 越界页：返回空页但总数仍正确（前端据此显示「第 N 页无数据」）
	items, total, err = List(ListQuery{Page: 9, Size: 10}, "")
	if err != nil {
		t.Fatalf("查询越界页失败: %v", err)
	}
	if len(items) != 0 || total != 25 {
		t.Fatalf("越界页应 0 条且总数 25，实际 %d/%d", len(items), total)
	}

	// 单页上限：请求 1000 条被压到 maxPageSize
	items, _, err = List(ListQuery{Page: 1, Size: 1000}, "")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(items) != 25 {
		t.Fatalf("仅 25 条数据时应全部返回，实际 %d", len(items))
	}
}

// TestListSortWhitelist 断言排序只认白名单列，且方向生效。
func TestListSortWhitelist(t *testing.T) {
	gdb := withChannelDB(t)
	for i := 1; i <= 3; i++ {
		c := model.Channel{Name: fmt.Sprintf("s-%d", i), AdapterID: "openai", Weight: i * 10, Priority: 1, Status: 1}
		if err := gdb.Create(&c).Error; err != nil {
			t.Fatalf("造渠道失败: %v", err)
		}
	}
	items, _, err := List(ListQuery{Page: 1, Size: 10, Sort: "weight", Order: "desc"}, "")
	if err != nil {
		t.Fatalf("按权重排序失败: %v", err)
	}
	if items[0].Weight != 30 {
		t.Fatalf("weight desc 首条应为 30，实际 %d", items[0].Weight)
	}

	// 注入尝试必须被白名单挡住
	for _, bad := range []string{"id; DROP TABLE channels", "password", "1=1"} {
		_, _, err := List(ListQuery{Page: 1, Sort: bad}, "")
		if !errors.Is(err, ErrInvalidSort) {
			t.Fatalf("排序字段 %q 应被拒绝，实际 err=%v", bad, err)
		}
	}
}

// TestListFilterAndNoPage 断言筛选与「不分页」兼容路径（适配器页的渠道计数依赖它）。
func TestListFilterAndNoPage(t *testing.T) {
	gdb := withChannelDB(t)
	for i := 1; i <= 6; i++ {
		status := StatusEnabled
		if i%2 == 0 {
			status = StatusDisabled
		}
		c := model.Channel{
			Name:      fmt.Sprintf("f-%d", i),
			AdapterID: "openai",
			Status:    int16(status),
		}
		if err := gdb.Create(&c).Error; err != nil {
			t.Fatalf("造渠道失败: %v", err)
		}
	}
	// 未传 page：返回全部（Adapters 页需要全量计数）
	all, total, err := List(ListQuery{}, "")
	if err != nil {
		t.Fatalf("全量查询失败: %v", err)
	}
	if len(all) != 6 || total != 6 {
		t.Fatalf("不分页应返回全部 6 条，实际 %d/%d", len(all), total)
	}

	// 状态筛选 + 关键词
	got, total, err := List(ListQuery{Keyword: "f-", Status: StatusDisabled}, "")
	if err != nil {
		t.Fatalf("筛选失败: %v", err)
	}
	if len(got) != 3 || total != 3 {
		t.Fatalf("停用渠道应为 3 条，实际 %d/%d", len(got), total)
	}
	for _, it := range got {
		if it.Status != model.ChannelStatusDisabled {
			t.Fatalf("筛选结果包含非停用渠道: %+v", it)
		}
	}
}
