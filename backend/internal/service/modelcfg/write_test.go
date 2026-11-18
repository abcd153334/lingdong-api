package modelcfg

import (
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	// 只为副作用：注册内置适配器，否则 AdapterSatisfiesModality 会把
	// 正常的 openai 渠道判成"不支持该模态"，用例失败原因与代码无关。
	_ "lingdong-api/internal/relay/adapters"
)

// 本文件覆盖 modelcfg 的**写库语义**（必须连库：唯一索引、软删墓碑、CAS）。
//
// 这些是人工保存与 AI apply 共用的路径，任何一条错都会同时污染两条链路。

const cfgTestSchema = "ld_test_modelcfg"

func withCfgDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + cfgTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + cfgTestSchema).Error; err != nil {
		t.Fatalf("建 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+cfgTestSchema), silent)
	if err != nil {
		t.Fatalf("连接 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(
		&model.Vendor{}, &model.Channel{}, &model.Model{}, &model.ModelBinding{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 与生产同一条唯一索引：软删行也占位，所以"先删后插"必须硬删。
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_model_channel
		ON model_bindings (model_id, channel_id)`).Error; err != nil {
		t.Fatalf("建绑定唯一索引失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + cfgTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

func seedVendorChannel(t *testing.T, gdb *gorm.DB) *model.Channel {
	t.Helper()
	if err := gdb.Create(&model.Vendor{ID: "v1", Name: "供应商"}).Error; err != nil {
		t.Fatalf("种子供应商失败: %v", err)
	}
	ch := &model.Channel{
		Name: "种子渠道", AdapterID: "openai", BaseURL: "https://api.example.com",
		Status: model.ChannelStatusEnabled, MultiKeyMode: "random",
		Weight: 5, Priority: 1, RPMLimit: 60, MaxConcurrency: 10,
	}
	if err := gdb.Create(ch).Error; err != nil {
		t.Fatalf("种子渠道失败: %v", err)
	}
	return ch
}

// TestCreateTxRebuildsAfterSoftDelete 守住"删掉的同名模型能建回来"。
func TestCreateTxRebuildsAfterSoftDelete(t *testing.T) {
	gdb := withCfgDB(t)
	seedVendorChannel(t, gdb)

	in := Input{ID: "m1", DisplayName: sp("M"), Vendor: "v1", Modality: "chat", BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices: model.Prices{Input: i64p(1)}}
	if _, err := CreateTx(gdb, in, nil); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	// 软删（墓碑仍占主键，这正是历史 7 次 500 的来源）。
	if err := gdb.Where("id = ?", "m1").Delete(&model.Model{}).Error; err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	m, err := CreateTx(gdb, in, nil)
	if err != nil {
		t.Fatalf("墓碑清理后应能重建，实际 %v", err)
	}
	if m.Version != 1 {
		t.Fatalf("新建模型版本应为 1，实际 %d", m.Version)
	}
	// 存活行再建必须被拒（不是覆盖）。
	if _, err := CreateTx(gdb, in, nil); err == nil {
		t.Fatal("同名存活模型应拒绝创建")
	}
}

// TestCreateTxRequiresBillingMode 守住"忘了填计费方式 ≠ 按量"。
func TestCreateTxRequiresBillingMode(t *testing.T) {
	gdb := withCfgDB(t)
	seedVendorChannel(t, gdb)
	_, err := CreateTx(gdb, Input{ID: "m2", Vendor: "v1", Modality: "audio", Prices: model.Prices{}}, nil)
	if err == nil {
		t.Fatal("未提交计费方式应被拒绝（音频按量 = 永久 0 元）")
	}
}

// TestReplaceBindingsTxHardDeletes 守住"替换绑定不会撞唯一索引"。
func TestReplaceBindingsTxHardDeletes(t *testing.T) {
	gdb := withCfgDB(t)
	ch := seedVendorChannel(t, gdb)
	in := Input{ID: "m3", DisplayName: sp("M"), Vendor: "v1", Modality: "chat", BillingMode: model.BillingModeTokens, BillingModeSet: true,
		Prices: model.Prices{Input: i64p(1)}}
	if _, err := CreateTx(gdb, in, nil); err != nil {
		t.Fatalf("建模型失败: %v", err)
	}
	b := []Binding{{ChannelID: ch.ID, UpstreamModel: "up", Weight: 1, Priority: 1, Enabled: true}}
	if err := ReplaceBindingsTx(gdb, "m3", b); err != nil {
		t.Fatalf("首次写入绑定失败: %v", err)
	}
	// 再写一次同样的集合：软删会留下占位行 → 23505。
	if err := ReplaceBindingsTx(gdb, "m3", b); err != nil {
		t.Fatalf("重复替换绑定时撞了唯一索引（说明用了软删）: %v", err)
	}
	var rows []model.ModelBinding
	if err := gdb.Where("model_id = ?", "m3").Find(&rows).Error; err != nil {
		t.Fatalf("查询绑定失败: %v", err)
	}
	if len(rows) != 1 || !rows[0].Enabled || rows[0].Weight != 1 {
		t.Fatalf("绑定集合不对（零值是否被 GORM 默认值吃掉？）: %+v", rows)
	}
}

// TestNormalizeBindingsRejects 覆盖绑定七条校验里最容易被漏掉的几条。
func TestNormalizeBindingsRejects(t *testing.T) {
	gdb := withCfgDB(t)
	ch := seedVendorChannel(t, gdb)

	disabled := &model.Channel{
		Name: "停用渠道", AdapterID: "openai", BaseURL: "https://api2.example.com",
		Status: model.ChannelStatusDisabled, MultiKeyMode: "random", Weight: 1, Priority: 1,
	}
	if err := gdb.Create(disabled).Error; err != nil {
		t.Fatalf("种子停用渠道失败: %v", err)
	}

	cases := []struct {
		name string
		item Binding
		want string
	}{
		{"缺上游模型", Binding{ChannelID: ch.ID, Weight: 1, Priority: 1, Enabled: true}, "上游模型名"},
		{"渠道不存在", Binding{ChannelID: 9999, UpstreamModel: "up", Weight: 1, Priority: 1, Enabled: true}, "不存在"},
		{"渠道已停用", Binding{ChannelID: disabled.ID, UpstreamModel: "up", Weight: 1, Priority: 1, Enabled: true}, "已停用"},
		{"权重越界", Binding{ChannelID: ch.ID, UpstreamModel: "up", Weight: 1001, Priority: 1, Enabled: true}, "权重与优先级"},
		{"优先级越界", Binding{ChannelID: ch.ID, UpstreamModel: "up", Weight: 1, Priority: 5000, Enabled: true}, "权重与优先级"},
	}
	for _, c := range cases {
		if _, err := NormalizeBindings(gdb, "chat", []Binding{c.item}); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s 应被拒绝且提示 %q，实际 %v", c.name, c.want, err)
		}
	}

	// 同一渠道重复出现：唯一索引会在写库时 23505，必须提前拦。
	if _, err := NormalizeBindings(gdb, "chat", []Binding{
		{ChannelID: ch.ID, UpstreamModel: "a", Weight: 1, Priority: 1, Enabled: true},
		{ChannelID: ch.ID, UpstreamModel: "b", Weight: 1, Priority: 1, Enabled: true},
	}); err == nil || !strings.Contains(err.Error(), "同一个渠道") {
		t.Fatalf("同渠道重复应被拒绝，实际 %v", err)
	}

	// 全部停用 = 模型不进目录，必须拦。
	if _, err := NormalizeBindings(gdb, "chat", []Binding{
		{ChannelID: ch.ID, UpstreamModel: "a", Weight: 1, Priority: 1, Enabled: false},
	}); err == nil {
		t.Fatal("没有任何启用绑定时应被拒绝")
	}
}

// TestFillMissingKeepsAgentLimits 锁死「只改价格的提交不能清掉窗口覆盖」。
//
// Updates() 是**全列写**：任何一次没带 context_window 的提交，若 FillMissing
// 不补库内值，就会把它写成 0（= 静默退回继承）。而"改价"是最常见的提交形态，
// 所以这个 bug 会天天发生，且界面上完全看不出来（运营填的 128000 悄悄没了）。
func TestFillMissingKeepsAgentLimits(t *testing.T) {
	cur := &model.Model{
		ID: "m1", Modality: "chat", BillingMode: model.BillingModeTokens,
		ContextWindow: 128_000, MaxOutputTokens: 4_096,
	}
	in := Input{ID: "m1", BillingMode: model.BillingModeTokens, BillingModeSet: true}
	FillMissing(&in, cur)
	if in.ContextWindow == nil || *in.ContextWindow != 128_000 {
		t.Fatalf("未提交的窗口应沿用库内值，实际 %v", in.ContextWindow)
	}
	if in.MaxOutputTokens == nil || *in.MaxOutputTokens != 4_096 {
		t.Fatalf("未提交的输出上限应沿用库内值，实际 %v", in.MaxOutputTokens)
	}
	// 显式 0 是"改回继承"，必须原样保留（不能被回填成库内值）。
	zero := 0
	in2 := Input{ID: "m1", BillingMode: model.BillingModeTokens, BillingModeSet: true, ContextWindow: &zero}
	FillMissing(&in2, cur)
	if in2.ContextWindow == nil || *in2.ContextWindow != 0 {
		t.Fatalf("显式 0 应保留为 0（= 继承），实际 %v", in2.ContextWindow)
	}
}
