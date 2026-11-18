package db

// M38（uploads 形状迁移）的幂等性与等值搬运。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/uploadcfg"
)

const uploadsMigTestSchema = "ld_test_m38"

func withUploadsMigDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + uploadsMigTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+uploadsMigTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.SystemConfig{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + uploadsMigTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// readUploads 读回当前 uploads 配置。
func readUploads(t *testing.T, gdb *gorm.DB) map[string]json.RawMessage {
	t.Helper()
	var raw string
	if err := gdb.Raw(`SELECT value::text FROM system_config WHERE key = 'uploads'`).Scan(&raw).Error; err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	if raw == "" {
		t.Fatal("配置行不存在")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("配置不是合法 JSON: %v", err)
	}
	return m
}

// TestMigrateUploadsConfigKeepsOperatorValue 旧全局上限必须搬进图片类，而不是被重置。
func TestMigrateUploadsConfigKeepsOperatorValue(t *testing.T) {
	gdb := withUploadsMigDB(t)
	if err := gdb.Exec(`INSERT INTO system_config(key, value, updated_at) VALUES ('uploads', ?, now())`,
		`{"enabled":true,"max_size_mb":37,"daily_limit_per_key":50,"retention_hours":12,"user_total_mb":100,"concurrency":2}`).Error; err != nil {
		t.Fatalf("写旧配置失败: %v", err)
	}

	rep, err := MigrateUploadsConfig(gdb, false)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Migrated || rep.OldMaxMB != 37 {
		t.Fatalf("迁移报告不符: %+v", rep)
	}

	m := readUploads(t, gdb)
	if _, stale := m["max_size_mb"]; stale {
		t.Fatal("max_size_mb 必须被删除（否则体积有两套口径）")
	}
	for _, k := range []string{"limits", "types", "compress"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺少新字段 %s", k)
		}
	}

	var s uploadcfg.Settings
	blob, _ := json.Marshal(m)
	if err := json.Unmarshal(blob, &s); err != nil {
		t.Fatalf("新配置无法解析: %v", err)
	}
	if s.Limits[uploadcfg.KindImage] != 37 {
		t.Fatalf("图片类上限应沿用旧的 37MB，实际 %d", s.Limits[uploadcfg.KindImage])
	}
	// 其余运维改过的四项必须原样保留。
	if s.DailyLimitPerKey != 50 || s.RetentionHours != 12 || s.UserTotalMB != 100 || s.Concurrency != 2 {
		t.Fatalf("运维已调的值被覆盖: %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("迁移结果必须合法: %v", err)
	}
}

// TestMigrateUploadsConfigIsIdempotent 第二次必须空转，不改写。
func TestMigrateUploadsConfigIsIdempotent(t *testing.T) {
	gdb := withUploadsMigDB(t)
	if err := gdb.Exec(`INSERT INTO system_config(key, value, updated_at) VALUES ('uploads', ?, now())`,
		`{"max_size_mb":10}`).Error; err != nil {
		t.Fatalf("写旧配置失败: %v", err)
	}
	if _, err := MigrateUploadsConfig(gdb, false); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	first := readUploads(t, gdb)

	rep, err := MigrateUploadsConfig(gdb, false)
	if err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
	if rep.Migrated {
		t.Fatal("已是新形状时不应再改写")
	}
	second := readUploads(t, gdb)
	b1, _ := json.Marshal(first)
	b2, _ := json.Marshal(second)
	if string(b1) != string(b2) {
		t.Fatalf("二次迁移改变了配置:\n%s\n%s", b1, b2)
	}
}

// TestMigrateUploadsConfigDryRun 试运行时只报告不写库。
func TestMigrateUploadsConfigDryRun(t *testing.T) {
	gdb := withUploadsMigDB(t)
	const old = `{"max_size_mb":10}`
	if err := gdb.Exec(`INSERT INTO system_config(key, value, updated_at) VALUES ('uploads', ?, now())`, old).Error; err != nil {
		t.Fatalf("写旧配置失败: %v", err)
	}
	rep, err := MigrateUploadsConfig(gdb, true)
	if err != nil || !rep.Migrated {
		t.Fatalf("试运行应报告将迁移: rep=%+v err=%v", rep, err)
	}
	if m := readUploads(t, gdb); len(m["max_size_mb"]) == 0 {
		t.Fatal("试运行不得写库")
	}
}
