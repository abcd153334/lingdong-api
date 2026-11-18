package db

import (
	"testing"
)

// TestEnsureUsageLookupIndexes 是 M18 的落地验证。
//
// 为什么必须真的建一次索引：`CREATE INDEX CONCURRENTLY` 有两个**只在运行时**才暴露的坑 ——
//  1. 它不能在事务里执行（会报 "cannot run inside a transaction block"），
//     而 GORM 默认给写操作套事务；
//  2. 中途失败会留下 **invalid** 索引，此后 `IF NOT EXISTS` 永远跳过 ——
//     索引看着存在、实际从不参与查询，属于最难发现的"静默失效"。
//
// auto-migrate 的普通流程完全覆盖不到这两点。
func TestEnsureUsageLookupIndexes(t *testing.T) {
	gdb := withIndexDB(t)
	// 连续两次：第二次必须走「标记已存在」的短路，不重复建、不报错。
	ensureUsageLookupIndexes(gdb)
	ensureUsageLookupIndexes(gdb)

	for _, name := range []string{"idx_usages_task_id", "idx_usages_channel_created"} {
		var valid *bool
		if err := gdb.Raw(`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass(?)`, name).
			Scan(&valid).Error; err != nil {
			t.Fatalf("查询索引 %s 失败: %v", name, err)
		}
		if valid == nil {
			t.Fatalf("索引 %s 未创建（CONCURRENTLY 失败时只会 Warn，不会阻断启动）", name)
		}
		if !*valid {
			t.Fatalf("索引 %s 存在但无效（indisvalid=false）：说明 CONCURRENTLY 中途失败，需要自愈", name)
		}
	}

	// 幂等性由 `IF NOT EXISTS` + indisvalid 自愈保证，不再有独立标记表：
	// 标记表是第二真相源（标记说"做过了"但索引其实建失败），已被删除。
	// 这里再跑一次只为确认"重复调用不报错、不把好索引判成坏的"。
	ensureUsageLookupIndexes(gdb)
	for _, name := range []string{"idx_usages_task_id", "idx_usages_channel_created"} {
		if valid, exists := indexValid(gdb, name); !exists || !valid {
			t.Fatalf("重复执行后索引 %s 应仍然有效（exists=%v valid=%v）", name, exists, valid)
		}
	}
}
