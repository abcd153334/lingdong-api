package db

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

const idxTestSchema = "ld_test_indexes"

func withIndexDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	// 先 DROP 再 CREATE（而不是 IF NOT EXISTS）：残留 schema 会让「表缺失」这类问题
	// 变成偶发红 —— AutoMigrate 建表用的是同一个 search_path，但只要 schema 里
	// 有历史残留（上一次运行被中断、或本机手工建过），建索引就会撞上
	// `relation "..." does not exist`，而错误只指向第一张缺失的表。
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + idxTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + idxTestSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+idxTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	// 迁移清单**直接复用 models()**（就是启动时 AutoMigrate 用的那一份）。
	//
	// 这里曾经手写一份"只列用得到的表"的清单，结果漂移了三次：
	// 加 uq_adapter_specs_slug 时漏 adapter_specs、加 agent_* 索引时漏 agent_*、
	// 加文档站索引时漏 doc_nodes/doc_node_models/doc_endpoints。每次症状都一样：
	// 整个 db 包的索引用例集体红，错误只指向**第一张**缺失的表
	// （`relation "doc_node_models" does not exist`），剩下还漏了哪些只能
	// 翻 indexes.go 一行行对。手写清单本身就是 bug 来源，改成同源后不可能再漂。
	//
	// 代价是每个用例多迁移几十张空表（本地实测 <1s），换来的是"新增索引不会悄悄
	// 让门禁变红"。
	if err := gdb.AutoMigrate(models()...); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + idxTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// 幂等创建：连续两次执行无错，且 4 个索引均有效。
func TestEnsureIndexesIdempotent(t *testing.T) {
	gdb := withIndexDB(t)
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("首次创建索引失败: %v", err)
	}
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("重复创建索引应幂等: %v", err)
	}
	for _, name := range []string{
		"idx_usages_pending", "idx_usages_user_created", "idx_usages_key_created", "uq_transactions_usage_ref",
		"idx_model_bindings_channel", "idx_channels_adapter", "idx_usages_model_created",
		"uq_channels_name", "uq_vendors_name",
		// 虎皮椒接入新增：订单生命周期与「一单最多一笔到账」。
		"idx_orders_status_expire", "idx_orders_user_id", "idx_orders_query", "uq_transactions_order_recharge",
		// B4 新增：轮询公平调度 / 到期扫描 / 提交幂等。
		"idx_tasks_poll", "idx_tasks_expire", "uq_tasks_idem_v2",
		// 文档站 v7：节点 key / 接口 key / 一个模型只能属于一篇模型族。
		"uq_doc_nodes_key", "uq_doc_endpoints_key_v2",
		// AI 接入助手：消息序号兜底唯一 / 提案与会话列表 / 预算闸按天汇总 / 素材列表。
		// 这四条过去不在断言名单里 —— 写错表名或列名时 EnsureIndexes 只记 Error，
		// 整包测试仍然全绿。
		"uq_agent_messages_seq", "idx_agent_proposals_session",
		"idx_agent_runs_created", "idx_agent_docs_created",
		// 探测凭据：判定键 + 过期清理。这两条过去只写在 GORM tag 里，
		// 缺了/建错都不会有任何报错（tag 建的索引进不了 verifyIndexes）。
		"idx_agent_dry_runs_lookup", "idx_agent_dry_runs_expires",
	} {
		var valid *bool
		if err := gdb.Raw(`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass(?)`, name).
			Scan(&valid).Error; err != nil {
			t.Fatalf("查询索引 %s 失败: %v", name, err)
		}
		if valid == nil || !*valid {
			t.Fatalf("索引 %s 缺失或无效", name)
		}
	}
}

// 文档站节点的 key 唯一性必须是**部分索引**（`WHERE key <> ”`）。
//
// 全表唯一会让"草稿节点先建、后填 key"这条路径建不出来；而不唯一则会让
// 两个节点抢同一个 URL，访问哪篇取决于 order by —— 两种都没有报错。
func TestDocNodesKeyIndexIsPartial(t *testing.T) {
	gdb := withIndexDB(t)
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	var def string
	if err := gdb.Raw(`SELECT indexdef FROM pg_indexes WHERE indexname = 'uq_doc_nodes_key'`).
		Scan(&def).Error; err != nil {
		t.Fatalf("查询索引定义失败: %v", err)
	}
	if !strings.Contains(def, "UNIQUE") || !strings.Contains(def, "key") {
		t.Fatalf("uq_doc_nodes_key 必须是 (key) 的唯一索引：%s", def)
	}
	if !strings.Contains(def, "WHERE") {
		t.Fatalf("uq_doc_nodes_key 必须是部分索引（否则空 key 会互相冲突）：%s", def)
	}
}

