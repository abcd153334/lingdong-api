package docdocs

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件覆盖「能力补齐」的**库语义**：CAS 只写 NULL、不覆盖人工值、幂等。
// 这三件事都无法用内存替身验证（CAS 是靠 WHERE 条件在库里生效的）。
const docdocsTestSchema = "ld_test_docdocs"

func withDocDocsDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("DROP SCHEMA IF EXISTS " + docdocsTestSchema + " CASCADE").Error; err != nil {
		t.Fatalf("清理测试 schema 失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + docdocsTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+docdocsTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	// 文档站全链路用例共用这个 schema：模型事实 + 绑定 + 渠道（在售判定需要 JOIN 渠道状态）
	// + v7 的九张内容表。
	// adapter_specs 也必须在：播种时按「模型绑定的渠道是不是 spec:」读规格声明的
	// 扩展字段（v7_specfields.go），缺表就是整站缺参数。
	if err := gdb.AutoMigrate(
		&model.Model{}, &model.ModelBinding{}, &model.Channel{}, &model.Vendor{},
		&model.AdapterSpec{},
		&model.DocSection{}, &model.DocNode{}, &model.DocEndpoint{},
		&model.DocEndpointField{}, &model.DocFieldEnum{}, &model.DocResponseVariant{},
		&model.DocPreset{}, &model.DocEndpointError{},
	); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	// 在售口径包含「绑定渠道必须启用」：种子绑定写的是 channel_id=1，
	// 所以必须真有一行启用的渠道，否则所有模型都不在售，Reconcile 什么也收不到。
	if err := gdb.Create(&model.Channel{
		ID: 1, Name: "测试渠道", AdapterID: "openai",
		BaseURL: "https://api.example.com", Status: model.ChannelStatusEnabled,
	}).Error; err != nil {
		t.Fatalf("种子渠道失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + docdocsTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// seedModel 建一行模型（capabilities 直接落原始 JSON，nil = NULL）。
func seedModel(t *testing.T, gdb *gorm.DB, id, modality string, endpoints []string, specPrices map[string]any, caps model.JSON) {
	t.Helper()
	m := model.Model{
		ID:          id,
		Modality:    modality,
		BillingMode: model.BillingModeSpec,
		Enabled:     true,
	}
	if len(endpoints) > 0 {
		m.Endpoints = model.MustJSON(endpoints)
	}
	if specPrices != nil {
		// 走严格解码器：键名写错会立刻炸，而不是留下一份"看起来有价、实际读不到"的行。
		sp, err := model.DecodeSpecPrices(model.MustJSON(specPrices))
		if err != nil {
			t.Fatalf("测试价目不合法: %v", err)
		}
		m.SpecPrices = sp
	}
	if caps != nil {
		m.Capabilities = caps
	}
	if err := gdb.Create(&m).Error; err != nil {
		t.Fatalf("种子模型 %s 失败: %v", id, err)
	}
}

func capsOf(t *testing.T, gdb *gorm.DB, id string) (string, bool) {
	t.Helper()
	var row struct {
		Caps *string
	}
	if err := gdb.Raw("SELECT capabilities::text AS caps FROM models WHERE id = ?", id).Scan(&row).Error; err != nil {
		t.Fatalf("读取 %s 的能力失败: %v", id, err)
	}
	if row.Caps == nil {
		return "", true // NULL
	}
	return *row.Caps, false
}

func TestEnsureCapabilitiesFillsNullRows(t *testing.T) {
	gdb := withDocDocsDB(t)

	// 1. 图像模型：显式声明 generations + edits → 推荐 t2i + i2i。
	seedModel(t, gdb, "img-both", "image",
		[]string{"images/generations", "images/edits"},
		map[string]any{"kind": "per_image", "tier_price": map[string]any{"1K": 100}},
		nil)
	// 2. 图像模型：只声明 generations → 只推荐 t2i（不能因模态默认端点含 edits 就多给）。
	seedModel(t, gdb, "img-gen", "image",
		[]string{"images/generations"},
		map[string]any{"kind": "per_image", "tier_price": map[string]any{"1K": 100}},
		nil)
	// 3. 图像模型：端点留空 → 只给最小兜底 t2i（绝不按默认端点推 i2i）。
	seedModel(t, gdb, "img-empty", "image", nil,
		map[string]any{"kind": "per_image", "tier_price": map[string]any{"1K": 100}},
		nil)
	// 4. 视频模型：配了参考图价 + 参考视频按秒 → t2v + i2v + ref_video。
	seedModel(t, gdb, "vid-ref", "video",
		[]string{"video/generations"},
		map[string]any{
			"kind":                   "per_video",
			"second_price":           map[string]any{"720p": 9000},
			"ref_image_price":        1000,
			"ref_video_second_price": map[string]any{"720p": 500},
		},
		nil)
	// 5. 音乐模型 → music。
	seedModel(t, gdb, "mus", "music",
		[]string{"audio/music/generations"},
		map[string]any{"kind": "per_task", "per_task_price": 1000},
		nil)
	// 6. 对话模型 → 能力为空（没有预设 TAB）。
	seedModel(t, gdb, "chat-1", "chat",
		[]string{"chat/completions"}, nil, nil)
	// 7. 已声明 `[]` → 必须原样保留（不能被兜底打开）。
	seedModel(t, gdb, "img-declared-empty", "image",
		[]string{"images/generations"},
		map[string]any{"kind": "per_image", "tier_price": map[string]any{"1K": 100}},
		model.JSON("[]"))
	// 8. 已声明具体能力 → 必须不被覆盖（CAS）。
	seedModel(t, gdb, "vid-declared", "video",
		[]string{"video/generations"},
		map[string]any{"kind": "per_video", "second_price": map[string]any{"720p": 9000}},
		model.MustJSON([]string{"t2v", "first_last"}))

	n, err := EnsureCapabilities()
	if err != nil {
		t.Fatalf("补齐失败: %v", err)
	}
	// 6 行写成了空数组（chat），仍算补齐；共 6 行 NULL 被填。
	if n != 6 {
		t.Fatalf("应补齐 6 行，实际 %d", n)
	}

	want := map[string]string{
		"img-both":           `["t2i", "i2i"]`,
		"img-gen":            `["t2i"]`,
		"img-empty":          `["t2i"]`,
		"vid-ref":            `["t2v", "i2v", "ref_video"]`,
		"mus":                `["music"]`,
		"chat-1":             `[]`,
		"img-declared-empty": `[]`,
		"vid-declared":       `["t2v", "first_last"]`,
	}
	for id, w := range want {
		got, isNull := capsOf(t, gdb, id)
		if isNull {
			t.Fatalf("模型 %s 的能力仍是 NULL", id)
		}
		if normalizeTierText(got) != normalizeTierText(w) {
			t.Fatalf("模型 %s 能力为 %s，期望 %s", id, got, w)
		}
	}

	// 幂等：再跑一次不应再动任何行。
	again, err := EnsureCapabilities()
	if err != nil {
		t.Fatalf("第二次补齐失败: %v", err)
	}
	if again != 0 {
		t.Fatalf("幂等性被破坏：第二次仍改了 %d 行", again)
	}
}

// normalizeTierText 抹掉 jsonb 输出的空格差异，便于比较。
func normalizeTierText(s string) string {
	var v []string
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
