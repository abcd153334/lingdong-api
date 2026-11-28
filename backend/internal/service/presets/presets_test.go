package presets

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/billing"
)

// 本文件守的是内置预设的**三条语义**（少任何一条都会让运营踩坑）：
//  1. 写入即停用：模型 / 渠道 / 绑定全部不可用，等运营填自己的密钥；
//  2. 只写一次：第二次启动不重复插入；
//  3. 删除不复活：运营删掉的模型不会被下一次启动塞回来。
//
// 第 3 条是最容易写错、也最招人烦的一条：逐行 OnConflict{DoNothing} 只防重复插入，
// 挡不住"删掉又长回来"。
//
// 断言**不写死行数**：预设组会继续增加（当前只有火山方舟，将来可能加别的**官方厂商**），
// 写死 7/1 的代价是每加一组就要改 4 个数字，改漏一个就是"测试绿但语义错"。
// 所有计数都从 presetGroups() 派生。

const presetSchema = "ld_test_presets"

func withPresetDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + presetSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+presetSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Vendor{}, &model.Model{}, &model.Channel{},
		&model.ModelBinding{}, &model.SystemConfig{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + presetSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

func enablePresets(t *testing.T) {
	t.Helper()
	// 先注册复原再改环境：t.Cleanup 是 LIFO，先注册的**最后**执行，
	// 那时 t.Setenv 已把环境变量还原，重新 Load 才是真正的复原值。
	t.Cleanup(func() { conf.Load() })
	t.Setenv("LD_BUILTIN_PRESETS", "1")
	conf.Load()
}

// seedModelCount / seedChannelCount 从预设声明派生期望行数（不写死数字）。
func seedModelCount() int {
	// 模型 ID 是全局主键：同一 ID 可以出现在两组（如 duomi 与方舟都提供 seedance），
	// 后一组只补绑定、不再建行。计数必须去重，否则会把"共享模型"算成两行。
	seen := map[string]bool{}
	for _, g := range presetGroups() {
		for _, m := range g.Models {
			seen[m.ID] = true
		}
	}
	return len(seen)
}

func seedChannelCount() int { return len(presetGroups()) }

// ① 写入内容与启用态：全部停用、密钥为空、绑定齐全。
func TestSeedWritesDisabledPresets(t *testing.T) {
	gdb := withPresetDB(t)
	enablePresets(t)

	if err := Seed(gdb); err != nil {
		t.Fatalf("写入内置预设失败: %v", err)
	}

	var models []model.Model
	if err := gdb.Find(&models).Error; err != nil {
		t.Fatalf("读取模型失败: %v", err)
	}
	if len(models) != seedModelCount() {
		t.Fatalf("应写入 %d 个模型，实际 %d", seedModelCount(), len(models))
	}
	// 计费方式按声明逐条核对（现网导出含 tokens/call/spec 三种）。
	modeByID := map[string]string{}
	for _, g := range presetGroups() {
		for _, s := range g.Models {
			mode := s.BillingMode
			if mode == "" {
				mode = model.BillingModeSpec
			}
			modeByID[s.ID] = mode
		}
	}
	for _, m := range models {
		if m.Enabled {
			t.Fatalf("模型 %s 必须默认停用（预置 ≠ 开通）", m.ID)
		}
		want, ok := modeByID[m.ID]
		if !ok {
			t.Fatalf("模型 %s 不在任何预设声明里", m.ID)
		}
		if m.BillingMode != want {
			t.Fatalf("模型 %s 计费方式应为 %q，实际 %q", m.ID, want, m.BillingMode)
		}
		// 能力可以合法为空（纯对话模型没有额外能力），但不能是 NULL ——
		// NULL 表示"没声明过"，文档侧栏会少 TAB。
		if string(m.Capabilities) == "null" {
			t.Fatalf("模型 %s 缺能力声明（NULL）：文档侧栏会少 TAB 且无报错", m.ID)
		}
		if want == model.BillingModeSpec && m.SpecPrices.Kind == "" {
			t.Fatalf("模型 %s 缺价目结构：保存期校验会直接拒", m.ID)
		}
	}

	// 逐组断言渠道与绑定（新加一组自动被覆盖到）。
	for _, g := range presetGroups() {
		var ch model.Channel
		// 按行标记找渠道：同 adapter 可能有多条（Metaso 标准/优先），按 adapter 取首条会串组。
		if err := gdb.Where("preset_group = ?", g.RowGroup()).First(&ch).Error; err != nil {
			t.Fatalf("未写入 %s 组的内置渠道: %v", g.MarkerKey, err)
		}
		if ch.Status != model.ChannelStatusDisabled {
			t.Fatalf("%s 组的内置渠道必须为「人工禁用」，实际 status=%d", g.MarkerKey, ch.Status)
		}
		if ch.BaseURL != g.Channel.BaseURL {
			t.Fatalf("%s 组的 BaseURL 应为 %q，实际 %q", g.MarkerKey, g.Channel.BaseURL, ch.BaseURL)
		}
		if strings.Contains(string(ch.Keys), "ak-") || strings.Contains(string(ch.Keys), "sk-") {
			t.Fatalf("%s 组不得预置任何密钥（假密钥会让探活永远红）：%s", g.MarkerKey, ch.Keys)
		}
		// 出厂图标必须指向真实存在的内置 key：预设层同样绕过 brandicon.Normalize 直接写库，
		// 写错 key 的表现是「门户上这条渠道永远显示首字」，且没有任何日志。
		if ch.Icon != "" {
			if _, err := brandicon.Normalize(ch.Icon); err != nil {
				t.Fatalf("%s 组内置渠道图标 %q 不合法: %v", g.MarkerKey, ch.Icon, err)
			}
		}

		if g.Vendor.ID != "" {
			var vendor model.Vendor
			if err := gdb.Where("id = ?", g.Vendor.ID).First(&vendor).Error; err != nil {
				t.Fatalf("%s 组未写入供应商 %s: %v", g.MarkerKey, g.Vendor.ID, err)
			}
			if vendor.Icon != "" {
				if _, err := brandicon.Normalize(vendor.Icon); err != nil {
					t.Fatalf("%s 组供应商图标 %q 不合法: %v", g.MarkerKey, vendor.Icon, err)
				}
			}
		}

		var bindings []model.ModelBinding
		if err := gdb.Where("channel_id = ?", ch.ID).Find(&bindings).Error; err != nil {
			t.Fatalf("读取 %s 组绑定失败: %v", g.MarkerKey, err)
		}
		if len(bindings) != len(g.Models) {
			t.Fatalf("%s 组应有 %d 条绑定，实际 %d", g.MarkerKey, len(g.Models), len(bindings))
		}
		for _, b := range bindings {
			if b.Enabled {
				t.Fatalf("绑定 %s 必须默认停用", b.ModelID)
			}
		}
	}
}

// ② 只写一次：重复调用不重复插入，也不改任何状态。
func TestSeedIsIdempotent(t *testing.T) {
	gdb := withPresetDB(t)
	enablePresets(t)

	for i := 0; i < 3; i++ {
		if err := Seed(gdb); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i+1, err)
		}
	}
	var models, channels int64
	if err := gdb.Model(&model.Model{}).Count(&models).Error; err != nil {
		t.Fatalf("统计模型失败: %v", err)
	}
	if err := gdb.Model(&model.Channel{}).Count(&channels).Error; err != nil {
		t.Fatalf("统计渠道失败: %v", err)
	}
	if int(models) != seedModelCount() || int(channels) != seedChannelCount() {
		t.Fatalf("重复写入不该增加行：models=%d（期望 %d） channels=%d（期望 %d）",
			models, seedModelCount(), channels, seedChannelCount())
	}
}

