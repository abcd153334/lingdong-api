package task

import (
	"context"
	"errors"
	"math"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/storage"
)

// RetentionReport 是一轮保留策略执行的统计（B7 §2.10）。
//
// 字段刻意区分「候选」与「实际删除」：只报告模式下 Candidates 有值、其余为 0，
// 运营据此判断「开开关会删掉多少」，而不是等到删完才知道量级。
type RetentionReport struct {
	// Enabled 表示本次是否真的执行删除（false = 只报告模式）。
	Enabled bool
	// Days 是本次使用的保留天数。
	Days int
	// Candidates 是超期终态任务数（按批上限截断前的总数）。
	Candidates int64
	// Tasks 是实际删除的任务行数。
	Tasks int
	// Files 是实际删除的产物记录数。
	Files int
	// Bytes 是随产物一起释放的字节数。
	Bytes int64
	// Skipped 是因对象删除失败而**整条跳过**的任务数（下轮重试）。
	Skipped int
}

// RetentionCandidates 统计超期终态任务数（不删任何东西）。
//
// 只算终态：queued/running 的任务即便创建时间很老也不能删 —— 它们还在占着预扣额度，
// 删掉任务行会让「冻结了但永远查不到任务」成为一笔无法追回的挂账。
func RetentionCandidates(ctx context.Context, days int, now time.Time) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	var n int64
	err := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("status IN ?", terminalStatuses()).
		Where("created_at < ?", cutoff(now, days)).
		Count(&n).Error
	return n, err
}

// PurgeTerminalTasks 按保留期删除终态任务与产物（days <= 0 时只统计，不删）。
//
// 顺序铁律（不可颠倒）：**先删对象、再删 files 行、最后删 tasks 行**。
//
//   - 对象删除失败 → 整条任务跳过（不删 files 行、不删 task 行），下一轮重试。
//     反过来先删行再删对象，一次存储抖动就会永久留下一批「行没了、对象还在」的孤儿，
//     除了手工扫桶再也无法发现 —— 而这类孤儿正是磁盘缓慢涨满的典型成因。
//   - files 行在对象之后删：行是对象的唯一索引，先删行等于放弃清理入口。
func PurgeTerminalTasks(ctx context.Context, days, batch int, now time.Time) (RetentionReport, error) {
	rep := RetentionReport{Days: days}
	if days <= 0 {
		// 只报告模式：给出终态任务的规模与最老一条的时间。
		//
		// 这里刻意**不**假装有一个「假想保留期」再算候选：凭空的期数（比如 7 天）
		// 会让运营误以为「开开关只删一星期前的」，而真实配置可能是 90 天。
		// 报告真实存量 + 最老年龄，让「要不要开、开多少天」有依据。
		n, oldest, err := TerminalStats(ctx)
		if err != nil {
			return rep, err
		}
		rep.Candidates = n
		metrics.Set(metrics.NameTaskRetentionReport, float64(n), "mode", "report")
		if n > 0 {
			age := 0.0
			if oldest != nil {
				age = now.Sub(*oldest).Hours() / 24
			}
			logger.L().Info("任务保留策略未启用（仅报告）",
				zap.Int64("terminal_tasks", n), zap.Float64("oldest_days", math.Round(age*10)/10))
		}
		return rep, nil
	}
	rep.Enabled = true
	n, err := RetentionCandidates(ctx, days, now)
	if err != nil {
		return rep, err
	}
	rep.Candidates = n
	metrics.Set(metrics.NameTaskRetentionReport, float64(n), "mode", "enforce")
	if n == 0 {
		return rep, nil
	}

	if batch <= 0 {
		batch = retentionBatch
	}
	var rows []model.Task
	usable := storage.UsableBackends()
	if err := db.DB.WithContext(ctx).
		Model(&model.Task{}).
		Where("status IN ?", terminalStatuses()).
		Where("created_at < ?", cutoff(now, days)).
		Where(purgeStorageFilter, usable).
		Order("created_at ASC").Limit(batch).Find(&rows).Error; err != nil {
		return rep, err
	}
	if len(rows) == 0 && n > 0 {
		// 有候选但一条都选不出来：不是「没活干」，而是候选全部卡在已失效的后端上。
		// 不报的话运维只会看到「清理任务天天跑、磁盘只增不减」。
		logger.L().Warn("任务保留清理：候选任务的产物后端均已失效，本轮无可清理项",
			zap.Int64("candidates", n), zap.Strings("usable_backends", usable))
	}

	for i := range rows {
		files, bytes, ok, perr := purgeTaskArtifacts(ctx, rows[i].ID)
		if perr != nil {
			rep.Skipped++
			logger.L().Warn("任务保留清理：产物删除失败，整条跳过待下轮重试",
				zap.Uint64("task_id", rows[i].ID), zap.Error(perr))
			continue
		}
		if !ok {
			rep.Skipped++
			continue
		}
		// 硬删任务行：保留策略的目的就是让数据真的消失，软删只会让表继续涨。
		// 账务事实留在 usages（append-only），删任务不影响对账。
		if err := db.DB.WithContext(ctx).Unscoped().Delete(&model.Task{}, "id = ?", rows[i].ID).Error; err != nil {
			rep.Skipped++
			logger.L().Warn("任务保留清理：任务行删除失败",
				zap.Uint64("task_id", rows[i].ID), zap.Error(err))
			continue
		}
		rep.Tasks++
		rep.Files += files
		rep.Bytes += bytes
		metrics.Inc(metrics.NameTaskPurgedTotal, "reason", "retention")
	}
	return rep, nil
}

