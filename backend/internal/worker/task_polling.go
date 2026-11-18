package worker

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/channelkeys"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/taskstate"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/channellimit"
	"lingdong-api/internal/service/task"
)

// 轮询参数（v2.0 §4.7.3；B4-1 改为「批查询 + 并发池」）。
const (
	taskPollInterval = 5 * time.Second
	// taskMaxNetworkFail 是单任务连续轮询失败上限（达到即判超时并全额退款）。
	taskMaxNetworkFail = 20
	// taskHealthFailAt 是「开始把轮询失败计入渠道健康」的连续失败次数。
	//
	// 为什么不每次失败都记：一个任务每 5s 轮询一次，全记的话只要 10 分钟就能用
	// **单个**卡住的任务把一条健康渠道的滑窗灌满失败样本，直接熔断。
	// 一段连败只记一个样本，才能让「失败率」反映渠道而不是反映某个倒霉任务。
	taskHealthFailAt = 5
	// taskFailMinDuration 是「判超时退款」的**最短持续时长**（与次数取「与」）。
	//
	// 为什么必须有这一条：taskMaxNetworkFail=20 配上 5 秒节拍只有 100 秒。
	// 一次两分钟的上游抖动（发布、扩容、区域性 5xx）会把这个窗口里**所有**在途任务
	// 判超时并全额退款 —— 而上游仍在生成、仍在计费：钱付了、东西丢了。
	// 10 分钟足够跨过一次抖动，又远小于 deadline（1h，最终兜底）。
	taskFailMinDuration = 10 * time.Minute
	// taskOrphanQueuedTTL 是「已冻结但没有上游任务号」的 queued 任务的存活上限。
	//
	// 这个值必须**大于最长的提交窗口**：图像提交的硬超时上限是 330s（见 relay 的提交
	// 与上游读超时），加上排队与重试，一次合法提交完全可能持续数分钟。取 5 分钟会误杀正在提交中的任务
	// （任务被判失败退款、上游随后建单成功 → 平台白付一次生成）。
	// 15 分钟 = 330s 的 2.7 倍，且有 deadline(1h) 与 task_reclaim(2h) 两层兜底在它之后。
	taskOrphanQueuedTTL = 15 * time.Minute

	// taskPollItemTimeout 是**单条任务查上游**的超时（B7-5）。
	//
	// 只包住出站查询，不包状态推进：Succeed 里要下载最多 10 个产物（每个可达 512MB），
	// 把结算也罩进 10 秒会把「正在下载」变成「deadline exceeded」，任务反复重试却永远结不了账。
	//
	// 10 秒的依据：轮询是一次 GET，正常 100–500ms。适配器默认超时是 60s，
	// 卡住的上游会把一个并发槽占满 60 秒 —— 8 并发 × 60s 下一轮 200 条要 25 分钟，
	// 期间新任务全部排队。10 秒足够跨洋 RTT 加抖动，又不会拖垮整轮。
	taskPollItemTimeout = 10 * time.Second
	// taskPollRoundBudget 是**一整轮**的墙钟预算（B7-5）。
	//
	// 单条有 10s 上限，但 200 条 / 8 并发最坏仍是 250s：这段时间内下一轮起不来
	// （Job 单协程串行），表现是「所有任务同时变慢」。超预算就停手，剩下的任务
	// 保留旧的 last_polled_at，下一轮仍在队头 —— 不会饿死，只是整体降速。
	// 取 30s = 6 个节拍：既给了突发流量足够的时间片，又保证节拍本身不漂。
	taskPollRoundBudget = 30 * time.Second

	// taskExpireInterval / taskOrphanInterval 是拆分出来的两个巡检 Job 的节拍（B7-5）。
	//
	// 为什么要拆：它们以前和轮询挤在同一个 Run 里，任何一边积压都会直接挤掉另一边的
	// 出站名额 —— 到期任务多的时候（上游集体超时）轮询被饿着，在途任务全被判超时；
	// 反过来轮询慢的时候到期任务迟迟不释放，用户余额白占。
	// 拆开后各自有独立节拍与独立批次，互不影响。
	//
	// 到期用 30s：退款及时性直接体现为「用户看到余额多久恢复」；
	// 孤儿用 2min：TTL 本身是 15 分钟，1/8 的粒度足够，且它是全表条件扫描。
	taskExpireInterval = 30 * time.Second
	taskOrphanInterval = 2 * time.Minute
)

