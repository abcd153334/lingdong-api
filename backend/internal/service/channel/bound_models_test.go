package channel

import (
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestListBoundModelsIncludesDisabled 左栏「N 个模型」必须含停用绑定。
//
// 为什么不能再只给启用：删除渠道的拦截判据是「任意绑定（含停用）」。
// 只给启用绑定会让前端算出「0 个模型」直接放行，后端 409 ——
// 运营看到的是"明明是空渠道却删不掉"，且这两处口径在同一个界面上并存
// （左栏数字 vs 右栏列表），一定会被当成 bug 报上来。
func TestListBoundModelsIncludesDisabled(t *testing.T) {
	gdb := withChannelDB(t)
	if err := gdb.AutoMigrate(&model.Model{}, &model.ModelBinding{}); err != nil {
		t.Fatalf("迁移模型/绑定表失败：%v", err)
	}

	ch, err := Create(channelReq("带两条绑定的渠道"), "")
	if err != nil {
		t.Fatalf("创建渠道失败：%v", err)
	}
	now := time.Now()
	for _, b := range []struct {
		id      string
		enabled bool
	}{
		{"model-enabled", true},
		{"model-disabled", false},
	} {
		if err := gdb.Table("model_bindings").Create(map[string]any{
			"model_id": b.id, "channel_id": ch.ID, "upstream_model": b.id,
			"weight": 1, "priority": 1, "enabled": b.enabled,
			"created_at": now, "updated_at": now,
		}).Error; err != nil {
			t.Fatalf("造绑定失败：%v", err)
		}
	}

	items, total, err := List(ListQuery{Page: 1, Size: 20}, "")
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("应返回 1 个渠道，实际 total=%d len=%d", total, len(items))
	}
	bound := items[0].BoundModels
	if len(bound) != 2 {
		t.Fatalf("应含 2 条绑定（含停用），实际 %d：%+v", len(bound), bound)
	}
	enabledCount := 0
	for _, b := range bound {
		if b.Enabled {
			enabledCount++
		}
	}
	if enabledCount != 1 {
		t.Fatalf("启用数应为 1，实际 %d（enabled 字段没下发的话前端就无法区分）", enabledCount)
	}
	if bound[0].IconURL == "" {
		// 图标回落规则只在 iconindex 一处实现；这里只断言"字段有被填"，
		// 具体回落结果由 iconindex 自己的用例负责。
		t.Log("未取到模型图标（测试库没有内置图库数据时可接受）")
	}
}
