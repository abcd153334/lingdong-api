package configsync

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

const syncTestSchema = "ld_test_configsync"

// withSyncEnv 准备「Redis + system_config 表」环境（缺任一项则跳过）。
func withSyncEnv(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过配置同步集成用例")
	}
	if !db.RedisReady() {
		db.InitRedis("127.0.0.1:6379", "", 15)
		if !db.RedisReady() {
			t.Skip("未找到可用 Redis，跳过配置同步集成用例")
		}
	}

	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + syncTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+syncTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.SystemConfig{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + syncTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// resetSyncState 复位包级状态（用例之间互不影响）。
func resetSyncState(t *testing.T) {
	t.Helper()
	setLastSeen(time.Time{})
	SetPricingInvalidator(nil)
	startedMu.Lock()
	started = false
	startedMu.Unlock()
}

// TestPricingBroadcastInvalidatesLocalCache 验证 P1-8 的实时通道：
// 另一个实例广播「定价失效」后，本实例的定价缓存必须被清掉（钩子被调用）。
func TestPricingBroadcastInvalidatesLocalCache(t *testing.T) {
	withSyncEnv(t)
	resetSyncState(t)

	var hits int64
	SetPricingInvalidator(func() { atomic.AddInt64(&hits, 1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Start(ctx)
	time.Sleep(300 * time.Millisecond) // 等订阅建立

	NotifyPricing()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&hits) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("广播定价失效后本机未清缓存（钩子未被调用）")
}

// TestSystemConfigBroadcastReloadsMirror 验证配置变更能通过 pub/sub 同步到内存镜像。
func TestSystemConfigBroadcastReloadsMirror(t *testing.T) {
	gdb := withSyncEnv(t)
	resetSyncState(t)

	row := model.SystemConfig{Key: "site", Value: model.MustJSON(map[string]any{"site_name": "远端实例改的名字"})}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Start(ctx)
	time.Sleep(300 * time.Millisecond)

	conf.System().Reload(map[string]json.RawMessage{}) // 先清空，确认确实被重载回来
	NotifySystem([]string{"site"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, ok := conf.System().Get("site"); ok && strings.Contains(string(raw), "远端实例改的名字") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("配置广播后内存镜像未更新")
}

// TestPollFallbackDetectsExternalChange 验证兜底通道：
// 即使 pub/sub 消息丢了（这里故意只改库不广播），轮询也必须把变更捡回来。
func TestPollFallbackDetectsExternalChange(t *testing.T) {
	gdb := withSyncEnv(t)
	resetSyncState(t)

	old := pollInterval
	pollInterval = 100 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })

	if err := gdb.Create(&model.SystemConfig{Key: "site", Value: model.MustJSON(map[string]any{"site_name": "初始"})}).Error; err != nil {
		t.Fatalf("写入配置失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Start(ctx) // 记录基线

	// 模拟「另一个实例直接改库但广播丢了」：延迟一拍再更新，确保 updated_at 变新。
	time.Sleep(150 * time.Millisecond)
	if err := gdb.Model(&model.SystemConfig{}).Where("key = ?", "site").
		Update("value", model.MustJSON(map[string]any{"site_name": "外部直接改库"})).Error; err != nil {
		t.Fatalf("更新配置失败: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, ok := conf.System().Get("site"); ok && strings.Contains(string(raw), "外部直接改库") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("轮询兜底未把外部变更捡回来")
}