// 环境变量默认值与上限（上限是为了防止误配置把 DB 连接池打爆）。
const (
	taskPollBatchDefault       = 200
	taskPollBatchMax           = 2000
	taskPollConcurrencyDefault = 8
	taskPollConcurrencyMax     = 64
)

// TaskPollingJob 轮询未终态异步任务并推进状态机。
//
// 职责收窄（B7-5）：只做「取批 + 并行查上游」。到期推进与孤儿回收拆成了
// TaskExpireJob / TaskOrphanJob —— 两者都不出站，和轮询抢同一个批次与节拍只会互相拖累。
//
// 状态推进全部走 service/task 的 CAS（迟到结果被拒），因此并发轮询同一任务不会双结算。
type TaskPollingJob struct{}

// NewTaskPollingJob 构造轮询任务。
func NewTaskPollingJob() *TaskPollingJob { return &TaskPollingJob{} }

// Name 返回任务标识。
func (*TaskPollingJob) Name() string { return "task_polling" }

// Interval 返回轮询节拍（§4.7.3：每 5s 一轮）。
func (*TaskPollingJob) Interval() time.Duration { return taskPollInterval }

// Run 执行一轮轮询。
func (j *TaskPollingJob) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	return j.pollPass(ctx, pollEnvInt("LD_TASK_POLL_BATCH", taskPollBatchDefault, taskPollBatchMax))
}

// TaskExpireJob 推进已过 deadline 的在途任务（expired + 全额释放）。
//
// 为什么独立成 Job：到期推进不出站、只写库，把它挂在轮询上会让「上游集体变慢」
// 与「大量任务到期」互相挤占同一个节拍 —— 前者让后者迟到，后者让前者拿不到出站名额。
type TaskExpireJob struct{}

// NewTaskExpireJob 构造到期推进任务。
func NewTaskExpireJob() *TaskExpireJob { return &TaskExpireJob{} }

// Name 返回任务标识。
func (*TaskExpireJob) Name() string { return "task_expire" }

// Interval 返回巡检节拍。
func (*TaskExpireJob) Interval() time.Duration { return taskExpireInterval }

// Run 执行一轮到期推进。
func (j *TaskExpireJob) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	j.expirePass(ctx, pollEnvInt("LD_TASK_POLL_BATCH", taskPollBatchDefault, taskPollBatchMax))
	return nil
}

// TaskOrphanJob 回收「冻结了但从未拿到上游任务号」的 queued 任务。
//
// 成因：进程在 Freeze 与 CAS(running) 之间被杀、或 CAS 一直失败。
// 这类任务以前只能等到 deadline（1h）才释放 —— 用户余额被白占一小时，
// 而且管理台看到的是「排队中」，完全误导。这里用 15 分钟（> 最长提交窗口）兜住。
type TaskOrphanJob struct{}

// NewTaskOrphanJob 构造孤儿任务回收任务。
func NewTaskOrphanJob() *TaskOrphanJob { return &TaskOrphanJob{} }

// Name 返回任务标识。
func (*TaskOrphanJob) Name() string { return "task_orphan" }

// Interval 返回巡检节拍。
func (*TaskOrphanJob) Interval() time.Duration { return taskOrphanInterval }

// Run 执行一轮孤儿回收。
func (j *TaskOrphanJob) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	j.orphanPass(ctx, pollEnvInt("LD_TASK_POLL_BATCH", taskPollBatchDefault, taskPollBatchMax))
	return nil
}

// expirePass 推进已过 deadline 的在途任务（expired + 全额释放）。
//
// 与轮询拆成两条查询的原因：轮询那条要能被 idx_tasks_poll 完整覆盖
// （`deadline_at > now()` 是过滤条件，混在一起会让排序走不上索引）；
// 而且到期任务根本不需要出站，没必要和上游查询抢同一批并发名额。
func (*TaskExpireJob) expirePass(ctx context.Context, batch int) {
	var rows []model.Task
	err := db.DB.WithContext(ctx).
		Where("status IN ?", []string{task.StatusQueued, task.StatusRunning}).
		Where("deadline_at IS NOT NULL AND deadline_at <= ?", time.Now()).
		Order("deadline_at ASC, id ASC").Limit(batch).Find(&rows).Error
	if err != nil {
		logger.L().Warn("扫描到期任务失败", zap.Error(err))
		return
	}
	for i := range rows {
		if ctx.Err() != nil {
			return
		}
		t := rows[i]
		if err := task.Expire(ctx, &t); err != nil {
			logger.L().Warn("任务超时推进失败", zap.Uint64("task_id", t.ID), zap.Error(err))
		}
		clearPollState(t.ID)
	}
}