// ③ 删除不复活：运营删掉一个模型后，下次启动不得把它塞回来（连绑定一起）。
func TestSeedDoesNotResurrect(t *testing.T) {
	gdb := withPresetDB(t)
	enablePresets(t)

	if err := Seed(gdb); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	const gone = "doubao-seedance-2-0-mini-260615"
	if err := gdb.Unscoped().Where("model_id = ?", gone).Delete(&model.ModelBinding{}).Error; err != nil {
		t.Fatalf("删除绑定失败: %v", err)
	}
	if err := gdb.Unscoped().Where("id = ?", gone).Delete(&model.Model{}).Error; err != nil {
		t.Fatalf("删除模型失败: %v", err)
	}

	if err := Seed(gdb); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	var n int64
	if err := gdb.Unscoped().Model(&model.Model{}).Where("id = ?", gone).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("运营删除的模型被复活了（%s）：删除是明确意图，重启不该覆盖", gone)
	}
}

// ⑤ 不夺权：运营先自建了同 id 的模型（已启用在跑），内置预设不得覆盖它。
//
// 这是「一次性写入」之外的第二道保护：标记只防"重写"，防不住"首次写入时
// 撞上运营已有的同 id 模型"。若这里覆盖，运营的模型会被改成停用 + 换成占位价目，
// 而现象只是"某天开始这个模型调不通了"。
func TestSeedDoesNotHijackExistingModel(t *testing.T) {
	gdb := withPresetDB(t)
	enablePresets(t)

	const mine = "doubao-seedream-5-0-260128"
	// 用 map 造行：enabled 带 default:true，结构体 Create 表达不了 false/true 的真值组合。
	if err := gdb.Table("models").Create(map[string]any{
		"id": mine, "display_name": "运营自建", "vendor": "bytedance", "modality": "image",
		"billing_mode": model.BillingModeCall, "enabled": true, "is_free": false,
		"version": 3, "created_at": time.Now(), "updated_at": time.Now(),
	}).Error; err != nil {
		t.Fatalf("构造运营自建模型失败: %v", err)
	}

	if err := Seed(gdb); err != nil {
		t.Fatalf("写入内置预设失败: %v", err)
	}

	var got model.Model
	if err := gdb.Where("id = ?", mine).First(&got).Error; err != nil {
		t.Fatalf("读取现有模型失败: %v", err)
	}
	if !got.Enabled {
		t.Fatal("运营自建的模型被内置预设停用了：启用态必须原样保留")
	}
	if got.DisplayName != "运营自建" {
		t.Fatalf("运营自建的模型被改名成 %q", got.DisplayName)
	}
	if got.BillingMode != model.BillingModeCall {
		t.Fatalf("运营自建的模型被改成 %q 计价", got.BillingMode)
	}

	var bindings []model.ModelBinding
	if err := gdb.Where("model_id = ?", mine).Find(&bindings).Error; err != nil {
		t.Fatalf("读取绑定失败: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("运营自建的模型被绑到内置渠道上了（%d 条）：等于把它挪到一条空密钥渠道", len(bindings))
	}

	var total int64
	if err := gdb.Model(&model.Model{}).Count(&total).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	// 「N-1 条新增 + 1 条既有」== 声明的模型总数。
	if int(total) != seedModelCount() {
		t.Fatalf("应是「%d 条新增 + 1 条既有」，实际 %d", seedModelCount()-1, total)
	}
}

// ④ 开关关掉时一行都不写（LD_BUILTIN_PRESETS=0）。
func TestSeedRespectsFlag(t *testing.T) {
	gdb := withPresetDB(t)
	t.Setenv("LD_BUILTIN_PRESETS", "0")
	conf.Load()
	// 复原：Env 是进程级单例，不清掉会把开关泄漏给同包其它用例。
	t.Cleanup(func() { conf.Load() })

	if err := Seed(gdb); err != nil {
		t.Fatalf("关闭开关时不应报错: %v", err)
	}
	var n int64
	if err := gdb.Model(&model.Model{}).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("开关关闭时不该写入任何模型，实际 %d", n)
	}
}