// 同一 usage 的重复结算流水会被部分唯一索引拦截。
func TestUniqueUsageRefBlocksDuplicateSettle(t *testing.T) {
	gdb := withIndexDB(t)
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	row := model.Transaction{UserID: 1, Type: "settle", Amount: -100, RefType: "usage", RefID: "1"}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	dup := model.Transaction{UserID: 1, Type: "settle", Amount: -100, RefType: "usage", RefID: "1"}
	if err := gdb.Create(&dup).Error; err == nil {
		t.Fatal("重复 usage 结算流水应被唯一索引拒绝")
	}
	// 其它 ref_type（人工调账/邀请）允许重复
	for i := 0; i < 2; i++ {
		if err := gdb.Create(&model.Transaction{UserID: 1, Type: "adjust", Amount: 1,
			RefType: "admin_adjust", RefID: "1"}).Error; err != nil {
			t.Fatalf("非 usage 维度不应受唯一索引限制: %v", err)
		}
	}
}

// 全量 AutoMigrate + 索引创建在空 schema 上可用（等价新库首次启动）。
func TestAutoMigrateOnEmptySchema(t *testing.T) {
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过数据库集成用例")
	}
	const schema = "ld_test_automigrate"
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+schema), silent)
	if err != nil {
		t.Fatalf("连接 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := AutoMigrate(gdb); err != nil {
		t.Fatalf("AutoMigrate 失败: %v", err)
	}
	// 新列存在且有默认值（空表插入不指定列应成功）
	if err := gdb.Exec(`INSERT INTO usages (user_id, key_id, status) VALUES (1,1,'pending')`).Error; err != nil {
		t.Fatalf("usages 新列默认值异常: %v", err)
	}
	var src string
	if err := gdb.Raw(`SELECT usage_source FROM usages LIMIT 1`).Scan(&src).Error; err != nil || src != "" {
		t.Fatalf("usage_source 默认值不符: %q err=%v", src, err)
	}

	// 充值套餐：两列默认值 + 门户索引 + 空表种子（等价「新库首装」的完整路径）
	var sortDefault string
	// `table_schema = current_schema()` 不能省：information_schema.columns 是**全库**
	// 视图，同一个测试库里还有 public 与其他测试 schema 的 packages 表。
	// 少了这个条件，查询会命中别人建的那张表 —— 表现是"默认值丢了"这种
	// 与本次改动毫无关系的红，而且换个 schema 创建顺序就变绿（最典型的假红）。
	if err := gdb.Raw(`SELECT column_default FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'packages' AND column_name = 'sort_order'`).Scan(&sortDefault).Error; err != nil {
		t.Fatalf("读取 sort_order 默认值失败: %v", err)
	}
	if !strings.Contains(sortDefault, "1000") {
		t.Fatalf("sort_order 默认值应为 1000（未排序语义），实际 %q", sortDefault)
	}
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	var idx int64
	if err := gdb.Raw(`SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema()
		AND indexname = 'idx_packages_portal'`).Scan(&idx).Error; err != nil || idx != 1 {
		t.Fatalf("idx_packages_portal 未创建: n=%d err=%v", idx, err)
	}
	if err := seedPackages(gdb); err != nil {
		t.Fatalf("种子失败: %v", err)
	}
	var rows []model.Package
	if err := gdb.Order("sort_order").Find(&rows).Error; err != nil {
		t.Fatalf("读取套餐失败: %v", err)
	}
	if len(rows) != 4 || rows[0].Name != "体验包" || rows[3].Name != "旗舰包" {
		t.Fatalf("空库首装应有 4 档且按 sort_order 有序，实际 %d 档", len(rows))
	}
	if rows[1].Badge != "recommend" {
		t.Fatalf("标准包应带 recommend 角标，实际 %q", rows[1].Badge)
	}
}

// 同名渠道在软删前不允许并存；软删后可复用同名（部分唯一索引条件 deleted_at IS NULL）。
func TestChannelNameUniqueRespectsSoftDelete(t *testing.T) {
	gdb := withIndexDB(t)
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	mk := func(name string) *model.Channel {
		return &model.Channel{Name: name, AdapterID: "openai", MultiKeyMode: "random", Weight: 1, Priority: 1, Status: 1, AutoBan: true}
	}
	first := mk("同名渠道")
	if err := gdb.Create(first).Error; err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	if err := gdb.Create(mk("同名渠道")).Error; err == nil {
		t.Fatal("同名渠道应被唯一索引拒绝（应用层查重之外的第二道防线）")
	}
	if err := gdb.Delete(&model.Channel{}, first.ID).Error; err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	if err := gdb.Create(mk("同名渠道")).Error; err != nil {
		t.Fatalf("软删后应可复用同名: %v", err)
	}
}