// orphanPass 回收「冻结了但从未拿到上游任务号」的 queued 任务（判据见 taskOrphanQueuedTTL）。
func (*TaskOrphanJob) orphanPass(ctx context.Context, batch int) {
	cutoff := time.Now().Add(-taskOrphanQueuedTTL)
	var rows []model.Task
	err := db.DB.WithContext(ctx).
		Where("status = ?", task.StatusQueued).
		Where("COALESCE(upstream_task_id, '') = ''").
		Where("created_at < ?", cutoff).
		Order("id ASC").Limit(batch).Find(&rows).Error
	if err != nil {
		logger.L().Warn("扫描未提交任务失败", zap.Error(err))
		return
	}
	for i := range rows {
		if ctx.Err() != nil {
			return
		}
		t := rows[i]
		if err := task.FailOrphanSubmit(ctx, &t, "任务未能提交到上游（进程中断或上游始终未受理），已释放预扣"); err != nil {
			logger.L().Warn("未提交任务回收失败", zap.Uint64("task_id", t.ID), zap.Error(err))
			continue
		}
		clearPollState(t.ID)
	}
}

// fetchPollBatch 取出本轮要轮询的任务（按公平顺序）。
//
// 抽成独立函数是为了可测：排序写成 `last_polled_at ASC` 少一个 NULLS FIRST，
// 表现是「新任务永远排在存量行后面、长时间不推进」，而在代码评审里几乎看不出来。
func (j *TaskPollingJob) fetchPollBatch(ctx context.Context, batch int) ([]model.Task, error) {
	var rows []model.Task
	err := db.DB.WithContext(ctx).
		Where("status IN ?", []string{task.StatusQueued, task.StatusRunning}).
		Where("COALESCE(upstream_task_id, '') <> ''").
		Where("deadline_at IS NULL OR deadline_at > ?", time.Now()).
		Order("last_polled_at ASC NULLS FIRST, id ASC").
		Limit(batch).Find(&rows).Error
	return rows, err
}

// pollPass 取一批在途任务并行轮询。
//
// 取批顺序 `last_polled_at ASC NULLS FIRST, id ASC` 是公平性关键：
//   - NULLS FIRST 让「从未轮询过」的新任务优先（否则历史行会一直压着新任务）；
//   - 每轮无论结果都更新 last_polled_at（与 CAS 分开），失败的任务排到队尾而不是霸占队头；
//   - 主键 id 作第二排序键，保证同一时刻的并列项有确定顺序（避免同一批反复被选中）。
func (j *TaskPollingJob) pollPass(ctx context.Context, batch int) error {
	rows, err := j.fetchPollBatch(ctx, batch)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	conc := pollEnvInt("LD_TASK_POLL_CONCURRENCY", taskPollConcurrencyDefault, taskPollConcurrencyMax)
	if conc > len(rows) {
		conc = len(rows)
	}
	// 批内预取渠道与密钥池：200 条同渠道从 200 次查询 + 200 次解密降到 1 + 1。
	pf := prefetchChannels(ctx, rows)
	// 本轮墙钟预算（B7-5）：到点就不再派发新任务。
	//
	// 不 kill 正在跑的那几个：中断一次已经发出去的轮询，结果只会是「这一轮白查，
	// 下一轮重来」，而它可能马上就能返回。停派发既能保证节拍不漂，又不浪费已发生的调用。
	budget := time.Now().Add(taskPollRoundBudget)
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
			// 单条任务的 panic 必须在本协程内吃掉：漏出去就是整个进程退出，
			// 所有 Job（订单关单、退款兜底、清理）一起停摆——一个畸形上游响应能引发全站事故。
			defer func() {
				if r := recover(); r != nil {
					logger.L().Error("任务轮询 panic（已隔离）",
						zap.Bool("poll_panic", true), zap.Uint64("task_id", t.ID), zap.Any("panic", r))
				}
			}()
			j.poll(ctx, &t, pf)
			j.markPolled(ctx, t.ID)
		}()
	}
	wg.Wait()
	if skipped > 0 {
		// Info 而不是 Warn：超预算是**设计内**的降速，不是故障。
		// 但必须记：它是「轮询跟不上提交速度」的唯一信号，容量体检要拿它当输入。
		logger.L().Info("轮询超出本轮预算，剩余任务顺延到下一轮",
			zap.Int("dispatched", dispatched), zap.Int("skipped", skipped),
			zap.Duration("budget", taskPollRoundBudget))
	}
	return nil
}

