package worker

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/task"
)

// TestHasArtifactOf 锁死「上游成功 → 是否算成功」的判定口径。
//
// 三种产物形态都必须算成功：单链接（视频）/ 多链接（多图、多段视频）/ 纯文本（歌词）。
// 判窄了的后果不是「少拿一个产物」，而是**整单被失败并全额退款**：上游已经生成好了，
// 成本已经产生，平台既没收到钱也没交付东西。
func TestHasArtifactOf(t *testing.T) {
	cases := []struct {
		name string
		st   *relay.TaskStatus
		want bool
	}{
		{"nil 状态", nil, false},
		{"单链接", &relay.TaskStatus{ResultURL: "https://cdn/x.mp4"}, true},
		{"单链接含空白", &relay.TaskStatus{ResultURL: "  https://cdn/x.mp4  "}, true},
		{"多链接", &relay.TaskStatus{ResultURLs: []string{"https://cdn/1.png", "https://cdn/2.png"}}, true},
		{"多链接里有空项但至少一个有效", &relay.TaskStatus{ResultURLs: []string{"", "https://cdn/2.png"}}, true},
		{"纯文本（歌词）", &relay.TaskStatus{ResultText: "[00:12] 第一句歌词"}, true},
		{"文本只有空白不算产物", &relay.TaskStatus{ResultText: "   \n\t"}, false},
		{"空数组不算产物", &relay.TaskStatus{ResultURLs: []string{}}, false},
		{"全部空项不算产物", &relay.TaskStatus{ResultURLs: []string{"", "   "}}, false},
		{"什么都没带", &relay.TaskStatus{Status: task.StatusSucceeded}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasArtifactOf(c.st); got != c.want {
				t.Fatalf("hasArtifactOf=%v，期望 %v（判错的后果是整单失败退款）", got, c.want)
			}
		})
	}
}

// TestPollEnvInt 锁死轮询参数的读取口径。
//
// 三条硬要求：非法值退回默认（不能 0 或负数 → 上限 0 等于整轮不干活）、
// 超上限截断（防误配置打爆 DB 连接池）、默认值本身不能超过上限。
func TestPollEnvInt(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		def  int
		max  int
		want int
	}{
		{"未设置用默认", "", 200, 2000, 200},
		{"合法值", "50", 200, 2000, 50},
		{"含空白", "  50  ", 200, 2000, 50},
		{"零值非法", "0", 200, 2000, 200},
		{"负数非法", "-5", 200, 2000, 200},
		{"非数字非法", "abc", 200, 2000, 200},
		{"小数非法", "1.5", 200, 2000, 200},
		{"超上限截断", "999999", 200, 2000, 2000},
		{"恰好等于上限", "2000", 200, 2000, 2000},
		{"默认值也受上限约束基线", "", 8, 64, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LD_TEST_POLL_ENV", c.raw)
			if got := pollEnvInt("LD_TEST_POLL_ENV", c.def, c.max); got != c.want {
				t.Fatalf("pollEnvInt(%q)=%d，期望 %d", c.raw, got, c.want)
			}
		})
	}
}

