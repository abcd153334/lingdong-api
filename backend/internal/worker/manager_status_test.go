package worker

import (
	"context"
	"testing"
	"time"
)

// probeJob 是测试用 Job：可指定是否 panic、是否计数。
type probeJob struct {
	name    string
	panicOn bool
	panics  int
}

func (j *probeJob) Name() string            { return j.name }
func (j *probeJob) Interval() time.Duration { return time.Hour }
func (j *probeJob) Run(context.Context) error {
	if j.panicOn {
		panic("boom")
	}
	return nil
}
func (j *probeJob) LastCount() int { return 7 }

// TestJobPanicIsIsolated 守住「一个 Job 崩溃不能打死进程」。
//
// 清理逻辑里有大量 nil 解引用风险（模型/切片/指针时间），而 Job 协程没有 recover 时，
// 一次 panic 会让整个进程退出 —— 用户看到的是「全站 502」，日志里只有一行 panic，
// 完全联想不到是「到期清理」引起的。
func TestJobPanicIsIsolated(t *testing.T) {
	bad := &probeJob{name: "bad", panicOn: true}
	good := &probeJob{name: "good"}

	m := NewManager([]Job{bad, good})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.startJobs(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		detail := jobsDetail()
		if len(detail) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var sawBad, sawGood bool
	for _, st := range jobsDetail() {
		switch st.Name {
		case "bad":
			sawBad = true
			if st.Panics != 1 {
				t.Fatalf("panic 应被记录 1 次，实际 %+v", st)
			}
			if st.LastError == "" {
				t.Fatal("panic 必须留下错误文案，否则健康页看起来一切正常")
			}
		case "good":
			sawGood = true
			if st.Runs == 0 {
				t.Fatalf("正常 Job 应继续运行，实际 %+v", st)
			}
			if st.Count != 7 {
				t.Fatalf("Counted 应把上一轮条数带出来，实际 %+v", st)
			}
		}
	}
	if !sawBad || !sawGood {
		t.Fatalf("两个 Job 都应出现在快照里: %+v", jobsDetail())
	}

	// 未跑过的 Job 不出现（零值 last_run_at 会被读成「1970 年跑过」）。
	m2 := NewManager([]Job{&probeJob{name: "never"}})
	_ = m2
	if got := jobsDetail(); len(got) != 2 {
		t.Fatalf("未运行的 Job 不应出现在快照里，实际 %+v", got)
	}
}
