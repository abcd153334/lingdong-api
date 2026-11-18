package db

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/model"
)

const seedChannelSchema = "ld_test_seed_channels"

// withChannelDB 迁移「渠道 / 模型 / 绑定」三张表。
//
// 这三张表放在一起是因为本次修的两个 bug 都出在它们的关系上：
// 渠道没有唯一索引（OnConflict 拦不住重复），绑定硬编码了自增主键。
func withChannelDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + seedChannelSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+seedChannelSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Vendor{}, &model.Model{}, &model.Channel{}, &model.ModelBinding{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + seedChannelSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// TestSeedChannelsOnlyWhenEmpty 重复调用不重复插入，删光后不复活。
//
// 这是「82 条渠道里 78 条是假渠道」的直接回归用例：改造前 seedChannels 用
// OnConflict{DoNothing}，而 channels 除主键外没有任何唯一索引 → 每次启动插一遍。
func TestSeedChannelsOnlyWhenEmpty(t *testing.T) {
	gdb := withChannelDB(t)

	for i := 0; i < 3; i++ {
		if err := seedChannels(gdb); err != nil {
			t.Fatalf("第 %d 次 seedChannels 失败: %v", i+1, err)
		}
	}
	var n int64
	if err := gdb.Unscoped().Model(&model.Channel{}).Count(&n).Error; err != nil {
		t.Fatalf("统计渠道失败: %v", err)
	}
	// 演示层只留一条 OpenAI 官方：火山方舟的数据面在 /api/v3，openai 适配器打不通，
	// 它改由内置预设层写入（停用 + 空密钥），不属于演示层。
	if n != 1 {
		t.Fatalf("渠道数应为 1（连跑 3 次仍为 1），实际 %d", n)
	}

	// 运营在管理台删光演示渠道（**软删**，`channel.Delete` 就是默认 Delete）→ 重启不得复活。
	// 机制：seedChannels 用 Unscoped().Count() 计数，软删行仍被计入 → 表"非空" → 不写。
	// 只有物理删除才会重新触发种子，而管理台不提供物理删除渠道的入口。
	if err := gdb.Where("1 = 1").Delete(&model.Channel{}).Error; err != nil {
		t.Fatalf("清空渠道失败: %v", err)
	}
	if err := seedChannels(gdb); err != nil {
		t.Fatalf("清空后 seedChannels 失败: %v", err)
	}
	if err := gdb.Model(&model.Channel{}).Count(&n).Error; err != nil {
		t.Fatalf("统计渠道失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("软删清空后不应复活，实际又出现 %d 条", n)
	}
}

// TestSeedIconsAreValidBuiltinKeys 出厂图标 key 必须真实存在。
//
// 为什么必须有用例：seed 走的是 `Table().Create()`，**不经过** brandicon.Normalize，
// 所以 `builtin:openai`（少个 -gpt）会安静地写进库 —— 门户上那个供应商永远显示首字，
// 而管理台里它的 icon 列看起来"有值"，运营改都改不回来（得先知道该改成什么）。
// 14 个供应商 + 演示渠道逐个校验，成本一次查表。
func TestSeedIconsAreValidBuiltinKeys(t *testing.T) {
	gdb := withChannelDB(t)
	if err := seedVendors(gdb); err != nil {
		t.Fatalf("seedVendors 失败: %v", err)
	}
	if err := seedChannels(gdb); err != nil {
		t.Fatalf("seedChannels 失败: %v", err)
	}

	var vendors []model.Vendor
	if err := gdb.Find(&vendors).Error; err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	}
	if len(vendors) == 0 {
		t.Fatal("没有供应商，用例失去意义")
	}
	for _, v := range vendors {
		if v.Icon == "" {
			continue // 「其他」刻意不给图标（不是品牌，显示首字）
		}
		if _, err := brandicon.Normalize(v.Icon); err != nil {
			t.Fatalf("供应商 %s 的图标 %q 不合法: %v", v.ID, v.Icon, err)
		}
		key, ok := brandicon.BuiltinKey(v.Icon)
		if !ok {
			t.Fatalf("供应商 %s 的出厂图标必须是内置 key，实际 %q", v.ID, v.Icon)
		}
		if _, hit := brandicon.Lookup(key); !hit {
			t.Fatalf("供应商 %s 引用了不存在的内置图标 %q", v.ID, key)
		}
	}

	var channels []model.Channel
	if err := gdb.Find(&channels).Error; err != nil {
		t.Fatalf("读取渠道失败: %v", err)
	}
	for _, ch := range channels {
		if ch.Icon == "" {
			continue
		}
		if _, err := brandicon.Normalize(ch.Icon); err != nil {
			t.Fatalf("渠道 %s 的图标 %q 不合法: %v", ch.Name, ch.Icon, err)
		}
	}
}

// TestSeedBindingsResolveRealChannels 绑定必须落在真实渠道上，且不产生孤儿。
func TestSeedBindingsResolveRealChannels(t *testing.T) {
	gdb := withChannelDB(t)

	if err := seedModels(gdb); err != nil {
		t.Fatalf("seedModels 失败: %v", err)
	}
	if err := seedChannels(gdb); err != nil {
		t.Fatalf("seedChannels 失败: %v", err)
	}
	// 先插入一条无关渠道，把自增主键推到 3+：硬编码 1/2 的写法在这里必然错位。
	stray := model.Channel{Name: "占位", AdapterID: "openai", BaseURL: "https://example.invalid",
		Keys: model.MustJSON([]string{}), Groups: model.MustJSON([]string{"default"}), Status: 2}
	if err := gdb.Create(&stray).Error; err != nil {
		t.Fatalf("插入占位渠道失败: %v", err)
	}

	if err := seedBindings(gdb); err != nil {
		t.Fatalf("seedBindings 失败: %v", err)
	}

	var bindings []model.ModelBinding
	if err := gdb.Find(&bindings).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if len(bindings) == 0 {
		t.Fatal("应写入至少一条绑定")
	}

	var channels []model.Channel
	if err := gdb.Find(&channels).Error; err != nil {
		t.Fatalf("读取渠道失败: %v", err)
	}
	byID := make(map[uint64]model.Channel, len(channels))
	for _, c := range channels {
		byID[c.ID] = c
	}
	for _, b := range bindings {
		c, ok := byID[b.ChannelID]
		if !ok {
			t.Fatalf("绑定 %s 指向不存在的渠道 %d", b.ModelID, b.ChannelID)
		}
		if c.ID == stray.ID {
			t.Fatalf("绑定 %s 落到了无关渠道 %d", b.ModelID, stray.ID)
		}
	}

	// 渠道被删 → 再跑一次不得留下指向已删渠道的孤儿绑定。
	if err := gdb.Unscoped().Where("base_url = ?", "https://api.openai.com").Delete(&model.Channel{}).Error; err != nil {
		t.Fatalf("删除 OpenAI 官方渠道失败: %v", err)
	}
	if err := gdb.Unscoped().Where("1 = 1").Delete(&model.ModelBinding{}).Error; err != nil {
		t.Fatalf("清空绑定失败: %v", err)
	}
	if err := seedBindings(gdb); err != nil {
		t.Fatalf("渠道缺失时 seedBindings 失败: %v", err)
	}
	var orphan int64
	if err := gdb.Model(&model.ModelBinding{}).
		Where("channel_id NOT IN (?)", gdb.Model(&model.Channel{}).Select("id")).Count(&orphan).Error; err != nil {
		t.Fatalf("统计孤儿绑定失败: %v", err)
	}
	if orphan != 0 {
		t.Fatalf("不应产生孤儿绑定，实际 %d 条", orphan)
	}
}
