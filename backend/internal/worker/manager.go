// Package worker 提供定时任务骨架与 Redis leader 锁（单副本语义）。
package worker

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/ulid"
)

const (
	lockKey       = "worker:leader"
	lockTTL       = 60 * time.Second
	renewInterval = 30 * time.Second
	// drainDefault 是优雅退出时等待在途 Job 的上限（LD_TASK_DRAIN 可覆盖）。
	//
	// 为什么需要一个上限：Job 里可能有正在跑的结算/退款（外部 HTTP + 事务），
	// 无限等待会让编排层的 SIGKILL 提前到来，反而在事务中途断电；
	// 但立刻退出又会把「已扣款未落账」留给回收任务。30s 是两者的折中。
	drainDefault = 30 * time.Second
)

// renewScript 是「比较持有者后续期」的原子脚本（P2-11）。
//
// 原实现是 GET → 比对 → EXPIRE 三步：若在 GET 与 EXPIRE 之间锁到期并被另一个实例拿到，
// 本实例仍会把自己的 EXPIRE 打到别人的锁上——两个实例同时认为自己是 leader（双跑）。
// 反过来 Expire 失败时原实现只打日志仍返回 true，同样会双跑。
var renewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0`)

// releaseScript 是 CAS-DEL：只删自己持有的锁，避免误删新 leader 的锁。
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("del", KEYS[1])
end
return 0`)

// Job 是定时任务接口：Interval 决定节拍（leadership 期间单协程串行执行），Run 执行一次。
type Job interface {
	Name() string
	Interval() time.Duration
	Run(ctx context.Context) error
}

// Counted 是**可选**接口：Job 实现它时，健康页会展示上一轮的处理条数。
//
// 只对「有明确产出」的 Job 有意义（清理类），所以不做成 Job 的必选方法。
type Counted interface {
	LastCount() int
}

// JobState 是单个 Job 的运行快照。
//
// 为什么需要它：旧状态只有 leader/redis_ready，「Job 悄悄停了」与
// 「Job 在跑但每轮都失败」在管理台上完全一样，只能靠翻日志推断。
type JobState struct {
	Name           string    `json:"name"`
	LastRunAt      time.Time `json:"last_run_at"`
	LastDurationMS int64     `json:"last_duration_ms"`
	LastError      string    `json:"last_error,omitempty"`
	Runs           uint64    `json:"runs"`
	Failures       uint64    `json:"failures"`
	Panics         uint64    `json:"panics"`
	// Count 是上一轮处理条数（未实现 Counted 的 Job 恒为 0）。
	Count int `json:"last_count"`
}

var (
	jobMu     sync.RWMutex
	jobStates = map[string]JobState{}
	jobTotal  int
)

// publishJobState 更新单个 Job 的快照（读-改-写在一把锁内完成）。
func publishJobState(name string, fn func(*JobState)) {
	jobMu.Lock()
	defer jobMu.Unlock()
	st := jobStates[name]
	fn(&st)
	jobStates[name] = st
}

