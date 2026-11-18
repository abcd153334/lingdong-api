package worker

// 终态复查（W1）：把「已判失败/超时、上游其实成功了」的任务捞回来。
//
// ── 为什么必须存在 ────────────────────────────────────────────────────────
// 平台判超时的依据是**自己的** deadline（1 小时），而上游的任务生命周期由它自己决定：
// 排队高峰、模型冷启动、批量任务都会让上游在 1 小时之后才完成。此时我们已经
// 全额退款并停止轮询（fetchPollBatch 明确排除终态），于是产物永远拿不到、
// 钱也永远收不回 —— 上游成本实实在在地付了，账上却是一笔「平台自己承担」的损失。
//
// ── 为什么必须独立成 Job（不能塞进 TaskPollingJob）─────────────────────────
//  1. `fetchPollBatch` 的 `ORDER BY last_polled_at` 依赖 idx_tasks_poll 的列序，
//     再挂一个「按 recheck_at 排序」的消费方就只能改那条索引 —— 而这正是
//     expirePass / orphanPass 被拆出去的原因（B7-5）。
//  2. 5 秒的循环里做「5 分钟一次」的事只能靠内存时间戳：重启丢、多实例不一致，
//     于是每轮都会退化成「每 5 秒查一次 6 小时窗口」，出站量放大 60 倍。
//  3. 事故现场要能**单独**关掉「对已退款用户补扣」这一条链路，而不影响在途任务轮询。
//
// ── 为什么退避与计数必须落库（不复用 taskPollFails）─────────────────────────
// 那个 map 是内存态：重启归零之后，退避会退回第一档 → 每轮重新高频出站；
// 而且终态任务进了那个 map 就再也没有地方清它（轮询只清自己取到的在途任务），
// 长跑实例上就是纯粹的内存泄漏。
//
// ── 资金安全的顺序（与主轮询相反）────────────────────────────────────────
// 主轮询是「Settle → CAS succeeded」；这里是「CAS succeeded → 记账」。
// 原因写在 task.settleLate 的注释里，一句话：迟到路径会和用户主动 Retry 抢同一条任务，
// 先扣钱再 CAS 会**多收**，先 CAS 再扣钱最坏只是**少收**，而少收有对账兜底可见。

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/task"
)

const (
	// taskRecheckInterval 是复查节拍。
	//
	// 5 分钟的依据：对方是「已经退款、没有人在等」的任务，分钟级响应完全够用；
	// 与主轮询的 5 秒差 60 倍，从物理上保证它不可能挤占在途任务的出站额度。
	taskRecheckInterval = 5 * time.Minute

	// taskRecheckRoundBudget 是一轮的墙钟预算。
	//
	// 超预算就停止派发（不 kill 已发出去的）：recheck_at 仍在过去，
	// 下一轮自然继续，**顺延不是故障**。把它当故障处理只会制造噪音告警。
	taskRecheckRoundBudget = 20 * time.Second
)

// 复查的环境变量默认值与上限。
const (
	taskRecheckBatchDefault = 50
	taskRecheckBatchMax     = 500
	// 并发上限 16 而不是与主轮询一样的 64：复查是明确的低优先级外挂，
	// 它的突发不应该把 DB 连接池与上游配额从用户请求那边抢走。
	taskRecheckConcDefault = 4
	taskRecheckConcMax     = 16
)

// pollUpstreamForRecheck 是查询上游任务状态的注入点。
//
// 与 retention.go 的 activeStore 同一手法：集成测试要在不连上游的前提下
// 驱动整条「命中 → CAS → 补收」路径，只能替换这一个函数。
var pollUpstreamForRecheck = relay.PollTaskOf

// TaskRecheckJob 定期复查终态任务的真实上游状态。
type TaskRecheckJob struct{}

// NewTaskRecheckJob 构造复查任务。
func NewTaskRecheckJob() *TaskRecheckJob { return &TaskRecheckJob{} }

// Name 返回任务标识（也是开关与环境变量的命名依据）。
func (*TaskRecheckJob) Name() string { return "task_recheck" }

// Interval 返回复查节拍。
func (*TaskRecheckJob) Interval() time.Duration { return taskRecheckInterval }

// Run 执行一轮复查。
func (j *TaskRecheckJob) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	if !recheckEnabled() {
		return nil
	}
	return j.recheckPass(ctx, pollEnvInt("LD_TASK_RECHECK_BATCH", taskRecheckBatchDefault, taskRecheckBatchMax))
}

