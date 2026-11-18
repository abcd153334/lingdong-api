package task

// 产物补转存（真机联调暴露的缺口）。
//
// ── 事实 ────────────────────────────────────────────────────────────────
// SUCCESS 路径里逐条转存，失败的条目在清单里留 `f=0`，注释说「供下一轮补」。
// 但**没有任何下一轮**：任务一旦进终态就再也不会被 Succeed 调用，于是那条产物
// 永远留在清单里、永远不出现给用户 —— 用户按满额/按条付了钱，少拿一首歌，
// 而平台侧没有任何信号（manifest 是内部列，客服看不到）。
//
// 真机实测（2026-11-16，多米 SUNO）：两次作曲任务各 2 首，OSS 各失败 1 首
// （`wsarecv: forcibly closed` / `use of closed network connection`），
// `/suno/fetch` 都只回 1 条 clip，`artifact_count=1`，第 2 首彻底拿不到。
//
// ── 规则 ────────────────────────────────────────────────────────────────
//  1. **只补交付，不补账**：结算在终态那一刻已经定了（按条计费按实际成功条数收）。
//     补成功之后也不追扣 —— 少收是平台的，多收是用户不可接受的；补救的目标
//     只有一个：让用户拿到他已经付过钱的东西。
//  2. **只补清单里已经有链接的条目**：链接在上游可能已过期（SUNO 的导出链接
//     通常几小时），补不上就留着 f=0，不要编造新的上游调用。
//  3. 与 SUCCESS 路径共用同一把并发锁：补转存与「迟到的成功回调」同时改清单时，
//     后者会把 f=0 当成没转存又下一遍（唯一索引冲突兜住，但白下一遍）。

import (
	"context"
	"time"

	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// ArtifactRepairWindow 是补转存的时间窗。
//
// 6 小时：SUNO 的导出链接有效期量级（实测数小时），再长的任务补也补不上；
// 同时把扫描范围钉在「最近的任务」上，避免每次全表扫历史行。
const ArtifactRepairWindow = 6 * time.Hour

// RepairArtifacts 补转存一个任务的缺失产物，返回本次补成功的条数。
//
// 语义：**幂等**。清单里 f>0 的条目直接跳过；文本产物按正文重落一次（小、便宜）。
// 不修改任务状态、不改已结算金额。
func RepairArtifacts(ctx context.Context, t *model.Task) (int, error) {
	if t == nil || t.ID == 0 {
		return 0, ErrNotFound
	}
	man := parseArtifactManifest(t.ArtifactManifest)
	if len(man) == 0 {
		return 0, nil
	}
	missing := 0
	for i := range man {
		if man[i].FileID == 0 {
			missing++
		}
	}
	if missing == 0 {
		return 0, nil
	}

	unlock, ok := lockSucceed(ctx, t.ID)
	if !ok {
		// 另一路正在收尾（Succeed / 迟成功）：让它先改清单，下一轮再补。
		return 0, nil
	}
	defer unlock()

	// 重新加载：调用方持有的可能是一份过期视图（上一轮补过一部分）。
	var fresh model.Task
	if err := db.DB.WithContext(ctx).Where("id = ?", t.ID).First(&fresh).Error; err == nil {
		t = &fresh
		man = parseArtifactManifest(fresh.ArtifactManifest)
	}

	fixed := 0
	for i := range man {
		if man[i].FileID > 0 {
			continue
		}
		entry := &man[i]
		if entry.Text {
			if id, ok := saveTextArtifactAt(ctx, t, t.ResultText, entry.Index); ok {
				entry.FileID = id
				fixed++
			}
			continue
		}
		if entry.URL == "" {
			// 没有链接的条目补不了（历史脏数据），保持原样。
			continue
		}
		if id, ok := transferArtifactWithRetry(ctx, t, entry.URL, entry.Index); ok {
			entry.FileID = id
			fixed++
		}
	}
	if fixed == 0 {
		return 0, nil
	}

	// 清单先落库（与 Succeed 同一顺序：事实优先于统计列）。
	if err := persistArtifactManifest(ctx, t.ID, man); err != nil {
		return fixed, err
	}
	mainID, count := manifestStats(man)
	updates := map[string]any{"artifact_count": count, "artifact_manifest": marshalArtifactManifest(man)}
	if mainID > 0 {
		updates["result_file_id"] = mainID
	}
	if err := db.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ?", t.ID).
		Updates(updates).Error; err != nil {
		return fixed, err
	}
	logger.L().Info("产物补转存成功",
		zap.Uint64("task_id", t.ID), zap.Int("fixed", fixed), zap.Int("artifact_count", count))
	return fixed, nil
}

// MissingArtifactTasks 返回「有缺失产物、值得补」的任务（按最近更新排序）。
//
// 判据全部落在 SQL 上，不用 Go 侧过滤：这张表是热点表，全量拉回来再筛会让
// 一次补转存把内存和网络都吃掉。
//
// `jsonb_array_length` 对非数组值会报错，所以先用 `jsonb_typeof` 守卫；
// 老行的 manifest 是 NULL，`jsonb_typeof(NULL)` 是 NULL（不满足 =），自然被排除。
func MissingArtifactTasks(ctx context.Context, since time.Time, limit int) ([]model.Task, error) {
	if limit <= 0 {
		limit = 20
	}
	var rows []model.Task
	err := db.DB.WithContext(ctx).
		Where("status = ?", "succeeded").
		Where("updated_at > ?", since).
		// 必须用 CASE 而不是「两个 AND 条件」：PostgreSQL 不保证 AND 的求值顺序，
		// 先把 `jsonb_array_length` 也算出来就会在非数组行上报
		// `cannot get array length of a non-array`（真机踩到，整个候选查询直接失败）。
		// CASE 只求值选中的分支，脏数据行因此安全地落到 0。
		Where("artifact_count < CASE WHEN jsonb_typeof(artifact_manifest) = 'array' " +
			"THEN jsonb_array_length(artifact_manifest) ELSE 0 END").
		Order("updated_at DESC, id DESC").
		Limit(limit).Find(&rows).Error
	return rows, err
}