// jobsDetail 返回所有**至少跑过一轮**的 Job 快照（按名字排序）。
//
// 没跑过的 Job 不输出：它们的 last_run_at 是零值，看起来像「1970 年跑过」，
// 比不显示更容易误判。
func jobsDetail() []JobState {
	jobMu.RLock()
	defer jobMu.RUnlock()
	out := make([]JobState, 0, len(jobStates))
	for _, st := range jobStates {
		if st.Runs > 0 {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// runOnce 执行一轮 Job 并把结果写进快照；panic 被隔离在这里，不会打死进程。
func (m *Manager) runOnce(ctx context.Context, j Job) {
	start := time.Now()
	var (
		runErr   error
		panicked any
		stack    []byte
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = r
				stack = debug.Stack()
			}
		}()
		runErr = j.Run(ctx)
	}()

	count := -1
	if c, ok := j.(Counted); ok {
		count = c.LastCount()
	}
	publishJobState(j.Name(), func(st *JobState) {
		st.Name = j.Name()
		st.LastRunAt = start
		st.LastDurationMS = time.Since(start).Milliseconds()
		st.Runs++
		if count >= 0 {
			st.Count = count
		}
		switch {
		case panicked != nil:
			st.Panics++
			st.LastError = fmt.Sprintf("panic: %v", panicked)
		case runErr != nil:
			st.Failures++
			st.LastError = runErr.Error()
		default:
			st.LastError = ""
		}
	})

	switch {
	case panicked != nil:
		// 一个清理逻辑的 nil 崩溃不该带走整个 HTTP 服务。
		metrics.Inc(metrics.NameWorkerJobPanic, "job", j.Name())
		logger.L().Error("Worker 任务 panic（已隔离，进程继续）",
			zap.String("job", j.Name()), zap.Any("panic", panicked),
			zap.ByteString("stack", stack))
	case runErr != nil:
		logger.L().Warn("Worker 任务执行失败", zap.String("job", j.Name()), zap.Error(runErr))
	}
}

// Status 是 Worker 运行态的只读快照（供 /api/health 与运维排障消费）。
//
// 为什么要暴露它：leader 锁与 Redis 状态是「任务为什么不动」的唯一答案。
// 没有这层可见性时，线上表现为「任务一直排队、没有任何报错」——
// 日志里只有一条启动时的 WARN，重启后又被淹没。
type Status struct {
	InstanceID string `json:"instance_id"`
	Leader     bool   `json:"leader"`
	RedisReady bool   `json:"redis_ready"`
	// StrictLeader 为 true 表示「Redis 不可用时不自认 leader」（默认）。
	StrictLeader bool `json:"strict_leader"`
	// PollingPaused 为 true 表示定时任务当前**没有在跑**（没有 leader 或已停止）。
	PollingPaused bool `json:"polling_paused"`
	// Reason 是暂停/降级的原因（redis_unavailable / lost_leader / stopped / standby / 空）。
	Reason string `json:"reason,omitempty"`
	Jobs   int    `json:"jobs"`
	// JobsDetail 是每个跑过的 Job 的最近一次结果（含失败原因与处理条数）。
	//
	// 只输出跑过的 Job：未运行过的零值快照看起来像「很久以前跑过」，更容易误判。
	JobsDetail []JobState `json:"jobs_detail"`
}

var (
	statusMu   sync.RWMutex
	lastStatus = Status{}
)

// StatusSnapshot 返回 Worker 当前运行态（并发安全；未启动时为零值 + strict 标记）。
func StatusSnapshot() Status {
	statusMu.RLock()
	s := lastStatus
	statusMu.RUnlock()
	s.StrictLeader = leaderStrict()
	s.JobsDetail = jobsDetail()
	if s.Jobs == 0 {
		// standby 实例从不 publish，Jobs 会是 0 —— 读起来像「没有任务」。
		s.Jobs = jobTotal
	}
	if s.InstanceID == "" {
		// 从未当选过：状态是零值，`polling_paused=false` 会读成「正在跑」。
		s.PollingPaused = true
		s.Reason = "standby"
	}
	return s
}

func publishStatus(s Status) {
	statusMu.Lock()
	lastStatus = s
	statusMu.Unlock()
}

// Manager 管理 Worker 生命周期、leader 选举与 Job 调度。
type Manager struct {
	instanceID string
	jobs       []Job

	mu         sync.Mutex
	cancel     context.CancelFunc
	jobsCancel context.CancelFunc
	// loopWG 等调度协程退出，jobsWG 等各 Job 协程退出。
	//
	// 为什么必须分两个：调度协程是 startJobs 的**唯一调用方**。若不等它退出就去 Wait
	// jobsWG，就可能出现「Wait 已返回、调度协程又 Add 了一批」——
	// WaitGroup 明确禁止在计数归零后与 Wait 并发 Add，那是未定义行为（可能直接 panic）。
	loopWG sync.WaitGroup
	jobsWG sync.WaitGroup
}

// NewManager 构造 Worker 管理器。
func NewManager(jobs []Job) *Manager {
	jobMu.Lock()
	jobTotal = len(jobs)
	jobMu.Unlock()
	return &Manager{instanceID: ulid.New(), jobs: jobs}
}

// Start 启动 Worker：取 Redis leader 锁（SETNX EX 60 / 30s 续期）并在当选期间调度 Job。
//
// Redis 不可用时是否降级由 LD_LEADER_LOCK_STRICT 决定（默认严格：不自认 leader）。
func (m *Manager) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	m.loopWG.Add(1)
	go func() {
		defer m.loopWG.Done()
		m.loop(ctx)
	}()
}

