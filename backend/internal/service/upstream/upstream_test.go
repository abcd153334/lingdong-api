package upstream

import (
	"context"
	"os"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// ---------- 纯函数用例 ----------

func TestNormName(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-4o": "gpt-4o",
		"gpt-4o":        "gpt-4o",
		"a/b/c":         "c",
	}
	for in, want := range cases {
		if got := normName(in); got != want {
			t.Fatalf("normName(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestConvertCost(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	// 正常输入
	p, free := convertCost(&devCost{Input: f(2.5), Output: f(10), CacheRead: f(1.25)})
	if free || p == nil || p.input == nil || *p.input != 2.5 || p.output == nil || *p.output != 10 || p.cache == nil || *p.cache != 1.25 {
		t.Fatalf("正常 cost 转换错误: %+v free=%v", p, free)
	}
	// 免费：input=0 且无输出
	if p, free := convertCost(&devCost{Input: f(0)}); !free || p == nil || *p.input != 0 || p.output != nil {
		t.Fatalf("免费 cost 判定错误: %+v free=%v", p, free)
	}
	// 无效：input 缺失 / 负数 / input=0 但有输出价 / nil
	for name, c := range map[string]*devCost{
		"nil":     nil,
		"input缺失": {Output: f(1)},
		"input负数": {Input: f(-1)},
		"零输入有输出":  {Input: f(0), Output: f(1)},
	} {
		if p, free := convertCost(c); p != nil || free {
			t.Fatalf("%s 应判为无有效单价，实际 %+v free=%v", name, p, free)
		}
	}
	// 负输出/负缓存视为缺失
	p, _ = convertCost(&devCost{Input: f(1), Output: f(-2), CacheRead: f(-3)})
	if p.output != nil || p.cache != nil {
		t.Fatalf("负输出/负缓存应归零为 nil: %+v", p)
	}
}

func TestGuessModality(t *testing.T) {
	cases := map[string]string{
		"text-embedding-3-large": "embedding",
		"bge-reranker-v2":        "rerank",
		"llama-guard-4":          "moderation",
		"whisper-1":              "audio",
		"sora-2":                 "video",
		"dall-e-3":               "image",
		"gpt-4o":                 "chat",
	}
	for in, want := range cases {
		if got := guessModality(in); got != want {
			t.Fatalf("guessModality(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------

const upTestSchema = "ld_test_g14_upstream"

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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + upTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+upTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Model{}); err != nil {
		t.Fatalf("迁移模型表失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + upTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// ---------- 真实上游拉取用例（默认跳过；仅在显式开启时执行） ----------

// TestFetchModelsDevLive 验证真实拉取链路（SSRF 校验 → HTTP → JSON 解析 → cost 字段口径）。
// 默认跳过以免测试依赖公网；本地/发布前可用 LD_LIVE_UPSTREAM=1 显式执行。
func TestFetchModelsDevLive(t *testing.T) {
	if os.Getenv("LD_LIVE_UPSTREAM") != "1" {
		t.Skip("未设置 LD_LIVE_UPSTREAM=1，跳过真实上游拉取用例")
	}
	providers, err := fetchModelsDev(context.Background())
	if err != nil {
		t.Fatalf("真实拉取失败: %v", err)
	}
	if len(providers) == 0 {
		t.Fatal("真实拉取返回空数据集")
	}
	p, ok := providers["openai"]
	if !ok {
		t.Fatalf("真实数据缺少 openai 来源（共 %d 个来源）", len(providers))
	}
	m, ok := p.Models["gpt-4o"]
	if !ok || m.Cost == nil || m.Cost.Input == nil || *m.Cost.Input <= 0 {
		t.Fatalf("openai/gpt-4o 价目解析异常: %+v", m)
	}
	t.Logf("真实拉取来源数=%d，openai/gpt-4o input=%v USD/1M tokens", len(providers), *m.Cost.Input)
}

// TestDiffWithLocal 覆盖：一致/有差异/新增（本地缺、本地无价目）/已停售、
// 免费与无单价条目剔除、跨供应商同名去重取本地供应商一致的来源、无关来源不做「新增」扩散。
func TestDiffWithLocal(t *testing.T) {
	gdb := withTestDB(t)
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() { db.DB = prev })

	f := func(v float64) *float64 { return &v }
	// 上游价一律 USD/1M，所以站点币种必须是 USD 才能直接比（人民币站要按汇率折算，
	// 那是另一条路径，由 usdPer1M 负责，不该混进这条用例）。
	restore := conf.SetBillingForTest(conf.CurrencyUSD, conf.DefaultCnyPerUsd)
	t.Cleanup(restore)
	// 本地价目是**微 / 百万 tokens**：$3.0/1M → 3_000_000，$0.15/1M → 150_000。
	//
	// gpt-4o 本地故意定 3.0（上游 2.5）→ diff；gpt-4o-mini 本地 0.15（上游 0.15）→ same。
	// 改造前这里用「本地倍率 × 2 == 上游单价」这类猜测式判定，本地 2.5 会被判成 diff ——
	// 那说明判定本身在比两个不同量纲的数，而不是在比价。
	im := func(v int64) *int64 { return &v }
	locals := []model.Model{
		{ID: "gpt-4o", DisplayName: "gpt-4o", Vendor: "openai", Modality: "chat",
			BillingMode: model.BillingModeTokens, Prices: model.Prices{Input: im(3_000_000)}},
		{ID: "gpt-4o-mini", DisplayName: "gpt-4o-mini", Vendor: "openai", Modality: "chat",
			BillingMode: model.BillingModeTokens, Prices: model.Prices{Input: im(150_000)}},
		{ID: "claude-3", DisplayName: "claude-3", Vendor: "anthropic", Modality: "chat"},
	}
	if err := gdb.Create(&locals).Error; err != nil {
		t.Fatalf("写入本地模型失败: %v", err)
	}

	providers := map[string]devProvider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]devModel{
			"gpt-4o":      {Cost: &devCost{Input: f(2.5), Output: f(10), CacheRead: f(1.25)}},
			"gpt-4o-mini": {Cost: &devCost{Input: f(0.15)}},
			"o3-mini":     {Cost: &devCost{Input: f(1.1)}},
			"free-model":  {Cost: &devCost{Input: f(0)}},
			"no-cost":     {},
			"gone-model":  {Cost: &devCost{Input: f(1)}, Status: "deprecated"},
		}},
		"azure": {ID: "azure", Name: "Azure", Models: map[string]devModel{
			"gpt-4o": {Cost: &devCost{Input: f(2.8), Output: f(10.5)}},
		}},
		"anthropic": {ID: "anthropic", Name: "Anthropic", Models: map[string]devModel{
			"claude-3": {Cost: &devCost{Input: f(3)}},
		}},
		"vancine": {ID: "vancine", Name: "Vancine", Models: map[string]devModel{
			"glm-5.3-flash": {Cost: &devCost{Input: f(0.12), Output: f(0.4)}},
		}},
	}

	items, err := diffWithLocal(context.Background(), providers)
	if err != nil {
		t.Fatalf("diffWithLocal 失败: %v", err)
	}
	got := map[string]PreviewItem{}
	for _, it := range items {
		got[it.Name] = it
	}

	if it, ok := got["gpt-4o"]; !ok || it.Status != "diff" || it.Vendor != "openai" {
		t.Fatalf("gpt-4o 应为 openai 来源的 diff（同名去重取本地供应商一致项），实际 %+v ok=%v", it, ok)
	}
	if it, ok := got["gpt-4o-mini"]; !ok || it.Status != "same" {
		t.Fatalf("gpt-4o-mini 应为 same，实际 %+v ok=%v", it, ok)
	}
	if it, ok := got["o3-mini"]; !ok || it.Status != "new" {
		t.Fatalf("o3-mini 应为 new（本地缺、来源与本地供应商相关），实际 %+v ok=%v", it, ok)
	}
	if it, ok := got["claude-3"]; !ok || it.Status != "new" || it.Modality != "chat" {
		t.Fatalf("claude-3 应为 new 且模态取本地，实际 %+v ok=%v", it, ok)
	}
	if it, ok := got["gone-model"]; !ok || it.Status != "deprecated" {
		t.Fatalf("gone-model 应保留为 deprecated，实际 %+v ok=%v", it, ok)
	}
	for _, absent := range []string{"free-model", "no-cost", "glm-5.3-flash"} {
		if _, ok := got[absent]; ok {
			t.Fatalf("%s 不应出现在预览中（免费/无单价/无关来源不扩散）", absent)
		}
	}
	// 输出顺序：新增 → 有差异 → 一致 → 已停售
	wantRank := []string{"new", "new", "diff", "same", "deprecated"}
	if len(items) != len(wantRank) {
		t.Fatalf("预览条数=%d，期望 %d：%+v", len(items), len(wantRank), items)
	}
	rank := map[string]int{"new": 0, "diff": 1, "same": 2, "deprecated": 3}
	for i := 1; i < len(items); i++ {
		if rank[items[i-1].Status] > rank[items[i].Status] {
			t.Fatalf("输出顺序未按 新增/有差异/一致/已停售 分组: %+v", items)
		}
	}
	if items[0].Status != "new" || items[len(items)-1].Status != "deprecated" {
		t.Fatalf("首尾分组错误: %+v", items)
	}
}

// TestPreviewUnknownAndDownSource 覆盖异常分支：未知来源与已下线来源均返回 ErrSourceUnavailable，
// 且不返回空列表（避免把失败伪装成「无差异」）。
func TestPreviewUnknownAndDownSource(t *testing.T) {
	for _, id := range []string{basellmID, "not-exists"} {
		items, err := Preview(context.Background(), id)
		if err == nil || !strings.Contains(err.Error(), "upstream source unavailable") {
			t.Fatalf("来源 %q 应返回 ErrSourceUnavailable，实际 err=%v", id, err)
		}
		if items != nil {
			t.Fatalf("失败时不应返回条目，实际 %+v", items)
		}
	}
	if s := Sources(); len(s) != 2 || s[1].ID != basellmID || s[1].OK {
		t.Fatalf("来源登记项异常: %+v", s)
	}
}
