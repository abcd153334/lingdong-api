package db

import (
	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// usageLookupIndexes 是「调用记录」新增的查询索引（全部走 CONCURRENTLY）。
//
// 为什么不放进 EnsureIndexes（启动期统一建索引）：
//   - 那批索引是**建表期**就需要的（唯一约束/外键替代），失败必须阻断启动；
//   - 而这两个是**大表上的纯性能索引**：usages 在生产上可能几千万行，普通
//     `CREATE INDEX` 会持 SHARE 锁阻塞全部写入（网关整站 502）。
//     `CONCURRENTLY` 建索引期间不阻塞读写，但**不能放进事务**，也不能让失败阻断启动。
//
// 索引与用途：
//   - idx_usages_task_id：部分索引 `WHERE task_id > 0`。同步调用占绝大多数（task_id=0），
//     部分索引体积只有全量索引的零头，而任务反查（"这条记录属于哪个任务"）正是按 task_id 等值查。
//   - idx_usages_channel_created：管理台按渠道 + 时间窗排障（"这条渠道昨天错了多少次"）。
//     顺序固定 (channel_id, created_at DESC)，与 logs 列表的 ORDER BY 方向一致。
var usageLookupIndexes = []struct {
	name string
	sql  string
}{
	{
		name: "idx_usages_task_id",
		sql:  `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usages_task_id ON usages (task_id) WHERE task_id > 0`,
	},
	{
		name: "idx_usages_channel_created",
		sql:  `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usages_channel_created ON usages (channel_id, created_at DESC)`,
	},
}

// ensureUsageLookupIndexes 创建调用记录的查询索引（幂等、失败只告警）。
//
// 幂等与自愈：CONCURRENTLY 建索引如果中途失败（连接中断 / 被取消），PostgreSQL 会留下
// 一个 **invalid** 索引（indisvalid=false）。它不参与查询但占着名字，
// 于是 `IF NOT EXISTS` 之后每次启动都会静默跳过 —— 索引看起来"建过"，实际永远不可用。
// 所以这里显式检查 indisvalid，发现失效就先 DROP 再重建一次。
func ensureUsageLookupIndexes(gdb *gorm.DB) {
	var failed int
	for _, idx := range usageLookupIndexes {
		if valid, exists := indexValid(gdb, idx.name); exists && !valid {
			// 失效索引：必须 DROP CONCURRENTLY 才能重建（普通 DROP 会短暂锁表）。
			if err := gdb.Exec("DROP INDEX CONCURRENTLY IF EXISTS " + idx.name).Error; err != nil {
				logger.L().Warn("M18 删除失效索引失败（下次启动重试）",
					zap.String("index", idx.name), zap.Error(err))
				failed++
				continue
			}
		}
		if err := gdb.Exec(idx.sql).Error; err != nil {
			// 不阻断启动：缺索引只是慢，不是不可用。
			logger.L().Warn("M18 创建索引失败（下次启动重试）",
				zap.String("index", idx.name), zap.Error(err))
			failed++
		}
	}
	if failed == 0 {
		logger.L().Info("调用记录查询索引已就绪")
	}
}

// indexValid 查询索引是否存在与是否有效（exists=false 表示索引名不存在）。
func indexValid(gdb *gorm.DB, name string) (valid, exists bool) {
	var rows []struct {
		IndIsValid bool
	}
	if err := gdb.Raw(
		`SELECT i.indisvalid AS ind_is_valid
		   FROM pg_index i
		   JOIN pg_class c ON c.oid = i.indexrelid
		  WHERE c.relname = ?`, name).Scan(&rows).Error; err != nil {
		return false, false
	}
	if len(rows) == 0 {
		return false, false
	}
	return rows[0].IndIsValid, true
}

// 编译期断言：M18 依赖 usages.task_id 列存在（由 model.Usage 的 AutoMigrate 提供）。
// 只做类型引用，避免"索引建在模型根本没声明的列上"这类静默漂移。
var _ = func(u *model.Usage) uint64 { return u.TaskID }