// PurgeTask 物理删除单个任务及其产物（管理台 purge=true）。
//
// 返回 (产物数, 字节数, error)。**仅终态可删**由调用方校验：这里不做状态判断，
// 因为它同时被保留策略复用，而保留策略的候选集已经带上了终态条件。
func PurgeTask(ctx context.Context, t *model.Task) (int, int64, error) {
	if t == nil || t.ID == 0 {
		return 0, 0, ErrNotFound
	}
	if !IsTerminal(t.Status) {
		return 0, 0, ErrNotTerminal
	}
	files, bytes, ok, err := purgeTaskArtifacts(ctx, t.ID)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return 0, 0, ErrArtifactPurgeFailed
	}
	if err := db.DB.WithContext(ctx).Unscoped().Delete(&model.Task{}, "id = ?", t.ID).Error; err != nil {
		return 0, 0, err
	}
	metrics.Inc(metrics.NameTaskPurgedTotal, "reason", "manual")
	return files, bytes, nil
}

// deleteRefVerified 是 storage.DeleteRefVerified 的**间接引用**（单测注入点）。
//
// 为什么留这个缝：清理逻辑要验证「对象删不掉时必须保留任务行」，而真实删除
// 会打到配置好的存储后端上。为了一个用例去改全局存储配置，会污染同包其它用例
// （Succeed 要往同一个目录写产物），故障排查成本远高于收益。
var deleteRefVerified = storage.DeleteRefVerified

// purgeStorageFilter 是「产物位置可尝试删除」的 SQL 谓词。
//
// 清理的候选集是 ORDER BY expires_at LIMIT batch。若最老的 N 行指向不可构建的后端
// 或缺失桶标识，逐条跳过后每轮都会选中同一批，后面的正常行永远排不上 ——
// 清理队列被饿死，而到期产物只增不减。判据与 storage.For/ForRef 同源，
// 不另写一份「配置是否完整」的检查。
const purgeStorageFilter = `NOT EXISTS (
	SELECT 1 FROM files f
	WHERE f.task_id = tasks.id
	  AND (f.storage_backend IS NULL OR f.storage_backend = '' OR f.storage_backend NOT IN ?
	       OR (f.storage_backend <> 'none' AND COALESCE(f.storage_bucket, '') = '')))`

