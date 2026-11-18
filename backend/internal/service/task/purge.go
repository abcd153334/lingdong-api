package task

import (
	"context"

	"gorm.io/gorm"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// PurgeResult 是一次性清理的结果摘要。
type PurgeResult struct {
	// Scanned 是命中的非终态任务行数。
	Scanned int
	// Canceled 是实际被 CAS 推进到 canceled 的行数（并发下可能少于 Scanned）。
	Canceled int
	// ReleasedQuota 是全额释放的预扣总额度（含用户余额与 KEY 额度两侧回退）。
	ReleasedQuota int64
}

// PurgeByAdapterPrefix 是一次性运维清理：把某适配器前缀下的**全部非终态任务**置 canceled 并全额释放预扣。
//
// 存在的理由：适配器被删除后，`worker/task_polling.go` 按 `tasks.platform` 取 TaskAdapter 恒失败，
// 任务会永远停在 queued/running，冻结额度永远不退。删除适配器代码**之前**必须先跑完本函数。
//
// 语义刻意保守：
//   - 只动 queued/running（终态任务的账已经结过，再释放就是重复退款）；
//   - 每行都走 CAS（`WHERE id=? AND status IN (...)`），并发下被别处推进的行直接跳过，
//     不会出现「轮询线程刚推到 succeeded，清理线程又退款」的双重结算；
//   - 释放走 `releaseFrozen`（与取消/失败同一条资金路径），不裸改 quota 列。
func PurgeByAdapterPrefix(ctx context.Context, prefix string) (PurgeResult, error) {
	var out PurgeResult
	if prefix == "" {
		return out, nil
	}
	var lastID uint64
	const batch = 200
	for {
		var rows []model.Task
		err := db.DB.WithContext(ctx).
			Where("platform LIKE ? AND status IN ? AND id > ?", prefix+"%", advanceFrom, lastID).
			Order("id ASC").Limit(batch).Find(&rows).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return out, err
		}
		if len(rows) == 0 {
			return out, nil
		}
		for i := range rows {
			t := &rows[i]
			out.Scanned++
			lastID = t.ID
			updated, casErr := cas(ctx, t.ID, advanceFrom, StatusCanceled,
				map[string]any{"error_msg": "适配器已下线，任务自动取消并全额退款"})
			if casErr != nil {
				return out, casErr
			}
			if !updated {
				continue
			}
			out.Canceled++
			out.ReleasedQuota += t.Frozen
			releaseFrozen(ctx, t, statusClientClosed, "适配器已下线，任务自动取消")
		}
		if len(rows) < batch {
			return out, nil
		}
	}
}
