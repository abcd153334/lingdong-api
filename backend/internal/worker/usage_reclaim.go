package worker

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/task"
)

// 回收参数：扫描节拍、单页批量、单轮最多翻几页、被视为「卡死」的年龄阈值。
//
// TTL 必须**大于**同步桥接的最长等待（spec.MaxBridgeTimeoutSecs = 900s，domi 默认 780s），
// 否则在途的同步请求会被当成「卡死」提前回收：ReclaimSettle 把这一单改成 settled，
// 请求随后成功时 Settle 撞 ErrSessionClosed——
// 用户拿到 200 与完整结果，账单却停在估算额、真实用量永不落库。
// 20 分钟 = 900s 等待上限 + 5 分钟结算/重试余量。
const (
	usageReclaimInterval = 5 * time.Minute
	usageReclaimBatch    = 200
	usageReclaimMaxPages = 20
	usageReclaimTTL      = 20 * time.Minute
)

// 回收指标的 result 分档（值域封闭，禁止拼用户/模型名）。
const (
	reclaimCharged        = "charged"
	reclaimReleasedOK     = "released_after_success"
	reclaimReleasedEmpty  = "released_empty"
	reclaimReleasedFailed = "released_failed"
	reclaimReleasedSuper  = "released_superseded"
	reclaimReleasedNoEvid = "released_no_evidence"
	reclaimError          = "error"
	// reclaimStuck 是「同一笔连续失败多轮」的专属分档：它意味着钱卡住了，
	// 与普通 error（下一轮可能就好了）不是一回事，必须能单独告警。
	reclaimStuck = "stuck"
)

// UsageReclaimJob 回收卡在 pending 的计费会话（结算失败/进程重启/客户端断流后未落账）。
//
// 幂等与并发安全：结算走 billing.ReclaimSettle / ReleaseByUsageID，事务内以
// status='pending' 判定，多实例重复执行只会成功一次（Redis leader 锁失效时也不会重复动账）。
type UsageReclaimJob struct{ mu sync.Mutex }

// NewUsageReclaimJob 构造用量回收任务。
func NewUsageReclaimJob() *UsageReclaimJob { return &UsageReclaimJob{} }

// Name 返回任务标识。
func (*UsageReclaimJob) Name() string { return "usage_reclaim" }

// Interval 返回节拍。
func (*UsageReclaimJob) Interval() time.Duration { return usageReclaimInterval }

// reclaimRow 是扫描结果：usage 及其**按 usages.task_id 反查**到的任务。
//
// TaskUsageID 是 tasks.usage_id（任务当前活跃会话）。它与 UsageID 不相等，
// 说明这一单已经被重试取代 —— 这是"只退不收"的唯一判据，见 reclaimOne 的处置顺序。
type reclaimRow struct {
	UsageID           uint64    `gorm:"column:usage_id"`
	UsageCreatedAt    time.Time `gorm:"column:usage_created_at"`
	CallKind          string    `gorm:"column:call_kind"`
	UpstreamRequestID string    `gorm:"column:upstream_request_id"`
	EstPromptTokens   int64     `gorm:"column:est_prompt_tokens"`
	TaskID            *uint64   `gorm:"column:task_id"`
	TaskStatus        *string   `gorm:"column:task_status"`
	TaskUsageID       uint64    `gorm:"column:task_usage_id"`
	ArtifactCount     int       `gorm:"column:artifact_count"`
	ResultFileID      uint64    `gorm:"column:result_file_id"`
}