// 运营已有同适配器的渠道（在跑流量）：预设必须**复用**它，一个字段都不许改，
// 新增绑定必须是停用的，且**运营自己的绑定不能被关掉**。
//
// 最后一条是本次重构修掉的隐患：旧代码按 channel_id 全量置停用，复用场景下会把
// 运营自己启用的绑定一起关掉，现象是"某天开始模型全调不通"。
func TestSeedReusesExistingChannel(t *testing.T) {
	gdb := withPresetDB(t)
	enablePresets(t)

	if err := gdb.Table("channels").Create(map[string]any{
		"name": "我的方舟", "adapter_id": arkAdapterID,
		"base_url": "https://ark.cn-beijing.volces.com/api/v3", "keys": `["ark-live-key"]`,
		"multi_key_mode": "polling", "groups": `["vip"]`,
		"weight": 7, "priority": 3, "max_concurrency": 8, "rpm_limit": 60,
		"status": model.ChannelStatusEnabled, "auto_ban": true, "version": 1,
		"created_at": time.Now(), "updated_at": time.Now(),
	}).Error; err != nil {
		t.Fatalf("构造运营自建渠道失败: %v", err)
	}
	var ch model.Channel
	if err := gdb.Where("adapter_id = ?", arkAdapterID).First(&ch).Error; err != nil {
		t.Fatalf("读取自建渠道失败: %v", err)
	}

	// 运营自己的模型与**启用中**的绑定：复用渠道时绝不能被预设的"置停用"波及。
	const mine = "ops-own-model"
	if err := gdb.Table("models").Create(map[string]any{
		"id": mine, "display_name": "运营自有", "vendor": "bytedance", "modality": "video",
		"billing_mode": model.BillingModeSpec, "enabled": true, "is_free": false,
		"version": 1, "created_at": time.Now(), "updated_at": time.Now(),
	}).Error; err != nil {
		t.Fatalf("构造运营自有模型失败: %v", err)
	}
	if err := gdb.Table("model_bindings").Create(map[string]any{
		"model_id": mine, "channel_id": ch.ID, "weight": 10, "priority": 1,
		"enabled": true, "created_at": time.Now(), "updated_at": time.Now(),
	}).Error; err != nil {
		t.Fatalf("构造运营启用中绑定失败: %v", err)
	}

	if err := Seed(gdb); err != nil {
		t.Fatalf("写入内置预设失败: %v", err)
	}

	if err := gdb.Where("adapter_id = ?", arkAdapterID).First(&ch).Error; err != nil {
		t.Fatalf("读取渠道失败: %v", err)
	}
	if ch.Name != "我的方舟" || ch.Status != model.ChannelStatusEnabled {
		t.Fatalf("运营渠道被改动了：name=%q status=%d", ch.Name, ch.Status)
	}
	if string(ch.Keys) != `["ark-live-key"]` {
		t.Fatalf("运营渠道的密钥被改动了：%s", ch.Keys)
	}
	if ch.MaxConcurrency != 8 || ch.RPMLimit != 60 || ch.Weight != 7 {
		t.Fatalf("运营渠道的并发/限流/权重被改动了：%d/%d/%d", ch.MaxConcurrency, ch.RPMLimit, ch.Weight)
	}

	var channels int64
	if err := gdb.Model(&model.Channel{}).Count(&channels).Error; err != nil {
		t.Fatalf("统计渠道失败: %v", err)
	}
	if int(channels) != seedChannelCount() {
		t.Fatalf("不该新建第二条同适配器渠道：channels=%d 期望 %d", channels, seedChannelCount())
	}

	var own model.ModelBinding
	if err := gdb.Where("model_id = ? AND channel_id = ?", mine, ch.ID).First(&own).Error; err != nil {
		t.Fatalf("读取运营绑定失败: %v", err)
	}
	if !own.Enabled {
		t.Fatal("运营的在跑绑定被预设关掉了：置停用的作用域必须限定到本次插入的模型")
	}

	var bindings []model.ModelBinding
	if err := gdb.Where("channel_id = ? AND model_id <> ?", ch.ID, mine).Find(&bindings).Error; err != nil {
		t.Fatalf("读取预设绑定失败: %v", err)
	}
	if len(bindings) != len(arkModelSeeds()) {
		t.Fatalf("应有 %d 条预设绑定，实际 %d", len(arkModelSeeds()), len(bindings))
	}
	for _, b := range bindings {
		if b.Enabled {
			t.Fatalf("新增绑定 %s 必须停用", b.ModelID)
		}
	}
}