// recheckEnabled 判断复查是否启用（LD_TASK_RECHECK=0 一键停）。
//
// 独立开关而不是靠批次大小=0：事故现场要的是「确定性停掉」，
// 而不是「配一个看起来像关闭的数值，然后怀疑它到底有没有生效」。
func recheckEnabled() bool {
	v := strings.TrimSpace(os.Getenv("LD_TASK_RECHECK"))
	return !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off"))
}

// recheckShadow 判断是否处于 shadow 模式（只观测，不补收、不交付）。
//
// 用途：新链路第一次上线时先跑一天，看「能捞回多少、命中率多高」，
// 确认口径无误再打开资金动作。shadow 模式下命中的任务会**关窗**并留日志，
// 因为它的目的就是「估算这条链路的价值」，不是「留一批待处理任务」。
func recheckShadow() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("LD_TASK_RECHECK_MODE")), "shadow")
}

// recheckPass 取一批到期该复查的任务，并发处理。
func (j *TaskRecheckJob) recheckPass(ctx context.Context, batch int) error {
	rows, err := fetchRecheckBatch(ctx, batch)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	conc := pollEnvInt("LD_TASK_RECHECK_CONCURRENCY", taskRecheckConcDefault, taskRecheckConcMax)
	if conc > len(rows) {
		conc = len(rows)
	}
	budget := time.Now().Add(taskRecheckRoundBudget)
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	dispatched, skipped := 0, 0
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		if time.Now().After(budget) {
			skipped = len(rows) - i
			break
		}
		t := rows[i]
		dispatched++
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// 单条 panic 必须在本协程内吃掉：漏出去就是整个进程退出，
			// 所有 Job（关单、退款兜底、清理）一起停摆。
			defer func() {
				if r := recover(); r != nil {
					logger.L().Error("终态复查 panic（已隔离）",
						zap.Bool("task_recheck_panic", true), zap.Uint64("task_id", t.ID), zap.Any("panic", r))
					metrics.Inc(metrics.NameTaskRecheckTotal, "result", "error")
				}
			}()
			j.recheckOne(ctx, &t)
		}()
	}
	wg.Wait()
	if skipped > 0 {
		// Info 而非 Warn：超预算是设计内降速。但必须记 —— 它是
		// 「失败任务积压超过 5 分钟节拍能处理的量」的唯一信号。
		logger.L().Info("终态复查超出本轮预算，剩余任务顺延到下一轮",
			zap.Int("dispatched", dispatched), zap.Int("skipped", skipped))
	}
	return nil
}

// fetchRecheckBatch 取本轮要复查的任务。
//
// 条件与 idx_tasks_recheck 的谓词逐字对应（status IN ? / recheck_until IS NOT NULL
// / recheck_at IS NOT NULL / 时间范围 / ORDER BY recheck_at, id）：
// 少一个条件就会退化成「过滤 + 全量排序」，失败任务一多就把 DB 打满。
func fetchRecheckBatch(ctx context.Context, batch int) ([]model.Task, error) {
	now := time.Now()
	var rows []model.Task
	err := db.DB.WithContext(ctx).
		Where("status IN ?", task.RecheckableStatuses()).
		Where("recheck_until IS NOT NULL AND recheck_until > ?", now).
		Where("recheck_at IS NOT NULL AND recheck_at <= ?", now).
		Order("recheck_at ASC, id ASC").
		Limit(batch).Find(&rows).Error
	return rows, err
}

