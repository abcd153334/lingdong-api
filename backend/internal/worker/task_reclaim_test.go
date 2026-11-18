package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/task"
)

const workerTestSchema = "ld_test_worker"

func workerTestDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + workerTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+workerTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}, &model.Usage{}, &model.Transaction{}, &model.Task{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + workerTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

func wiptr(v int64) *int64 { return &v }

// TestTaskReclaimReleasesOrphanFreeze 断言孤儿预扣被回收（资金回退 + 置 timeout），且重复执行不重复退款。
func TestTaskReclaimReleasesOrphanFreeze(t *testing.T) {
	gdb := workerTestDB(t)
	ctx := context.Background()

	u := &model.User{Username: "orphan1", PasswordHash: "x", Balance: 5000, Group: "default", AffCode: "o1"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	k := &model.APIKey{UserID: u.ID, KeyCipher: "x", KeyHash: "h1", Name: "k", Status: 1, Group: "default"}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}

	snap := billing.Snapshot{ModelID: "vid", BillingMode: model.BillingModeSpec, SpecPrice: model.SpecPrices{Kind: billing.SpecKindPerTask, PerTaskPrice: wiptr(1000)}}
	sess, err := billing.New(gdb).Freeze(ctx, billing.FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Group: "default", Snapshot: snap, Amount: 1000,
	})
	if err != nil {
		t.Fatalf("Freeze 失败: %v", err)
	}

	// 停在 queued、仍有冻结额、创建时间早于 TTL（模拟上游从未接手）。
	orphan := &model.Task{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Kind: task.KindVideo,
		Status: task.StatusQueued, Frozen: 1000, UsageID: sess.UsageID,
		PricingSnapshot: model.MustJSON(snap),
		CreatedAt:       time.Now().Add(-3 * time.Hour),
	}
	if err := gdb.Create(orphan).Error; err != nil {
		t.Fatalf("造孤儿任务失败: %v", err)
	}
	// 新鲜在途任务（未超 TTL，独立计费会话）不应被回收。
	sess2, err := billing.New(gdb).Freeze(ctx, billing.FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Group: "default", Snapshot: snap, Amount: 1000,
	})
	if err != nil {
		t.Fatalf("Freeze#2 失败: %v", err)
	}
	fresh := &model.Task{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Modality: "video", Kind: task.KindVideo,
		Status: task.StatusQueued, Frozen: 1000, UsageID: sess2.UsageID,
		PricingSnapshot: model.MustJSON(snap), CreatedAt: time.Now(),
	}
	if err := gdb.Create(fresh).Error; err != nil {
		t.Fatalf("造新鲜任务失败: %v", err)
	}

	job := NewTaskReclaimJob()
	if err := job.Run(ctx); err != nil {
		t.Fatalf("回收执行失败: %v", err)
	}

	var got, untouched model.Task
	if err := gdb.First(&got, orphan.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusFailed || got.Kind != task.KindTimeout {
		t.Fatalf("孤儿任务应置 failed/timeout，实际 status=%s kind=%s", got.Status, got.Kind)
	}
	if err := gdb.First(&untouched, fresh.ID).Error; err != nil {
		t.Fatal(err)
	}
	if untouched.Status != task.StatusQueued {
		t.Fatalf("未超 TTL 的在途任务不应被回收，实际 %s", untouched.Status)
	}

	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	// 两次冻结共 2000；仅孤儿（1000）被回退 → 4000 余额 / 1000 冻结。
	if after.Balance != 4000 || after.Frozen != 1000 {
		t.Fatalf("应仅回退孤儿预扣，实际 quota=%d frozen=%d", after.Balance, after.Frozen)
	}

	var releaseCount int64
	if err := gdb.Model(&model.Transaction{}).Where("type = ? AND user_id = ?", "release", u.ID).Count(&releaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if releaseCount != 1 {
		t.Fatalf("应恰好 1 条 release 流水，实际 %d", releaseCount)
	}

	// 重复执行：状态已 failed，不再命中，且不重复退款。
	if err := job.Run(ctx); err != nil {
		t.Fatalf("二次回收执行失败: %v", err)
	}
	var again model.User
	if err := gdb.First(&again, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if again.Balance != 4000 {
		t.Fatalf("重复执行不应重复退款，实际 balance=%d", again.Balance)
	}
}