// 渠道名被别的适配器占用时：只写模型行，不建绑定，且 **Seed 不报错**
// （预设写入失败会让启动 fatal —— 一个名字冲突不该把服务拖死）。
func TestSeedSkipsChannelOnNameConflict(t *testing.T) {
	gdb := withPresetDB(t)
	enablePresets(t)

	if err := gdb.Table("channels").Create(map[string]any{
		"name": arkChannelName, "adapter_id": "openai",
		"base_url": "https://example.com", "keys": `[]`, "multi_key_mode": "polling",
		"groups": `["default"]`, "status": model.ChannelStatusEnabled, "version": 1,
		"created_at": time.Now(), "updated_at": time.Now(),
	}).Error; err != nil {
		t.Fatalf("构造同名异构渠道失败: %v", err)
	}

	if err := Seed(gdb); err != nil {
		t.Fatalf("名称冲突不该让 Seed 失败（启动会 fatal）：%v", err)
	}

	var n int64
	if err := gdb.Model(&model.Channel{}).Where("adapter_id = ?", arkAdapterID).Count(&n).Error; err != nil {
		t.Fatalf("统计方舟渠道失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("名称冲突时不该新建方舟渠道，实际 %d 条", n)
	}

	seeds := arkModelSeeds()
	var m model.Model
	if err := gdb.Where("id = ?", seeds[0].ID).First(&m).Error; err != nil {
		t.Fatalf("模型行仍应写入: %v", err)
	}

	// 只统计"本组模型"的绑定：其它组会正常建绑定，全局计数会误判。
	var bindings int64
	if err := gdb.Model(&model.ModelBinding{}).Where("model_id = ?", seeds[0].ID).Count(&bindings).Error; err != nil {
		t.Fatalf("统计绑定失败: %v", err)
	}
	if bindings != 0 {
		t.Fatalf("没有可用渠道时不该建绑定（会把模型挂到异构适配器上），实际 %d 条", bindings)
	}
}

// TestPresetGroupsAreWellFormed 锁住“一组预设的声明本身”合法。
//
// 这些错误都不会在编译期暴露：MarkerKey 重复 = 后一组永远不写入；
// 模型 ID 跨组重复 = 两组互相顶掉（第二组拿到 taken 直接跳过，绑定也建不出来）。
func TestPresetGroupsAreWellFormed(t *testing.T) {
	markers := map[string]bool{}
	modelIDs := map[string]bool{}
	for _, g := range presetGroups() {
		if g.MarkerKey == "" {
			t.Fatal("预设组缺 MarkerKey")
		}
		if markers[g.MarkerKey] {
			t.Fatalf("MarkerKey 重复：%s（共用标记会让后一组永远不写入）", g.MarkerKey)
		}
		markers[g.MarkerKey] = true
		if g.Version < 1 {
			t.Fatalf("%s：Version 必须 >= 1", g.MarkerKey)
		}
		if len(g.Models) == 0 {
			t.Fatalf("%s：预设组没有模型", g.MarkerKey)
		}
		if g.Channel.AdapterID == "" || g.Channel.Name == "" || g.Channel.BaseURL == "" {
			t.Fatalf("%s：渠道声明不完整", g.MarkerKey)
		}
		if !strings.Contains(g.Channel.BaseURL, "://") {
			t.Fatalf("%s：BaseURL 不是绝对地址：%q", g.MarkerKey, g.Channel.BaseURL)
		}
		for _, m := range g.Models {
			// 同一模型 ID 允许跨组（全局模型可挂多条渠道）：先写入的组拥有价目，
			// 后一组只补绑定。这里只要求声明本身完整。
			modelIDs[m.ID] = true
			if m.ID == "" || m.DisplayName == "" || m.Modality == "" {
				t.Fatalf("%s：模型声明不完整：%+v", g.MarkerKey, m)
			}
			if len(m.Endpoints) == 0 {
				t.Fatalf("%s/%s：缺端点声明（文档与路由都靠它）", g.MarkerKey, m.ID)
			}
			// 纯对话/嵌入模型可以没有额外能力；媒体模型必须有（决定文档 TAB）。
			if m.Modality != "chat" && m.Modality != "embedding" && len(m.Caps) == 0 {
				t.Fatalf("%s/%s：缺能力声明（文档侧栏会少 TAB）", g.MarkerKey, m.ID)
			}
		}
	}
}

// TestPresetSeedsPriceInvariants 锁定内置价目的**结构不变量**（纯函数，不连库）。
//
// 这些不变量错一条就是一类错账，而且都不会报错：
//   - 兜底价 ≠ 默认档价 → 不传 size 的请求按另一档收（lite 曾按 4K 收 2K 的单）；
//   - 视频没配 resolution_fixed → 不传 resolution 的请求在 resolveVideoTier 直接 400
//     （Ark 官方默认 720p，用户照文档不传分辨率就提交不了）；
//   - 图层拆分的 max_n ≠ 冻结张数 → 冻结 17 > max_n 直接 ErrInvalidTier（功能不可用），
//     或冻结 1 张、结算 17 张被截顶（少收 16 张）。
func TestPresetSeedsPriceInvariants(t *testing.T) {
	for _, g := range presetGroups() {
		for _, s := range g.Models {
			mode := s.BillingMode
			if mode != "" && mode != model.BillingModeSpec {
				continue // tokens/call 不走规格价目
			}
			sp := s.Spec
			// 现网导出的模型可能没有固定档（调用方必须显式传分辨率）；
			// 代码里策展的方舟组仍强制要求，避免"照文档不传分辨率就 400"。
			strictTier := g.MarkerKey == arkPresetMarkerKey
			switch s.Modality {
			case "image":
				if sp.Kind != "per_image" {
					t.Fatalf("%s/%s：图像模型 kind 应为 per_image，实际 %q", g.MarkerKey, s.ID, sp.Kind)
				}
				// 默认档必须是**价目里存在的键**（保存期硬校验同一条）。
				if sp.ResolutionFixed == "" {
					t.Fatalf("%s/%s：缺默认档", g.MarkerKey, s.ID)
				}
				if _, ok := sp.TierPrice[sp.ResolutionFixed]; !ok {
					t.Fatalf("%s/%s：默认档 %s 不在档位集合 %v 里",
						g.MarkerKey, s.ID, sp.ResolutionFixed, tierKeysOf(sp.TierPrice))
				}
			case "video":
				if sp.Kind != "per_video" {
					t.Fatalf("%s/%s：视频模型 kind 应为 per_video，实际 %q", g.MarkerKey, s.ID, sp.Kind)
				}
				if strictTier && sp.ResolutionFixed == "" {
					t.Fatalf("%s/%s：必须配 resolution_fixed（否则不传分辨率的请求直接 400）", g.MarkerKey, s.ID)
				}
				if sp.ResolutionFixed != "" {
					if _, ok := sp.SecondPrice[sp.ResolutionFixed]; !ok {
						t.Fatalf("%s/%s：固定档 %s 没有秒价（冻结必然失败）", g.MarkerKey, s.ID, sp.ResolutionFixed)
					}
				}
				if sp.MaxSeconds == nil || *sp.MaxSeconds < 1 {
					t.Fatalf("%s/%s：必须配 max_seconds（冻结与结算上限的唯一依据）", g.MarkerKey, s.ID)
				}
				// 指针非 nil = 「显式声明过」，比"必须为 true"更本质：
				// 上游不认 -1 的模型必须显式写 false，否则没人知道这是漏配还是有意为之。
				if strictTier && sp.AllowAutoDuration == nil {
					t.Fatalf("%s/%s：必须显式声明 allow_auto_duration", g.MarkerKey, s.ID)
				}
			}
		}
	}
}

// TestArkSeedsAllowAutoDuration 按上游能力断言智能时长开关。
//
// Ark 的模型都接受 seconds:-1；**不认 -1 的上游必须显式写 false** ——
// 声明成 true 会让平台把 -1 当成"交给上游决定"，而下游必然 400。
func TestArkSeedsAllowAutoDuration(t *testing.T) {
	for _, s := range arkModelSeeds() {
		if s.Modality != "video" {
			continue
		}
		if s.Spec.AllowAutoDuration == nil {
			t.Fatalf("%s：allow_auto_duration 未声明", s.ID)
		}
		if !*s.Spec.AllowAutoDuration {
			t.Fatalf("%s：Ark 视频模型接受 seconds:-1，应声明 true", s.ID)
		}
	}
}

// TestArkSeedsTierSets 锁定三个 Ark 图像模型的档位集合（最容易"看着对却写错"的一处）。
//
// pro / flash 是 1K / 1.5K / 2K，lite 是 2K / 3K / 4K。给 lite 加 1K 或给 pro 加 4K
// 都不会在保存时报错（档位本身合法），只会在用户真的用那一档时被上游 400 ——
// 而那时平台已经冻结成功，于是变成"反复冻结-退款"。
func TestArkSeedsTierSets(t *testing.T) {
	want := map[string][]string{
		"doubao-seedream-5-0-pro-260628":   {"1K", "1.5K", "2K"},
		"doubao-seedream-5-0-flash-260915": {"1K", "1.5K", "2K"},
		"doubao-seedream-5-0-260128":       {"2K", "3K", "4K"},
	}
	seen := 0
	for _, s := range arkModelSeeds() {
		expect, ok := want[s.ID]
		if !ok {
			continue
		}
		seen++
		got := tierKeysOf(s.Spec.TierPrice)
		if strings.Join(got, ",") != strings.Join(expect, ",") {
			t.Fatalf("%s 档位集合 = %v，期望 %v", s.ID, got, expect)
		}
		// 声明了图层拆分的模型，max_n 必须容得下 1 底图 + 16 图层。
		// 只对声明了该能力的模型断言：lite 没有图层拆分，它的 15 是组图张数上限。
		if hasCap(s.Caps, apidoc.CapImageLayer) {
			if s.Spec.MaxN == nil || *s.Spec.MaxN != billing.ImageLayerDecompMax {
				t.Fatalf("%s：声明了图层拆分，max_n 应为 %d，实际 %s",
					s.ID, billing.ImageLayerDecompMax, intPtrText(s.Spec.MaxN))
			}
		} else if s.Spec.MaxN != nil && *s.Spec.MaxN >= billing.ImageLayerDecompMax {
			t.Fatalf("%s：没有图层拆分能力，max_n 不该放到 %d（那是一次调用 17 张的收费上限）",
				s.ID, *s.Spec.MaxN)
		}
	}
	if seen != len(want) {
		t.Fatalf("只找到 %d 个图像模型，期望 %d（模型改名/删除时这段守卫要同步）", seen, len(want))
	}
}

// tierKeysOf 按**价目档位的规范顺序**返回键。
//
// 不用 lexical 排序：字符串序会把 1.5K 排在 1K 前面（'.' < 'K'），断言失败时的
// 实际值读起来反直觉，人很容易把正确配置看成错的。
func tierKeysOf(tiers map[string]int64) []string {
	rank := make(map[string]int, len(billing.ImageTierKeys)+len(billing.VideoTierKeys))
	for i, k := range billing.ImageTierKeys {
		rank[k] = i
	}
	for i, k := range billing.VideoTierKeys {
		rank[k] = i
	}
	out := make([]string, 0, len(tiers))
	for k := range tiers {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, iok := rank[out[i]]
		rj, jok := rank[out[j]]
		if iok && jok {
			return ri < rj
		}
		if iok != jok {
			return iok
		}
		return out[i] < out[j]
	})
	return out
}

// hasCap 判断能力清单里是否声明了某项能力。
func hasCap(caps []apidoc.Cap, want apidoc.Cap) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// intPtrText 把 *int64 打成可读文本（直接 %v 会打出地址，断言失败时看不出值）。
func intPtrText(v *int64) string {
	if v == nil {
		return "未配置"
	}
	return strconv.FormatInt(*v, 10)
}
