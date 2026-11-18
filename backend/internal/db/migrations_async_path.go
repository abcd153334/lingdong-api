package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// MigrationNameAsyncPath 是「图像异步并入同一路径」的数据动作标识。
const MigrationNameAsyncPath = "m43_async_path"

// AsyncPathReport 是迁移结果摘要（供启动日志）。
type AsyncPathReport struct {
	// Models 是被改写的模型行数（endpoints 里含旧端点）。
	Models int64
	// Specs 是被改写的规格适配器行数（spec 里含旧端点键）。
	Specs int64
	// Docs 是删掉的失效文档行数（旧端点已不存在于契约）。
	Docs int64
}

// MigrateAsyncPath 把「图像异步专用路径」并入正本端点。
//
// 背景：图像异步曾经是独立端点 `images/generations/async`，对应一条独立路由与
// 一篇独立文档。实际上上游是**同一套流程**（提交后要么在本次响应里返回图、
// 要么返回任务号），平台要不要等只由请求里的 `async` 决定 —— 于是同一个能力
// 有了两个名字：模型声明、文档导航、适配器能力矩阵各要维护两份，
// 文档站上还出现「同步出图」与「异步出图」两篇字段表几乎相同的文章。
//
// 现在契约里只有 `images/generations`（`Transport.AcceptsAsync`），旧端点键
// 在 `apidoc.KindOfEndpointKey` 里查不到。**不迁移的后果不是报错，而是静默失效**：
// `models.endpoints` 里那串字面量还在，但没有任何代码认识它 ——
// 该模型在文档站上少一个端点、绑定体检不认识它、出站选路也不会命中。
//
// 三处数据一起处理（缺一处就留下"半个旧世界"）：
//  1. `models.endpoints`：旧键换成新键并**去重**（有的模型两个都声明了，直接替换会变成重复项）；
//  2. `adapter_specs.spec`：规格适配器的端点键与 `async.poll` 引用（JSON 里的键名）；
//  3. `doc_endpoint_docs`：旧端点的文档行（契约里已经没有它，留着就是永远打不开的导航项）。
//
// 幂等：data_migrations 标记 + 判据本身（替换后不再包含旧键）。dryRun 只数行数不写库。
func MigrateAsyncPath(gdb *gorm.DB, dryRun bool) (AsyncPathReport, error) {
	var rep AsyncPathReport
	if !dryRun && gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", MigrationNameAsyncPath).Count(&n).Error; err != nil {
			return rep, fmt.Errorf("db: 查询异步路径迁移标记失败: %w", err)
		}
		if n > 0 {
			return rep, nil
		}
	}

	const oldKey = "images/generations/async"
	// 有序 + 去重：endpoints 是集合，但顺序也是运营排出来的（文档顺序按声明走），
	// 排一次序会让「人工调整过的端点顺序」在每次升级后被抹平。
	const flat = `
		SELECT m.id AS id,
		       CASE WHEN t.e = '` + oldKey + `' THEN 'images/generations' ELSE t.e END AS k,
		       t.ord AS ord
		  FROM models m
		  CROSS JOIN LATERAL jsonb_array_elements_text(m.endpoints) WITH ORDINALITY AS t(e, ord)
		 WHERE m.endpoints @> '"` + oldKey + `"'::jsonb`
	const dedup = `SELECT DISTINCT ON (id, k) id, k, ord FROM (` + flat + `) f ORDER BY id, k, ord`
	const rewrite = `WITH d AS (` + dedup + `),
		agg AS (SELECT id, jsonb_agg(k ORDER BY ord) AS arr FROM d GROUP BY id)
		UPDATE models m SET endpoints = agg.arr FROM agg WHERE m.id = agg.id`

	// 规格适配器：JSON 里的键名与 async.poll 的取值都可能是旧键。
	// 必须先换 `@poll` 那条：先做短串替换会把 `.../async@poll` 变成 `...@poll` 的不完整键。
	specSQL := []string{
		`UPDATE adapter_specs SET spec = replace(spec::text, '"` + oldKey + `@poll"', '"images/generations@poll"')::jsonb
		  WHERE spec::text LIKE '%` + oldKey + `@poll%'`,
		`UPDATE adapter_specs SET spec = replace(spec::text, '"` + oldKey + `"', '"images/generations"')::jsonb
		  WHERE spec::text LIKE '%` + oldKey + `"%'`,
	}
	docSQL := `DELETE FROM doc_endpoint_docs WHERE doc_id = '` + oldKey + `' OR doc_id LIKE '` + oldKey + `@%'` +
		` OR doc_id = 'tasks/content' OR doc_id LIKE 'tasks/content@%'`

	if dryRun {
		if err := gdb.Raw(`SELECT count(*) FROM models WHERE endpoints @> '"` + oldKey + `"'::jsonb`).
			Scan(&rep.Models).Error; err != nil {
			return rep, err
		}
		if gdb.Migrator().HasTable("adapter_specs") {
			if err := gdb.Raw(`SELECT count(*) FROM adapter_specs WHERE spec::text LIKE '%` + oldKey + `%'`).
				Scan(&rep.Specs).Error; err != nil {
				return rep, err
			}
		}
		if gdb.Migrator().HasTable("doc_endpoint_docs") {
			if err := gdb.Raw(`SELECT count(*) FROM doc_endpoint_docs WHERE doc_id = '` + oldKey + `'`).
				Scan(&rep.Docs).Error; err != nil {
				return rep, err
			}
		}
		return rep, nil
	}

	if gdb.Migrator().HasTable("models") {
		res := gdb.Exec(rewrite)
		if res.Error != nil {
			// 阻断启动：留着旧键的模型会静默少一个端点（文档站看不见、绑定体检不认识），
			// 那种"配置还在但没人读"的状态比启动失败难查得多。
			return rep, fmt.Errorf("db: 改写 models.endpoints 失败: %w", res.Error)
		}
		rep.Models = res.RowsAffected
	}
	if gdb.Migrator().HasTable("adapter_specs") {
		for i, q := range specSQL {
			res := gdb.Exec(q)
			if res.Error != nil {
				return rep, fmt.Errorf("db: 改写 adapter_specs（第 %d 条）失败: %w", i+1, res.Error)
			}
			rep.Specs += res.RowsAffected
		}
	}
	if gdb.Migrator().HasTable("doc_endpoint_docs") {
		res := gdb.Exec(docSQL)
		if res.Error != nil {
			return rep, fmt.Errorf("db: 清理失效接口文档失败: %w", res.Error)
		}
		rep.Docs = res.RowsAffected
	}

	if err := gdb.Create(&model.DataMigration{
		Name: MigrationNameAsyncPath, AppliedAt: time.Now(),
	}).Error; err != nil {
		return rep, fmt.Errorf("db: 写入异步路径迁移标记失败: %w", err)
	}
	return rep, nil
}

// LogAsyncPath 打印迁移结果（无变化时不打日志，避免每次启动刷屏）。
func LogAsyncPath(rep AsyncPathReport) {
	if rep.Models == 0 && rep.Specs == 0 && rep.Docs == 0 {
		return
	}
	logger.L().Info("已把图像异步并入 images/generations",
		zap.Int64("models", rep.Models), zap.Int64("adapter_specs", rep.Specs), zap.Int64("docs", rep.Docs))
}
