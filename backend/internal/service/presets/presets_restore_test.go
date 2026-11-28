package presets

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
)

// 本文件守的是「恢复出厂」与「老库认领」两条**破坏性/一次性**语义：
//
//  1. Restore 是唯一的覆盖入口：Seed 只补不改，运营改坏了只能靠它；
//  2. Restore 绝不碰密钥/余额/健康快照/主键 —— 覆盖配置 ≠ 重置凭据；
//  3. keep_enabled=true（默认）保留当前启停，只有勾选后才回到停用；
//  4. dry-run 计数必须与真跑一致（同一条代码路径 + 事务回滚，不是另写一套模拟）；
//  5. Adopt 只补行标记，不改业务字段；模型必须确实绑在该渠道上才认领。

// restoreTestDB 在共享测试基座上补迁移 data_migrations（Adopt 的幂等标记表）。
func restoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb := withPresetDB(t)
	if err := gdb.AutoMigrate(&model.DataMigration{}); err != nil {
		t.Fatalf("迁移 data_migrations 失败: %v", err)
	}
	return gdb
}

// channelOfGroup 取某组在库里的渠道行（恢复用例的唯一入口）。
func channelOfGroup(t *testing.T, gdb *gorm.DB, g presetGroup) model.Channel {
	t.Helper()
	var ch model.Channel
	if err := gdb.Where("preset_group = ?", g.RowGroup()).First(&ch).Error; err != nil {
		t.Fatalf("读取组 %s 的渠道失败: %v", g.RowGroup(), err)
	}
	return ch
}

func mustSeed(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	if err := Seed(gdb); err != nil {
		t.Fatalf("写入内置预设失败: %v", err)
	}
}

// ① dry-run：报出"将被复用"的计数，但一行都不改。
func TestRestoreDryRunReportsWithoutWriting(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	ch := channelOfGroup(t, gdb, g)
	if err := gdb.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Update("name", "被运营改过的名字").Error; err != nil {
		t.Fatalf("改名失败: %v", err)
	}

	res, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), DryRun: true, KeepEnabled: true})
	if err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	if len(res.Groups) != 1 || res.Groups[0].Channels.Reused != 1 {
		t.Fatalf("dry-run 应报告 1 条渠道被复用，实际 %+v", res.Groups)
	}
	if !res.DryRun {
		t.Fatal("dry_run 标志必须原样回传")
	}
	// 回滚后名称必须还是运营改过的那个值。
	if got := channelOfGroup(t, gdb, g); got.Name != "被运营改过的名字" {
		t.Fatalf("dry-run 不应写库，名称却被改成 %q", got.Name)
	}
}

// ② 真跑：名称/地址/官网回到出厂值，dry-run 与真跑计数一致。
func TestRestoreOverwritesChannelFacts(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	ch := channelOfGroup(t, gdb, g)
	if err := gdb.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Updates(map[string]any{"name": "改坏的名字", "base_url": "https://wrong.example.com", "website": ""}).Error; err != nil {
		t.Fatalf("改坏渠道失败: %v", err)
	}

	pre, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), DryRun: true, KeepEnabled: true})
	if err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	if _, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), KeepEnabled: true}); err != nil {
		t.Fatalf("恢复出厂失败: %v", err)
	}

	got := channelOfGroup(t, gdb, g)
	if got.Name != g.Channel.Name || got.BaseURL != g.Channel.BaseURL || got.Website != g.Channel.Website {
		t.Fatalf("渠道事实未回到出厂值：name=%q base=%q website=%q", got.Name, got.BaseURL, got.Website)
	}
	// 计数一致：dry-run 不是另一套推算逻辑。
	if pre.Groups[0].Channels != (RestoreCounts{Reused: 1}) {
		t.Fatalf("dry-run 计数与预期不符：%+v", pre.Groups[0].Channels)
	}
}

