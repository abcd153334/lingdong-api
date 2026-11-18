package storage

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
)

// PurgeStats 是一轮到期清理的结果。
//
// 为什么不做成「返回删了几条」：旧实现只返回计数，于是「对象删不掉」与
// 「没有到期件」都表现为 0，运维只能翻日志（而日志在 0 条时根本不打）。
type PurgeStats struct {
	Kind string
	// Scanned 是本轮取到的到期行数。
	Scanned int
	// Gone 是复核确认对象已不存在的行数（含本来就不存在的）。
	Gone int
	// Deleted 是真正删掉记录的行数。
	Deleted int
	// Unverified 是「删了但复核仍能读到 / 无法复核」的行数（保留记录，下轮重试）。
	Unverified int
	// Failed 是后端不可构建、请求失败等无法开始删除的行数。
	Failed int
	// Missed 是记录已被并发删除（RowsAffected=0）的行数。
	Missed int
	// Bytes 是成功删除的记录声明的字节数合计。
	Bytes int64
	// Samples 是前几条失败样本（id/backend/bucket/key/reason），便于定位。
	Samples []string
	// Blocked 是到期但本轮根本没进候选集的行数，按原因分档。
	Blocked map[string]int64
}

const purgeSampleMax = 5

// PurgeRow 是一条待清理记录：记录的 ID 与它的对象位置。
type PurgeRow struct {
	ID    string
	Ref   Ref
	Bytes int64
}

// PurgeOps 是「清理哪张表」的差异部分；循环与统计只有一份实现。
type PurgeOps struct {
	// Kind 是日志与指标里的分类（upload / artifact）。
	Kind string
	// Pick 取一批到期且可尝试的行（必须按 expires_at 升序，避免老行被饿死）。
	Pick func(ctx context.Context, batch int) ([]PurgeRow, error)
	// DeleteRow 删除记录行，返回受影响行数。
	DeleteRow func(ctx context.Context, id string) (int64, error)
	// Blocked 统计「到期但后端不可构建/位置缺失」的行数，按原因分档。
	Blocked func(ctx context.Context) (map[string]int64, error)
}

// PurgeExpired 执行一轮到期清理：对象复核删除 → 记录删除。
//
// 顺序不可颠倒：先删记录就失去了对象的唯一定位线索（key 只在记录里）。
// 任何「无法确认对象已消失」的情况都保留记录，交给下一轮或人工处理。
func PurgeExpired(ctx context.Context, batch int, ops PurgeOps) (PurgeStats, error) {
	st := PurgeStats{Kind: ops.Kind}
	if ops.Blocked != nil {
		blocked, err := ops.Blocked(ctx)
		if err != nil {
			return st, err
		}
		st.Blocked = blocked
		// 先把两个已知原因归零再写实际值：gauge 不会因为「这一轮没有该原因」自动消失，
		// 否则配置修好之后看板会永远停在旧数字上（「清理一直停摆」的假告警）。
		for _, reason := range []string{"backend", "bucket"} {
			metrics.Set(metrics.NameStoragePurgeBlocked, float64(blocked[reason]), "kind", st.Kind, "reason", reason)
		}
	}

	rows, err := ops.Pick(ctx, batch)
	if err != nil {
		return st, err
	}
	st.Scanned = len(rows)

	for _, row := range rows {
		// key 为空 = 当初就没写成对象（写路径 bug 或极早期数据）。
		// 没有对象可删，直接删记录：否则这一行会永久卡在候选集头部。
		if row.Ref.Key == "" {
			if n, derr := ops.DeleteRow(ctx, row.ID); derr == nil && n > 0 {
				st.Gone++
				st.Deleted++
				metrics.Inc(metrics.NameStoragePurgeTotal, "kind", st.Kind, "result", "gone")
				continue
			}
			st.Failed++
			st.sample(row, "empty_key")
			metrics.Inc(metrics.NameStoragePurgeTotal, "kind", st.Kind, "result", "failed")
			continue
		}

		outcome, derr := DeleteRefVerified(ctx, row.Ref)
		if outcome != DeleteGone {
			if errors.Is(derr, ErrDeleteUnverified) {
				st.Unverified++
				st.sample(row, "unverified")
				metrics.Inc(metrics.NameStoragePurgeTotal, "kind", st.Kind, "result", "unverified")
			} else {
				st.Failed++
				st.sample(row, "backend")
				metrics.Inc(metrics.NameStoragePurgeTotal, "kind", st.Kind, "result", "failed")
			}
			continue
		}

		n, derr := ops.DeleteRow(ctx, row.ID)
		switch {
		case derr != nil:
			st.Failed++
			st.sample(row, "db")
			metrics.Inc(metrics.NameStoragePurgeTotal, "kind", st.Kind, "result", "failed")
		case n == 0:
			// 已被用户删除或另一个清理任务删掉：对象已经处理过，不算失败也不算脏数据。
			st.Missed++
			metrics.Inc(metrics.NameStoragePurgeTotal, "kind", st.Kind, "result", "missed")
		default:
			st.Gone++
			st.Deleted++
			st.Bytes += row.Bytes
			metrics.Inc(metrics.NameStoragePurgeTotal, "kind", st.Kind, "result", "gone")
		}
	}
	st.log()
	return st, nil
}

func (s *PurgeStats) sample(row PurgeRow, reason string) {
	if len(s.Samples) >= purgeSampleMax {
		return
	}
	s.Samples = append(s.Samples, row.ID+"|"+row.Ref.Backend+"|"+row.Ref.Bucket+"|"+row.Ref.Key+"|"+reason)
}

// log 每轮只打一条聚合日志（不逐行）。
//
// 「有失败」必须 Warn：静默 continue 是本次整改要消灭的形态之一。
// 「什么都没扫到」降到 Debug：5 分钟一条 Info 会把真正的问题淹掉。
func (s *PurgeStats) log() {
	fields := []zap.Field{
		zap.String("kind", s.Kind), zap.Int("scanned", s.Scanned),
		zap.Int("deleted", s.Deleted), zap.Int("gone", s.Gone),
		zap.Int("unverified", s.Unverified), zap.Int("failed", s.Failed),
		zap.Int("missed", s.Missed), zap.Int64("bytes", s.Bytes),
	}
	if len(s.Blocked) > 0 {
		fields = append(fields, zap.Any("blocked", s.Blocked))
	}
	switch {
	case s.Unverified > 0 || s.Failed > 0 || len(s.Blocked) > 0:
		fields = append(fields, zap.Strings("samples", s.Samples))
		logger.L().Warn("到期清理存在问题（记录已保留，下轮重试）", fields...)
	case s.Deleted > 0:
		logger.L().Info("到期清理完成", fields...)
	default:
		logger.L().Debug("到期清理：本轮无到期项", fields...)
	}
}
