package relay

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pagination"
	"lingdong-api/internal/pkg/sorting"
	"lingdong-api/internal/service/logs"
)

const usageTestSchema = "ld_test_s17"

// withUsageDB 在独立 schema 中准备 usages/api_keys/users 表（需 LD_TEST_DSN，否则跳过）。
func withUsageDB(t *testing.T) *gorm.DB {
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + usageTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+usageTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Usage{}, &model.APIKey{}, &model.User{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + usageTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

// ctxWithQuery 构造带查询串的 gin.Context（用于解析筛选参数）。
// usagePageOf 是测试侧的 usagePage 包装：用同一条 query 严格解析分页参数。
//
// 生产调用方（PortalUsage / usageList）同样是「先 pagination.Parse 再查询」，
// 所以这里一起验证解析链路，不绕过它直接塞 Params。
func usagePageOf(t *testing.T, rawQuery string, q *gorm.DB, order sorting.Order) ([]logs.PortalItem, int64, int, int, error) {
	t.Helper()
	c := ctxWithQuery(rawQuery)
	p, err := pagination.Parse(c)
	if err != nil {
		t.Fatalf("分页参数解析失败(%q): %v", rawQuery, err)
	}
	return usagePage(c, q, order, p)
}

func ctxWithQuery(rawQuery string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/portal/usage?"+rawQuery, nil)
	return c
}

// TestParseUsageFilter 覆盖严格/宽松两种解析模式与区间颠倒。
func TestParseUsageFilter(t *testing.T) {
	valid := "2026-10-01T00:00:00%2B08:00"
	if f, ok := parseUsageFilter(ctxWithQuery("start="+valid+"&modality=chat&key_id=7"), true); !ok ||
		f.Start.IsZero() || f.Modality != "chat" || f.KeyID != 7 {
		t.Fatalf("合法参数应解析成功: ok=%v f=%+v", ok, f)
	}

	// 严格模式：非法日期直接失败（门户禁止静默丢弃筛选条件）
	if _, ok := parseUsageFilter(ctxWithQuery("start=not-a-time"), true); ok {
		t.Fatal("严格模式下非法日期应失败")
	}
	// 宽松模式：非法日期按未传处理（/v1/usage 兼容行为）
	f, ok := parseUsageFilter(ctxWithQuery("start=not-a-time"), false)
	if !ok || !f.Start.IsZero() {
		t.Fatalf("宽松模式下非法日期应被忽略: ok=%v start=%v", ok, f.Start)
	}
	// 区间颠倒：两种模式都失败
	reversed := "start=" + valid + "&end=2026-09-01T00:00:00%2B08:00"
	if _, ok := parseUsageFilter(ctxWithQuery(reversed), true); ok {
		t.Fatal("严格模式下区间颠倒应失败")
	}
	if _, ok := parseUsageFilter(ctxWithQuery(reversed), false); ok {
		t.Fatal("宽松模式下区间颠倒也应失败")
	}
}

// TestUsageStatusText 覆盖计费会话态 → 门户文案（与 Usage 页状态标签一致）。
func TestUsageStatusText(t *testing.T) {
	cases := []struct {
		status string
		http   int
		want   string
	}{
		{"settled", 200, "已成功"},
		{"pending", 200, "进行中"},
		{"released", 429, "失败(429)"},
		{"released", 0, "失败"},
	}
	for _, c := range cases {
		if got := usageStatusText(c.status, c.http); got != c.want {
			t.Fatalf("status=%s http=%d → %q，期望 %q", c.status, c.http, got, c.want)
		}
	}
}

// TestUsageCSV 覆盖门户导出列、无名称时回落 KEY id、金额币种与转义。
//
// 这是**纯函数**测试，绝不能依赖 DB：旧实现在第一行调了 withUsageDB(t)，
// 于是未设 LD_TEST_DSN 时永久 SKIP —— 表头改了本地一直"绿"，只有门禁（设了 DSN）才炸。
func TestUsageCSV(t *testing.T) {
	// 金额列断言具体数值，必须先固定结算口径（否则断言随部署币种漂移）。
	restore := conf.SetBillingForTest(conf.CurrencyUSD, conf.DefaultCnyPerUsd)
	defer restore()
	rows := []model.Usage{
		{
			ID: 1, KeyID: 7, Model: "gpt,4", Modality: "chat", Status: "settled", HTTPStatus: 200,
			PromptTokens: 10, CompletionTokens: 5, Charge: 500000, LatencyMs: 120,
			CreatedAt: time.Date(2026, 10, 20, 10, 30, 0, 0, time.Local),
		},
		{
			ID: 2, KeyID: 8, Model: "dall-e", Modality: "image", Status: "released", HTTPStatus: 502,
			Charge: 250000, LatencyMs: 40, ErrorMsg: `上游"超时"`,
			CreatedAt: time.Date(2026, 10, 20, 11, 0, 0, 0, time.Local),
		},
	}
	out := usageCSV(rows, map[uint64]string{7: "主密钥"})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("应为表头+2 行: %q", out)
	}
	// 门户导出表头（C1 扩容）：补各 token 通道 + 金额币种 + 三种耗时 + 请求号。
	// 与管理台 E5 的差别只在「渠道/成本/IP/上游单号」这几列 —— 那些不能给终端用户。
	// 金额后必须跟**币种列**：同一份导出可能含切币种前后两种记录，表头写死币种会标错账。
	// 缓存写入两列单列（Anthropic 语义下 cache_write 单价是普通输入的 1.25–2 倍）：
	// 合并进「缓存tok」会让用户拿 CSV 永远算不平金额。
	const header = "时间,模型,类型,密钥,状态,HTTP,输入tok,输出tok,缓存tok,缓存写5mtok,缓存写1htok," +
		"图片tok,音频入tok,音频出tok," +
		"金额,币种,总耗时ms,服务耗时ms,首字ms,请求ID,错误"
	if lines[0] != header {
		t.Fatalf("表头不符: %s", lines[0])
	}
	// 第 1 行：含逗号的模型名被整体加引号；同名 KEY 取展示名；500000 微 = 0.5 美元
	want0 := `2026-10-20 10:30:00,"gpt,4",chat,主密钥,已成功,200,10,5,0,0,0,0,0,0,0.500000,USD,120,0,0,,`
	if lines[1] != want0 {
		t.Fatalf("第 1 行不符:\n%s\n期望:\n%s", lines[1], want0)
	}
	// 第 2 行：未命名 KEY 回落 id；错误列是分类文案（不再回传服务商原文）
	if !strings.Contains(lines[2], `,image,8,失败(502),502,0,0,0,0,0,0,0,0,0.250000,USD,40,0,0,,服务暂时不可用，本次未扣费`) {
		t.Fatalf("第 2 行不符: %s", lines[2])
	}
}

// TestModalityDistAndUsagePage 覆盖类型分布聚合（同源同口径）、筛选传导与分页。
func TestModalityDistAndUsagePage(t *testing.T) {
	gdb := withUsageDB(t)
	seedUsage(t, gdb, 1, 7, "chat", 300, "settled")
	seedUsage(t, gdb, 1, 7, "chat", 200, "settled")
	seedUsage(t, gdb, 1, 8, "image", 900, "settled")
	seedUsage(t, gdb, 2, 9, "chat", 9999, "settled") // 他人数据必须隔离

	dist, err := modalityDist(ctxWithQuery("").Request.Context(), usageFilter{}, 1)
	if err != nil {
		t.Fatalf("类型分布失败: %v", err)
	}
	if len(dist) != 2 || dist[0].Modality != "image" || dist[0].Requests != 1 || dist[0].Charge != 900 {
		t.Fatalf("无权重的类型分布不符: %+v", dist)
	}
	if dist[1].Modality != "chat" || dist[1].Requests != 2 || dist[1].Charge != 500 {
		t.Fatalf("chat 聚合不符: %+v", dist[1])
	}

	// 筛选传导：仅 chat
	dist, err = modalityDist(ctxWithQuery("").Request.Context(), usageFilter{Modality: "chat"}, 1)
	if err != nil {
		t.Fatalf("类型分布失败: %v", err)
	}
	if len(dist) != 1 || dist[0].Modality != "chat" || dist[0].Charge != 500 {
		t.Fatalf("按类型筛选后应只剩 chat: %+v", dist)
	}

	// 分页与排序（id 倒序）：page_size=2 → 2 行 / total=3
	// 显式传空 order：本用例只验证分页与 id 倒序兜底，
	// 排序规则本身由接口层解析，不属于这个函数的职责。
	items, total, page, size, err := usagePageOf(t, "page=1&page_size=2",
		usageFilter{}.apply(db.DB.Model(&model.Usage{}).Where("user_id = ?", 1)), sorting.Order{})
	if err != nil {
		t.Fatalf("分页失败: %v", err)
	}
	if total != 3 || page != 1 || size != 2 || len(items) != 2 {
		t.Fatalf("分页不符: total=%d page=%d size=%d len=%d", total, page, size, len(items))
	}
	if items[0].ID <= items[1].ID {
		t.Fatalf("应按 id 倒序: %d,%d", items[0].ID, items[1].ID)
	}

	// 时间窗口：仅取最后一小时 → 排除早期记录
	early := time.Now().Add(-48 * time.Hour)
	if err := gdb.Model(&model.Usage{}).Where("user_id = ?", 1).Where("modality = ?", "image").
		Update("created_at", early).Error; err != nil {
		t.Fatalf("改时间失败: %v", err)
	}
	_, total, _, _, err = usagePageOf(t, "",
		usageFilter{Start: time.Now().Add(-time.Hour)}.apply(db.DB.Model(&model.Usage{}).Where("user_id = ?", 1)), sorting.Order{})
	if err != nil {
		t.Fatalf("分页失败: %v", err)
	}
	if total != 2 {
		t.Fatalf("时间窗口应命中 2 条: %d", total)
	}
}

// seedUsage 造一条调用记录（页面对照需要可控的 modality/quota/status）。
func seedUsage(t *testing.T, gdb *gorm.DB, userID, keyID uint64, modality string, quota int64, status string) {
	t.Helper()
	u := model.Usage{
		UserID: userID, KeyID: keyID, Modality: modality, Model: "m-" + modality,
		Status: status, HTTPStatus: 200, PromptTokens: 10, CompletionTokens: 5,
		Charge: quota, LatencyMs: 10, CreatedAt: time.Now(),
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("造调用记录失败: %v", err)
	}
}
