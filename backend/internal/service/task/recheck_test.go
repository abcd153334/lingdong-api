package task

// 终态复查窗口（W1）的纯单测：不连 DB、不连 Redis。
//
// 这一组守的都是「改一个数就静默错」的地方：
//   - 两个 CAS 起点集合一旦重叠，幂等保证消失，表现是偶发双收（极难复现）；
//   - 退避序列写错，表现是出站量翻倍或窗口被白丢（日志上完全看不出来）；
//   - 窗口上限不截断，表现是上线当天所有历史失败任务一起变成候选。

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestLateFromDisjointAdvanceFrom：迟到推进与正常推进的起点集合必须不相交。
//
// 这是整条复查链路**唯一的**结构性幂等保证。Redis 不可用时 lockSucceed 会放行，
// 那时就只剩这一层在防「同一任务被两条路径同时收尾」。
func TestLateFromDisjointAdvanceFrom(t *testing.T) {
	from := make(map[string]bool, len(advanceFrom))
	for _, s := range advanceFrom {
		from[s] = true
	}
	for _, s := range RecheckableStatuses() {
		if from[s] {
			t.Fatalf("状态 %q 同时出现在 advanceFrom 与 lateFrom：两个 CAS 会互相命中，"+
				"同一任务可能被正常收尾和迟到收尾各推进一次", s)
		}
	}
	// 终态之外的状态不许进 lateFrom：进了就等于「queued 也能被迟到成功覆盖」，
	// 而那时候正常路径正持有这条任务。
	for _, s := range RecheckableStatuses() {
		if !IsTerminal(s) {
			t.Errorf("lateFrom 里的 %q 不是终态", s)
		}
	}
}

// TestRecheckableStatusesIsCopy：返回的必须是副本，改它不能影响 CAS 用的那份。
func TestRecheckableStatusesIsCopy(t *testing.T) {
	got := RecheckableStatuses()
	if len(got) == 0 {
		t.Fatal("lateFrom 不该为空")
	}
	got[0] = "queued"
	if RecheckableStatuses()[0] == "queued" {
		t.Fatal("RecheckableStatuses 返回了内部切片：调用方改一个值就能把 queued 塞进 lateFrom")
	}
}

// TestRecheckDelayMonotonic：退避必须单调不减，且 6 小时窗口内不超过 13 次。
//
// 上界的含义是「出站预算」：每多一次就是一次真实上游 GET。
// 72 次（5 分钟一次）与 11 次（本序列）差 6.5 倍，上游集体超时时就是能不能扛住的差别。
func TestRecheckDelayMonotonic(t *testing.T) {
	prev := time.Duration(0)
	var total time.Duration
	runs := 0
	for i := 0; i < 40; i++ {
		d := recheckDelay(i)
		if d < prev {
			t.Fatalf("第 %d 档退避（%v）小于上一档（%v）：退避不允许回退", i, d, prev)
		}
		prev = d
		total += d
		// 统计**默认窗口**（6h）内会发生的复查次数，而不是硬上限 72h ——
		// 后者是误配保护，不是正常运行时的工作量。
		if total <= recheckWindowDefault {
			runs++
		}
	}
	if runs > 13 {
		t.Fatalf("%v 窗口内安排了 %d 次复查，超过 13 次上限", recheckWindowDefault, runs)
	}
	if runs < 8 {
		t.Fatalf("%v 窗口内只有 %d 次复查，太少：上游在 1 小时后才完成的占比并不低", recheckWindowDefault, runs)
	}
	if recheckDelay(0) < time.Minute {
		t.Fatalf("首档退避 %v 太短：判超时的同一秒上游几乎不可能刚好完成", recheckDelay(0))
	}
}

// TestRecheckWindowCap：误配必须被硬上限截断，而不是照单全收。
func TestRecheckWindowCap(t *testing.T) {
	t.Setenv("LD_TASK_RECHECK_HOURS", "1000")
	if got := recheckWindow(); got != recheckWindowMax {
		t.Fatalf("1000h 应被截断到 %v，实际 %v", recheckWindowMax, got)
	}
	t.Setenv("LD_TASK_RECHECK_HOURS", "abc")
	if got := recheckWindow(); got != recheckWindowDefault {
		t.Fatalf("非法值应回落到默认 %v，实际 %v", recheckWindowDefault, got)
	}
	// 0 与负数不能被当成「窗口为 0」：那等于该功能静默关闭，而配置看起来是"填了值"。
	for _, v := range []string{"0", "-1"} {
		t.Setenv("LD_TASK_RECHECK_HOURS", v)
		if got := recheckWindow(); got != recheckWindowDefault {
			t.Errorf("%q 应回落到默认 %v，实际 %v（0 会让功能静默消失）", v, recheckWindowDefault, got)
		}
	}
	t.Setenv("LD_TASK_RECHECK_HOURS", "3")
	if got := recheckWindow(); got != 3*time.Hour {
		t.Fatalf("3h 应原样生效，实际 %v", got)
	}
}