// Stop 停止 Worker：先取消（通知 Job 退出）再等待在途 Job 结束，最后释放 leader 锁。
//
// 顺序不可交换：先 Wait 后 cancel 会永远等不到（Job 循环只在 ctx.Done 时退出）。
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	timeout := drainTimeout()
	deadline := time.Now().Add(timeout)

	// 第一步：等调度协程退出（它退出前一定已经 stopJobs）。
	if !waitUntil(&m.loopWG, deadline) {
		logger.L().Warn("Worker 调度协程未在退出窗口内结束，跳过后续等待",
			zap.String("instance_id", m.instanceID), zap.Duration("drain", timeout))
		return
	}
	// 第二步：等在途 Job 结束。它们可能正在结算/退款（外部 HTTP + 事务），
	// 硬切断会把「已扣款未落账」留给回收任务，甚至留下半截事务。
	if !waitUntil(&m.jobsWG, deadline) {
		// 剩下的由 task_reclaim / usage_reclaim 兜底：比让编排层 SIGKILL 更可控。
		logger.L().Warn("Worker 退出等待超时，仍有任务在途（回收任务会兜底）",
			zap.String("instance_id", m.instanceID), zap.Duration("drain", timeout))
	}
}

// waitUntil 等待 WaitGroup 归零或到达 deadline（返回 false 表示超时）。
func waitUntil(wg *sync.WaitGroup, deadline time.Time) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	remain := time.Until(deadline)
	if remain <= 0 {
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(remain):
		return false
	}
}

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()

	leading := false
	for {
		if !leading {
			// 竞选抖动：多实例同时失去 leader 时，若都在同一 tick 抢锁会形成惊群。
			if !sleepJitter(ctx) {
				return
			}
			if m.acquire(ctx) {
				leading = true
				m.startJobs(ctx)
			}
		} else if !m.renew(ctx) {
			leading = false
			m.stopJobs()
			logger.L().Warn("Worker 失去 leader，暂停任务调度", zap.String("instance_id", m.instanceID))
			publishStatus(Status{
				InstanceID: m.instanceID, Leader: false, RedisReady: db.RedisReady(),
				PollingPaused: true, Reason: "lost_leader", Jobs: len(m.jobs),
			})
		}

		select {
		case <-ctx.Done():
			m.stopJobs()
			m.release(ctx)
			publishStatus(Status{
				InstanceID: m.instanceID, Leader: false, RedisReady: db.RedisReady(),
				PollingPaused: true, Reason: "stopped", Jobs: len(m.jobs),
			})
			return
		case <-ticker.C:
		}
	}
}

// startJobs 为每个 Job 启动一个协程按其节拍执行（先执行一次，便于重启后立即恢复）。
func (m *Manager) startJobs(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	m.jobsCancel = cancel
	m.mu.Unlock()
	for _, job := range m.jobs {
		j := job
		interval := j.Interval()
		if interval <= 0 {
			interval = time.Minute
		}
		m.jobsWG.Add(1)
		go func() {
			defer m.jobsWG.Done()
			tk := time.NewTicker(interval)
			defer tk.Stop()
			for {
				m.runOnce(ctx, j)
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
				}
			}
		}()
	}
}