// recheckOne 复查单条任务。
//
// 结果分档（与 task_recheck_total 的标签基数一致）：
//
//	hit            上游仍在跑，等下一档
//	settled        上游成功，已补收
//	free           上游成功，但补收 0 元（免费模型/余额耗尽）
//	miss           上游明确失败，窗口关闭
//	artifacts_lost 上游成功但产物一个都没拿到，等下一档
//	upstream_failed 查询本身失败（网络/限流），等下一档
//	skipped        本轮跳过（领取失败/渠道不可用）
//	error          我们这边的错误
func (j *TaskRecheckJob) recheckOne(ctx context.Context, t *model.Task) {
	outcome := "error"
	defer func() { metrics.Inc(metrics.NameTaskRecheckTotal, "result", outcome) }()

	// L2 乐观领取：把 recheck_at 推到租约终点并让 count+1。
	// 失败说明另一实例/另一轮已经领走（或该行已被推进），本轮直接让出。
	if !claimRecheck(ctx, t) {
		outcome = "skipped"
		return
	}
	nextCount := t.RecheckCount + 1

	ta, ok := relay.GetTaskAdapter(t.Platform)
	if !ok {
		// 适配器已下线：再等也不会有结果，直接关窗。
		outcome = "skipped"
		logger.L().Warn("终态复查：渠道适配器不支持轮询，关闭窗口",
			zap.Bool("task_recheck_abandoned", true),
			zap.Uint64("task_id", t.ID), zap.String("adapter", t.Platform))
		abandonRecheck(ctx, t.ID, "adapter_missing")
		return
	}

	pc, ok := loadPollChannel(ctx, t, nil)
	if !ok {
		// 渠道缺失/整池冷却/被限流：都是**我方**的临时状态，退避而不是关窗。
		// 关窗会让「渠道冷却 60 秒」变成「放弃这个任务的产物」。
		outcome = "skipped"
		deferRecheck(ctx, t.ID, nextCount)
		return
	}

	// 只罩住出站 GET：后面的产物下载（最多 10 个 × 512MB）不能被 10 秒判死。
	pollCtx, cancelPoll := context.WithTimeout(ctx, taskPollItemTimeout)
	st, err := pollUpstreamForRecheck(pollCtx, ta, pc.cfg, &relay.TaskPollRequest{
		UpstreamTaskID: t.UpstreamTaskID, SubKind: t.SubTaskKind,
	})
	cancelPoll()
	if err != nil || st == nil {
		if err == nil {
			err = errors.New("适配器返回空任务状态")
		}
		// key 级：401/403 说明这把 key 已失效，冷掉它，下一轮复查换一把再试。
		// 不记渠道健康：复查有 24h 窗口，把每次查询失败都算进失败率会把渠道熔断。
		relay.CoolKeyIfAuth(pc.ch, pc.ref, err)
		outcome = "upstream_failed"
		logger.L().Warn("终态复查上游查询失败（按退避重试）",
			zap.Bool("task_recheck_upstream_failed", true),
			zap.Uint64("task_id", t.ID), zap.Int("recheck_count", nextCount), zap.Error(err))
		deferRecheck(ctx, t.ID, nextCount)
		return
	}

	switch normalizeUpstreamStatus(st.Status) {
	case task.StatusSucceeded:
		if !hasArtifactOf(st) {
			// 上游说成功却没产物：可能产物还在生成。不判成功、不扣款，等下一档。
			outcome = "artifacts_lost"
			logger.L().Warn("终态复查：上游报成功但没有产物，按退避重试",
				zap.Bool("task_recheck_artifacts_lost", true), zap.Uint64("task_id", t.ID))
			deferRecheck(ctx, t.ID, nextCount)
			return
		}
		if recheckShadow() {
			outcome = "hit"
			logger.L().Info("终态复查命中迟到成功（shadow 模式：不补收、不交付）",
				zap.Bool("task_recheck_shadow_hit", true),
				zap.Uint64("task_id", t.ID), zap.Int("recheck_count", nextCount),
				zap.Int("artifacts", artifactCountOf(st)))
			abandonRecheck(ctx, t.ID, "shadow")
			return
		}
		if e := task.SucceedLate(ctx, t, st); e != nil {
			if errors.Is(e, task.ErrArtifactsLost) {
				outcome = "artifacts_lost"
				deferRecheck(ctx, t.ID, nextCount)
				return
			}
			outcome = "error"
			logger.L().Error("终态复查收尾失败（按退避重试）",
				zap.Bool("task_recheck_failed", true), zap.Uint64("task_id", t.ID), zap.Error(e))
			deferRecheck(ctx, t.ID, nextCount)
			return
		}
		outcome = "hit"
		// 窗口由 SucceedLate 的 CAS 一并关闭（recheck_at/recheck_until 置 NULL），
		// 这里不能再写一次：那次 UPDATE 的条件是 status IN (expired,failed)，
		// 此刻状态已是 succeeded，写不进任何一行，只会白跑一条 SQL。

	case task.StatusFailed, task.StatusCanceled:
		// 上游自己认了失败：再查也不会有产物，窗口可以关。
		// 这条分支是「复查成本」的主要刹车 —— 大多数超时任务上游确实是失败了。
		outcome = "miss"
		logger.L().Info("终态复查：上游确认失败，关闭窗口",
			zap.Bool("task_recheck_miss", true), zap.Uint64("task_id", t.ID),
			zap.Int("recheck_count", nextCount))
		abandonRecheck(ctx, t.ID, "upstream_failed")

	default:
		// 上游还在 queued/running：这正是窗口存在的意义，退避后再查。
		outcome = "hit"
		logger.L().Info("终态复查：上游仍在处理，按退避继续",
			zap.Bool("task_recheck_pending", true),
			zap.Uint64("task_id", t.ID), zap.Int("recheck_count", nextCount),
			zap.String("upstream_status", st.Status))
		deferRecheck(ctx, t.ID, nextCount)
	}
}