// ③ 默认保留启停：运营手动启用的渠道与模型，恢复配置后仍是启用态。
func TestRestoreKeepsEnabledByDefault(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	ch := channelOfGroup(t, gdb, g)
	if err := gdb.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Update("status", model.ChannelStatusEnabled).Error; err != nil {
		t.Fatalf("启用渠道失败: %v", err)
	}
	m := g.Models[0]
	if err := gdb.Model(&model.Model{}).Where("id = ?", m.ID).Update("enabled", true).Error; err != nil {
		t.Fatalf("启用模型失败: %v", err)
	}

	if _, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), KeepEnabled: true}); err != nil {
		t.Fatalf("恢复出厂失败: %v", err)
	}
	if got := channelOfGroup(t, gdb, g); got.Status != model.ChannelStatusEnabled {
		t.Fatalf("keep_enabled=true 却把渠道状态改成了 %d", got.Status)
	}
	var row model.Model
	if err := gdb.Where("id = ?", m.ID).First(&row).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if !row.Enabled {
		t.Fatal("keep_enabled=true 却把模型停用了")
	}
}

// ④ 勾选 keep_enabled=false：渠道/模型/绑定全部回到停用（出厂态）。
func TestRestoreCanDisableEverything(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	ch := channelOfGroup(t, gdb, g)
	if err := gdb.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Update("status", model.ChannelStatusEnabled).Error; err != nil {
		t.Fatalf("启用渠道失败: %v", err)
	}
	if err := gdb.Model(&model.ModelBinding{}).Where("channel_id = ?", ch.ID).
		Update("enabled", true).Error; err != nil {
		t.Fatalf("启用绑定失败: %v", err)
	}

	if _, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), KeepEnabled: false}); err != nil {
		t.Fatalf("恢复出厂失败: %v", err)
	}
	if got := channelOfGroup(t, gdb, g); got.Status != model.ChannelStatusDisabled {
		t.Fatalf("keep_enabled=false 时渠道应停用，实际 %d", got.Status)
	}
	var bindings []model.ModelBinding
	if err := gdb.Where("channel_id = ?", ch.ID).Find(&bindings).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	for _, b := range bindings {
		if b.Enabled {
			t.Fatalf("keep_enabled=false 时绑定 %s 仍启用", b.ModelID)
		}
	}
}

// ⑤ 密钥/余额/健康快照绝不被覆盖（覆盖配置 ≠ 重置凭据）。
func TestRestorePreservesSecretsAndBalance(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	ch := channelOfGroup(t, gdb, g)
	const cipher = `["enc:v1:AAAA"]`
	balance := 12.34
	if err := gdb.Model(&model.Channel{}).Where("id = ?", ch.ID).
		Updates(map[string]any{"keys": cipher, "balance_cny": balance}).Error; err != nil {
		t.Fatalf("写入密钥/余额失败: %v", err)
	}

	if _, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), KeepEnabled: false}); err != nil {
		t.Fatalf("恢复出厂失败: %v", err)
	}
	got := channelOfGroup(t, gdb, g)
	if string(got.Keys) != cipher {
		t.Fatalf("恢复出厂覆盖了密钥池：%s", string(got.Keys))
	}
	if got.BalanceCNY == nil || *got.BalanceCNY != balance {
		t.Fatalf("恢复出厂覆盖了余额：%v", got.BalanceCNY)
	}
}

// ⑥ 未知组必须报错（不能让 API 静默成功）。
func TestRestoreRejectsUnknownGroup(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	if _, err := Restore(gdb, RestoreOptions{Group: "no-such-group", DryRun: true}); !errors.Is(err, ErrUnknownPresetGroup) {
		t.Fatalf("应返回 ErrUnknownPresetGroup，实际 %v", err)
	}
}

// ⑥b 开关关掉时恢复出厂必须拒绝：否则「启动不写、点一下又写」会有两种事实。
func TestRestoreRefusedWhenPresetsDisabled(t *testing.T) {
	gdb := restoreTestDB(t)
	// 显式关闭（默认就是关，这里写死是为了不让默认值变化悄悄改变本用例语义）。
	t.Cleanup(func() { conf.Load() })
	t.Setenv("LD_BUILTIN_PRESETS", "0")
	conf.Load()

	if _, err := Restore(gdb, RestoreOptions{DryRun: true}); !errors.Is(err, ErrPresetsDisabled) {
		t.Fatalf("应返回 ErrPresetsDisabled，实际 %v", err)
	}
}