// stopJobs 停止全部 Job 协程。
func (m *Manager) stopJobs() {
	m.mu.Lock()
	cancel := m.jobsCancel
	m.jobsCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) acquire(ctx context.Context) bool {
	if !db.RedisReady() {
		if leaderStrict() {
			// 严格模式（默认）：**不自认 leader**。
			//
			// 为什么默认严格：自认 leader 在多副本部署下等于「每个副本都跑一遍结算/退款」，
			// 而资金类 Job 不是幂等就能兜住的（同一笔退款可以被两个进程同时发起）。
			// 宁可暂停任务（Redis 恢复后 30s 内自动恢复），也不能双跑。
			logger.L().Error("Redis 不可用，Worker 定时任务已暂停（严格模式，恢复后自动继续）",
				zap.String("instance_id", m.instanceID),
				zap.String("fallback", "无 Redis 的单副本部署可设 LD_LEADER_LOCK_STRICT=0 恢复降级模式"))
			publishStatus(Status{
				InstanceID: m.instanceID, Leader: false, RedisReady: false,
				PollingPaused: true, Reason: "redis_unavailable", Jobs: len(m.jobs),
			})
			return false
		}
		logger.L().Warn("Redis 不可用，Worker 降级为单实例模式（跳过分发锁）",
			zap.String("instance_id", m.instanceID))
		publishStatus(Status{
			InstanceID: m.instanceID, Leader: true, RedisReady: false,
			PollingPaused: false, Reason: "redis_unavailable_fallback", Jobs: len(m.jobs),
		})
		return true
	}
	ok, err := db.RDB.SetNX(ctx, lockKey, m.instanceID, lockTTL).Result()
	if err != nil {
		logger.L().Warn("Worker 取 leader 锁失败", zap.Error(err))
		return false
	}
	if ok {
		logger.L().Info("本实例成为 Worker leader", zap.String("instance_id", m.instanceID))
		publishStatus(Status{
			InstanceID: m.instanceID, Leader: true, RedisReady: true,
			PollingPaused: false, Jobs: len(m.jobs),
		})
	}
	return ok
}

// renew 续期并返回是否仍持有 leader。
//
// fail-closed：Redis 不可用时按 LD_LEADER_LOCK_STRICT 决定（默认不自认 leader）；
// **Redis 可用却操作失败**时一律按「已失去 leader」处理并暂停任务，
// 宁可少跑一轮定时任务，也不能两个实例同时跑结算/回收类任务。
func (m *Manager) renew(ctx context.Context) bool {
	if !db.RedisReady() {
		return !leaderStrict()
	}
	n, err := renewScript.Run(ctx, db.RDB, []string{lockKey}, m.instanceID,
		int64(lockTTL/time.Millisecond)).Int()
	if err != nil {
		logger.L().Warn("Worker 续期失败，暂停任务调度（fail-closed）",
			zap.String("instance_id", m.instanceID), zap.Error(err))
		return false
	}
	return n > 0
}

func (m *Manager) release(ctx context.Context) {
	if !db.RedisReady() {
		return
	}
	if _, err := releaseScript.Run(ctx, db.RDB, []string{lockKey}, m.instanceID).Result(); err != nil {
		logger.L().Warn("Worker 释放 leader 锁失败", zap.String("instance_id", m.instanceID), zap.Error(err))
	}
}

// leaderStrict 读取「Redis 不可用时是否禁止自认 leader」（LD_LEADER_LOCK_STRICT，默认 1）。
//
// 每次调用都读 env（不在构造时缓存）：灰度时改环境变量重启即可，不需要改代码路径；
// 且零值 Manager（单测里直接字面量构造）也会拿到与线上一致的默认语义。
func leaderStrict() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LD_LEADER_LOCK_STRICT"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// drainTimeout 读取优雅退出的等待上限（LD_TASK_DRAIN，默认 30s）。
func drainTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LD_TASK_DRAIN"))
	if raw == "" {
		return drainDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		logger.L().Warn("LD_TASK_DRAIN 非法，使用默认值", zap.String("value", raw), zap.Duration("default", drainDefault))
		return drainDefault
	}
	if d > 5*time.Minute {
		// 超过 5 分钟已经没有意义：编排层通常先 SIGTERM、15~30s 后 SIGKILL。
		return 5 * time.Minute
	}
	return d
}

// sleepJitter 在 1–3s 之间随机等待（竞选抖动）；ctx 取消时返回 false。
func sleepJitter(ctx context.Context) bool {
	d := time.Second + time.Duration(rand.Int63n(int64(2*time.Second)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
