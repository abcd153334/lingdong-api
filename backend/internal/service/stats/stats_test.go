package stats

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/service/pricing"
)

// TestCountPriceMissing 未配价判定与 billing.Estimate 的 ErrPriceMissing 口径一致。
//
// 口径要点：nil（未配置）≠ 0（显式免费）。
// 0 元模型是运营刻意免费（门户会打"免费"标签），不该算进"待补价格"的告警里；
// 把它算进去，健康度面板会永远红着，运营很快就会学会忽略它。
func TestCountPriceMissing(t *testing.T) {
	price := func(v int64) *int64 { return &v }
	cat := &pricing.Snapshot{Models: []pricing.ModelItem{
		{ID: "a", BillingMode: model.BillingModeTokens, Prices: model.Prices{Input: price(150000)}, Enabled: true}, // 已配价
		{ID: "b", BillingMode: model.BillingModeTokens, Enabled: true},                                             // 未配价（token）
		{ID: "b0", BillingMode: model.BillingModeTokens, Prices: model.Prices{Input: price(0)}, Enabled: true},     // 显式免费，不算缺价
		{ID: "c", BillingMode: model.BillingModeCall, Prices: model.Prices{Call: price(10000)}, Enabled: true},     // 已配价（按次）
		{ID: "d", BillingMode: model.BillingModeCall, Enabled: true},                                               // 未配价（按次）
		{ID: "e", BillingMode: model.BillingModeSpec, Enabled: true, SpecPrices: model.SpecPrices{ // 已配价（规格）
			Kind: "per_image", TierPrice: map[string]int64{"1K": 28000}}},
		{ID: "f", BillingMode: model.BillingModeSpec, Enabled: true},    // 未配价（规格）
		{ID: "g", BillingMode: model.BillingModeTokens, Enabled: false}, // 停用不计入
	}}
	if got := countPriceMissing(cat); got != 3 {
		t.Fatalf("未配价计数=%d，期望 3（b/d/f）", got)
	}
}

// ---------- 数据库集成用例（需 LD_TEST_DSN；未设置则跳过） ----------

const testSchema = "ld_test_s11"

// withTestDB 在独立 schema 中准备 usages/usage_daily/usages_archive 等表。
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
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + testSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+testSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Usage{}, &model.UsageArchive{}, &model.UsageDaily{}, &model.Channel{}, &model.User{}, &model.Order{}, &model.Task{}, &model.Transaction{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + testSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

func seedUsage(t *testing.T, gdb *gorm.DB, modelID string, modality string, charge int64) {
	t.Helper()
	u := &model.Usage{
		UserID: 1, KeyID: 1, ChannelID: 1, Model: modelID, Modality: modality,
		Status: "settled", HTTPStatus: 200, Kind: "consume",
		PromptTokens: 10, CompletionTokens: 5, Charge: charge,
	}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造 usage 失败: %v", err)
	}
}

// TestRollupMatchesDetail 聚合结果必须与明细求和逐维一致，且重复执行幂等。
func TestRollupMatchesDetail(t *testing.T) {
	gdb := withTestDB(t)
	seedUsage(t, gdb, "gpt-4o-mini", "text", 300)
	seedUsage(t, gdb, "gpt-4o-mini", "text", 700)
	seedUsage(t, gdb, "kling-v1", "video", 5000)

	ctx := context.Background()
	if _, err := RollupDaily(ctx); err != nil {
		t.Fatalf("聚合失败: %v", err)
	}

	type agg struct {
		Model    string
		Charge   int64
		Requests int64
	}
	var daily []agg
	if err := gdb.Raw(`SELECT model, SUM(charge) AS charge, SUM(requests) AS requests
	                     FROM usage_daily GROUP BY model ORDER BY model`).Scan(&daily).Error; err != nil {
		t.Fatalf("读取聚合失败: %v", err)
	}
	var detail []agg
	if err := gdb.Raw(`SELECT model, SUM(charge) AS charge, COUNT(*) AS requests
	                     FROM usages GROUP BY model ORDER BY model`).Scan(&detail).Error; err != nil {
		t.Fatalf("读取明细失败: %v", err)
	}
	if len(daily) != len(detail) {
		t.Fatalf("维度数量不一致: daily=%d detail=%d", len(daily), len(detail))
	}
	for i := range daily {
		if daily[i] != detail[i] {
			t.Fatalf("第 %d 维复算不一致: daily=%+v detail=%+v", i, daily[i], detail[i])
		}
	}

	// 重复执行幂等（不叠加）
	if _, err := RollupDaily(ctx); err != nil {
		t.Fatalf("二次聚合失败: %v", err)
	}
	var again []agg
	if err := gdb.Raw(`SELECT model, SUM(charge) AS charge, SUM(requests) AS requests
	                     FROM usage_daily GROUP BY model ORDER BY model`).Scan(&again).Error; err != nil {
		t.Fatalf("二次读取失败: %v", err)
	}
	for i := range again {
		if again[i] != daily[i] {
			t.Fatalf("二次聚合非幂等: %+v ≠ %+v", again[i], daily[i])
		}
	}
}