// TestRecheckFieldsOfNoUpstreamID：没有上游单号就不开窗。
//
// 开了窗也没有可查的对象，只会让索引里多一批必然落空的行，
// 而且它们要占满整个窗口期才自然消失。
func TestRecheckFieldsOfNoUpstreamID(t *testing.T) {
	// ID=0 且无单号：连库都不会查，直接返回 nil（这条路径不需要 DB）。
	if f := recheckFieldsOf(context.Background(), &model.Task{}); f != nil {
		t.Fatalf("无单号的任务不该开窗，实际 %v", f)
	}
	if f := recheckFieldsOf(context.Background(), nil); f != nil {
		t.Fatalf("nil 任务不该开窗，实际 %v", f)
	}
}

// TestRecheckFieldsOfOpens：有单号时必须写出三个字段，且时间点落在预期区间。
func TestRecheckFieldsOfOpens(t *testing.T) {
	now := time.Now()
	task := &model.Task{ID: 0, UpstreamTaskID: "up-1"}
	// ID=0 时不会回查库，单号取内存值。
	f := recheckFieldsOf(context.Background(), task)
	if f == nil {
		t.Fatal("有单号必须开窗")
	}
	until, ok := f["recheck_until"].(time.Time)
	if !ok {
		t.Fatalf("recheck_until 类型不对：%T", f["recheck_until"])
	}
	at, ok := f["recheck_at"].(time.Time)
	if !ok {
		t.Fatalf("recheck_at 类型不对：%T", f["recheck_at"])
	}
	if !at.After(now) {
		t.Errorf("首次复查时刻 %v 不该早于当前 %v", at, now)
	}
	if !until.After(at) {
		t.Errorf("窗口终点 %v 必须晚于首次复查 %v", until, at)
	}
	if until.Sub(now) < recheckWindowDefault-time.Minute {
		t.Errorf("窗口长度异常：%v", until.Sub(now))
	}
	if c, _ := f["recheck_count"].(int); c != 0 {
		t.Errorf("开窗时计数必须归零，实际 %v", f["recheck_count"])
	}
}

// TestRetryResetFieldsClearsRecheck：重试必须清掉旧窗口。
//
// 漏清的后果不是报错，而是「重试后又失败」时两个窗口叠加 ——
// 复查频率翻倍，而且第二次的退避直接从上一轮的高档起步，白丢一段窗口。
func TestRetryResetFieldsClearsRecheck(t *testing.T) {
	f := retryResetFields(KindVideo, time.Now().Add(time.Hour))
	for _, k := range []string{"recheck_until", "recheck_at"} {
		v, ok := f[k]
		if !ok {
			t.Fatalf("retryResetFields 漏了 %q：旧窗口会留到新一轮", k)
		}
		if v != nil {
			t.Errorf("%q 必须置 nil，实际 %v", k, v)
		}
	}
	if c, ok := f["recheck_count"]; !ok || c != 0 {
		t.Errorf("recheck_count 必须归零，实际 %v（存在=%v）", c, ok)
	}
}

// TestCloseRecheckWindowFields：关窗只清两个时间列，计数必须保留。
//
// 计数是「查了几次才捞回来」的唯一证据，清掉就只能知道发生过、说不出过程。
func TestCloseRecheckWindowFields(t *testing.T) {
	f := closeRecheckWindow()
	if len(f) != 2 {
		t.Fatalf("关窗只该动两个时间列，实际 %v", f)
	}
	for _, k := range []string{"recheck_until", "recheck_at"} {
		if v, ok := f[k]; !ok || v != nil {
			t.Errorf("%q 必须存在且为 nil，实际 %v（存在=%v）", k, v, ok)
		}
	}
	if _, ok := f["recheck_count"]; ok {
		t.Error("关窗不许清 recheck_count")
	}
}
