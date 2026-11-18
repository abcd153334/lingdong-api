package relay

import (
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/sorting"
)

// 本文件验证 usagePage 真的把 order 拼进了 SQL。
//
// 这条链路的特殊之处：同一个函数还被 /v1/usage 兼容端点复用，那个端点契约里
// **没有** sort 参数。所以"解析"（谁读 c.Query("sort")）与"执行"（拼 SQL）必须
// 分开测：这里只喂 Order，验证执行侧不丢东西。

// TestUsagePageSortReachesSQL 默认 id DESC 与 charge 升/降序必须互不相同。
func TestUsagePageSortReachesSQL(t *testing.T) {
	gdb := withUsageDB(t)
	// 金额与插入顺序错开（300, 100, 200）：默认 id DESC 得 200,100,300，
	// charge ASC 得 100,200,300，charge DESC 得 300,200,100 —— 三者互不相同。
	seedUsage(t, gdb, 1, 7, "chat", 300, "settled")
	seedUsage(t, gdb, 1, 7, "chat", 100, "settled")
	seedUsage(t, gdb, 1, 7, "chat", 200, "settled")

	// 每次都要新建查询链：Count 之后复用同一条 *gorm.DB 会带上 COUNT 的 select/order。
	query := func() *gorm.DB {
		return usageFilter{}.apply(db.DB.Model(&model.Usage{}).Where("user_id = ?", 1))
	}

	def, total, _, _, err := usagePageOf(t, "page=1&page_size=10", query(), sorting.Order{})
	if err != nil {
		t.Fatalf("默认列表失败: %v", err)
	}
	if total != 3 || len(def) != 3 {
		t.Fatalf("默认列表条数不符: total=%d len=%d", total, len(def))
	}
	if def[0].Charge != 200 || def[2].Charge != 300 {
		t.Fatalf("默认应按 id 倒序: %d,%d,%d", def[0].Charge, def[1].Charge, def[2].Charge)
	}

	asc, _, _, _, err := usagePageOf(t, "page=1&page_size=10", query(),
		sorting.Order{Column: "charge", Desc: false})
	if err != nil {
		t.Fatalf("charge 升序失败: %v", err)
	}
	if asc[0].Charge != 100 || asc[2].Charge != 300 {
		t.Fatalf("charge ASC 应按金额升序: %d,%d,%d", asc[0].Charge, asc[1].Charge, asc[2].Charge)
	}

	desc, _, _, _, err := usagePageOf(t, "page=1&page_size=10", query(),
		sorting.Order{Column: "charge", Desc: true})
	if err != nil {
		t.Fatalf("charge 降序失败: %v", err)
	}
	if desc[0].Charge != 300 || desc[2].Charge != 100 {
		t.Fatalf("charge DESC 应按金额降序: %d,%d,%d", desc[0].Charge, desc[1].Charge, desc[2].Charge)
	}
}
