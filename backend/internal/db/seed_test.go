package db

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

const seedTestSchema = "ld_test_seed"

// withSeedDB 只迁移套餐与系统配置两张表：这里验证的是种子/默认值的语义，
// 不是整套启动流程（那由 main 的集成测试与手工验收覆盖）。
func withSeedDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + seedTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+seedTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Package{}, &model.SystemConfig{}, &model.App{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + seedTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// TestSeedPackagesOnlyWhenEmpty 空表写 4 档；重复调用不重复；删光后不复活。
func TestSeedPackagesOnlyWhenEmpty(t *testing.T) {
	gdb := withSeedDB(t)

	if err := seedPackages(gdb); err != nil {
		t.Fatalf("首次种子失败: %v", err)
	}
	var rows []model.Package
	if err := gdb.Order("sort_order").Find(&rows).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("应写入 4 档，实际 %d", len(rows))
	}
	names := []string{"体验包", "标准包", "专业包", "旗舰包"}
	for i, n := range names {
		if rows[i].Name != n {
			t.Fatalf("第 %d 档应为 %s，实际 %s", i+1, n, rows[i].Name)
		}
	}
	// 只有标准包带角标
	for _, r := range rows {
		want := ""
		if r.Name == "标准包" {
			want = "recommend"
		}
		if r.Badge != want {
			t.Fatalf("%s 的角标应为 %q，实际 %q", r.Name, want, r.Badge)
		}
		if !r.Enabled {
			t.Fatalf("%s 默认应启用", r.Name)
		}
	}
	// 赠送额必须是**整货币单位**：门户只展示两位小数，
	// 送 19.9999 元会让运营在页面上永远看到一串 9。
	for _, r := range rows {
		if r.Bonus <= 0 || r.Bonus%money.Scale != 0 {
			t.Fatalf("%s 的赠送额 %d 微不是整单位（应能被 %d 整除）", r.Name, r.Bonus, money.Scale)
		}
		if r.PriceCent <= 0 {
			t.Fatalf("%s 的售价必须为正，实际 %d 分", r.Name, r.PriceCent)
		}
	}

	// 二次调用不重复
	if err := seedPackages(gdb); err != nil {
		t.Fatalf("二次种子失败: %v", err)
	}
	var n int64
	_ = gdb.Model(&model.Package{}).Count(&n)
	if n != 4 {
		t.Fatalf("重复种子不应新增，实际 %d", n)
	}

	// 运营通过接口「删光」= 软删（Delete 走的是软删）。守卫按 Unscoped 计数，
	// 所以软删行仍然挡住种子 —— 历史订单还在引用这些套餐，不能让它们凭空复活。
	if err := gdb.Where("1 = 1").Delete(&model.Package{}).Error; err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	if res := gdb.Model(&model.Package{}).Count(&n); res.Error != nil || n != 0 {
		t.Fatalf("软删后默认 scope 应为 0，实际 %d（err=%v）", n, res.Error)
	}
	if err := seedPackages(gdb); err != nil {
		t.Fatalf("三次种子失败: %v", err)
	}
	_ = gdb.Unscoped().Model(&model.Package{}).Count(&n)
	if n != 4 {
		t.Fatalf("软删后不应复活，实际 %d 行", n)
	}

	// 真·空表（运维手工清空）才会重新写入：这是「全新部署」的唯一入口。
	if err := gdb.Exec("DELETE FROM packages").Error; err != nil {
		t.Fatalf("硬清空失败: %v", err)
	}
	if err := seedPackages(gdb); err != nil {
		t.Fatalf("四次种子失败: %v", err)
	}
	_ = gdb.Unscoped().Model(&model.Package{}).Count(&n)
	if n != 4 {
		t.Fatalf("空表应重新写入 4 档，实际 %d", n)
	}
}

// TestSeedBalanceLimitDefault 首装默认上限必须是 $1,000（否则旗舰套餐买不了）。
func TestSeedBalanceLimitDefault(t *testing.T) {
	gdb := withSeedDB(t)
	if err := seedSystemConfig(gdb); err != nil {
		t.Fatalf("种子失败: %v", err)
	}
	var row model.SystemConfig
	if err := gdb.Where("key = ?", "balance_limit").First(&row).Error; err != nil {
		t.Fatalf("读取 balance_limit 失败: %v", err)
	}
	var v int64
	if err := json.Unmarshal(row.Value, &v); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if want := int64(7200) * money.Scale; v != want {
		t.Fatalf("默认上限应为 %d 微（7200 货币单位），实际 %d", want, v)
	}
}

