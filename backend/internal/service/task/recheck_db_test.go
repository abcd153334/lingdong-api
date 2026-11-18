package task

// 终态复查窗口（W1）的数据库集成用例。
//
// 纯函数用例（recheck_test.go）只能证明「字段映射写对了」，
// 而这条链路最容易出错的地方恰恰都在**写库那一刻**：
//   - 开窗字段有没有真的随 CAS 落库（写进 extra 但 cas 忘了合并 = 静默不开窗）；
//   - 调用方拿着过期视图（内存里 upstream_task_id 为空）时会不会漏开窗；
//   - 迟到推进的 CAS 起点是否真的与正常推进互斥。
// 这三件都不会在编译期或纯函数层面暴露，只能在真实库上跑。

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
)

// emptyTaskStatus 是「上游说成功但没给任何产物链接」的状态。
//
// 用它而不是造一个带链接的状态，是因为本文件的用例**不测产物转存**
// （那要连网下载），只测状态推进与窗口收尾：空的产物清单让
// 「全失败闸门」不触发（该闸门只在上游给了产物、却一个都没转存成功时生效）。
func emptyTaskStatus() *relay.TaskStatus {
	return &relay.TaskStatus{Status: StatusSucceeded, StatusMatched: true}
}

// seedRecheckTask 造一条任务行（默认 queued + 有上游单号）。
func seedRecheckTask(t *testing.T, gdb *gorm.DB, mut func(*model.Task)) *model.Task {
	t.Helper()
	row := &model.Task{
		UserID: 1, Model: "m", Kind: KindVideo,
		Status: StatusQueued, UpstreamTaskID: "up-1",
		PricingSnapshot: model.MustJSON(map[string]any{}),
	}
	if mut != nil {
		mut(row)
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造任务行失败: %v", err)
	}
	return row
}

// reloadRecheck 取最新一行（断言一律读库，不读内存，否则测的是自己写的变量）。
func reloadRecheck(t *testing.T, gdb *gorm.DB, id uint64) model.Task {
	t.Helper()
	var got model.Task
	if err := gdb.First(&got, id).Error; err != nil {
		t.Fatalf("回读任务失败: %v", err)
	}
	return got
}

// TestExpireOpensRecheckWindow 校验「超时」这条最主要的窗口来源真的开了窗。
//
// 断言的是三件事同时成立：until 在未来（窗口非空）、at 在未来（不会立刻被扫）、
// count 归零（退避从头开始）。少任何一个，复查 Job 都取不到这条任务。
func TestExpireOpensRecheckWindow(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	row := seedRecheckTask(t, gdb, nil)

	before := time.Now()
	if err := Expire(ctx, row); err != nil {
		t.Fatalf("Expire 失败: %v", err)
	}
	got := reloadRecheck(t, gdb, row.ID)
	if got.Status != StatusExpired {
		t.Fatalf("状态应为 expired，实际 %s", got.Status)
	}
	if got.RecheckUntil == nil || got.RecheckAt == nil {
		t.Fatalf("超时任务必须开窗，实际 until=%v at=%v", got.RecheckUntil, got.RecheckAt)
	}
	if got.RecheckUntil.Before(before.Add(recheckWindowDefault - time.Minute)) {
		t.Fatalf("窗口终点过早: %v", *got.RecheckUntil)
	}
	if got.RecheckAt.Sub(before) < recheckFirstDelay-time.Minute {
		t.Fatalf("首次复查时刻过近（会在判超时那一刻就出站）: %v", *got.RecheckAt)
	}
	if got.RecheckCount != 0 {
		t.Fatalf("首次开窗计数应为 0，实际 %d", got.RecheckCount)
	}
}

// TestExpireKeepsWindowClosedWithoutUpstreamID 校验无上游单号时**不开窗**。
//
// 没有单号就没有可查的对象：开窗只会在 idx_tasks_recheck 里堆积
// 「每 2 分钟查一次、每次都查不到东西」的垃圾行，还会挤占真正可捞回任务的名额。
func TestExpireKeepsWindowClosedWithoutUpstreamID(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	row := seedRecheckTask(t, gdb, func(r *model.Task) { r.UpstreamTaskID = "" })

	if err := Expire(ctx, row); err != nil {
		t.Fatalf("Expire 失败: %v", err)
	}
	got := reloadRecheck(t, gdb, row.ID)
	if got.Status != StatusExpired {
		t.Fatalf("状态应为 expired，实际 %s", got.Status)
	}
	if got.RecheckUntil != nil || got.RecheckAt != nil {
		t.Fatalf("无单号不应开窗，实际 until=%v at=%v", got.RecheckUntil, got.RecheckAt)
	}
}

