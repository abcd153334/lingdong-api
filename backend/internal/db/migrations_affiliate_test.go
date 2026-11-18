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
	"lingdong-api/internal/pkg/affcfg"
)

const affCfgTestSchema = "ld_test_aff_cfg"

func withAffCfgDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + affCfgTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + affCfgTestSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+affCfgTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.SystemConfig{}); err != nil {
		t.Fatalf("迁移 system_config 失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + affCfgTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// readAff 读取当前 aff 行的键值（值用 float64 承载，够判断「键在不在」与相等）。
func readAff(t *testing.T, gdb *gorm.DB) map[string]any {
	t.Helper()
	var row model.SystemConfig
	if err := gdb.Where(`"key" = ?`, "aff").First(&row).Error; err != nil {
		t.Fatalf("读 aff 行失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(row.Value, &out); err != nil {
		t.Fatalf("解析 aff 行失败: %v", err)
	}
	return out
}

func seedAffRow(t *testing.T, gdb *gorm.DB, raw string) {
	t.Helper()
	if err := gdb.Create(&model.SystemConfig{Key: "aff", Value: model.JSON(raw)}).Error; err != nil {
		t.Fatalf("写 aff 行失败: %v", err)
	}
}

// TestAffConfigMigrateNormalizesLegacyRow 旧结构整行必须被归一成出厂结构。
//
// 为什么必须有：`Seed` 用 OnConflict{DoNothing}，已存在的行不会被更新。
// 迁移没跑或判据写错时，新键读出来全是 0 —— 开关开着、一分不发，且不报错。
func TestAffConfigMigrateNormalizesLegacyRow(t *testing.T) {
	gdb := withAffCfgDB(t)
	seedAffRow(t, gdb, `{"enabled":true,"reward_for_inviter":10000000,"reward_for_invitee":5000000,"min_transfer":10000000}`)

	rep, err := MigrateAffiliateConfig(gdb)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Updated || rep.Skipped {
		t.Fatalf("旧结构应被归一: %+v", rep)
	}
	for _, k := range []string{"reward_for_inviter", "reward_for_invitee"} {
		if !affContains(rep.Removed, k) {
			t.Fatalf("旧键 %s 应被删除: %+v", k, rep.Removed)
		}
	}

	got := readAff(t, gdb)
	if _, stale := got["reward_for_inviter"]; stale {
		t.Fatalf("旧键必须被清掉（留着会误导排障）: %+v", got)
	}
	d := affcfg.Defaults()
	want := map[string]float64{
		"inviter_rate":     float64(d.InviterRate),
		"invitee_amount":   float64(d.InviteeAmount),
		"max_rewards":      float64(d.MaxRewards),
		"hold_hours":       float64(d.HoldHours),
		"min_transfer":     float64(d.MinTransfer),
		"daily_invite_cap": float64(d.DailyInviteCap),
	}
	for k, v := range want {
		n, ok := got[k].(float64)
		if !ok || n != v {
			t.Fatalf("键 %s 期望 %v，实际 %v（全量 %+v）", k, v, got[k], got)
		}
	}
	if enabled, _ := got["enabled"].(bool); !enabled {
		t.Fatalf("enabled 应保持开启: %+v", got)
	}

	// 幂等：第二次跑必须跳过
	rep2, err := MigrateAffiliateConfig(gdb)
	if err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
	if rep2.Updated || !rep2.Skipped {
		t.Fatalf("已收敛的行应跳过: %+v", rep2)
	}
}

// TestAffConfigMigrateDropsLegacyKeysOnMixedRow 混合行（旧键 + 新键）也必须清理。
//
// 这是上一版判据（「有没有 inviter_rate」）被真实数据证伪的现场：新版管理台
// 保存过一次后，行里两套键并存，旧判据命中便永远跳过 —— 旧键再也不会被清掉。
func TestAffConfigMigrateDropsLegacyKeysOnMixedRow(t *testing.T) {
	gdb := withAffCfgDB(t)
	d := affcfg.Defaults()
	seedAffRow(t, gdb, `{"enabled":true,"reward_for_inviter":1,"quota_for_invitee":2,`+
		`"inviter_rate":50000,"invitee_amount":1000000,"max_rewards":5,`+
		`"hold_hours":24,"min_transfer":2000000,"daily_invite_cap":5}`)

	rep, err := MigrateAffiliateConfig(gdb)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Updated {
		t.Fatalf("混合行应被归一: %+v", rep)
	}
	if len(rep.Removed) != 2 {
		t.Fatalf("应删掉 2 个旧键，实际 %+v", rep.Removed)
	}
	if len(rep.Added) != 0 || len(rep.Fixed) != 0 {
		t.Fatalf("新键都在域内，不该补/修: added=%v fixed=%v", rep.Added, rep.Fixed)
	}
	got := readAff(t, gdb)
	// 运营填的值必须保留（不是回退出厂值）
	if n, _ := got["inviter_rate"].(float64); n != 50000 {
		t.Fatalf("运营配置被覆盖: %+v", got)
	}
	_ = d
}

// TestAffConfigMigrateSkipsWhenMissing 没有 aff 行时（全新库）必须跳过，由 Seed 负责写入。
func TestAffConfigMigrateSkipsWhenMissing(t *testing.T) {
	gdb := withAffCfgDB(t)
	rep, err := MigrateAffiliateConfig(gdb)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Skipped || rep.Updated {
		t.Fatalf("无 aff 行时应跳过: %+v", rep)
	}
}

// TestAffConfigMigrateKeepsOperatorValues 已收敛时**不得**改写任何值（幂等读路径）。
func TestAffConfigMigrateKeepsOperatorValues(t *testing.T) {
	gdb := withAffCfgDB(t)
	seedAffRow(t, gdb, `{"enabled":true,"inviter_rate":50000,"invitee_amount":1000000,`+
		`"max_rewards":5,"hold_hours":24,"min_transfer":2000000,"daily_invite_cap":5}`)

	rep, err := MigrateAffiliateConfig(gdb)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !rep.Skipped {
		t.Fatalf("合法配置应跳过: %+v", rep)
	}
	got := readAff(t, gdb)
	if n, _ := got["inviter_rate"].(float64); n != 50000 {
		t.Fatalf("运营配置被覆盖: %+v", got)
	}
	if n, _ := got["hold_hours"].(float64); n != 24 {
		t.Fatalf("运营配置被覆盖: %+v", got)
	}
}

// TestAffConfigMigrateFixesOutOfRange 越界值回退**出厂值**（不是边界值）。
//
// 边界值会让「hold_hours=0（本意是关掉冷却）」变成 24 小时 —— 一次配置错误
// 被静默改写成「刚好够套利」的冷却期，比直接回退出厂值更难查。
func TestAffConfigMigrateFixesOutOfRange(t *testing.T) {
	gdb := withAffCfgDB(t)
	seedAffRow(t, gdb, `{"enabled":true,"inviter_rate":9000000,"invitee_amount":1000000,`+
		`"max_rewards":5,"hold_hours":0,"min_transfer":2000000,"daily_invite_cap":5}`)

	rep, err := MigrateAffiliateConfig(gdb)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !affContains(rep.Fixed, "inviter_rate") || !affContains(rep.Fixed, "hold_hours") {
		t.Fatalf("两个越界键都应被修: %+v", rep.Fixed)
	}
	d := affcfg.Defaults()
	got := readAff(t, gdb)
	if n, _ := got["inviter_rate"].(float64); n != float64(d.InviterRate) {
		t.Fatalf("越界比例应回退出厂值 %d，实际 %v", d.InviterRate, got["inviter_rate"])
	}
	if n, _ := got["hold_hours"].(float64); n != float64(d.HoldHours) {
		t.Fatalf("hold_hours=0 应回退出厂值 %d（不是边界 24），实际 %v", d.HoldHours, got["hold_hours"])
	}
}

func affContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