// markPolled 记录「这条任务本轮已被轮询过」。
//
// 必须**无条件**写（不看 CAS 结果），且用不带取消的 context：
//   - 不看 CAS：被 CAS 拒绝的任务（并发推进到终态/失败）若还停在旧的 last_polled_at，
//     下一轮又会排在最前面，把整批名额占满 —— 饥饿；
//   - 不带取消：leader 切换/优雅退出时 ctx 已取消，此时更需要把进度落下来，
//     否则重启后这批任务还是排在队头。
func (j *TaskPollingJob) markPolled(ctx context.Context, id uint64) {
	err := db.DB.WithContext(context.WithoutCancel(ctx)).
		Model(&model.Task{}).Where("id = ?", id).
		Update("last_polled_at", time.Now()).Error
	if err != nil {
		logger.L().Warn("更新轮询时间失败（下一轮可能重复选中）", zap.Uint64("task_id", id), zap.Error(err))
	}
}

// pollChannel 是一次出站要用的渠道上下文（渠道行 + 授权配置 + 选中的密钥）。
//
// 打成一个结构体而不是四个返回值：密钥必须与渠道成对出现，
// 任何一处拿「渠道 A 的密钥」配「渠道 B 的配置」都会 401，而现象是「上游鉴权失败」，
// 排查方向完全错（会去查密钥是否过期，而不是查参数错配）。
type pollChannel struct {
	ch  *model.Channel
	cfg *relay.ChannelConfig
	// ref 必须整枚带出来（不能只留 ref.Key）：失败时要用它的 ID 冷掉这把 key，
	// 只留明文就只能"知道用了哪把"，却无法把"这把它坏了"记到 key 级状态上。
	ref relay.KeyRef
}

// channelPrefetch 是本批次预取的渠道行与**已解密**密钥池。
//
// 为什么要预取：200 条任务同一渠道时，旧实现要查 200 次 channels 全行（含密钥密文）
// 并解密 200 次，每 5 秒一轮。一次 IN 查询 + 一次解密就够。
//
// 为什么**不跨轮**缓存：渠道停用/换 key 必须立即生效；密钥明文进长生命周期缓存
// 还会带来「轮换后旧 key 仍可用」「内存转储可读」两个问题。
// 单轮复用已经消掉 199/200 的开销，再往后就是拿正确性换性能。
type channelPrefetch struct {
	ch  map[uint64]*model.Channel
	dec map[uint64]channelkeys.Result
}

// prefetchChannels 批量取渠道并解密（失败时退化为空表，调用方逐条走库）。
func prefetchChannels(ctx context.Context, rows []model.Task) *channelPrefetch {
	seen := make(map[uint64]bool, len(rows))
	ids := make([]uint64, 0, len(rows))
	for i := range rows {
		id := rows[i].ChannelID
		if id != 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return &channelPrefetch{}
	}
	pf := &channelPrefetch{
		ch:  make(map[uint64]*model.Channel, len(ids)),
		dec: make(map[uint64]channelkeys.Result, len(ids)),
	}
	var chs []model.Channel
	if err := db.DB.WithContext(ctx).Where("id IN ?", ids).Find(&chs).Error; err != nil {
		// 预取失败不能拖垮整轮：返回空表，下面逐条查询（性能退化，但任务照常推进）。
		logger.L().Warn("批量预取渠道失败，本轮退化为逐条查询",
			zap.Int("channels", len(ids)), zap.Error(err))
		return &channelPrefetch{}
	}
	for i := range chs {
		ch := &chs[i]
		pf.ch[ch.ID] = ch
		pf.dec[ch.ID] = channelkeys.Decrypt(ch.Keys)
	}
	return pf
}

// lookup 取预取结果；未命中返回 ok=false（调用方回落单条查询）。
func (p *channelPrefetch) lookup(id uint64) (*model.Channel, channelkeys.Result, bool) {
	if p == nil || p.ch == nil {
		return nil, channelkeys.Result{}, false
	}
	ch, ok := p.ch[id]
	if !ok {
		return nil, channelkeys.Result{}, false
	}
	return ch, p.dec[id], true
}