// reclaimSQL 是扫描语句。
//
// 四个关键决策（改之前先读）：
//  1. **关联锚点必须是 usages.task_id**，不是 tasks.usage_id。
//     后者是「任务当前会话」，重试会改写它（task.Retry 的 CAS），于是旧会话 join 不到任务，
//     被当成孤儿按冻结额再收一次；而 usages.task_id 写入后永不改写（billing.BindTask）。
//  2. **在途过滤下推到 WHERE**。放在 Go 里 `continue` 会让被跳过的行占满 LIMIT：
//     视频任务 20–60 分钟都在 queued/running，超过 batch 条就整轮空转，后面的孤儿永远轮不到。
//  3. **u.task_id > 0 写进 JOIN 条件**：同步行（task_id=0）占绝大多数，
//     放进 ON 可以避免每行都去探一次 tasks 主键。
//  4. **不用 `OR t.usage_id = u.id` 兜底**：那会让 planner 放弃索引探针退化成全表扫。
//     legacy 行（task_id=0 但 tasks.usage_id 指向它）由一次性回填修好（db/migrations_reclaim.go）。
const reclaimSQL = `
SELECT u.id AS usage_id, u.created_at AS usage_created_at,
       COALESCE(u.call_kind, '') AS call_kind,
       COALESCE(u.upstream_request_id, '') AS upstream_request_id,
       u.est_prompt_tokens,
       t.id AS task_id, t.status AS task_status,
       COALESCE(t.usage_id, 0) AS task_usage_id,
       COALESCE(t.artifact_count, 0) AS artifact_count,
       COALESCE(t.result_file_id, 0) AS result_file_id
  FROM usages u
  LEFT JOIN tasks t ON u.task_id > 0 AND t.id = u.task_id AND t.deleted_at IS NULL
 WHERE u.status = 'pending' AND u.created_at < ? AND u.id > ?
   AND NOT EXISTS (SELECT 1 FROM tasks act
                    WHERE act.id = u.task_id AND act.usage_id = u.id
                      AND act.deleted_at IS NULL AND act.status IN ('queued','running'))
 ORDER BY u.id LIMIT ?`

// Run 执行一轮回收。
func (j *UsageReclaimJob) Run(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	cutoff := time.Now().Add(-usageReclaimTTL)
	dryRun := os.Getenv("BILLING_DRYRUN") == "1"
	var cursor uint64
	reclaimed := 0

	for page := 0; page < usageReclaimMaxPages; page++ {
		if ctx.Err() != nil {
			return nil
		}
		var rows []reclaimRow
		if err := db.DB.WithContext(ctx).Raw(reclaimSQL, cutoff, cursor, usageReclaimBatch).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		cursor = rows[len(rows)-1].UsageID

		for _, r := range rows {
			if ctx.Err() != nil {
				return nil
			}
			if dryRun {
				logger.L().Info("用量回收（dry-run，未动账）",
					zap.Bool("billing_reclaim_dryrun", true), zap.Uint64("usage_id", r.UsageID))
				continue
			}
			if j.reclaimOne(ctx, r) {
				reclaimed++
				// 成功即清零：这个计数表达的是「连续」失败，不是累计失败。
				j.clearStrike(r.UsageID)
			}
		}
		if len(rows) < usageReclaimBatch {
			break
		}
	}
	if reclaimed > 0 {
		logger.L().Info("用量回收完成", zap.Int("rows", reclaimed))
	}
	return nil
}

