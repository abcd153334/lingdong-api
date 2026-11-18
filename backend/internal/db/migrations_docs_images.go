package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
)

// M60：图片三族（GPT Image / Nano Banana / Seedream）的接口页按新契约重播。
//
// 背景（本轮逐条对照官方文档 + 代码发现）：
//  1. Seedream 页写了 `async` 字段与「已受理」变体，但方舟的 images/generations
//     只有同步形态 —— 照文档发 async:true 没有任何渠道能承接。
//     族级异步声明（`FamilyDoc.AsyncSubmit`）上线后，这一页必须重播才会去掉它，
//     否则站点继续显示旧文案，且发布闸门会把它判成 E_ASYNC_UNEXPECTED（草稿）。
//  2. Seedream `size` 的默认值写死了「2K（图层拆分为 auto）」。官方是**按模型**不同
//     （pro / flash 默认 2K，lite 默认 2048x2048，图层场景默认 auto），一页三模型
//     写死一个值，另外两个模型的读者看到的就是错的。
//  3. Seedream 笔记里有一句「该模型的张数上限必须 ≥ 17」——那是平台配置要求，
//     不是协议事实，不该出现在对外文档里。
//
// 为什么不能只改代码：`docdocs.SeedDocs` 的规则是「已存在就跳过」，
// 播种内容变了、库里已发布的页不会跟着变，且不报错。
//
// 圈定范围：`origin='seed' AND key LIKE 'ep-images-%'`，含三族的 generations /
// edits / variations 以及派生出来的「查询任务状态」页（Seedream 的那篇会就此消失，
// 因为 `familyEmitsTasks` 现在按族判据派生）。
//
// 幂等：`data_migrations` 标记 + 全部语句是 `DELETE ... WHERE`。
const migrationDocsImagesReplay = "m60_docs_images_replay"

// M62：同一批图片接口页**再重播一次**。
//
// 为什么需要第二次而不是改 M60：M60 已经在线上跑过并留下 `data_migrations` 标记，
// 改它的常量不会重跑；而 M60 之后又改了三处页面正文 ——
//  1. gpt-image 的 size 换算表不再手写，改由 apidoc.GPTImageTierPixels 渲染
//     （旧文案挂着 gpt-image-2 的另一套像素 16:9 → 1672x941，代码从来不发它）；
//  2. 补上 1:3 / 3:1 两行（官方有、代码此前没有），并标明它们没有 1K 档；
//  3. 新增一句实测：直接写比例上游接受，但出图尺寸由上游决定（16:9 → 1360x768）。
//
// 圈定范围与 M60 完全一致（幂等：同一个 DELETE + 重新播种）。
const migrationDocsImagesReplay2 = "m62_docs_images_replay2"

// imageSeedTargets 圈定要重播的图片接口页（**必须带表别名 `e`**）。
const imageSeedTargets = `e.origin = 'seed' AND e.key LIKE 'ep-images-%'`

// M63：视频四族接口页重播（只为把 Notes 里的内部用词改掉）。
//
// 为什么必须重播而不是只改代码：`origin='seed'` 的页面内容存在 `doc_*` 表里，
// 播种规则是"已存在就跳过"。文案守卫（E_TEXT_FORBIDDEN）会在发布时拦下这些词，
// 但**播种路径不跑守卫** —— 于是页面上留着「渠道」「上游」这类内部视角用词，
// 而它们仍然显示为 published；一旦有人编辑并保存，发布会被阻断，现象是
// "这篇文档改不动"，原因却藏在几行说明文字里。
const migrationDocsVideosReplay = "m63_docs_videos_replay"

// videoDocReplayTargets 圈定要重播的视频接口页（**必须带表别名 `e`**）。
//
// 与 migrations_docs_video_material.go 的 `videoSeedTargets` 分开：那个只圈提交页，
// 这里连「查询任务状态」页一起重播（它们共用同一段节点导语，用词也要改）。
const videoDocReplayTargets = `e.origin = 'seed' AND e.key LIKE 'ep-videos-%'`