// loadPollChannel 取渠道 + 选密钥 + 计入渠道 RPM。
//
// 抽出来给「主轮询」与「终态复查」共用（两处都要打上游、都要受同一套限流约束）。
// 为什么不各写一份：PickKey 的口径一旦分叉，典型漏项是「复查那份忘了 AcquireRPM」——
// 于是复查成了绕过渠道限流的旁路，上游限流被我们自己的后台任务打穿，
// 而主轮询那一侧看起来完全正常，没人会去查另一个 Job。
//
// pf 是本批次预取的渠道与已解密密钥池（可为 nil，复查链路逐条走库）。
// 返回 ok=false 表示本轮跳过（渠道缺失 / 整池冷却 / 被限流），**不是任务失败**：
// 这三种都是我方保护动作，算成任务失败会让「上游限流 15 秒」变成「任务被判失败退款」。
func loadPollChannel(ctx context.Context, t *model.Task, pf *channelPrefetch) (*pollChannel, bool) {
	ch, res, hit := pf.lookup(t.ChannelID)
	if !hit {
		// 未命中（或预取整体失败）：回落单条查询。宁可慢一条，不可让整轮白跑。
		var one model.Channel
		if err := db.DB.WithContext(ctx).First(&one, t.ChannelID).Error; err != nil {
			logger.L().Warn("任务渠道缺失", zap.Uint64("task_id", t.ID), zap.Uint64("channel_id", t.ChannelID), zap.Error(err))
			return nil, false
		}
		ch, res = &one, channelkeys.Decrypt(one.Keys)
	}
	ref, err := relay.PickKeyDecrypted(ch, res, nil)
	if err != nil {
		// 整池冷却（限流/鉴权）：这一轮不打上游，**也不记连败**。
		logger.L().Warn("任务渠道无可用密钥，跳过本轮", zap.Uint64("task_id", t.ID), zap.Error(err))
		return nil, false
	}
	// 轮询计入渠道 RPM（P1-5），但**不占并发槽**：轮询是后台行为，
	// 占槽会与用户请求抢额度，表现成「有任务在跑时新请求 429」。
	if err := channellimit.AcquireRPM(ctx, ch); err != nil {
		logger.L().Info("轮询被渠道限流，跳过本轮", zap.Uint64("task_id", t.ID), zap.Error(err))
		return nil, false
	}
	return &pollChannel{ch: ch, cfg: relay.ChannelConfigFor(ch, ref.Key), ref: ref}, true
}