// reclaimOne 处置单条 pending 会话，返回是否真的动账。
//
// 判据顺序不可交换：**先判"被重试取代"，再判在途/终态**。
// 反过来的话，旧会话会命中"任务已交付 → 补结"分支，把钱再收一遍。
func (j *UsageReclaimJob) reclaimOne(ctx context.Context, r reclaimRow) bool {
	// 1) 任务存在，但当前会话已经指向别人 → 这一单被重试取代，只退不收。
	if r.TaskID != nil && r.TaskUsageID != r.UsageID {
		return j.release(ctx, r, reclaimReleasedSuper, model.AnomalyReclaimedRetrySuperseded,
			"reclaimed: 旧会话已被重试取代，全额退回冻结")
	}

	// 2) 任务存在且在途：SQL 已过滤，走到这里说明状态在两次查询之间变了，留给下一轮。
	if r.TaskID != nil && r.TaskStatus != nil &&
		(*r.TaskStatus == task.StatusQueued || *r.TaskStatus == task.StatusRunning) {
		return false
	}

	// 3) 任务存在且终态：按「有没有交付」分流。
	if r.TaskID != nil {
		delivered := r.ArtifactCount > 0 || r.ResultFileID > 0
		if r.TaskStatus != nil && *r.TaskStatus == task.StatusSucceeded && delivered {
			// 已交付但没结算：按 usages 行上的归一列重算，并按**实际落地张数**下调。
			//
			// 以前这里一律按冻结额收：规格模型按 max_n 冻 10 张、实际只出 2 张时，
			// 只要 Settle 失败一次（进程重启/上游超时），用户就要为没拿到的 8 张付钱。
			// artifact_count 是回收期唯一能证明"到底交付了几张"的事实，只降不升。
			// artifact_count 是回收期唯一能证明"到底交付了几张"的事实，只降不升。
			// 但「有产物、清单却没数」（ResultFileID>0 且 ArtifactCount==0）是**未知**，
			// 不是"确定交付 0 条"——必须传 -1，否则 per_clip 会被误判免单。
			delivered := int64(r.ArtifactCount)
			if r.ArtifactCount <= 0 && r.ResultFileID > 0 {
				delivered = -1
			}
			charge, err := billing.New(db.DB).ReclaimSettle(ctx, r.UsageID,
				model.AnomalyReclaimedAfterSuccess, delivered)
			if err != nil {
				j.fail(r, "reclaim_after_success", err)
				return false
			}
			metrics.Inc(metrics.NameUsageReclaim, "result", reclaimCharged)
			logger.L().Warn("用量回收：任务已交付但未结算，已按实际交付量补结",
				zap.Bool("billing_reclaim_after_success", true),
				zap.Uint64("usage_id", r.UsageID), zap.Uint64("task_id", *r.TaskID),
				zap.Int("artifact_count", r.ArtifactCount), zap.Int64("charge", charge))
			return true
		}
		extra := ""
		reason := "reclaimed: 任务已终态且无产物，重试释放冻结"
		result := reclaimReleasedFailed
		if r.TaskStatus != nil && *r.TaskStatus == task.StatusSucceeded {
			// 标的成功但零产物：上游/转存没给出东西，按无交付退款。
			extra = model.AnomalyReclaimedEmptySuccess
			reason = "reclaimed: 任务标记成功但零产物，按无交付退款"
			result = reclaimReleasedEmpty
		} else if strings.TrimSpace(r.UpstreamRequestID) != "" {
			// 失败但上游受理过：平台很可能已经付了成本。金额上仍全额退（用户没拿到东西），
			// 只打标让对账能算出"替用户垫了多少"。
			extra = model.AnomalyReclaimedUpstreamCostLost
		}
		return j.release(ctx, r, result, extra, reason)
	}

	// 4) 没有任务行：先看有没有"上游已受理"的证据。
	//
	// 证据判据用 usages.upstream_request_id（提交/结算成功时落库）。没有证据的孤儿
	// 最常见来源是「Freeze 与 tasks.Create 之间崩溃」—— 上游**一定没被调用**，
	// 按冻结额收就是纯多收。默认仍按历史口径（charge），灰度开关可切 refund。
	if strings.TrimSpace(r.UpstreamRequestID) == "" && reclaimNoEvidenceRefund() {
		return j.release(ctx, r, reclaimReleasedNoEvid, model.AnomalyReclaimedNoEvidence,
			"reclaimed: 无任务且无上游受理证据，全额退回冻结")
	}
	// 没有任务行 = 拿不到 artifact_count，传 -1（未知）→ 按冻结额，与改造前一致。
	charge, err := billing.New(db.DB).ReclaimSettle(ctx, r.UsageID, model.AnomalyReclaimedOrphan, -1)
	if err != nil {
		j.fail(r, "reclaim", err)
		return false
	}
	metrics.Inc(metrics.NameUsageReclaim, "result", reclaimCharged)
	logger.L().Warn("用量回收：已按估算结算并释放差额",
		zap.Bool("billing_reclaim", true), zap.Uint64("usage_id", r.UsageID), zap.Int64("charge", charge))
	return true
}

