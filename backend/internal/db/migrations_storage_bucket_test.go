package db

// M39（对象位置补 storage_bucket）的回填与幂等性。

import (
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

const bucketMigTestSchema = "ld_test_m39"

func withBucketMigDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + bucketMigTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + bucketMigTestSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+bucketMigTestSchema), silent)
	if err != nil {
		t.Fatalf("连接 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.SystemConfig{}, &model.Upload{}, &model.File{}, &model.Icon{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + bucketMigTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func setStorageCfg(t *testing.T, gdb *gorm.DB, raw string) {
	t.Helper()
	if err := gdb.Exec(`DELETE FROM system_config WHERE key = 'storage'`).Error; err != nil {
		t.Fatalf("清理配置失败: %v", err)
	}
	if err := gdb.Create(&model.SystemConfig{Key: "storage", Value: model.JSON(raw)}).Error; err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
}

// TestMigrateStorageBucket 覆盖回填的三条规则与幂等性。
func TestMigrateStorageBucket(t *testing.T) {
	gdb := withBucketMigDB(t)
	setStorageCfg(t, gdb, `{"active":"oss","channels":{"oss":{"bucket":"bucket-b"}}}`)

	now := time.Now()
	rows := []model.Upload{
		// 当前后端 + 空桶 → 回填 B
		{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", UserID: 1, StorageBackend: "oss", StorageKey: "files/1/a", CreatedAt: now, ExpiresAt: now},
		// 已记录桶 → 不动
		{ID: "01BBBBBBBBBBBBBBBBBBBBBBBB", UserID: 1, StorageBackend: "oss", StorageBucket: "bucket-a", StorageKey: "files/1/b", CreatedAt: now, ExpiresAt: now},
		// 本地后端 → 不动（本地没有桶概念）
		{ID: "01CCCCCCCCCCCCCCCCCCCCCCCC", UserID: 1, StorageBackend: "none", StorageKey: "files/1/c", CreatedAt: now, ExpiresAt: now},
	}
	for i := range rows {
		if err := gdb.Create(&rows[i]).Error; err != nil {
			t.Fatalf("造行失败: %v", err)
		}
	}

	rep, err := MigrateStorageBucket(gdb, false)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if rep.Skipped || rep.Bucket != "bucket-b" {
		t.Fatalf("应回填 bucket-b，实际 %+v", rep)
	}
	if rep.ByTable["uploads"] != 1 {
		t.Fatalf("uploads 应回填 1 行，实际 %+v", rep.ByTable)
	}
	if rep.LeftEmpty != 1 {
		t.Fatalf("本地行的空桶是合法的，但仍应被计数（LeftEmpty=1），实际 %d", rep.LeftEmpty)
	}

	var got []model.Upload
	if err := gdb.Order("id").Find(&got).Error; err != nil {
		t.Fatalf("回查失败: %v", err)
	}
	if got[0].StorageBucket != "bucket-b" {
		t.Fatalf("当前后端的空桶行应回填，实际 %q", got[0].StorageBucket)
	}
	if got[1].StorageBucket != "bucket-a" {
		t.Fatalf("已有桶不得被覆盖，实际 %q", got[1].StorageBucket)
	}
	if got[2].StorageBucket != "" {
		t.Fatalf("本地行不应被回填，实际 %q", got[2].StorageBucket)
	}

	// 幂等：再跑一次不改任何行。
	rep2, err := MigrateStorageBucket(gdb, false)
	if err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
	if rep2.ByTable["uploads"] != 0 || rep2.ByTable["files"] != 0 {
		t.Fatalf("二次迁移应 0 行，实际 %+v", rep2.ByTable)
	}
}

// TestMigrateStorageBucketLocalSkipped 守住「local 后端不需要桶」。
func TestMigrateStorageBucketLocalSkipped(t *testing.T) {
	gdb := withBucketMigDB(t)
	setStorageCfg(t, gdb, `{"active":"none","channels":{"none":{}}}`)

	now := time.Now()
	if err := gdb.Create(&model.Upload{
		ID: "01DDDDDDDDDDDDDDDDDDDDDDDD", UserID: 1, StorageBackend: "none",
		StorageKey: "files/1/d", CreatedAt: now, ExpiresAt: now,
	}).Error; err != nil {
		t.Fatalf("造行失败: %v", err)
	}
	rep, err := MigrateStorageBucket(gdb, false)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Skipped {
		t.Fatalf("active=none 应跳过回填，实际 %+v", rep)
	}
}

// TestMigrateStorageBucketDryRun 守住「只看不写」。
func TestMigrateStorageBucketDryRun(t *testing.T) {
	gdb := withBucketMigDB(t)
	setStorageCfg(t, gdb, `{"active":"oss","channels":{"oss":{"bucket":"bucket-b"}}}`)

	now := time.Now()
	row := model.Upload{ID: "01EEEEEEEEEEEEEEEEEEEEEEEE", UserID: 1, StorageBackend: "oss", StorageKey: "k", CreatedAt: now, ExpiresAt: now}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatalf("造行失败: %v", err)
	}
	rep, err := MigrateStorageBucket(gdb, true)
	if err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	if rep.ByTable["uploads"] != 1 {
		t.Fatalf("dry-run 应报告待回填 1 行，实际 %+v", rep.ByTable)
	}
	var back model.Upload
	if err := gdb.Where("id = ?", row.ID).Take(&back).Error; err != nil {
		t.Fatalf("回查失败: %v", err)
	}
	if back.StorageBucket != "" {
		t.Fatalf("dry-run 不得写库，实际 %q", back.StorageBucket)
	}
}