// poll 轮询单个任务并推进状态。
func (j *TaskPollingJob) poll(ctx context.Context, t *model.Task, pf *channelPrefetch) {
	ta, ok := relay.GetTaskAdapter(t.Platform)
	if !ok {
		logger.L().Warn("任务渠道不支持异步轮询", zap.Uint64("task_id", t.ID), zap.String("adapter", t.Platform))
		return
	}
	pc, ok := loadPollChannel(ctx, t, pf)
	if !ok {
		return
	}
	ch, key := pc.ch, pc.ref.Key

	// 出站查询单条限时（B7-5）：只罩住这一次 GET，不罩状态推进 ——
	// 后面的 Succeed 要下载产物，10 秒不够，也不该被它的时长决定。
	pollCtx, cancelPoll := context.WithTimeout(ctx, taskPollItemTimeout)
	st, err := relay.PollTaskOf(pollCtx, ta, pc.cfg, &relay.TaskPollRequest{
		UpstreamTaskID: t.UpstreamTaskID, SubKind: t.SubTaskKind,
	})
	cancelPoll()
	if err != nil || st == nil {
		if err == nil {
			// 适配器返回 (nil, nil)：契约上不该发生，但绝不能当成成功（会把任务判失败退款）。
			err = errors.New("适配器返回空任务状态")
		}
		j.onPollFailure(ctx, t, ch, pc.ref, err)
		return
	}
	// 从连败中恢复：配对记一条成功样本，让渠道失败率不被单任务的连败拉满。
	if n := clearTaskPollFail(t.ID); n > 0 {
		relay.RecordChannelSuccess(ch)
	}
	// 状态词不认识：仍然按 running 等（钱已冻结、上游可能真在跑），
	// 但必须能被看见 —— 否则 status_map 少写一个值的表现是"任务一直跑到 deadline"，
	// 用户已经在投诉，运维手上却什么都没有。按任务去重，避免 5 秒一条刷屏。
	if raw := strings.TrimSpace(st.Status); raw == "" {
		// 适配器没给状态：按 running 等（与未知词同处置），但不打"未识别"——
		// 那是空响应，不是词表缺项，混在一起会让指标失去指向性。
	} else if _, known := taskstate.Normalize(raw); known {
		clearUnmatchedWarn(t.ID)
	} else if firstUnknown(t.ID) {
		metrics.Inc(metrics.NameTaskStatusUnmatched, "adapter", t.Platform)
		logger.L().Warn("上游状态词未识别，按运行中处理",
			zap.Bool("task_status_unmatched", true),
			zap.Uint64("task_id", t.ID), zap.String("adapter", t.Platform),
			zap.String("raw_status", raw))
	}

	switch normalizeUpstreamStatus(st.Status) {
	case task.StatusSucceeded:
		if !hasArtifactOf(st) {
			if e := task.Fail(ctx, t, "上游返回成功但缺少产物"); e != nil {
				logger.L().Warn("任务失败推进失败", zap.Uint64("task_id", t.ID), zap.Error(e))
			}
			return
		}
		if e := task.Succeed(ctx, t, st); e != nil {
			// 结算失败会保持 running 等下一轮重试，所以这里既是日志也是**告警口径**：
			// 该计数持续 >0 说明账务侧有系统性故障（钱没扣成），不能只靠翻日志发现。
			metrics.Inc(metrics.NameTaskSettleFailures, "stage", "poll")
			logger.L().Warn("任务结算失败（下轮重试）", zap.Uint64("task_id", t.ID), zap.Error(e))
		}
	case task.StatusFailed, task.StatusCanceled:
		// 上游自报的失败原因可能回显主机名/密钥（部分上游会把请求头带进错误体），
		// 落库前用渠道级脱敏：这里拿得到 ch 与 key，比任务域的通用兜底更彻底。
		if e := task.Fail(ctx, t, relay.RedactUpstream(upstreamReason(st), ch, key)); e != nil {
			logger.L().Warn("任务失败推进失败", zap.Uint64("task_id", t.ID), zap.Error(e))
		}
	default:
		// queued/running/未知：按 §4.7.1 视为运行中，等待下一轮。
	}
}

