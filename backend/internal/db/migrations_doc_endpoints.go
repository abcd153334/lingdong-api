package db

import (
	"fmt"

	"gorm.io/gorm"
)

// MigrationNameDocEndpoint 是「接口文档主键从 kind 改为 doc_id」的数据动作标识。
//
// 为什么必须改主键：同一条路径可以有两套报文（原生 OpenAI 与即梦原生报文），
// 那是两篇字段表完全不同的文档。单列主键下它们只能二选一 —— 结果就是
// "字段表必然对一半模型说谎"，而页面与接口都不会报错。
//
// 为什么必须**先于 AutoMigrate** 跑：GORM 会按新 struct 直接给 doc_id 加主键约束，
// 而存量行 doc_id 是空的（NOT NULL 冲突），整个启动就卡在建表阶段。
const MigrationNameDocEndpoint = "m35_doc_endpoint_identity"

// DocEndpointMigrationReport 是一次迁移的结果（供启动日志核对）。
type DocEndpointMigrationReport struct {
	// Rekeyed 表示主键是否被改写（已改过则为 false）。
	Rekeyed bool `json:"rekeyed"`
	// DroppedColumns 是被删除的派生列（section / group）。
	DroppedColumns []string `json:"dropped_columns"`
}

// MigrateDocEndpointIdentity 执行 m35（幂等）。
//
// 六步，顺序不能换：
//  1. 补 doc_id 列并回填（= endpoint_kind，这是主篇身份的既有等价写法）；
//  2. 置 NOT NULL（否则主键建不起来）；
//  3. 删旧主键；
//  4. 建新主键 doc_id；
//  5. 删 section / group（一篇文档可挂多个板块，单值列只能是假的）；
//  6. 删掉已下线端点的残行（EnsureGenericDocs 只插不删，残行会让"新增数"永远对不上）。
func MigrateDocEndpointIdentity(gdb *gorm.DB, dryRun bool) (DocEndpointMigrationReport, error) {
	var rep DocEndpointMigrationReport
	if !gdb.Migrator().HasTable("doc_endpoint_docs") {
		// 全新库：AutoMigrate 会按新 schema 建表，不需要任何数据动作。
		return rep, nil
	}
	hasDocID := gdb.Migrator().HasColumn("doc_endpoint_docs", "doc_id")
	pkIsDocID := hasDocID && gdb.Migrator().HasIndex("doc_endpoint_docs", "doc_endpoint_docs_pkey")
	if dryRun {
		rep.Rekeyed = !pkIsDocID
		for _, c := range []string{"section", "group"} {
			if gdb.Migrator().HasColumn("doc_endpoint_docs", c) {
				rep.DroppedColumns = append(rep.DroppedColumns, c)
			}
		}
		return rep, nil
	}

	stmts := []string{
		`ALTER TABLE doc_endpoint_docs ADD COLUMN IF NOT EXISTS doc_id text`,
		`UPDATE doc_endpoint_docs SET doc_id = endpoint_kind WHERE doc_id IS NULL OR doc_id = ''`,
		`ALTER TABLE doc_endpoint_docs ALTER COLUMN doc_id SET NOT NULL`,
		`ALTER TABLE doc_endpoint_docs DROP CONSTRAINT IF EXISTS doc_endpoint_docs_pkey`,
		`ALTER TABLE doc_endpoint_docs ADD CONSTRAINT doc_endpoint_docs_pkey PRIMARY KEY (doc_id)`,
		`ALTER TABLE doc_endpoint_docs DROP COLUMN IF EXISTS section`,
		`ALTER TABLE doc_endpoint_docs DROP COLUMN IF EXISTS "group"`,
		// 读面收敛：这 4 个端点已下线，行留着只会让管理台出现点进去 404 的文档。
		`DELETE FROM doc_endpoint_docs WHERE endpoint_kind IN
			('video/list','video/get','video/content','video/cancel')`,
	}
	for i, sql := range stmts {
		if err := gdb.Exec(sql).Error; err != nil {
			return rep, fmt.Errorf("db: m35 第 %d 步失败: %w", i+1, err)
		}
	}
	rep.Rekeyed = true
	for _, c := range []string{"section", "group"} {
		if gdb.Migrator().HasColumn("doc_endpoint_docs", c) {
			rep.DroppedColumns = append(rep.DroppedColumns, c)
		}
	}
	return rep, nil
}