// TestFailWithStaleViewStillOpensWindow 校验「调用方视图过期」时仍然开窗。
//
// 这是 W1 里最隐蔽的一个坑：提交线程刚写完 upstream_task_id，
// 另一条路径（超时扫描 / 孤儿回收）拿着**提交前读到的**任务结构判失败。
// 内存里 upstream_task_id 是空串，库里却有值 ——
// 如果开窗只看内存，这条任务就永远不会被复查，产物和钱一起丢。
func TestFailWithStaleViewStillOpensWindow(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	row := seedRecheckTask(t, gdb, nil)

	stale := *row
	stale.UpstreamTaskID = "" // 模拟「读完之后别处才写入单号」

	if err := FailTimeout(ctx, &stale, "轮询连续失败"); err != nil {
		t.Fatalf("FailTimeout 失败: %v", err)
	}
	got := reloadRecheck(t, gdb, row.ID)
	if got.Status != StatusFailed {
		t.Fatalf("状态应为 failed，实际 %s", got.Status)
	}
	if got.RecheckUntil == nil {
		t.Fatal("库里已有上游单号时必须开窗（回查分支未生效）")
	}
}

// TestSucceedLateClosesWindowAndDelivers 校验迟到成功的收尾：
// 状态推进到 succeeded、窗口关闭、且**不因为账目缺失就丢产物**。
//
// 这里把 usage_id 置 0（账目已归档的最极端形态），期望：
// 状态仍推进、error_msg 写出「未补收」的人话，而不是报错退出。
// 资金上「少收」由对账可见；产物上「没交付」则完全不可挽回。
func TestSucceedLateClosesWindowAndDelivers(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	at := time.Now().Add(-time.Minute)
	row := seedRecheckTask(t, gdb, func(r *model.Task) {
		r.Status, r.UsageID = StatusFailed, 0
		r.RecheckUntil, r.RecheckAt, r.RecheckCount = &until, &at, 3
	})

	if err := SucceedLate(ctx, row, emptyTaskStatus()); err != nil {
		t.Fatalf("SucceedLate 失败: %v", err)
	}
	got := reloadRecheck(t, gdb, row.ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("迟到成功应推进到 succeeded，实际 %s", got.Status)
	}
	if got.RecheckUntil != nil || got.RecheckAt != nil {
		t.Fatalf("succeeded 之后必须关窗，实际 until=%v at=%v", got.RecheckUntil, got.RecheckAt)
	}
	if got.RecheckCount != 3 {
		t.Fatalf("关窗不应清零复查次数（观测值），实际 %d", got.RecheckCount)
	}
	if got.ErrorMsg == "" {
		t.Fatal("未补收时必须给用户一句人话原因，否则详情页只是「已完成」")
	}
}

// TestSucceedLateLosesRaceToRetry 校验「用户抢先重试」时不会重复扣款。
//
// 竞争源是真实的：用户看着「失败」才点的重试，而复查恰好同时命中。
// 期望 CAS 落空（状态仍是 queued）且不留 succeeded 痕迹 ——
// 钱一分未动，代价只是一次白下载。若这里改成「先记账后 CAS」，就会变成双收。
func TestSucceedLateLosesRaceToRetry(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	at := time.Now().Add(-time.Minute)
	row := seedRecheckTask(t, gdb, func(r *model.Task) {
		r.Status, r.UsageID = StatusFailed, 0
		r.RecheckUntil, r.RecheckAt = &until, &at
	})

	// 用户点了重试：状态回到 queued（与 Retry 的 CAS 结果一致）。
	if err := gdb.Model(&model.Task{}).Where("id = ?", row.ID).
		Update("status", StatusQueued).Error; err != nil {
		t.Fatalf("模拟重试失败: %v", err)
	}

	if err := SucceedLate(ctx, row, emptyTaskStatus()); err != nil {
		t.Fatalf("SucceedLate 在竞态下不应报错（报错只会让 Job 反复重试同一行）: %v", err)
	}
	got := reloadRecheck(t, gdb, row.ID)
	if got.Status != StatusQueued {
		t.Fatalf("重试后的任务不应被迟到路径改写，实际 %s", got.Status)
	}
	if got.Settled != 0 {
		t.Fatalf("CAS 落空时不得留下结算额，实际 %d", got.Settled)
	}
}