// TestSeedPackagesCurrencyAgnostic 套餐不携带币种：全站只有一个锁定币种。
//
// 旧实现给 packages 带 currency 列并「按站点币种现算额度」，那套设计的前提是
// 「同一套餐可能以不同币种售卖」。现在币种是站点级锁定值，套餐的 price_cent
// 天然就是该币种的金额 —— 再存一份 currency 只会出现「套餐标 USD、站点已切 CNY」
// 这种自相矛盾的行，门户按币种过滤后整排套餐静默消失。
//
// 因此本用例守的是**列不存在**这件事，而不是某个换算结果。
func TestSeedPackagesCurrencyAgnostic(t *testing.T) {
	gdb := withSeedDB(t)
	if err := seedPackages(gdb); err != nil {
		t.Fatalf("种子失败: %v", err)
	}
	if !gdb.Migrator().HasTable(&model.Package{}) {
		t.Fatal("packages 表应已创建")
	}
	for _, col := range []string{"currency", "base_quota", "credit_quota"} {
		if gdb.Migrator().HasColumn(&model.Package{}, col) {
			t.Fatalf("packages 不应再有 %q 列：币种是站点级唯一值，套餐只存 price_cent/bonus", col)
		}
	}
}

// TestSeedDefaultApp 守「开箱即用」与「不覆盖运营改动」两件事。
//
// 没有默认应用时，全新部署下托管密钥链路对任何接入方都不可达：
// GET /api/user/apps 恒空、门户没有应用入口、文档也只讲自建 KEY。
func TestSeedDefaultApp(t *testing.T) {
	gdb := withSeedDB(t)
	if err := seedDefaultApp(gdb); err != nil {
		t.Fatalf("首次种子失败: %v", err)
	}

	var app model.App
	if err := gdb.Where("slug = ?", defaultAppSlug).First(&app).Error; err != nil {
		t.Fatalf("应写入默认应用: %v", err)
	}
	if app.Status != model.AppStatusEnabled {
		t.Fatalf("默认应用应启用，实际 %d", app.Status)
	}
	// 开箱即用的前提：新用户能自助签发，否则还得先找管理员。
	if !app.AllowSelfIssue {
		t.Fatal("默认应用必须允许自助签发（否则新用户无法接入）")
	}
	if app.Name == "" {
		t.Fatal("默认应用必须有名称")
	}

	// 幂等：重复启动不新增。
	if err := seedDefaultApp(gdb); err != nil {
		t.Fatalf("二次种子失败: %v", err)
	}
	var n int64
	if err := gdb.Model(&model.App{}).Count(&n).Error; err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("重复种子不应新增，实际 %d 行", n)
	}

	// 运营改过的字段绝不能被重启覆盖（存在即跳过，不是 upsert）。
	if err := gdb.Model(&model.App{}).Where("slug = ?", defaultAppSlug).
		Updates(map[string]any{"name": "运营改过的名字", "allow_self_issue": false, "status": 2}).Error; err != nil {
		t.Fatalf("模拟运营改动失败: %v", err)
	}
	if err := seedDefaultApp(gdb); err != nil {
		t.Fatalf("三次种子失败: %v", err)
	}
	var after model.App
	if err := gdb.Where("slug = ?", defaultAppSlug).First(&after).Error; err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if after.Name != "运营改过的名字" || after.AllowSelfIssue || after.Status != 2 {
		t.Fatalf("种子不得覆盖运营改动，实际 name=%q allow_self_issue=%v status=%d",
			after.Name, after.AllowSelfIssue, after.Status)
	}
}

// TestSeedSystemConfigNoLegacyKeys 种子里不得再出现额度刻度/充值倍率这类旧键。
// 它们的危害不是"多一个键"，而是**读端仍会读**：只要有一个旧键存在，
// 就有人会照着它去调，而新计费完全忽略它 —— 改完毫无变化，且没有任何报错。
func TestSeedSystemConfigNoLegacyKeys(t *testing.T) {
	gdb := withSeedDB(t)
	if err := seedSystemConfig(gdb); err != nil {
		t.Fatalf("种子失败: %v", err)
	}
	var rows []model.SystemConfig
	if err := gdb.Find(&rows).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	have := make(map[string]bool, len(rows))
	for _, r := range rows {
		have[r.Key] = true
	}
	for _, dead := range []string{"recharge_ratio", "recharge_multiplier", "group_ratios"} {
		if have[dead] {
			t.Fatalf("种子不应再写入 %q（该概念已删除）", dead)
		}
	}
	for _, must := range []string{"group_discount", "cny_per_usd", "balance_limit", "min_topup"} {
		if !have[must] {
			t.Fatalf("种子必须写入 %q", must)
		}
	}
}