// 历史库已有重名时，唯一索引创建失败不能阻断启动（跳过 + WARN）。
func TestDuplicateNamesSkipUniqueIndex(t *testing.T) {
	gdb := withIndexDB(t)
	// 先绕过索引造出重名（此时索引尚未创建）
	for i := 0; i < 2; i++ {
		if err := gdb.Create(&model.Channel{Name: "重复", AdapterID: "openai", MultiKeyMode: "random"}).Error; err != nil {
			t.Fatalf("造重名数据失败: %v", err)
		}
	}
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("存在重名时不应阻断启动: %v", err)
	}
	var valid *bool
	if err := gdb.Raw(`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass('uq_channels_name')`).
		Scan(&valid).Error; err != nil {
		t.Fatalf("查询索引失败: %v", err)
	}
	if valid != nil {
		t.Fatal("重名场景下 uq_channels_name 应被跳过")
	}
}

// 一单最多一笔到账流水；退款（type=refund）与人工调账不受该约束。
//
// 这是并发回调的第二道防线：回调与主动查单可能同时命中同一订单，
// 订单行锁能挡住绝大多数情况，但唯一索引保证「即使锁失效也只会入账一次」。
func TestUniqueOrderRecharge(t *testing.T) {
	gdb := withIndexDB(t)
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	first := model.Transaction{UserID: 1, Type: "recharge", Amount: 100, RefType: "order", RefID: "42"}
	if err := gdb.Create(&first).Error; err != nil {
		t.Fatalf("首笔到账失败: %v", err)
	}
	dup := model.Transaction{UserID: 1, Type: "recharge", Amount: 100, RefType: "order", RefID: "42"}
	if err := gdb.Create(&dup).Error; err == nil {
		t.Fatal("同一订单的重复到账应被唯一索引拒绝")
	}
	// 退款：同一订单可以有退款流水（可能先退款再人工补账再退款，业务上不禁止）。
	if err := gdb.Create(&model.Transaction{UserID: 1, Type: "refund", Amount: -100,
		RefType: "order", RefID: "42"}).Error; err != nil {
		t.Fatalf("退款流水不应受 recharge 唯一索引限制: %v", err)
	}
	// 人工调账：ref_type 不同，更不受影响。
	if err := gdb.Create(&model.Transaction{UserID: 1, Type: "adjust", Amount: 5,
		RefType: "admin_adjust", RefID: "42"}).Error; err != nil {
		t.Fatalf("人工调账不应受订单唯一索引限制: %v", err)
	}
}

// 提交幂等键：同一 KEY 内唯一，跨 KEY 互不影响，空键完全不约束。
//
// 这是「重复提交造成上游双倍成本」的第二道防线（B4-2）：先查再插在并发下必然都查不到，
// 只有唯一索引能在两条并发事务里只放行一条。
func TestUniqueTaskIdempotencyKey(t *testing.T) {
	gdb := withIndexDB(t)
	if err := EnsureIndexes(gdb); err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	mk := func(keyID uint64, idem string) *model.Task {
		return &model.Task{KeyID: keyID, UserID: 1, Model: "m", Status: "queued", IdempotencyKey: idem}
	}
	if err := gdb.Create(mk(1, "abc")).Error; err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if err := gdb.Create(mk(1, "abc")).Error; err == nil {
		t.Fatal("同一 KEY 的同一幂等键必须被唯一索引拒绝（否则会向上游重复建单）")
	}
	// 不同 KEY 用同一个幂等键是合法的：幂等域是「KEY」，不是全局。
	if err := gdb.Create(mk(2, "abc")).Error; err != nil {
		t.Fatalf("不同 KEY 复用同一幂等键不应冲突: %v", err)
	}
	// 未声明幂等键的提交可以有无数条。
	for i := 0; i < 3; i++ {
		if err := gdb.Create(mk(1, "")).Error; err != nil {
			t.Fatalf("空幂等键不应受唯一约束: %v", err)
		}
	}
	// 软删后允许复用同一幂等键（与默认作用域口径一致）。
	var first model.Task
	if err := gdb.Where("key_id = ? AND idempotency_key = ?", 1, "abc").First(&first).Error; err != nil {
		t.Fatalf("读取首条任务失败: %v", err)
	}
	if err := gdb.Delete(&model.Task{}, first.ID).Error; err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	if err := gdb.Create(mk(1, "abc")).Error; err != nil {
		t.Fatalf("软删后应可复用同一幂等键: %v", err)
	}
}

// 一个模型只属于一族这条约束已经上移到 `apidoc.FamilyDocOfModelID`（模型 ID 命中唯一一条
// FamilyDoc），不再靠 `doc_node_models` 上的部分唯一索引 —— 那张表已在 M47 删除。
// 这里不再有对应断言：真正的守卫是启动期的 EnsureIndexes（required 索引建不出来会直接失败）。
