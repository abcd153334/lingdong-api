package spec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
)

// 本文件覆盖规格服务的持久层语义：唯一性、引用保护、乐观锁、写后失效。
//
// 为什么这些必须连库测：它们全部依赖**索引与事务行为**（部分唯一索引的
// deleted_at 条件、RowsAffected 的 CAS 语义），用内存替身测等于没测。

const specTestSchema = "ld_test_adapter_spec"

func withSpecDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + specTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理测试 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + specTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+specTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.AdapterSpec{}, &model.Channel{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	// 与 db.EnsureIndexes 同口径的部分唯一索引：slug 只在未软删行上唯一。
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_adapter_specs_slug
		ON adapter_specs (slug) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("创建唯一索引失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	// 把规格解析器接进注册表（生产由 main 调用）。不装的话 relay 侧永远解析不到，
	// 「写后失效」这类用例会变成永远为假的断言。
	Install()
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + specTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
		// 缓存与注册表是进程级单例：用例之间必须清干净，否则会串味。
		cache.Purge("")
		for _, slug := range []string{"relay-a", "relay-b"} {
			relay.ForgetAdapter(AdapterID(slug))
		}
	})
	return gdb
}

const minimalSpec = `{"slug":"relay-a","label":"中转 A","endpoints":{
	"chat/completions":{"path":"/v1/chat/completions","passthrough":true}}}`

func createSpec(t *testing.T, in Input) *Item {
	t.Helper()
	item, err := Create(context.Background(), in)
	if err != nil {
		t.Fatalf("创建规格失败: %v", err)
	}
	return item
}

// TestCreateSpecFillsProjection 断言投影列（slug/label/vendor）与 DSL 同批写入。
func TestCreateSpecFillsProjection(t *testing.T) {
	withSpecDB(t)

	item := createSpec(t, Input{
		Slug: "relay-a", Label: "中转 A", Vendor: "openai",
		Spec: json.RawMessage(`{"endpoints":{"chat/completions":{"path":"/v1/chat/completions","passthrough":true}}}`),
	})
	if item.Slug != "relay-a" || item.Label != "中转 A" || item.Vendor != "openai" {
		t.Fatalf("投影列未写入: %+v", item)
	}
	if item.AdapterID != "spec:relay-a" {
		t.Fatalf("适配器标识不符: %q", item.AdapterID)
	}
	if item.Version != 1 || item.Status != model.SpecStatusEnabled {
		t.Fatalf("初始版本/状态不符: version=%d status=%d", item.Version, item.Status)
	}
	// 规范化后的 DSL 必须能被运行时解析（保存即生效）。
	if _, err := ResolveBySlug("relay-a"); err != nil {
		t.Fatalf("新建规格后应能立即解析: %v", err)
	}
}

// TestCreateSpecDuplicateSlug 断言重名被拒（应用层提示 + 索引兜底）。
func TestCreateSpecDuplicateSlug(t *testing.T) {
	withSpecDB(t)
	createSpec(t, Input{Slug: "relay-a", Label: "中转 A", Spec: json.RawMessage(minimalSpec)})

	_, err := Create(context.Background(), Input{Slug: "relay-a", Label: "另一个", Spec: json.RawMessage(minimalSpec)})
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("重名应返回 ErrSlugTaken，实际 %v", err)
	}
}