// TestArchiveMovesExpired 超期明细按 logs_retention_days 迁入归档表。
func TestArchiveMovesExpired(t *testing.T) {
	gdb := withTestDB(t)
	seedUsage(t, gdb, "gpt-4o-mini", "text", 100)
	old := time.Now().AddDate(0, 0, -400)
	if err := gdb.Exec(`UPDATE usages SET created_at = ? WHERE charge = 100`, old).Error; err != nil {
		t.Fatalf("回填旧时间失败: %v", err)
	}
	seedUsage(t, gdb, "gpt-4o-mini", "text", 200)

	n, err := ArchiveUsages(context.Background(), 100)
	if err != nil {
		t.Fatalf("归档失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("归档行数=%d，期望 1", n)
	}
	var hot, cold int64
	gdb.Raw(`SELECT COUNT(*) FROM usages WHERE charge = 100`).Scan(&hot)
	gdb.Raw(`SELECT COUNT(*) FROM usages_archive WHERE charge = 100`).Scan(&cold)
	if hot != 0 || cold != 1 {
		t.Fatalf("归档分布错误: hot=%d cold=%d", hot, cold)
	}
}

// TestRangeBoardDailyWindow 锁定 d7/d30 档位聚合的日期窗口：
// `date >= CURRENT_DATE - ?` 曾使 PG 把参数解析成 date（走 date-date=integer），
// 进而报 "operator does not exist: date >= integer"（42883）。
// 现在窗口由 Go 按业务日算好再传（见 TrendWindow），因此用例也用同一套 DayKey 造数据 ——
// 继续写 CURRENT_DATE 会让用例自己依赖 DB 会话时区，16:00–24:00 UTC 之间随机翻红。
func TestRangeBoardDailyWindow(t *testing.T) {
	gdb := withTestDB(t)
	u := &model.User{Username: "alice", PasswordHash: "x", Group: "default", Role: 1, Status: 1, AffCode: "rb-window"}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	now := BizNow()
	if err := gdb.Exec(`INSERT INTO usage_daily (date, model, modality, user_id, requests, charge) VALUES
		(?::date, 'gpt-4o', 'chat', ?, 2, 300),
		(?::date, 'gpt-4o', 'chat', ?, 1, 700)`,
		DayKey(now), u.ID, DayKey(now.AddDate(0, 0, -10)), u.ID).Error; err != nil {
		t.Fatalf("造聚合失败: %v", err)
	}

	d7, err := rangeBoard(context.Background(), 7, "")
	if err != nil {
		t.Fatalf("d7 聚合失败: %v", err)
	}
	if d7.Users[0].Charge != 300 || d7.ModelTop[0].Value != 300 {
		t.Fatalf("d7 窗口错误: users=%+v model_top=%+v", d7.Users, d7.ModelTop)
	}

	d30, err := rangeBoard(context.Background(), 30, "")
	if err != nil {
		t.Fatalf("d30 聚合失败: %v", err)
	}
	if d30.Users[0].Charge != 1000 || d30.ModelTop[0].Value != 1000 {
		t.Fatalf("d30 窗口错误: users=%+v model_top=%+v", d30.Users, d30.ModelTop)
	}
}

// TestUserOverviewScopeAndWindow 锁定门户主页聚合口径（v2）：
//
//	① 严格按 user_id 隔离；② 今日**实时直查 usages 明细**（不读 usage_daily）；
//	③ 本月 = usage_daily（月初~昨日）+ 今日实时，上界是开区间（不重复计数今日）；
//	④ 充值走 transactions 净额（含退款负数）；⑤ 趋势末点恒等于今日；⑥ 非法 days 回落 7。
func TestUserOverviewScopeAndWindow(t *testing.T) {
	gdb := withTestDB(t)

	// 固定时钟：口径测试不能依赖「跑测试那天恰好是几号」，否则月初跑绿、月末跑红。
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, Location())
	prevNow := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = prevNow })

	u1 := &model.User{Username: "overview_a", PasswordHash: "x", Group: "default", Role: 1, Status: 1, Balance: 9000, Frozen: 1234, AffCode: "ovw-aff-a"}
	u2 := &model.User{Username: "overview_b", PasswordHash: "x", Group: "default", Role: 1, Status: 1, Balance: 100, AffCode: "ovw-aff-b"}
	for _, u := range []*model.User{u1, u2} {
		if err := gdb.Create(u).Error; err != nil {
			t.Fatalf("造用户失败: %v", err)
		}
	}

	// 今日明细（u1）：2 条 settled + 1 条 released + 1 条 pending。
	// 3 个金额 100/200/0/0：released 与 pending 的 charge 天然为 0。
	type usageSeed struct {
		status string
		charge int64
		age    time.Duration
	}
	for _, c := range []usageSeed{
		{"settled", 100, -1 * time.Hour},
		{"settled", 200, -3 * time.Hour},
		{"released", 0, -5 * time.Hour},
		{"pending", 0, -10 * time.Minute},
	} {
		if err := gdb.Create(&model.Usage{
			CreatedAt: now.Add(c.age), UserID: u1.ID, KeyID: 1, ChannelID: 1,
			Model: "gpt-4o", Modality: "chat", Status: c.status, HTTPStatus: 200,
			Kind: "relay", CallKind: "sync", Charge: c.charge,
			PromptTokens: 10, CompletionTokens: 5,
		}).Error; err != nil {
			t.Fatalf("造 usage 失败: %v", err)
		}
	}
	// u2 今日 9999：必须被隔离
	if err := gdb.Create(&model.Usage{
		CreatedAt: now.Add(-time.Hour), UserID: u2.ID, KeyID: 2, ChannelID: 1,
		Model: "gpt-4o", Modality: "chat", Status: "settled", HTTPStatus: 200,
		Kind: "relay", CallKind: "sync", Charge: 9999,
	}).Error; err != nil {
		t.Fatalf("造 u2 usage 失败: %v", err)
	}

	// 聚合表：昨日 700（进本月）、40 天前 500（必须被本月与 d30 同时排除）。
	if err := gdb.Exec(`INSERT INTO usage_daily (date, model, modality, user_id, requests, errors, charge, prompt_tokens, completion_tokens) VALUES
		(?::date, 'gpt-4o', 'chat', ?, 1, 0, 700, 3, 2),
		(?::date, 'gpt-4o', 'chat', ?, 1, 0, 500, 1, 1)`,
		DayKey(now.AddDate(0, 0, -1)), u1.ID,
		DayKey(now.AddDate(0, 0, -40)), u1.ID).Error; err != nil {
		t.Fatalf("造聚合失败: %v", err)
	}

	// 充值：本月一笔到账 1.50 元 + 一笔退款 -0.50 元 → 净 1.00 元 = 1_000_000 微。
	for _, tx := range []model.Transaction{
		{UserID: u1.ID, Type: model.TxnRecharge, Amount: 1_500_000, CreatedAt: now.Add(-2 * time.Hour)},
		{UserID: u1.ID, Type: model.TxnRefund, Amount: -500_000, CreatedAt: now.Add(-1 * time.Hour)},
	} {
		if err := gdb.Create(&tx).Error; err != nil {
			t.Fatalf("造流水失败: %v", err)
		}
	}
	if err := gdb.Create(&model.Task{UserID: u1.ID, Model: "kling-v1", Modality: "video", Status: "running", Kind: "task"}).Error; err != nil {
		t.Fatalf("造任务失败: %v", err)
	}

	ctx := context.Background()
	ov, err := UserOverview(ctx, u1.ID, 7)
	if err != nil {
		t.Fatalf("聚合 u1 失败: %v", err)
	}
	// 今日来自明细（含 pending/released），不是来自 usage_daily 的今日桶（那里我们一行都没写）。
	if ov.Today.Requests != 4 || ov.Today.Errors != 1 || ov.Today.Pending != 1 {
		t.Fatalf("今日计数错误（应为 4 总/1 失败/1 进行中）: %+v", ov.Today)
	}
	if ov.Today.Charge != 300 || ov.Today.PromptTokens != 40 {
		t.Fatalf("今日用量错误: %+v", ov.Today)
	}
	// 本月 = 昨日 700 + 今日 300；40 天前那 500 不在本月。
	if ov.Month.Charge != 1000 || ov.Month.Requests != 5 {
		t.Fatalf("本月用量错误（今日被重复计数或跨月串入）: %+v", ov.Month)
	}
	if ov.Month.Pending != 1 {
		t.Fatalf("本月进行中应等于今日: %+v", ov.Month)
	}
	// 充值净额：1_500_000 - 500_000。用绝对值断言，量纲改回去会立刻红。
	if ov.MonthRecharge != 1_000_000 {
		t.Fatalf("本月充值=%d，期望 1000000（净额）", ov.MonthRecharge)
	}
	if ov.GeneratedAt == "" {
		t.Fatal("generated_at 不得为空")
	}

	if len(ov.Trend) != 7 {
		t.Fatalf("d7 趋势长度=%d，期望 7", len(ov.Trend))
	}
	// 首点 = 3-09，窗口外（40 天前的 500）不得出现
	if ov.Trend[0].Charge != 0 {
		t.Fatalf("窗口外数据未被排除: 首点=%+v", ov.Trend[0])
	}
	// 昨日点 = 700
	if got := ov.Trend[len(ov.Trend)-2].Charge; got != 700 {
		t.Fatalf("昨日趋势点=%d，期望 700", got)
	}
	last := ov.Trend[len(ov.Trend)-1]
	// 不变式：趋势末点恒等于今日费用（聚合表今日桶为空也必须相等）
	if last.Charge != ov.Today.Charge {
		t.Fatalf("趋势末点(%d) != 今日费用(%d)", last.Charge, ov.Today.Charge)
	}
	if last.Recharge != 1_000_000 {
		t.Fatalf("趋势末点充值=%d，期望 1000000", last.Recharge)
	}
	if ov.ActiveTasks != 1 {
		t.Fatalf("在途任务数=%d，期望 1", ov.ActiveTasks)
	}

	// 隔离：u2 只看到自己的 9999
	ov2, err := UserOverview(ctx, u2.ID, 30)
	if err != nil {
		t.Fatalf("聚合 u2 失败: %v", err)
	}
	if ov2.Today.Charge != 9999 || ov2.Month.Charge != 9999 {
		t.Fatalf("u2 用量=%d/%d，期望 9999（隔离失败）", ov2.Today.Charge, ov2.Month.Charge)
	}
	if ov2.MonthRecharge != 0 {
		t.Fatalf("u2 充值应为 0: %d", ov2.MonthRecharge)
	}
	if len(ov2.Trend) != 30 {
		t.Fatalf("d30 趋势长度=%d，期望 30", len(ov2.Trend))
	}
	if ov2.ActiveTasks != 0 {
		t.Fatalf("u2 在途任务应为 0: %d", ov2.ActiveTasks)
	}

	// 非法 days 回落 7
	for _, bad := range []int{0, -3, 15, 365} {
		got, err := UserOverview(ctx, u1.ID, bad)
		if err != nil {
			t.Fatalf("days=%d 聚合失败: %v", bad, err)
		}
		if len(got.Trend) != 7 {
			t.Fatalf("days=%d 未回落 7，实际长度=%d", bad, len(got.Trend))
		}
	}
}
