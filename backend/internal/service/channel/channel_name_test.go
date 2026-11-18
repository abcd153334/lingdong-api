package channel

import (
	"errors"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

const channelTestSchema = "ld_test_channel_name"

// withChannelDB 提供 schema 隔离的 DB（未设置 LD_TEST_DSN 时跳过）。
func withChannelDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过渠道数据库集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + channelTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+channelTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + channelTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// channelReq 构造一个通过校验的渠道入参。
func channelReq(name string) Req {
	return Req{
		Name: name, AdapterID: "openai", BaseURL: "https://api.example.com",
		Keys: []string{"sk-test-1"}, MultiKeyMode: ModeRandom,
		Groups: []string{"default"}, Weight: 1, Priority: 1,
	}
}

// TestCreateRejectsDuplicateName 断言同名渠道被 409 语义拦下，且软删后名字可复用。
func TestCreateRejectsDuplicateName(t *testing.T) {
	gdb := withChannelDB(t)

	first, err := Create(channelReq("主渠道"), "")
	if err != nil {
		t.Fatalf("首次创建应成功：%v", err)
	}

	if _, err := Create(channelReq("主渠道"), ""); !errors.Is(err, ErrNameExists) {
		t.Fatalf("重名应返回 ErrNameExists，实际 %v", err)
	}
	if _, err := Create(channelReq("备用渠道"), ""); err != nil {
		t.Fatalf("不同名应可创建：%v", err)
	}

	// 软删后名字允许复用（Count 自动排除 deleted_at 非空行）
	if err := gdb.Delete(&model.Channel{}, first.ID).Error; err != nil {
		t.Fatalf("软删失败：%v", err)
	}
	if _, err := Create(channelReq("主渠道"), ""); err != nil {
		t.Fatalf("软删后应可复用名称：%v", err)
	}
}

// TestUpdateRejectsDuplicateName 断言改名撞车被拦，改回自身名称不受影响。
func TestUpdateRejectsDuplicateName(t *testing.T) {
	withChannelDB(t)

	a, err := Create(channelReq("渠道 A"), "")
	if err != nil {
		t.Fatalf("创建 A 失败：%v", err)
	}
	b, err := Create(channelReq("渠道 B"), "")
	if err != nil {
		t.Fatalf("创建 B 失败：%v", err)
	}

	// 把 B 改名成 A 的名称 → 拒绝
	req := channelReq("渠道 A")
	req.Keys = nil // 更新不要求密钥
	if _, err := Update(b.ID, req, ""); !errors.Is(err, ErrNameExists) {
		t.Fatalf("改名撞车应返回 ErrNameExists，实际 %v", err)
	}

	// B 保持原名更新 → 允许（排除自身）
	req = channelReq("渠道 B")
	req.Keys = nil
	if _, err := Update(b.ID, req, ""); err != nil {
		t.Fatalf("保持原名更新应成功：%v", err)
	}

	// A 保持原名更新 → 允许
	req = channelReq("渠道 A")
	req.Keys = nil
	if _, err := Update(a.ID, req, ""); err != nil {
		t.Fatalf("保持原名更新应成功：%v", err)
	}
}

// TestCreateKeepsZeroValueFields 是 GORM 默认值回填缺陷的回归用例：
// auto_ban 列 default:true、weight 列 default:1，创建时传 false/0 必须原样落库。
func TestCreateKeepsZeroValueFields(t *testing.T) {
	gdb := withChannelDB(t)

	req := channelReq("零值渠道")
	req.AutoBan = false
	req.Weight = 0
	item, err := Create(req, "")
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}

	var got model.Channel
	if err := gdb.First(&got, item.ID).Error; err != nil {
		t.Fatalf("读取渠道失败：%v", err)
	}
	if got.AutoBan {
		t.Fatal("auto_ban=false 被列默认值覆盖为 true（关闭自动熔断失效）")
	}
	if got.Weight != 0 {
		t.Fatalf("weight=0 被列默认值覆盖为 %d", got.Weight)
	}
}