// ⑦ 运营删掉的模型：恢复出厂要把它（和绑定）复活，且仍归本组。
func TestRestoreRevivesSoftDeletedModel(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	m := g.Models[0]
	if err := gdb.Where("id = ?", m.ID).Delete(&model.Model{}).Error; err != nil {
		t.Fatalf("软删模型失败: %v", err)
	}

	if _, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), KeepEnabled: true}); err != nil {
		t.Fatalf("恢复出厂失败: %v", err)
	}
	var row model.Model
	if err := gdb.Where("id = ?", m.ID).First(&row).Error; err != nil {
		t.Fatalf("模型未被复活: %v", err)
	}
	if row.PresetGroup != g.RowGroup() {
		t.Fatalf("复活后内置标记应为 %s，实际 %q", g.RowGroup(), row.PresetGroup)
	}
	var n int64
	if err := gdb.Model(&model.ModelBinding{}).Where("model_id = ?", m.ID).Count(&n).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if n == 0 {
		t.Fatal("复活模型后绑定应重建")
	}
}

// ⑧ 运营自建的模型（无内置标记）：恢复出厂不碰它、也不给它建绑定。
func TestRestoreSkipsOperatorOwnedModel(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	m := g.Models[0]
	if err := gdb.Model(&model.Model{}).Where("id = ?", m.ID).
		Updates(map[string]any{"preset_group": "", "display_name": "运营自己起的名字"}).Error; err != nil {
		t.Fatalf("清除内置标记失败: %v", err)
	}

	res, err := Restore(gdb, RestoreOptions{Group: g.RowGroup(), KeepEnabled: true})
	if err != nil {
		t.Fatalf("恢复出厂失败: %v", err)
	}
	if res.Groups[0].Models.Skipped == 0 {
		t.Fatal("未认领的模型应被计入 skipped")
	}
	var row model.Model
	if err := gdb.Where("id = ?", m.ID).First(&row).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if row.DisplayName != "运营自己起的名字" {
		t.Fatalf("恢复出厂改了未认领模型的展示名：%q", row.DisplayName)
	}
}

// ⑨ 老库认领：标记全空时 Adopt 补回组标记，且只补标记（名称/密钥/状态不变）。
func TestAdoptClaimsLegacyRows(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	// 模拟升级前的老库：行上没有 preset_group。
	if err := gdb.Model(&model.Channel{}).Where("1 = 1").
		Update("preset_group", "").Error; err != nil {
		t.Fatalf("清空渠道标记失败: %v", err)
	}
	if err := gdb.Model(&model.Model{}).Where("1 = 1").
		Update("preset_group", "").Error; err != nil {
		t.Fatalf("清空模型标记失败: %v", err)
	}
	if err := Adopt(gdb); err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	var claimed int64
	if err := gdb.Model(&model.Channel{}).Where("preset_group <> ''").Count(&claimed).Error; err != nil {
		t.Fatalf("统计渠道标记失败: %v", err)
	}
	if claimed != int64(len(presetGroups())) {
		t.Fatalf("应认领 %d 条渠道，实际 %d", len(presetGroups()), claimed)
	}
	var modelsClaimed int64
	if err := gdb.Model(&model.Model{}).Where("preset_group <> ''").Count(&modelsClaimed).Error; err != nil {
		t.Fatalf("统计模型标记失败: %v", err)
	}
	if modelsClaimed == 0 {
		t.Fatal("模型也应被认领")
	}

	// 二次调用不应重复做事（标记表命中直接返回）。
	if err := Adopt(gdb); err != nil {
		t.Fatalf("第二次认领应静默返回，实际 %v", err)
	}
}

// ⑩ 认领的边界：模型没绑在这条渠道上就不能被标成内置。
func TestAdoptIgnoresUnboundModel(t *testing.T) {
	gdb := restoreTestDB(t)
	enablePresets(t)
	mustSeed(t, gdb)

	g := presetGroups()[0]
	m := g.Models[0]
	if err := gdb.Where("model_id = ?", m.ID).Delete(&model.ModelBinding{}).Error; err != nil {
		t.Fatalf("删除绑定失败: %v", err)
	}
	if err := gdb.Model(&model.Channel{}).Where("1 = 1").Update("preset_group", "").Error; err != nil {
		t.Fatalf("清空渠道标记失败: %v", err)
	}
	if err := gdb.Model(&model.Model{}).Where("1 = 1").Update("preset_group", "").Error; err != nil {
		t.Fatalf("清空模型标记失败: %v", err)
	}

	if err := Adopt(gdb); err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	var row model.Model
	if err := gdb.Where("id = ?", m.ID).First(&row).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if row.PresetGroup != "" {
		t.Fatalf("未被绑定到本渠道的模型不该被认领，实际标记 %q", row.PresetGroup)
	}
}