// TestCreateSpecRejectsMismatchedSlug 断言 DSL 与表单两处 slug 不一致时报错而不是静默覆盖。
func TestCreateSpecRejectsMismatchedSlug(t *testing.T) {
	withSpecDB(t)
	_, err := Create(context.Background(), Input{
		Slug: "relay-b", Label: "中转 B",
		Spec: json.RawMessage(`{"slug":"relay-a","label":"中转 B","endpoints":{
			"chat/completions":{"path":"/v1/chat/completions","passthrough":true}}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "slug") {
		t.Fatalf("slug 不一致应报错并定位到 slug，实际 %v", err)
	}
}

// TestUpdateSpecVersionCAS 断言乐观锁：旧版本号写入被拒，新版本号成功且 version 自增。
func TestUpdateSpecVersionCAS(t *testing.T) {
	withSpecDB(t)
	item := createSpec(t, Input{Slug: "relay-a", Label: "中转 A", Spec: json.RawMessage(minimalSpec)})

	_, err := Update(context.Background(), item.ID, Input{
		Label: "中转 A2", Version: item.Version, Spec: json.RawMessage(minimalSpec),
	})
	if err != nil {
		t.Fatalf("持当前版本应能保存: %v", err)
	}
	// 用旧版本号再写一次：必须被拒（否则会静默覆盖别人的改动）。
	_, err = Update(context.Background(), item.ID, Input{
		Label: "中转 A3", Version: item.Version, Spec: json.RawMessage(minimalSpec),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧版本应返回 ErrConflict，实际 %v", err)
	}
	got, err := Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Label != "中转 A2" || got.Version != item.Version+1 {
		t.Fatalf("更新结果不符: label=%q version=%d", got.Label, got.Version)
	}
}

// TestUpdateSpecSlugImmutable 断言 slug 不可改（改 slug 会让所有引用渠道集体失联）。
func TestUpdateSpecSlugImmutable(t *testing.T) {
	withSpecDB(t)
	item := createSpec(t, Input{Slug: "relay-a", Label: "中转 A", Spec: json.RawMessage(minimalSpec)})
	_, err := Update(context.Background(), item.ID, Input{
		Slug: "relay-b", Label: "中转 A", Spec: json.RawMessage(minimalSpec),
	})
	if !errors.Is(err, ErrSlugImmutable) {
		t.Fatalf("改 slug 应被拒，实际 %v", err)
	}
}

// TestDeleteSpecBlockedByChannel 断言仍有渠道引用时禁止删除。
//
// 强删的后果是「配置看起来正常、每次请求都 500 适配器未注册」，最难排查，
// 所以这里宁可让运营先改渠道。
func TestDeleteSpecBlockedByChannel(t *testing.T) {
	gdb := withSpecDB(t)
	item := createSpec(t, Input{Slug: "relay-a", Label: "中转 A", Spec: json.RawMessage(minimalSpec)})

	ch := model.Channel{Name: "引用它的渠道", AdapterID: "spec:relay-a", MultiKeyMode: "random", Status: 1, AutoBan: true}
	if err := gdb.Create(&ch).Error; err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	if err := Delete(context.Background(), item.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("有渠道引用时应拒绝删除，实际 %v", err)
	}
	if n, err := UsageCount(context.Background(), "relay-a"); err != nil || n != 1 {
		t.Fatalf("引用计数不符: n=%d err=%v", n, err)
	}
	// 软删渠道后应可删除规格。
	if err := gdb.Delete(&ch).Error; err != nil {
		t.Fatalf("软删渠道失败: %v", err)
	}
	if err := Delete(context.Background(), item.ID); err != nil {
		t.Fatalf("无引用时应可删除: %v", err)
	}
	// 删除后同名 slug 必须可以重建（部分唯一索引带 deleted_at IS NULL）。
	createSpec(t, Input{Slug: "relay-a", Label: "重建", Spec: json.RawMessage(minimalSpec)})
}

// TestDisabledSpecNotResolved 断言停用后不再参与选路（停用必须是真开关）。
func TestDisabledSpecNotResolved(t *testing.T) {
	withSpecDB(t)
	item := createSpec(t, Input{Slug: "relay-a", Label: "中转 A", Spec: json.RawMessage(minimalSpec)})

	/***** 停用前：可解析 *****/
	if _, err := ResolveBySlug("relay-a"); err != nil {
		t.Fatalf("启用状态应能解析: %v", err)
	}
	if err := SetStatus(context.Background(), item.ID, model.SpecStatusDisabled); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, err := ResolveBySlug("relay-a"); err == nil {
		t.Fatal("停用后不得再解析（否则停用只是给管理台看的）")
	}
	// 停用后重新启用：必须立即可解析（注册表负缓存要被清掉）。
	if err := SetStatus(context.Background(), item.ID, model.SpecStatusEnabled); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if _, err := ResolveBySlug("relay-a"); err != nil {
		t.Fatalf("重新启用后应能立即解析: %v", err)
	}
}

// TestInvalidateClearsRelayNegativeCache 断言写库后注册表的负缓存被清掉。
//
// 真实时序：运营新建规格 → 立刻去建渠道 → 保存触发 adapterRegistered（CanResolve）。
// 若负缓存没清，保存会报「适配器不存在」，而规格明明刚建好（最长要等 5s 才自愈）。
func TestInvalidateClearsRelayNegativeCache(t *testing.T) {
	withSpecDB(t)
	// 先制造一次解析失败，把名字写进 5s 负缓存。
	if _, ok := relay.GetAdapter("spec:relay-a"); ok {
		t.Fatal("库里没有规格时不应能解析")
	}
	createSpec(t, Input{Slug: "relay-a", Label: "中转 A", Spec: json.RawMessage(minimalSpec)})
	if !relay.CanResolve("spec:relay-a") {
		t.Fatal("新建规格后应立即能被注册表解析（负缓存未清）")
	}
}

// TestListCountsChannels 断言列表一次算出引用数（避免前端 N+1 或显示 0）。
func TestListCountsChannels(t *testing.T) {
	gdb := withSpecDB(t)
	createSpec(t, Input{Slug: "relay-a", Label: "中转 A", Spec: json.RawMessage(minimalSpec)})
	createSpec(t, Input{Slug: "relay-b", Label: "中转 B", Vendor: "metaso",
		Spec: json.RawMessage(`{"endpoints":{"chat/completions":{"path":"/v1/chat/completions","passthrough":true}}}`)})

	for i := 0; i < 2; i++ {
		ch := model.Channel{Name: "渠道" + string(rune('a'+i)), AdapterID: "spec:relay-a", MultiKeyMode: "random", Status: 1, AutoBan: true}
		if err := gdb.Create(&ch).Error; err != nil {
			t.Fatalf("建渠道失败: %v", err)
		}
	}
	items, err := List(context.Background())
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应有 2 条规格，实际 %d", len(items))
	}
	for _, it := range items {
		switch it.Slug {
		case "relay-a":
			if it.ChannelCount != 2 {
				t.Fatalf("relay-a 引用数应为 2，实际 %d", it.ChannelCount)
			}
		case "relay-b":
			if it.ChannelCount != 0 {
				t.Fatalf("relay-b 引用数应为 0，实际 %d", it.ChannelCount)
			}
		}
		// 列表不下发 DSL 原文（列表带上 64KB 的 JSON 是纯浪费）。
		if len(it.Spec) != 0 {
			t.Fatalf("列表不应下发 spec 原文: %s", it.Slug)
		}
		if len(it.Endpoints) == 0 || len(it.Caps) == 0 {
			t.Fatalf("列表应下发端点与能力派生结果: %+v", it)
		}
	}
}