// release 走统一出口退掉一条冻结，并把结果计入指标。
func (j *UsageReclaimJob) release(ctx context.Context, r reclaimRow, result, anomaly, reason string) bool {
	released, err := billing.New(db.DB).ReleaseByUsageID(ctx, r.UsageID, billing.SettleInput{
		StatusCode:        504,
		ErrorMsg:          reason,
		UsageSource:       "none",
		Anomaly:           anomaly,
		CallKind:          billing.CallKindAsync,
		UpstreamRequestID: r.UpstreamRequestID,
	})
	if err != nil {
		j.fail(r, "release", err)
		return false
	}
	if !released {
		// 已被结算/回收抢先：不是错误，但要能看见。
		metrics.Inc(metrics.NameUsageSettleClosed, "phase", "reclaim")
		return false
	}
	metrics.Inc(metrics.NameUsageReclaim, "result", result)
	logger.L().Warn("用量回收：已释放冻结",
		zap.Bool("billing_reclaim_released", true), zap.String("result", result),
		zap.Uint64("usage_id", r.UsageID), zap.String("anomaly", anomaly))
	return true
}

// reclaimStuckAt 是「连续失败几轮算卡住」。
//
// 回收是 5 分钟一轮：3 轮 ≈ 15 分钟。单次失败可能只是数据库抖动，
// 同一笔钱连续 3 轮都动不了就一定是需要人看的（钱冻着、用户余额已被扣、
// 任务也不会有结果）。这个阈值只影响告警级别，不影响重试。
const reclaimStuckAt = 3

// fail 统一记录回收失败（指标 + 结构化日志 + 连续失败计数）。
func (j *UsageReclaimJob) fail(r reclaimRow, stage string, err error) {
	if err == billing.ErrSessionClosed {
		// 已被别的路径处理：不是故障。
		return
	}
	metrics.Inc(metrics.NameTaskSettleFailures, "stage", stage)
	if n := j.strike(r.UsageID); n >= reclaimStuckAt {
		// 单次失败是抖动，连续失败是「这笔钱卡住了」：用户余额已扣、冻结不释放、
		// 任务也不会再有结果。以前只有一行 Error 日志，没有任何判定「它卡了很久」。
		logger.L().Error("用量回收连续失败（资金卡住，需人工介入）",
			zap.Bool("billing_reclaim_stuck", true), zap.String("stage", stage),
			zap.Uint64("usage_id", r.UsageID), zap.Int64("consecutive", n), zap.Error(err))
		metrics.Inc(metrics.NameUsageReclaim, "result", reclaimStuck)
		return
	}
	logger.L().Error("用量回收失败",
		zap.Bool("billing_reclaim_failed", true), zap.String("stage", stage),
		zap.Uint64("usage_id", r.UsageID), zap.Error(err))
}

// strike 累加「同一 usage 的连续回收失败次数」（24h 窗口）。
//
// Redis 不可用时返回 0（不告警）：这个计数只用于提高告警级别，
// 不能因为它自己不可用而把失败日志也吞掉。
func (j *UsageReclaimJob) strike(usageID uint64) int64 {
	if !db.RedisReady() {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := "ld:reclaim:fail:" + strconv.FormatUint(usageID, 10)
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		return 0
	}
	if n == 1 {
		_ = db.RDB.Expire(ctx, key, 24*time.Hour).Err()
	}
	return n
}

// clearStrike 在回收成功后清掉计数（失败计数只表示「连续」，不是累计）。
func (j *UsageReclaimJob) clearStrike(usageID uint64) {
	if !db.RedisReady() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = db.RDB.Del(ctx, "ld:reclaim:fail:"+strconv.FormatUint(usageID, 10)).Err()
}

// reclaimNoEvidenceRefund 决定「无任务行、且 usages.upstream_request_id 为空」的孤账怎么处置。
//
// 默认 **refund**：这类孤账最常见的来源是「Freeze 与 tasks.Create 之间崩溃」——
// 上游根本没被调用过，按冻结额收就是纯多收（用户拿到 0 却付了全额）。
// 反过来收错（本该收却退了）的代价只是少收一次，而且有 anomaly 留痕可复核。
//
// LD_RECLAIM_NO_EVIDENCE=charge 可退回历史口径（观察期或对账口径不一致时用）。
func reclaimNoEvidenceRefund() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("LD_RECLAIM_NO_EVIDENCE")), "charge")
}
