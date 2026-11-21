package task

// 应用维度的两半：**筛得准**（List 的 key_id 子查询）与**显示得对**（FillApp）。
//
// 为什么两半都要测：只测筛选时，页面上应用列可能是空的；只测补名时，
// 筛选可能根本没生效（参数被忽略，列表还是全量）—— 两者都不会报错。

import (
	"context"
	"testing"

	"lingdong-api/internal/model"
)

func TestTaskListAppFilterAndFill(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()

	app := &model.App{Slug: "portal", Name: "官网门户", DisplayName: "官网门户", Status: model.AppStatusEnabled, AllowSelfIssue: true}
	if err := gdb.Create(app).Error; err != nil {
		t.Fatalf("造应用失败: %v", err)
	}
	const uid = 5151
	hosted := &model.APIKey{UserID: uid, AppID: app.ID, Kind: model.KeyKindHosted, KeyHash: "task-app-h1"}
	direct := &model.APIKey{UserID: uid, AppID: 0, Kind: model.KeyKindUser, KeyHash: "task-app-d1"}
	for _, k := range []*model.APIKey{hosted, direct} {
		if err := gdb.Create(k).Error; err != nil {
			t.Fatalf("造密钥失败: %v", err)
		}
	}
	for i, keyID := range []uint64{hosted.ID, direct.ID} {
		row := &model.Task{UserID: uid, KeyID: keyID, Status: "queued", Modality: "text", Model: "m"}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造任务 %d 失败: %v", i, err)
		}
	}

	// ① 不筛：两条都在，且 total 与页内一致。
	rows, total, err := List(ctx, AllScope(), ListFilter{UserID: uid, PageSize: 10})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("不筛应有 2 条，实际 total=%d len=%d", total, len(rows))
	}

	// ② 只看该应用：total 必须跟着变（Count 与 Find 共用 build()）。
	rows, total, err = List(ctx, AllScope(), ListFilter{UserID: uid, PageSize: 10, AppID: app.ID, AppScoped: true})
	if err != nil {
		t.Fatalf("按应用筛选失败: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].KeyID != hosted.ID {
		t.Fatalf("按应用筛选应只剩 hosted 那条，实际 total=%d rows=%+v", total, rows)
	}

	// ③ 只看平台直连：这是 AppScoped 存在的理由（0 不能等价于"没传"）。
	_, total, err = List(ctx, AllScope(), ListFilter{UserID: uid, PageSize: 10, AppScoped: true})
	if err != nil {
		t.Fatalf("按平台直连筛选失败: %v", err)
	}
	if total != 1 {
		t.Fatalf("平台直连应只有 1 条，实际 %d", total)
	}

	// ④ 补名：Hosted 有应用名，平台直连的 app_id=0 且名字为空。
	views := make([]AdminView, 0, len(rows))
	rows, _, _ = List(ctx, AllScope(), ListFilter{UserID: uid, PageSize: 10})
	for i := range rows {
		views = append(views, AdminViewOf(&rows[i], "/api/admin/tasks", nil))
	}
	FillApp(ctx, views)
	byKey := map[uint64]AdminView{}
	for _, v := range views {
		byKey[v.KeyID] = v
	}
	if got := byKey[hosted.ID]; got.AppID != app.ID || got.AppName != "官网门户" {
		t.Fatalf("托管密钥任务的应用应为 %d/官网门户，实际 %d/%q", app.ID, got.AppID, got.AppName)
	}
	if got := byKey[direct.ID]; got.AppID != 0 || got.AppName != "" {
		t.Fatalf("平台直连任务应为 0/空，实际 %d/%q", got.AppID, got.AppName)
	}
}