// MigrateDocsVideosReplay 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsVideosReplay(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsVideosReplay).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询视频文档重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}
	var exists int64
	if err := gdb.Model(&model.DocEndpoint{}).
		Where("origin = ? AND key LIKE 'ep-videos-%'", model.DocOriginSeed).
		Count(&exists).Error; err != nil {
		return false, fmt.Errorf("db: 查询视频接口页失败: %w", err)
	}
	if exists == 0 {
		return false, nil
	}

	const endpointIDs = `SELECT e.id FROM doc_endpoints e WHERE ` + videoDocReplayTargets
	const fieldIDs = `SELECT f.id FROM doc_endpoint_fields f
	                    JOIN doc_endpoints e ON e.id = f.endpoint_id
	                   WHERE ` + videoDocReplayTargets

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			`DELETE FROM doc_field_enums WHERE field_id IN (` + fieldIDs + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoints e WHERE ` + videoDocReplayTargets,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播视频文档种子: %w", err)
			}
		}
		if err := tx.Exec(`
			UPDATE doc_nodes
			   SET status = 'published', published_at = COALESCE(published_at, now())
			 WHERE origin = 'seed' AND status <> 'published'`).Error; err != nil {
			return fmt.Errorf("恢复种子节点发布态: %w", err)
		}
		return tx.Create(&model.DataMigration{
			Name: migrationDocsVideosReplay, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsVideosReplay 是 M63 的日志。
func LogDocsVideosReplay(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("视频四族接口页已重播（M63：清掉说明里的内部视角用词）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}

// MigrateDocsImagesReplay 只执行一次（`data_migrations` 标记），返回是否真的执行过。
func MigrateDocsImagesReplay(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsImagesReplay).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询图片文档重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}
	// 图片页还不存在（首次部署在 M60 之后才播种）时不必打标记：
	// 打了标记会让「先升级、后建库」的机器永远拿不到重播。
	var exists int64
	if err := gdb.Model(&model.DocEndpoint{}).
		Where("origin = ? AND key LIKE 'ep-images-%'", model.DocOriginSeed).
		Count(&exists).Error; err != nil {
		return false, fmt.Errorf("db: 查询图片接口页失败: %w", err)
	}
	if exists == 0 {
		return false, nil
	}

	const endpointIDs = `SELECT e.id FROM doc_endpoints e WHERE ` + imageSeedTargets
	const fieldIDs = `SELECT f.id FROM doc_endpoint_fields f
	                    JOIN doc_endpoints e ON e.id = f.endpoint_id
	                   WHERE ` + imageSeedTargets

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			`DELETE FROM doc_field_enums WHERE field_id IN (` + fieldIDs + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoints e WHERE ` + imageSeedTargets,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播图片文档种子: %w", err)
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
			Name: migrationDocsImagesReplay, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func MigrateDocsImagesReplay2(gdb *gorm.DB) (bool, error) {
	if !gdb.Migrator().HasTable(&model.DocEndpoint{}) {
		return false, nil
	}
	if gdb.Migrator().HasTable(&model.DataMigration{}) {
		var n int64
		if err := gdb.Model(&model.DataMigration{}).
			Where("name = ?", migrationDocsImagesReplay2).Count(&n).Error; err != nil {
			return false, fmt.Errorf("db: 查询图片文档重播标记失败: %w", err)
		}
		if n > 0 {
			return false, nil
		}
	}
	// 图片页还不存在（首次部署在 M60 之后才播种）时不必打标记：
	// 打了标记会让「先升级、后建库」的机器永远拿不到重播。
	var exists int64
	if err := gdb.Model(&model.DocEndpoint{}).
		Where("origin = ? AND key LIKE 'ep-images-%'", model.DocOriginSeed).
		Count(&exists).Error; err != nil {
		return false, fmt.Errorf("db: 查询图片接口页失败: %w", err)
	}
	if exists == 0 {
		return false, nil
	}

	const endpointIDs = `SELECT e.id FROM doc_endpoints e WHERE ` + imageSeedTargets
	const fieldIDs = `SELECT f.id FROM doc_endpoint_fields f
	                    JOIN doc_endpoints e ON e.id = f.endpoint_id
	                   WHERE ` + imageSeedTargets

	err := gdb.Transaction(func(tx *gorm.DB) error {
		stmts := []string{
			`DELETE FROM doc_field_enums WHERE field_id IN (` + fieldIDs + `)`,
			`DELETE FROM doc_endpoint_fields WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_response_variants WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_presets WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoint_errors WHERE endpoint_id IN (` + endpointIDs + `)`,
			`DELETE FROM doc_endpoints e WHERE ` + imageSeedTargets,
		}
		for _, q := range stmts {
			if err := tx.Exec(q).Error; err != nil {
				return fmt.Errorf("重播图片文档种子: %w", err)
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
			Name: migrationDocsImagesReplay2, AppliedAt: time.Now(),
		}).Error
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// LogDocsImagesReplay 只在真的执行过时打一行。
func LogDocsImagesReplay(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("图片三族接口页已按新契约重播（M60）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}

// LogDocsImagesReplay2 是 M62 的日志（与 M60 分开，方便在日志里确认第二次真的跑了）。
func LogDocsImagesReplay2(applied bool) {
	if !applied {
		return
	}
	logger.L().Warn("图片三族接口页已重播（M62：尺寸换算表由事实渲染、补 1:3/3:1）",
		zap.String("at", time.Now().Format(time.RFC3339)))
}