// claimRecheck 是 L2 乐观领取。
//
// 把 recheck_at 推到 now()+RecheckLease 并让 count+1，条件是「recheck_at 仍是读到的那个值」：
//   - 双实例同时跑：只有一条 UPDATE 能命中（另一条读到的是新值，条件不成立）；
//   - 领取后崩溃：租约到期自动重新变成候选，不需要任何清理任务；
//   - recheck_count 与 recheck_at 同一个 UPDATE 写，不会出现「计数涨了但租约没占上」
//     （那会让退避档位凭空跳一级）。
func claimRecheck(ctx context.Context, t *model.Task) bool {
	if t.RecheckAt == nil {
		return false
	}
	res := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND recheck_at = ?", t.ID, *t.RecheckAt).
		Updates(map[string]any{
			"recheck_at":    time.Now().Add(task.RecheckLease),
			"recheck_count": t.RecheckCount + 1,
		})
	if res.Error != nil {
		logger.L().Warn("领取终态复查失败", zap.Uint64("task_id", t.ID), zap.Error(res.Error))
		return false
	}
	return res.RowsAffected > 0
}

// deferRecheck 按退避把下次复查时刻往后推。
//
// 条件带 status IN 终态：如果这条任务已经被别的路径推进成 succeeded，
// 就不要把它重新拉回候选集合（虽然索引谓词已排除，但白写一次 UPDATE 也是浪费）。
func deferRecheck(ctx context.Context, taskID uint64, count int) {
	at := time.Now().Add(task.RecheckDelay(count))
	res := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status IN ?", taskID, task.RecheckableStatuses()).
		Update("recheck_at", at)
	if res.Error != nil {
		// 写失败不影响正确性（recheck_at 仍停在租约终点，最坏是晚 10 分钟再查），
		// 但必须记：持续失败意味着这条任务会一直按租约的最短间隔被选中。
		logger.L().Warn("延后终态复查失败", zap.Uint64("task_id", taskID), zap.Error(res.Error))
	}
}

// abandonRecheck 关闭复查窗口（置 NULL，行立即从 idx_tasks_recheck 消失）。
//
// 「放弃」这个动作必须有明确的 reason 并进日志：复查窗口是资金找回的唯一机会，
// 每一条被提前关掉的任务都值得事后统计「为什么放弃、放弃了多少」。
func abandonRecheck(ctx context.Context, taskID uint64, reason string) {
	if err := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status IN ?", taskID, task.RecheckableStatuses()).
		Updates(map[string]any{"recheck_until": nil, "recheck_at": nil}).Error; err != nil {
		logger.L().Warn("关闭终态复查窗口失败",
			zap.Uint64("task_id", taskID), zap.String("reason", reason), zap.Error(err))
	}
}

// artifactCountOf 只用于日志里报「上游给了几个产物」，不参与任何判定。
//
// 刻意不建切片：判定用的归一化（去空、折叠单链接）在 task.artifactURLs，
// 这里再造一份「差不多」的实现，两份迟早对不上，而日志里的数字一旦与判定不一致，
// 排查时会把人带向完全错误的方向。
func artifactCountOf(st *relay.TaskStatus) int {
	if st == nil {
		return 0
	}
	n := 0
	for _, u := range st.ResultURLs {
		if strings.TrimSpace(u) != "" {
			n++
		}
	}
	if n == 0 && strings.TrimSpace(st.ResultURL) != "" {
		n = 1
	}
	return n
}