// TestPollBatchOrdering 验证取批顺序真的是「未轮询优先 → 最久未轮询优先 → id 升序」。
//
// 这是 B4-1 的公平性核心：旧实现每轮固定 `id ASC`（且全局 Sleep 1s），
// 存量任务一多，新提交的任务可能几十分钟都轮不到一次 —— 用户侧看到的是「一直在排队」。
// 排序写错不会报错，只会让某些任务饿死，所以必须用真实 SQL 断言。
func TestPollBatchOrdering(t *testing.T) {
	gdb := workerTestDB(t)
	ctx := context.Background()
	j := NewTaskPollingJob()

	now := time.Now()
	old := now.Add(-time.Hour)
	mid := now.Add(-10 * time.Minute)

	mk := func(tag string, lastPolled *time.Time) *model.Task {
		t.Helper()
		row := &model.Task{
			UserID: 1, Model: tag, Modality: "video", Kind: task.KindVideo,
			Status: task.StatusRunning, UpstreamTaskID: "up-" + tag,
			DeadlineAt: func() *time.Time { d := now.Add(time.Hour); return &d }(),
		}
		if err := gdb.Create(row).Error; err != nil {
			t.Fatalf("造任务失败: %v", err)
		}
		if lastPolled != nil {
			if err := gdb.Model(&model.Task{}).Where("id = ?", row.ID).
				Update("last_polled_at", *lastPolled).Error; err != nil {
				t.Fatalf("写 last_polled_at 失败: %v", err)
			}
		}
		return row
	}

	never := mk("never", nil)
	stale := mk("stale", &old)
	fresh := mk("fresh", &mid)

	// 干扰项：这些都不该出现在轮询批次里。
	noUpstream := &model.Task{UserID: 1, Model: "no-up", Modality: "video", Kind: task.KindVideo,
		Status: task.StatusQueued, DeadlineAt: func() *time.Time { d := now.Add(time.Hour); return &d }()}
	if err := gdb.Create(noUpstream).Error; err != nil {
		t.Fatal(err)
	}
	expired := &model.Task{UserID: 1, Model: "expired", Modality: "video", Kind: task.KindVideo,
		Status: task.StatusRunning, UpstreamTaskID: "up-exp",
		DeadlineAt: func() *time.Time { d := now.Add(-time.Minute); return &d }()}
	if err := gdb.Create(expired).Error; err != nil {
		t.Fatal(err)
	}
	done := &model.Task{UserID: 1, Model: "done", Modality: "video", Kind: task.KindVideo,
		Status: task.StatusSucceeded, UpstreamTaskID: "up-done"}
	if err := gdb.Create(done).Error; err != nil {
		t.Fatal(err)
	}

	rows, err := j.fetchPollBatch(ctx, 10)
	if err != nil {
		t.Fatalf("取批失败: %v", err)
	}
	if len(rows) != 3 {
		ids := make([]uint64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		t.Fatalf("应只取到 3 条可轮询任务，实际 %d 条: %v", len(rows), ids)
	}
	wantOrder := []uint64{never.ID, stale.ID, fresh.ID}
	for i, want := range wantOrder {
		if rows[i].ID != want {
			got := make([]uint64, 0, len(rows))
			for _, r := range rows {
				got = append(got, r.ID)
			}
			t.Fatalf("取批顺序应为 [未轮询, 最久未轮询, 较新]，实际 %v（期望首项 %d）", got, want)
		}
	}

	// 批次上限必须真的生效（否则 batch 参数形同虚设）。
	if rows, err := j.fetchPollBatch(ctx, 2); err != nil {
		t.Fatalf("取批失败: %v", err)
	} else if len(rows) != 2 {
		t.Fatalf("batch=2 应只返回 2 条，实际 %d", len(rows))
	}

	// markPolled 必须能把队头任务推到队尾：这是「不饿死」的另一半。
	j.markPolled(ctx, never.ID)
	rows2, err := j.fetchPollBatch(ctx, 10)
	if err != nil {
		t.Fatalf("取批失败: %v", err)
	}
	if len(rows2) == 0 || rows2[len(rows2)-1].ID != never.ID {
		last := uint64(0)
		if len(rows2) > 0 {
			last = rows2[len(rows2)-1].ID
		}
		t.Fatalf("记录轮询时间后该任务应排到队尾，实际队尾 id=%d（本任务 id=%d）", last, never.ID)
	}
}

// TestPollEnvIntDefaultsWithinMax 防止默认值本身越过上限后被「静默截断成别的数」。
func TestPollEnvIntDefaultsWithinMax(t *testing.T) {
	if taskPollBatchDefault > taskPollBatchMax {
		t.Fatalf("默认批次 %d 超过上限 %d", taskPollBatchDefault, taskPollBatchMax)
	}
	if taskPollConcurrencyDefault > taskPollConcurrencyMax {
		t.Fatalf("默认并发 %d 超过上限 %d", taskPollConcurrencyDefault, taskPollConcurrencyMax)
	}
}

// TestNormalizeUpstreamStatus 是第二道防线：适配器之外的写法也要归一到底。
func TestNormalizeUpstreamStatus(t *testing.T) {
	cases := map[string]string{
		"succeeded": task.StatusSucceeded,
		"succeed":   task.StatusSucceeded, // 可灵原生写法
		"SUCCESS":   task.StatusSucceeded,
		"finished":  task.StatusSucceeded,
		"failed":    task.StatusFailed,
		"error":     task.StatusFailed,
		"canceled":  task.StatusCanceled,
		"cancelled": task.StatusCanceled,
		"queued":    task.StatusQueued,
		"submitted": task.StatusQueued, // seedance 原生写法
		// 未知写法（含数字码）一律按 running 处理：宁可多轮一次，也不能误判失败退款
		"1":       task.StatusRunning,
		"running": task.StatusRunning,
		"":        task.StatusRunning,
		"奇怪状态":    task.StatusRunning,
	}
	for in, want := range cases {
		if got := normalizeUpstreamStatus(in); got != want {
			t.Fatalf("normalizeUpstreamStatus(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// TestPollGiveUpNeedsBothCountAndDuration 守「判超时退款」的两个条件缺一不可。
//
// 改造前只有次数（20 × 5s ≈ 100 秒）：一次两分钟的上游抖动会把所有在途任务判死退款，
// 而上游仍在生成计费。这里穷举三条边界，因为它们在联调里几乎触发不到。
func TestPollGiveUpNeedsBothCountAndDuration(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		n     int
		since time.Duration // 距今多久开始连败
		want  bool
	}{
		{"次数够但只失败 100 秒", taskMaxNetworkFail, 100 * time.Second, false},
		{"次数够且失败 10 分钟", taskMaxNetworkFail, taskFailMinDuration, true},
		{"时间够但次数不够", taskMaxNetworkFail - 1, 30 * time.Minute, false},
		{"次数多且时间够", taskMaxNetworkFail + 5, time.Hour, true},
	}
	for _, c := range cases {
		if got := pollGiveUp(c.n, now.Add(-c.since), now); got != c.want {
			t.Fatalf("%s：pollGiveUp(%d, %v)=%v，期望 %v", c.name, c.n, c.since, got, c.want)
		}
	}
}

// TestOnPollFailureSkipsUncountedClasses 守「429/401 不计任务连败」。
//
// 这两档都不是「上游坏了」：429 是我方被限流，401 是这一把 key 失效（下轮换 key）。
// 计进连败的后果是一次 200 任务的限流把整批任务判超时退款 —— 上游还在生成。
func TestOnPollFailureSkipsUncountedClasses(t *testing.T) {
	j := &TaskPollingJob{}
	ch := &model.Channel{ID: 9001, AdapterID: "openai"}
	tk := &model.Task{ID: 9002, Status: task.StatusRunning}
	clearPollState(tk.ID)
	defer clearPollState(tk.ID)

	// 401 会冷却 key（ref.ID 为空时不做 key 级动作），但不该计连败。
	j.onPollFailure(context.Background(), tk, ch, relay.KeyRef{}, &relay.HTTPStatusError{Code: 401})
	j.onPollFailure(context.Background(), tk, ch, relay.KeyRef{}, &relay.HTTPStatusError{Code: 429})
	if n := clearTaskPollFail(tk.ID); n != 0 {
		t.Fatalf("401/429 不得计入任务连败，实际计数 %d", n)
	}

	// 5xx 是唯一该计连败的一档。
	j.onPollFailure(context.Background(), tk, ch, relay.KeyRef{}, &relay.HTTPStatusError{Code: 503})
	if n := clearTaskPollFail(tk.ID); n != 1 {
		t.Fatalf("5xx 应计一次连败，实际 %d", n)
	}
}