// onPollFailure 处理一次轮询失败：先分档，再决定「重试 / 换 key / 立即失败」。
//
// 分档的必要性：改造前所有错误都走同一条路（计连败 → 20 次判超时退款），
// 但 404（上游任务已被清理）、429（我方被限流）、401（这把 key 失效）的正确处置
// 完全不同。一律当"连续失败"的结果是：上游明明还在生成，任务已经被判超时退款。
//
// 顺序是刻意的（B4-5）：「先记健康、再判超时」。反过来的话，最后一次失败会先把任务
// 判死并退款，渠道健康样本永远拿不到——而这条渠道恰恰最该被熔断。
func (j *TaskPollingJob) onPollFailure(ctx context.Context, t *model.Task, ch *model.Channel,
	ref relay.KeyRef, err error) {
	class := relay.ClassifyPoll(err)
	// key 级：只认 401/403（判据在 relay.CoolKeyIfAuth，与 classify 同源）。
	//
	// 不加这一句的后果是「一枚失效的 key 会被每一轮轮询反复打」——
	// 轮询拿的是渠道里随机/轮转的一把，坏的那把只是在混战中偶尔被选中，
	// 却足以让一个已经生成好的任务走到"连续失败超限"被判超时退款。
	relay.CoolKeyIfAuth(ch, ref, err)

	switch class {
	case relay.PollFailureNotFound:
		// 404/410 是**确定性结论**：上游已经不认这个任务号，等下去必然超时退费，
		// 而且最后写进 error_msg 的是"连续失败"，与真实原因无关（排障方向全错）。
		// 不计渠道健康：上游清理任务不是渠道故障。
		logger.L().Warn("上游任务不存在，判定失败并全额退款",
			zap.Bool("poll_not_found", true),
			zap.Uint64("task_id", t.ID), zap.Uint64("channel_id", ch.ID),
			zap.String("adapter", ch.AdapterID), zap.String("upstream_id", t.UpstreamTaskID), zap.Error(err))
		if e := task.Fail(ctx, t, "上游任务不存在（可能已被上游清理），无法继续查询"); e != nil {
			logger.L().Warn("任务失败推进失败", zap.Uint64("task_id", t.ID), zap.Error(e))
		}
		clearPollState(t.ID)
		return
	case relay.PollFailureAuth, relay.PollFailureRateLimited:
		// 这两档都不是「上游坏了」，**不计任务连败**：
		//   auth → 下一轮 PickKey 会跳过被冷却的 key，通常一轮就恢复；
		//   429  → 我方被限流，任务本身还在正常生成。
		// 计进连败的后果是：一次 200 任务的限流把整批任务判超时退款。
		// deadline（1h）仍是最终兜底，不会永远挂着。
		logger.L().Info("轮询被上游限流/鉴权拦截，本轮不计连败",
			zap.String("poll_class", pollClassName(class)),
			zap.Uint64("task_id", t.ID), zap.Uint64("channel_id", ch.ID), zap.Error(err))
		return
	}

	n, since := incTaskPollFail(t.ID)
	if n == taskHealthFailAt {
		// 渠道级归因保持空 ref：它按连败阈值计数，不能被 key 级判据改写。
		relay.RecordChannelAttempt(ch, relay.KeyRef{}, err)
	}
	// 「次数够」且「持续够」才判死（见 taskFailMinDuration）。
	if !pollGiveUp(n, since, time.Now()) {
		return
	}
	relay.RecordChannelAttempt(ch, relay.KeyRef{}, err)
	reason := "上游轮询连续失败超限"
	if class == relay.PollFailureOther {
		// 4xx 不是上游故障，绝大多数是查询路径/参数写错：给一句能指方向的文案，
		// 否则运维会照着"上游不稳定"去查状态页。
		reason = "上游轮询连续失败（查询路径或鉴权配置可能有误）"
	}
	logger.L().Warn("任务轮询连续失败超限，判定失败并全额退款",
		zap.Bool("poll_give_up", true), zap.String("poll_class", pollClassName(class)),
		zap.Uint64("task_id", t.ID), zap.Uint64("channel_id", ch.ID),
		zap.String("adapter", ch.AdapterID), zap.Int("fails", n),
		zap.Duration("failed_for", time.Since(since)), zap.Error(err))
	if e := task.FailTimeout(ctx, t, reason); e != nil {
		logger.L().Warn("任务失败推进失败", zap.Uint64("task_id", t.ID), zap.Error(e))
	}
	// 无论成没成功都要清计数：失败说明已达终态（不再出现在轮询批次里），
	// 不清就会在 map 里永久留一条僵尸记录（长跑实例上这是纯粹的内存泄漏）。
	clearPollState(t.ID)
}

// pollGiveUp 判断连败是否已到「判超时退款」的程度。
//
// 抽成纯函数是为了能穷举三条边界（次数够但时间不够 / 时间够但次数不够 / 都够）——
// 这三条只在故障时才会走到，靠联调几乎触发不了，而判错的代价是把还在生成的任务退款。
func pollGiveUp(n int, since, now time.Time) bool {
	return n >= taskMaxNetworkFail && now.Sub(since) >= taskFailMinDuration
}

// pollClassName 是日志里的档位名（只用于排障，不参与判定）。
func pollClassName(c relay.PollFailureClass) string {
	switch c {
	case relay.PollFailureNotFound:
		return "not_found"
	case relay.PollFailureAuth:
		return "auth"
	case relay.PollFailureRateLimited:
		return "rate_limited"
	case relay.PollFailureOther:
		return "client"
	default:
		return "transient"
	}
}

// hasArtifactOf 判断「上游已成功」的响应里是否真的带来产物。
//
// 成功判定必须覆盖三种产物形态：单链接 / 多链接 / 纯文本（歌词）。
// 只看 ResultURL 会把「多图任务」与「歌词任务」判成失败并全额退款，
// 而上游其实已经生成好了 —— 钱付了、东西丢了，是用户侧最容易投诉的那种事故。
//
// 抽成独立函数只为可测：这条规则一旦被改窄，事故表现是「部分模型全量失败」，很难在评审里看出来。
func hasArtifactOf(st *relay.TaskStatus) bool {
	if st == nil {
		return false
	}
	if strings.TrimSpace(st.ResultURL) != "" {
		return true
	}
	for _, u := range st.ResultURLs {
		if strings.TrimSpace(u) != "" {
			return true
		}
	}
	return strings.TrimSpace(st.ResultText) != ""
}

