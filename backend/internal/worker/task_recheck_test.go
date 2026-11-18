package worker

// 终态复查的**选行与领取**逻辑（W1）。
//
// 这一层不测「上游说了什么」，只测「哪些行有资格被查、谁有权查」——
// 恰恰是复查链路里最容易写错、也最贵的地方：
//   - 少一个条件 → 每 5 分钟扫全表失败任务，出站量按数量级放大；
//   - 多一个条件 → 该捞的任务永远捞不回来，而它看起来完全正常（计数为 0）；
//   - 领取不互斥 → 双实例同时补收，同一笔钱扣两次。
// 上游分支（succeeded/failed/pending）分别落到 task.SucceedLate 与关窗/退避，
// 已在 service/task 的集成用例里覆盖。

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/service/task"
)

// seedRecheckRow 造一条任务行，recheck 三列按传入值落库。
func seedRecheckRow(t *testing.T, gdb *gorm.DB, status string, until, at *time.Time) uint64 {
	t.Helper()
	row := &model.Task{
		UserID: 1, Model: "m", Kind: task.KindVideo, Status: status,
		UpstreamTaskID: "up-" + status, PricingSnapshot: model.MustJSON(map[string]any{}),
		RecheckUntil: until, RecheckAt: at,
	}
	if err := gdb.Create(row).Error; err != nil {
		t.Fatalf("造任务行失败: %v", err)
	}
	return row.ID
}

// TestFetchRecheckBatchSelectsOnlyLiveWindows 校验批次判据：
// 只取「终态 + 窗口未到期 + 已到复查时刻」的行，其余四种一律不取。
//
// 五种行刻意都造出来，因为「不取」的四类才是真正会出事的地方：
//   - succeeded/running 仍在正常轮询，复查再取一次就是重复出站；
//   - 窗口已过期：再查也不该有结果，取它只会把候选集变成历史全表；
//   - 复查时刻在未来：刚判失败就立刻查必然落空。
func TestFetchRecheckBatchSelectsOnlyLiveWindows(t *testing.T) {
	gdb := workerTestDB(t)
	ctx := context.Background()
	// 截断到微秒：Postgres 的 timestamptz 只有微秒精度，
	// 直接拿纳秒级的 time.Now() 做等值比较，读回来必然差几百纳秒。
	now := time.Now().Truncate(time.Microsecond)
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	until := now.Add(4 * time.Hour)

	liveExpired := seedRecheckRow(t, gdb, task.StatusExpired, &until, &past)
	liveFailed := seedRecheckRow(t, gdb, task.StatusFailed, &until, &past)
	seedRecheckRow(t, gdb, task.StatusSucceeded, &until, &past) // 已成功：不再复查
	seedRecheckRow(t, gdb, task.StatusRunning, &until, &past)   // 在途：主轮询负责
	seedRecheckRow(t, gdb, task.StatusExpired, &past, &past)    // 窗口已过期
	seedRecheckRow(t, gdb, task.StatusExpired, &until, &future) // 还没到复查时刻
	seedRecheckRow(t, gdb, task.StatusExpired, nil, nil)        // 从未开窗

	rows, err := fetchRecheckBatch(ctx, 50)
	if err != nil {
		t.Fatalf("取批次失败: %v", err)
	}
	got := map[uint64]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if len(rows) != 2 || !got[liveExpired] || !got[liveFailed] {
		t.Fatalf("应只取 2 条开窗中的终态任务，实际 %d 条: %v", len(rows), got)
	}
}

