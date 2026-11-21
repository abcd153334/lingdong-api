package order

// 应用维度的四象限：`AppID` 与 `AppScoped` 是**两个**字段，组合语义必须被钉死。
//
// 为什么不能只测"筛某个应用能筛出来"：
//   - `AppID=0` 在旧语义里表示"不按应用过滤"（门户/会话票下单路径依赖它），
//     如果把它改成"筛平台直连"，那两条路径会突然只看自充订单，列表少一大半；
//   - 新增 `AppScoped` 之后，`(AppID=0, AppScoped=false)` 与 `(0,true)` 的结论相反，
//     任何一次"顺手简化"都会把其中一个悄悄改掉，而接口仍然 200。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

func TestOrderListAppFilterQuadrants(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	// 专用 user_id：本包的集成用例共用一个 schema 且不互相清表，
	// 按用户隔离才能保证 total 断言不被别的用例影响。
	const uid = 4242
	expire := time.Now().Add(time.Hour)
	for _, appID := range []uint64{0, 5, 9} {
		o := &model.Order{
			UserID: uid, AppID: appID,
			OutTradeNo: fmt.Sprintf("APPQ-%d", appID),
			AmountCent: 100, Credited: 100, Status: "pending", ExpireAt: &expire,
		}
		if err := gdb.Create(o).Error; err != nil {
			t.Fatalf("造订单失败: %v", err)
		}
	}
	svc := New(gdb)

	run := func(name string, f ListFilter) []uint64 {
		t.Helper()
		f.UserID, f.Limit = uid, 10
		items, total, err := svc.List(ctx, f)
		if err != nil {
			t.Fatalf("%s: 查询失败 %v", name, err)
		}
		if int(total) != len(items) {
			t.Fatalf("%s: total(%d) 与页内条数(%d) 不一致", name, total, len(items))
		}
		ids := make([]uint64, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.AppID)
		}
		return ids
	}

	cases := []struct {
		name   string
		filter ListFilter
		want   []uint64
	}{
		{
			name:   "未传 app_id：不过滤（含平台直连）",
			filter: ListFilter{},
			want:   []uint64{9, 5, 0},
		},
		{
			// 旧语义必须保持：门户/会话票把 AppID=0 传进来表示"不按应用过滤"。
			name:   "AppID=0 且未 scoped：仍是不过滤",
			filter: ListFilter{AppID: 0},
			want:   []uint64{9, 5, 0},
		},
		{
			name:   "只看平台直连",
			filter: ListFilter{AppID: 0, AppScoped: true},
			want:   []uint64{0},
		},
		{
			name:   "只看某应用",
			filter: ListFilter{AppID: 5, AppScoped: true},
			want:   []uint64{5},
		},
		{
			name:   "AppID 非 0 时 scoped 可省（历史调用点）",
			filter: ListFilter{AppID: 9},
			want:   []uint64{9},
		},
	}
	for _, tc := range cases {
		got := run(tc.name, tc.filter)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: 命中 %v，期望 %v", tc.name, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: 命中 %v，期望 %v（顺序为 id 倒序）", tc.name, got, tc.want)
			}
		}
	}
}
