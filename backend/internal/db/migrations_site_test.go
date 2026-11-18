package db

// M40（site 形状迁移）的幂等性与等值搬运。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

const siteMigTestSchema = "ld_test_m40"

func withSiteMigDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + siteMigTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+siteMigTestSchema), silent)
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
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + siteMigTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func putSite(t *testing.T, gdb *gorm.DB, raw string) {
	t.Helper()
	row := model.SystemConfig{Key: "site", Value: model.MustJSON(json.RawMessage(raw))}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatalf("写入 site 行失败: %v", err)
	}
}

func readSiteRow(t *testing.T, gdb *gorm.DB) map[string]any {
	t.Helper()
	var row model.SystemConfig
	if err := gdb.Where("key = ?", "site").First(&row).Error; err != nil {
		t.Fatalf("读取 site 行失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(row.Value, &m); err != nil {
		t.Fatalf("解析 site 失败: %v", err)
	}
	return m
}

func TestMigrateSiteConfig(t *testing.T) {
	gdb := withSiteMigDB(t)

	// 旧形状：单串地址（带尾斜杠）+ 废弃的 logo_text
	putSite(t, gdb, `{"site_name":"站点","api_base_url":"https://api.example.com/","logo_text":"灵","site_url":"https://www.example.com"}`)

	rep, err := MigrateSiteConfig(gdb, false)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if rep.ConvertedRows != 1 || rep.StrippedRows != 1 {
		t.Fatalf("行数不符: %+v", rep)
	}

	m := readSiteRow(t, gdb)
	if _, ok := m["api_base_url"]; ok {
		t.Fatalf("旧键必须删除: %v", m)
	}
	if _, ok := m["logo_text"]; ok {
		t.Fatalf("logo_text 必须删除: %v", m)
	}
	bases, _ := m["api_base_urls"].([]any)
	if len(bases) != 1 || bases[0] != "https://api.example.com" {
		t.Fatalf("数组化结果不符（应去尾斜杠）: %v", m["api_base_urls"])
	}
	// 其它字段必须原样保留
	if m["site_url"] != "https://www.example.com" || m["site_name"] != "站点" {
		t.Fatalf("迁移不得改动其它字段: %v", m)
	}

	// 幂等：连跑第二次不应再改任何行
	rep2, err := MigrateSiteConfig(gdb, false)
	if err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
	if rep2.ConvertedRows != 0 || rep2.StrippedRows != 0 {
		t.Fatalf("二次迁移应无事可做: %+v", rep2)
	}
}

func TestMigrateSiteConfigEmptyBase(t *testing.T) {
	gdb := withSiteMigDB(t)
	// 空串要搬成 []，不能搬成 [""]（后者会让前端渲染出一行空输入）
	putSite(t, gdb, `{"site_name":"站点","api_base_url":"","logo_text":"灵"}`)
	if _, err := MigrateSiteConfig(gdb, false); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	m := readSiteRow(t, gdb)
	bases, ok := m["api_base_urls"].([]any)
	if !ok || len(bases) != 0 {
		t.Fatalf("空地址应搬成空数组: %v", m["api_base_urls"])
	}
}

func TestMigrateSiteConfigKeepsExistingArray(t *testing.T) {
	gdb := withSiteMigDB(t)
	// 已经迁移过的行（新形状）不得被旧键覆盖
	putSite(t, gdb, `{"site_name":"站点","api_base_urls":["https://new.example.com"],"api_base_url":"https://old.example.com"}`)
	rep, err := MigrateSiteConfig(gdb, false)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if rep.ConvertedRows != 0 {
		t.Fatalf("已有数组时不应再转换: %+v", rep)
	}
	m := readSiteRow(t, gdb)
	bases, _ := m["api_base_urls"].([]any)
	if len(bases) != 1 || bases[0] != "https://new.example.com" {
		t.Fatalf("数组不得被旧键覆盖: %v", m["api_base_urls"])
	}
	if _, ok := m["api_base_url"]; ok {
		t.Fatalf("残留的旧键必须清掉: %v", m)
	}
}

func TestMigrateSiteConfigNoRow(t *testing.T) {
	gdb := withSiteMigDB(t)
	// 全新库（尚未 seed）时不得 panic / 报错
	if _, err := MigrateSiteConfig(gdb, true); err != nil {
		t.Fatalf("dry-run 不应报错: %v", err)
	}
	if _, err := MigrateSiteConfig(gdb, false); err != nil {
		t.Fatalf("空库不应报错: %v", err)
	}
}
