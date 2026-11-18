package audit

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
)

const testSchema = "ld_test_s11_audit"

// withTestDB 在独立 schema 中准备 admin_audit/users 表（需 LD_TEST_DSN；未设置则跳过）。
func withTestDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.AdminAudit{}, &model.User{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// TestListTimeWindowJoinAlias 锁定审计列表的时间过滤：
// 条件曾以未限定列名拼进 `admin_audit a LEFT JOIN users u`，两张表都有 created_at，
// 触发 "column reference created_at is ambiguous"（42702）；修复后须限定 a. 前缀。
func TestListTimeWindowJoinAlias(t *testing.T) {
	gdb := withTestDB(t)
	// 操作人用户名缺失 → actor 回退 #id（同时验证 LEFT JOIN + COALESCE 回落）
	if err := gdb.Create(&model.User{
		Username: "", PasswordHash: "x", Group: "default", Role: 0, Status: 1,
	}).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	if err := gdb.Create(&model.AdminAudit{
		AdminID: 1, Action: "key.create", TargetType: "key", TargetID: "9",
		Detail: model.JSON(`{"name":"k1"}`),
	}).Error; err != nil {
		t.Fatalf("造审计失败: %v", err)
	}

	now := time.Now()
	items, total, err := List(context.Background(), Filter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, 1, 20)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("命中 total=%d rows=%d，期望 1/1", total, len(items))
	}
	if items[0].Actor != "#1" {
		t.Fatalf("actor=%q，期望 #1", items[0].Actor)
	}
	if items[0].Action != "key.create" || items[0].TargetID != "9" {
		t.Fatalf("行内容错误: %+v", items[0])
	}

	// 数据驱动筛选源：动作集合应含刚写入的动作
	stats, err := Actions(context.Background())
	if err != nil {
		t.Fatalf("动作集合失败: %v", err)
	}
	if len(stats) != 1 || stats[0].Action != "key.create" || stats[0].Value != 1 {
		t.Fatalf("动作集合错误: %+v", stats)
	}
}
