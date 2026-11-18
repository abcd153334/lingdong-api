package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M59：Muse 视频提交页按**裁剪后的兼容写法段**重播。
//
// 背景（真机测试后逐字读页面发现）：
//  1. 兼容段原来是 `VideoCompatNote()` 全表照搬，于是 Muse 页上写着
//     「`size` → `resolution`，旧写法仍然可用」「`width + height` → `resolution`」
//     —— 本族根本没有 resolution（上游不收、适配器出网前丢弃、价目固定 720p），
//     读者照着传只会得到「传了没反应」，与本页刻意不写 resolution 的理由自相矛盾。
//  2. 同一段还写着「`input_reference` → `image / first_frame / last_frame / audio / video`」，
//     而本族 audio / video 素材会被适配器直接 400，first_frame 也只是被当普通参考图展开。
//     这三条留在页面上就是把「不支持的语义」写成「旧写法仍然可用」。
//
// 现在兼容段由 `VideoCompatNoteOf` 按族裁剪，只留 duration / ratio / image_urls 三条
// （三条都能在本页字段表里找到落点）。门禁 `TestVideoCompatNoteMatchesFamilyFields`
// 双向核对，同类缺陷不会再回来。
//
// 为什么不能只改代码：`docdocs.SeedDocs` 的规则是「已存在就跳过」——
// 播种内容变了、库里已发布的页不会跟着变，站点会继续显示旧文案且不报错。
//
// 圈定范围只有一个接口页（M57/M58 那套 `ep-%video-submit%` 会顺带重播三族，
// 但它们的文案没变，这里就没必要扩大影响面）。
//
// 幂等：`data_migrations` 标记 + 全部语句是 `DELETE ... WHERE`。
const migrationDocsMuseCompat = "m59_docs_muse_compat"

// museCompatSeedTargets 圈定 Muse 视频提交页（**必须带表别名 `e`**：会被嵌进与
// doc_endpoint_fields 的 JOIN 里）。
const museCompatSeedTargets = `e.origin = 'seed' AND e.key = 'ep-videos-muse-video-submit'`

// MigrateDocsMuseCompat 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsMuseCompat(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsMuseCompat).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询 Muse 兼容段重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}
	// 页面还不存在（首次部署在 M59 之后才播种）时不必打标记：
	// 留着标记会让「先升级、后建库」的机器永远拿不到重播。
	var exists int64
	if err := gdb.Model(&model.DocEndpoint{}).
		Where("origin = ? AND key = ?", model.DocOriginSeed, "ep-videos-muse-video-submit").
		Count(&exists).Error; err != nil {
		return false, fmt.Errorf("db: 查询 Muse 提交页失败: %w", err)
	}
	if exists == 0 {
		return false, nil
	}

	const endpointIDs = `SELECT e.id FROM doc_endpoints e WHERE ` + museCompatSeedTargets
	const fieldIDs = `SELECT f.id FROM doc_endpoint_fields f
	                    JOIN doc_endpoints e ON e.id = f.endpoint_id
	                   WHERE ` + museCompatSeedTargets

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			`DELETE FROM doc_field_enums WHERE field_id IN (` + fieldIDs + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoints e WHERE ` + museCompatSeedTargets,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播 Muse 兼容段种子: %w", err)
			}
		}
		// 被删空的节点会被自动转成草稿，这里纠正回发布态。
		if err := tx.Exec(`
			UPDATE doc_nodes
			   SET status = 'published', published_at = COALESCE(published_at, now())
			 WHERE origin = 'seed' AND status <> 'published'`).Error; err != nil {
			return fmt.Errorf("恢复种子节点发布态: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationDocsMuseCompat, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsMuseCompat 只在真的执行过时打一行。
func LogDocsMuseCompat(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("Muse 视频提交页已按裁剪后的兼容段重播（M59）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
