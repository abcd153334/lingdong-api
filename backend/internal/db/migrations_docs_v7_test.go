package db

import (
	"testing"
)

// TestMigrateDocsV7DropsLegacyTables 守 M46：五张 v6 表必须被删掉，且重复执行无副作用。
//
// 为什么值得单独测：这是本项目唯一一条**删表**迁移。删错的症状不是报错，而是
// 「表还在、代码不读它」—— 下次有人 grep 到 `doc_groups` 会以为文档站有两套模型。
func TestMigrateDocsV7DropsLegacyTables(t *testing.T) {
	gdb := withIndexDB(t)

	// models() 已经不再建这五张表（否则每次启动重建、M46 再删，来回拉锯），
	// 所以这里手工建一个最小形态来模拟存量库。
	for _, ddl := range []string{
		`CREATE TABLE doc_groups (key text PRIMARY KEY, section text, sort int, hidden boolean DEFAULT false)`,
		`CREATE TABLE doc_group_members (group_key text, model_id text, PRIMARY KEY (group_key, model_id))`,
		`CREATE TABLE doc_group_texts (group_key text PRIMARY KEY)`,
		`CREATE TABLE doc_pages (slug text PRIMARY KEY, sort int)`,
		`CREATE TABLE doc_endpoint_docs (endpoint_kind text PRIMARY KEY)`,
	} {
		if err := gdb.Exec(ddl).Error; err != nil {
			t.Fatalf("建旧表失败: %v\n%s", err, ddl)
		}
	}

	applied, err := MigrateDocsV7DropLegacy(gdb)
	if err != nil {
		t.Fatalf("删表失败: %v", err)
	}
	if !applied {
		t.Fatal("有旧表时 applied 必须为 true（否则日志会谎报没事发生）")
	}
	for _, tbl := range []string{"doc_groups", "doc_group_members", "doc_group_texts", "doc_pages", "doc_endpoint_docs"} {
		var exists bool
		if err := gdb.Raw(`SELECT to_regclass(?) IS NOT NULL`, tbl).Scan(&exists).Error; err != nil {
			t.Fatalf("查询 %s 是否存在失败: %v", tbl, err)
		}
		if exists {
			t.Fatalf("%s 应已被删除", tbl)
		}
	}

	// 幂等：再跑一次不该报错，也不该再报 applied。
	if applied2, err := MigrateDocsV7DropLegacy(gdb); err != nil {
		t.Fatalf("重复执行应幂等: %v", err)
	} else if applied2 {
		t.Fatal("表已不存在时 applied 必须为 false")
	}
}
