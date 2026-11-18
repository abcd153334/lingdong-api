// Package calllog 记录**预检期失败**的 0 费调用记录（C1 · 决策 3）。
//
// 为什么要单独一套写入器，而不是复用 billing.Freeze：
//   - 这些失败发生在鉴权/参数校验/定价预检阶段，**根本没有计费会话**，
//     也不能建会话（建了就要冻结额度，而"没有余额"正是部分失败的原因）；
//   - 它们的量级完全不同：一次 429 风暴或脚本拿错 key 能在几秒内打出上万条，
//     同步 INSERT 会把网关拖垮 —— 所以必须批量 + 异步 + 满了丢弃。
//
// 语义（必须与"真实计费行"区分清楚）：
//   - status = rejected，charge = 0，不写 transactions，不动 users.request_count；
//   - 计入 usage_daily.requests 与 usage_daily.errors（看板失败率的分母/分子）；
//   - started_at = finished_at = 写入时刻（预检失败没有可度量的耗时）。
package calllog

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/textx"

	"lingdong-api/internal/service/callio"
)

const (
	// queueSize 是缓冲深度：足够吸收一次突发风暴，又不至于让进程退出时丢太多。
	queueSize = 1024
	// batchMax 是单批最多行数（一次 INSERT 一条语句，避免 N 次往返）。
	batchMax = 100
	// flushInterval 是空闲时的最长等待：低流量下不能让记录无限期留在内存里。
	flushInterval = 2 * time.Second
	// flushTimeout 是单批写入的超时（独立于请求上下文，客户端断开也要落库）。
	flushTimeout = 5 * time.Second
)

// Reject 是一条预检期失败记录。
type Reject struct {
	UserID     uint64
	KeyID      uint64
	Model      string
	Modality   string
	HTTPStatus int
	ErrorMsg   string
	// ErrorCode 是业务码（errs 的 Code）：事件类型按它分类，不按 HTTP 状态猜。
	ErrorCode string
	RequestID string
	IP        string
	KeyPrefix string // 仅在拿不到 KEY 记录时用于识别（不落库，只进日志）
}

var (
	once    sync.Once
	queue   chan model.Usage
	dropped atomic.Int64
	// stopped 表示已停止接收（进程退出中）。
	stopped atomic.Bool
)

// Enabled 报告是否记录预检失败（LD_CALL_PREFLIGHT_LOG）。
func Enabled() bool {
	if !conf.EnvConfig().CallPreflightLog {
		return false
	}
	// 没有数据库（单测/工具）时直接不启用，避免 nil 指针。
	return db.DB != nil
}

// Record 入队一条预检失败记录（不阻塞：队列满则丢弃并计数）。
//
// 为什么不阻塞：调用点是 401/429 的返回路径，阻塞等于把"限流"变成"网关整体变慢"，
// 而记录本身是**观测数据**，丢了不影响任何账务与响应。
func Record(r Reject) {
	if !Enabled() {
		return
	}
	start()
	now := time.Now()
	row := model.Usage{
		CreatedAt:  now,
		UserID:     r.UserID,
		KeyID:      r.KeyID,
		Model:      r.Model,
		Modality:   r.Modality,
		HTTPStatus: r.HTTPStatus,
		Kind:       "relay",
		Status:     "rejected",
		CallKind:   "precheck",
		Charge:     0,
		ErrorMsg:   textx.TruncateRunes(sanitize(r.ErrorMsg), 500),
		ErrorCode:  r.ErrorCode,
		RequestID:  r.RequestID,
		IP:         r.IP,
		StartedAt:  &now,
		FinishedAt: &now,
	}
	select {
	case queue <- row:
	default:
		dropped.Add(1)
		logger.L().Debug("预检失败记录队列已满，丢弃", zap.Uint64("user_id", r.UserID))
	}
}

// Dropped 返回因队列满而丢弃的条数（供 /metrics 与测试）。
func Dropped() int64 { return dropped.Load() }

// start 惰性启动写入协程（一次）。
func start() {
	once.Do(func() {
		queue = make(chan model.Usage, queueSize)
		go run()
	})
}

// run 是写入主循环：攒批、超时或退出时刷盘。
func run() {
	buf := make([]model.Usage, 0, batchMax)
	timer := time.NewTimer(flushInterval)
	defer timer.Stop()
	for {
		select {
		case row, ok := <-queue:
			if !ok {
				flush(buf)
				return
			}
			buf = append(buf, row)
			if len(buf) >= batchMax {
				flush(buf)
				buf = buf[:0]
				resetTimer(timer)
			}
		case <-timer.C:
			if len(buf) > 0 {
				flush(buf)
				buf = buf[:0]
			}
			resetTimer(timer)
		}
	}
}

// resetTimer 安全地重置定时器（Go 1.23 之后 Stop 语义不再需要 drain，但保留兼容写法）。
func resetTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(flushInterval)
}

// flush 写一批；失败只 Warn（观测数据丢了不影响业务）。
func flush(buf []model.Usage) {
	if len(buf) == 0 || db.DB == nil {
		return
	}
	rows := make([]model.Usage, len(buf))
	copy(rows, buf)
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := db.DB.WithContext(ctx).CreateInBatches(rows, batchMax).Error; err != nil {
		logger.L().Warn("预检失败记录写入失败", zap.Int("rows", len(rows)), zap.Error(err))
	}
}

// Flush 等待队列清空并落库（进程退出 / 测试用）。
//
// 实现方式：往队列里塞一个"标记行"再等它被消费完是不可靠的（会被一起写库）。
// 这里直接关闭队列让 run 退出并 flush 剩余，因此**只能在退出时调用一次**。
func Flush() {
	if stopped.Swap(true) {
		return
	}
	// 未启动过（一条都没记录）时无需处理。
	if queue == nil {
		return
	}
	close(queue)
	// 给 run 一点时间做完最后一次 flush；进程退出的场景下这是最好的努力。
	deadline := time.Now().Add(flushTimeout)
	for time.Now().Before(deadline) && len(queue) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

// sanitize 复用报文的脱敏出口：错误文案里可能带 key 前缀（上游把 key 回显在报错里）。
// 不另写一套规则 —— 两套规则必然漂移，而漏掉的那一条就是泄露。
func sanitize(s string) string {
	if s == "" {
		return s
	}
	return string(callio.Sanitize([]byte(s)))
}
