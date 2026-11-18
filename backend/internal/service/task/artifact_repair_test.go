package task

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// 本文件锁「补转存的候选判定」。
//
// 为什么不测整条补转存：真正的转存在 `transferArtifactAt` 里要打上游 + 写对象存储，
// 单测里既没有对象存储也没有真链接。那条路径已用**真机**验证过（2026-11-16，
// 两个 SUNO 任务各缺一首，工具跑一次把两首补齐、artifact_count 由 1 变 2）。
// 这里守住的是更容易悄悄坏掉的那一半：候选查询 —— 写错一个条件就变成
// 「扫不到」或「每轮全表扫」，而两种都不会报错。

func TestMissingArtifactTasks(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)

	// 缺一条（manifest 2 条、count 1）。
	missing := &model.Task{
		UserID: 1, PublicID: "task_repair_missing", Model: "chirp-hawk", Modality: "music",
		Status: "succeeded", ArtifactCount: 1,
		ArtifactManifest: model.JSON(`[{"i":0,"f":9},{"i":1,"u":"https://cdn.example/a.mp3"}]`),
		CreatedAt:        old, UpdatedAt: old.Add(time.Minute),
	}
	// 完整（不该被扫出来）。
	done := &model.Task{
		UserID: 1, PublicID: "task_repair_done", Model: "chirp-hawk", Modality: "music",
		Status: "succeeded", ArtifactCount: 2,
		ArtifactManifest: model.JSON(`[{"i":0,"f":9},{"i":1,"f":10}]`),
		CreatedAt:        old, UpdatedAt: old.Add(time.Minute),
	}
	// 非数组清单（脏数据/老行）：jsonb_typeof 守卫必须让它安全通过而不是让整条 SQL 报错。
	dirty := &model.Task{
		UserID: 1, PublicID: "task_repair_dirty", Model: "chirp-hawk", Modality: "music",
		Status: "succeeded", ArtifactCount: 0, ArtifactManifest: model.JSON(`{"bad":true}`),
		CreatedAt: old, UpdatedAt: old.Add(time.Minute),
	}
	// 不在窗口内（太老）：补不上，别每轮都试。
	stale := &model.Task{
		UserID: 1, PublicID: "task_repair_stale", Model: "chirp-hawk", Modality: "music",
		Status: "succeeded", ArtifactCount: 0,
		ArtifactManifest: model.JSON(`[{"i":0,"u":"https://cdn.example/b.mp3"}]`),
		CreatedAt:        old.Add(-48 * time.Hour), UpdatedAt: old.Add(-24 * time.Hour),
	}
	for _, row := range []*model.Task{missing, done, dirty, stale} {
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("建任务失败: %v", err)
		}
	}

	rows, err := MissingArtifactTasks(ctx, time.Now().Add(-ArtifactRepairWindow), 20)
	if err != nil {
		t.Fatalf("候选查询失败: %v", err)
	}
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.PublicID)
	}
	if len(got) != 1 || got[0] != missing.PublicID {
		t.Fatalf("候选应只有 %s，实际 %v", missing.PublicID, got)
	}

	// 清单已齐的任务：直接返回 0，不触发任何网络/存储调用。
	var fresh model.Task
	if err := gdb.Where("public_id = ?", done.PublicID).First(&fresh).Error; err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	n, rerr := RepairArtifacts(ctx, &fresh)
	if n != 0 || rerr != nil {
		t.Fatalf("完整任务不该补任何东西：n=%d err=%v", n, rerr)
	}

	// 空清单（老行）：也必须是 0 而不是报错。
	empty := &model.Task{
		UserID: 1, PublicID: "task_repair_empty", Model: "chirp-hawk", Modality: "music",
		Status: "succeeded", CreatedAt: old, UpdatedAt: old.Add(time.Minute),
	}
	if err := gdb.Create(empty).Error; err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if n, rerr := RepairArtifacts(ctx, empty); n != 0 || rerr != nil {
		t.Fatalf("空清单应是 no-op：n=%d err=%v", n, rerr)
	}
}
