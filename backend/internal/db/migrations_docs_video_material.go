package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M57：视频提交页按**素材字段收敛**重播，并清掉已下架的「视频生成」通用节点。
//
// 背景：视频提交的素材写法从 `input_reference[].type`（6 值枚举）改成 5 个
// 自解释顶层字段（image / first_frame / last_frame / audio / video），
// 生成方式由「哪些字段有值」推断。
//
// 为什么必须有这一步：`docdocs.SeedDocs` 的规则是「已存在就跳过」，
// 代码改了而库里的旧字段表不会变 —— 站点上会继续显示旧契约，且不报任何错。
//
// 与 M56 的两处差别：
//  1. 还要删节点：上一轮新增的 `video-generate` 通用节点已下架。播种只补缺、
//     从不删，不删它就永久留在侧栏里（还带一篇指向旧字段表的接口页）。
//  2. 删完节点要**把种子节点纠正成已发布**：`seedOneNode` 对已存在节点不碰状态，
//     而接口页被删空的节点会被自动转成草稿。
//
// 幂等：`data_migrations` 标记 + 全部语句是 `DELETE ... WHERE`。
// 失败阻断启动 —— 只删一半的库会让文档站静默少页（没有报错，只是空白）。
const migrationDocsVideoMaterial = "m57_docs_video_material"

// videoSeedTargets 圈定要重播的接口页（**必须带表别名 `e`**：它会被嵌进与
// doc_endpoint_fields 的 JOIN 里，不加限定的话 `key`/`origin` 会变成歧义列）。
//
// 用 `ep-%video-submit%` 而不是枚举族：新增一族时不必回来改这里；
// 代价是字形相近的页面也会被重播一次，而重播的输入就是当前代码 —— 内容一致。
const videoSeedTargets = `e.origin = 'seed' AND (e.key LIKE 'ep-%video-submit%'
	OR e.node_id IN (SELECT id FROM doc_nodes
	                  WHERE origin = 'seed' AND key = 'video-generate'))`

// MigrateDocsVideoMaterial 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsVideoMaterial(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsVideoMaterial).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询视频素材字段重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}

	const endpointIDs = `SELECT e.id FROM doc_endpoints e WHERE ` + videoSeedTargets
	const fieldIDs = `SELECT f.id FROM doc_endpoint_fields f
	                    JOIN doc_endpoints e ON e.id = f.endpoint_id
	                   WHERE ` + videoSeedTargets

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			// 子表没有外键（历史上刻意不加），必须逐张显式删；漏一张就是孤儿行，
			// 而孤儿行不报错，只会在下一次重播时被当成"已有内容"挡住。
			`DELETE FROM doc_field_enums WHERE field_id IN (` + fieldIDs + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoints e WHERE ` + videoSeedTargets,
			// 通用节点本身：已从 SeedNodes 移除，播种不会重建它。
			`DELETE FROM doc_nodes WHERE origin = 'seed' AND key = 'video-generate'`,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播视频素材字段种子: %w", err)
			}
		}
		if err := tx.Exec(`
			UPDATE doc_nodes
			   SET status = 'published', published_at = COALESCE(published_at, now())
			 WHERE origin = 'seed' AND status <> 'published'`).Error; err != nil {
			return fmt.Errorf("恢复种子节点发布态: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationDocsVideoMaterial, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsVideoMaterial 只在真的执行过时打一行。
func LogDocsVideoMaterial(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("视频提交页已按素材字段收敛重播（M57）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