// purgeTaskArtifacts 删除任务的全部产物（对象 → 行）。
//
// 返回 ok=false 表示「有对象删不掉 / 无法确认删掉」，调用方必须放弃删除任务行
// （保留下轮重试）。这里的返回设计成三值而不是只返 error：
// 部分失败既不是「完全成功」也不该让整批中断。
func purgeTaskArtifacts(ctx context.Context, taskID uint64) (files int, bytes int64, ok bool, err error) {
	var rows []model.File
	if err := db.DB.WithContext(ctx).Where("task_id = ?", taskID).Order("artifact_index ASC").Find(&rows).Error; err != nil {
		return 0, 0, false, err
	}
	if len(rows) == 0 {
		return 0, 0, true, nil
	}
	for i := range rows {
		row := rows[i]
		if row.StorageKey != "" {
			ref := storage.Ref{Backend: row.StorageBackend, Bucket: row.StorageBucket, Key: row.StorageKey}
			// 行没了对象就再也定位不到，所以删不掉（或无法确认删掉）对象绝不删行。
			if _, derr := deleteRefVerified(ctx, ref); derr != nil {
				return files, bytes, false, derr
			}
		}
		if derr := db.DB.WithContext(ctx).Delete(&model.File{}, "id = ?", row.ID).Error; derr != nil {
			return files, bytes, false, derr
		}
		files++
		bytes += row.SizeBytes
	}
	return files, bytes, true, nil
}

// InflightCount 统计在途任务数（queued + running），供 task_inflight 指标使用。
//
// 用「查库」而不是「进程内计数」：进程重启后内存计数必然归零，而库里的在途任务是真实存在的，
// 指标一旦与事实长期偏离，告警就会变成狼来了。
func InflightCount(ctx context.Context) int64 {
	// 未初始化数据库时返回 0 而不是 panic：指标采集函数在测试里也会被调用，
	// 一个采集器崩掉会连带整个 /metrics 不可用（那里已做 recover，但没必要制造噪声）。
	if db.DB == nil {
		return 0
	}
	var n int64
	if err := db.DB.WithContext(ctx).Model(&model.Task{}).
		Where("status IN ?", []string{StatusQueued, StatusRunning}).Count(&n).Error; err != nil {
		return 0
	}
	return n
}

// TerminalStats 返回终态任务的规模与最老任务的创建时间（只报告模式的输出）。
func TerminalStats(ctx context.Context) (count int64, oldest *time.Time, err error) {
	var row struct {
		N   int64
		Min *time.Time
	}
	err = db.DB.WithContext(ctx).Model(&model.Task{}).
		Select("COUNT(*) AS n, MIN(created_at) AS min").
		Where("status IN ?", terminalStatuses()).
		Scan(&row).Error
	return row.N, row.Min, err
}

// StorageUsage 统计 files 表的产物总量（字节 + 行数），供容量监控使用。
func StorageUsage(ctx context.Context) (bytes int64, files int64, err error) {
	if db.DB == nil {
		return 0, 0, errors.New("task: 数据库未初始化")
	}
	var row struct {
		B int64
		N int64
	}
	err = db.DB.WithContext(ctx).Model(&model.File{}).
		Select("COALESCE(SUM(size_bytes), 0) AS b, COUNT(*) AS n").Scan(&row).Error
	return row.B, row.N, err
}

// retentionBatch 是单轮清理的任务上限（与产物清理同量级，避免一次长事务锁表）。
const retentionBatch = 200

// terminalStatuses 返回全部终态（与 IsTerminal 同一口径，新增状态只需改 IsTerminal）。
func terminalStatuses() []string {
	all := []string{StatusSucceeded, StatusFailed, StatusExpired, StatusCanceled}
	out := make([]string, 0, len(all))
	for _, s := range all {
		if IsTerminal(s) {
			out = append(out, s)
		}
	}
	return out
}

// cutoff 返回「早于此时刻」的边界（保留 N 天）。
func cutoff(now time.Time, days int) time.Time {
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}