// normalizeUpstreamStatus 归一化上游状态文案（§4.7.1：未知状态一律按 running 处理）。
//
// 适配器应当把状态归一到底（不得透出上游原文），这里是第二道防线：
// 插件适配器与将来新增的厂商可能直接返回 "succeed" / "finished" 这类写法。
func normalizeUpstreamStatus(s string) string {
	// 别名表只有一份（taskstate.Normalize）：可灵的 "succeed"、nano 的数字 "3"、
	// AutoDL 的 "completed" 在三条链路上必须给出同一个结论。
	if st, ok := taskstate.Normalize(s); ok {
		return st
	}
	// 不认识的写法按「还在跑」处理：上游新增排队态时误判成失败会立即退款并丢弃产物。
	return task.StatusRunning
}

// upstreamReason 提取上游失败原因（缺省给出兜底文案）。
func upstreamReason(st *relay.TaskStatus) string {
	if r := strings.TrimSpace(st.Error); r != "" {
		return r
	}
	return "上游任务失败"
}

// taskPollFails 是「单任务连续轮询失败」计数（内存态；进程重启归零，不影响到期兜底）。
//
// 包级单例而不是 Job 的字段（B7-5）：计数现在有三个使用方 —— 轮询（自增/清零）、
// 到期推进与孤儿回收（任务终结后清零）。拆成多个 Job 后如果各持一份，
// 终结的任务只能清掉自己那一份，其余 map 里的记录会永久留存 —— 长跑实例上就是内存泄漏。
//
// since 是「本轮连败的起点」：判超时必须「次数够」且「持续够」，只看次数会让
// 100 秒的抖动杀掉所有在途任务（见 taskFailMinDuration）。
var taskPollFails = struct {
	sync.Mutex
	m map[uint64]pollFailState
}{m: make(map[uint64]pollFailState)}

type pollFailState struct {
	n     int
	since time.Time
}

// incTaskPollFail 递增连续失败计数，返回当前次数与本轮连败起点。
func incTaskPollFail(id uint64) (int, time.Time) {
	taskPollFails.Lock()
	defer taskPollFails.Unlock()
	s := taskPollFails.m[id]
	if s.n == 0 {
		s.since = time.Now()
	}
	s.n++
	taskPollFails.m[id] = s
	return s.n, s.since
}

// unmatchedWarned 是「未识别状态词」的去重集合（taskID → 已告警）。
//
// 同一个任务每 5 秒轮询一次，不去重就是每分钟 12 条一模一样的 Warn：
// 真正的新问题会被自己的噪音淹掉。任务进终态或状态恢复可识别时删除。
var unmatchedWarned sync.Map

// firstUnknown 报告这是该任务第一次出现未识别状态（只有第一次打点+告警）。
func firstUnknown(id uint64) bool {
	_, loaded := unmatchedWarned.LoadOrStore(id, struct{}{})
	return !loaded
}

// clearUnmatchedWarn 在状态恢复可识别时清掉去重标记（下次再出现要重新告警）。
func clearUnmatchedWarn(id uint64) { unmatchedWarned.Delete(id) }

// clearPollState 清掉一个任务的**全部**进程内轮询态（连败计数 + 未识别告警去重）。
//
// 终态路径必须走它：两处各清一半，会让另一份在长跑实例上永久留存 —— 内存泄漏，
// 而且下一次同 id 复用时（不会发生，但语义上）会静默吞掉告警。
func clearPollState(id uint64) {
	clearTaskPollFail(id)
	clearUnmatchedWarn(id)
}

// clearTaskPollFail 清除连续失败计数，返回清除前的连败次数（>0 表示刚从连败中恢复）。
func clearTaskPollFail(id uint64) int {
	taskPollFails.Lock()
	defer taskPollFails.Unlock()
	n := taskPollFails.m[id].n
	delete(taskPollFails.m, id)
	return n
}

// pollEnvInt 读取正整数环境变量（缺省/非法用默认值，超过上限按上限截断）。
//
// 上限不是为了"保守"，而是防误配置：Batch 填成 100000 会让每轮把整表读进内存，
// 并发填成 1000 会瞬间耗尽 DB 连接池（MaxOpen 默认 50），表现为全站 500。
func pollEnvInt(name string, def, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logger.L().Warn("轮询参数非法，使用默认值",
			zap.String("name", name), zap.String("value", raw), zap.Int("default", def))
		return def
	}
	if n > max {
		return max
	}
	return n
}