// TestFetchRecheckBatchRespectsLimit 校验 batch 上限真的生效（否则一轮会把候选集全打光）。
func TestFetchRecheckBatchRespectsLimit(t *testing.T) {
	gdb := workerTestDB(t)
	ctx := context.Background()
	// 截断到微秒：Postgres 的 timestamptz 只有微秒精度，
	// 直接拿纳秒级的 time.Now() 做等值比较，读回来必然差几百纳秒。
	now := time.Now().Truncate(time.Microsecond)
	past, until := now.Add(-time.Minute), now.Add(time.Hour)
	for i := 0; i < 5; i++ {
		seedRecheckRow(t, gdb, task.StatusFailed, &until, &past)
	}
	rows, err := fetchRecheckBatch(ctx, 3)
	if err != nil {
		t.Fatalf("取批次失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("batch=3 应只返回 3 条，实际 %d", len(rows))
	}
}

// TestClaimRecheckIsExclusive 校验领取互斥：同一个 recheck_at 只有一方能领走。
//
// 这就是「双实例同时补收」的防线：领取是把 recheck_at 推到 now()+lease，
// 条件带「仍等于读到的那个值」。第二方读到的是新值 → 条件不成立 → 让出。
func TestClaimRecheckIsExclusive(t *testing.T) {
	gdb := workerTestDB(t)
	ctx := context.Background()
	// 截断到微秒：Postgres 的 timestamptz 只有微秒精度，
	// 直接拿纳秒级的 time.Now() 做等值比较，读回来必然差几百纳秒。
	now := time.Now().Truncate(time.Microsecond)
	past, until := now.Add(-time.Minute), now.Add(time.Hour)
	id := seedRecheckRow(t, gdb, task.StatusFailed, &until, &past)

	rows, err := fetchRecheckBatch(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("准备失败: rows=%d err=%v", len(rows), err)
	}
	// 两个实例拿到的是**同一份**快照（都读到原 recheck_at）。
	a, b := rows[0], rows[0]
	if !claimRecheck(ctx, &a) {
		t.Fatal("首次领取应成功")
	}
	if claimRecheck(ctx, &b) {
		t.Fatal("同一 recheck_at 不应被领取第二次（会双路补收）")
	}
	var got model.Task
	if err := gdb.First(&got, id).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.RecheckCount != 1 {
		t.Fatalf("领取只应让计数 +1，实际 %d", got.RecheckCount)
	}
	if got.RecheckAt == nil || !got.RecheckAt.After(now.Add(task.RecheckLease-time.Minute)) {
		t.Fatalf("领取后 recheck_at 应推到租约终点，实际 %v", got.RecheckAt)
	}
}

// TestDeferRecheckBacksOff 校验退避真的把下次复查往后推，且不碰已成功退出的行。
func TestDeferRecheckBacksOff(t *testing.T) {
	gdb := workerTestDB(t)
	ctx := context.Background()
	// 截断到微秒：Postgres 的 timestamptz 只有微秒精度，
	// 直接拿纳秒级的 time.Now() 做等值比较，读回来必然差几百纳秒。
	now := time.Now().Truncate(time.Microsecond)
	past, until := now.Add(-time.Minute), now.Add(time.Hour)
	id := seedRecheckRow(t, gdb, task.StatusFailed, &until, &past)

	deferRecheck(ctx, id, 3) // 第 3 档（15 分钟）
	var got model.Task
	if err := gdb.First(&got, id).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.RecheckAt == nil || got.RecheckAt.Before(now.Add(10*time.Minute)) {
		t.Fatalf("第 3 档应至少推后 10 分钟，实际 %v", got.RecheckAt)
	}
	if got.RecheckUntil == nil {
		t.Fatal("退避不应关闭窗口")
	}

	// 已被别的路径推进成 succeeded：退避不得把它拉回候选集。
	done := seedRecheckRow(t, gdb, task.StatusSucceeded, &until, &past)
	deferRecheck(ctx, done, 1)
	var d model.Task
	if err := gdb.First(&d, done).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if d.RecheckAt == nil || !d.RecheckAt.Equal(past) {
		t.Fatalf("succeeded 行的 recheck_at 不应被改写，实际 %v", d.RecheckAt)
	}
}

// TestAbandonRecheckClearsWindow 校验关窗：两列都置 NULL，计数保留。
func TestAbandonRecheckClearsWindow(t *testing.T) {
	gdb := workerTestDB(t)
	ctx := context.Background()
	// 截断到微秒：Postgres 的 timestamptz 只有微秒精度，
	// 直接拿纳秒级的 time.Now() 做等值比较，读回来必然差几百纳秒。
	now := time.Now().Truncate(time.Microsecond)
	past, until := now.Add(-time.Minute), now.Add(time.Hour)
	id := seedRecheckRow(t, gdb, task.StatusFailed, &until, &past)
	if err := gdb.Model(&model.Task{}).Where("id = ?", id).
		Update("recheck_count", 4).Error; err != nil {
		t.Fatalf("置计数失败: %v", err)
	}

	abandonRecheck(ctx, id, "test")
	var got model.Task
	if err := gdb.First(&got, id).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.RecheckUntil != nil || got.RecheckAt != nil {
		t.Fatalf("关窗必须两列都置 NULL，实际 until=%v at=%v", got.RecheckUntil, got.RecheckAt)
	}
	if got.RecheckCount != 4 {
		t.Fatalf("关窗不应清零复查次数（那是观测值），实际 %d", got.RecheckCount)
	}
}
