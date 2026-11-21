package appref

// appref 的集成用例：三条"看上去对、实际会静默错"的点
//   1. 已软删的密钥仍必须解析出应用（历史日志/任务恰恰多半指向轮换掉的密钥）；
//   2. app_id=0 的密钥返回空 Ref，但**必须出现在 map 里**（与"查漏了"区分开）；
//   3. display_name 非空时优先于后台名 name。

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
)

const testSchema = "ld_test_appref"

// hashSeq 保证每把造出来的密钥 key_hash 唯一（该列有 uniqueIndex）。
var hashSeq atomic.Int64

func withDB(t *testing.T) *gorm.DB {
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
		t.Fatalf("创建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.App{}, &model.APIKey{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func mkApp(t *testing.T, gdb *gorm.DB, slug, name, display string, status int16) *model.App {
	t.Helper()
	a := &model.App{Slug: slug, Name: name, DisplayName: display, Status: status, AllowSelfIssue: true}
	if err := gdb.Create(a).Error; err != nil {
		t.Fatalf("造应用失败: %v", err)
	}
	return a
}

func mkKey(t *testing.T, gdb *gorm.DB, appID, userID uint64, kind string) *model.APIKey {
	t.Helper()
	k := &model.APIKey{
		AppID: appID, UserID: userID, Kind: kind, Name: "k",
		KeyHash: fmt.Sprintf("hash-%d-%d", userID, hashSeq.Add(1)),
	}
	if err := gdb.Create(k).Error; err != nil {
		t.Fatalf("造密钥失败: %v", err)
	}
	return k
}

// TestRefsOfKeysResolvesDeletedKey：已软删的密钥仍要解析出应用。
//
// 这条是防"历史日志应用列整片变空"的回归线：api_keys 有 gorm.DeletedAt，
// 一旦有人图省事改成 GORM 查询，这里立刻红。
func TestRefsOfKeysResolvesDeletedKey(t *testing.T) {
	gdb := withDB(t)
	app := mkApp(t, gdb, "portal", "官网门户", "", model.AppStatusEnabled)
	key := mkKey(t, gdb, app.ID, 7, model.KeyKindHosted)
	if err := gdb.Delete(&model.APIKey{}, key.ID).Error; err != nil {
		t.Fatalf("软删密钥失败: %v", err)
	}
	if err := gdb.First(&model.APIKey{}, key.ID).Error; err == nil {
		t.Fatal("前置条件不成立：密钥应当已被软删")
	}

	refs, err := RefsOfKeys(context.Background(), gdb, []uint64{key.ID})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got, ok := refs[key.ID]
	if !ok {
		t.Fatal("已软删密钥必须出现在结果里（否则历史行无法归因）")
	}
	if got.ID != app.ID || got.Name != "官网门户" {
		t.Fatalf("应用解析错误: %+v", got)
	}
}

// TestRefsOfKeysPlatformDirect：app_id=0 返回空 Ref，且必须在 map 里。
func TestRefsOfKeysPlatformDirect(t *testing.T) {
	gdb := withDB(t)
	key := mkKey(t, gdb, 0, 8, model.KeyKindUser)

	refs, err := RefsOfKeys(context.Background(), gdb, []uint64{key.ID})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got, ok := refs[key.ID]
	if !ok {
		t.Fatal("平台直连密钥也必须在结果里（与『查漏了』区分开）")
	}
	if got.ID != 0 || got.Name != "" || got.Slug != "" {
		t.Fatalf("平台直连应得空 Ref，实际 %+v", got)
	}
}

// TestNamesPrefersDisplayName：对外品牌名优先于后台名；停用应用也要能取到。
func TestNamesPrefersDisplayName(t *testing.T) {
	gdb := withDB(t)
	branded := mkApp(t, gdb, "portal", "web-portal", "官网门户", model.AppStatusEnabled)
	plain := mkApp(t, gdb, "shadow", "影画", "", model.AppStatusDisabled)

	refs, err := Names(context.Background(), gdb, []uint64{branded.ID, plain.ID, 999999})
	if err != nil {
		t.Fatalf("取名字失败: %v", err)
	}
	if refs[branded.ID].Name != "官网门户" {
		t.Fatalf("display_name 应优先，实际 %q", refs[branded.ID].Name)
	}
	if refs[plain.ID].Name != "影画" {
		t.Fatalf("display_name 为空应回落后台名，实际 %q", refs[plain.ID].Name)
	}
	if _, ok := refs[999999]; ok {
		t.Fatal("不存在的应用不该凭空出现")
	}
}

// TestDedupe：IN 列表去重且剔除 0（0 是「无应用」的哨兵值，不是合法主键）。
func TestDedupe(t *testing.T) {
	got := dedupe([]uint64{3, 0, 3, 5, 5})
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Fatalf("去重结果错误: %v", got)
	}
	if empty := dedupe(nil); len(empty) != 0 {
		t.Fatalf("空入参应返回空: %v", empty)
	}
}

// TestEmptyInputNoQuery：空入参绝不能碰数据库（这里传 nil gdb，碰了就 panic）。
func TestEmptyInputNoQuery(t *testing.T) {
	refs, err := RefsOfKeys(context.Background(), nil, nil)
	if err != nil || len(refs) != 0 {
		t.Fatalf("空入参应直接返回空 map: %v %v", refs, err)
	}
	names, err := Names(context.Background(), nil, []uint64{0})
	if err != nil || len(names) != 0 {
		t.Fatalf("全为 0 的入参应直接返回空 map: %v %v", names, err)
	}
}
