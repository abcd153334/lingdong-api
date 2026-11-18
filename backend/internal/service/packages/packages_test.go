package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
)

const testSchema = "ld_test_pkg"

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
	if err := gdb.AutoMigrate(&model.Package{}, &model.Order{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	// 与 indexes.go 同构的部分唯一索引：并发建同名必须落 409，不能落 500。
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_packages_name
		ON packages (name) WHERE deleted_at IS NULL`).Error; err != nil {
		t.Fatalf("创建唯一索引失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	// 固定配置：ratio=69,444（¥216 → $30）、上限 $1,000，用例不依赖外部环境。
	conf.System().Set("recharge_ratio", json.RawMessage(`69444`))
	conf.System().Set("balance_limit", json.RawMessage(`500000000`))
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

func iptr(v int) *int       { return &v }
func bptr(v bool) *bool     { return &v }
func sptr(v string) *string { return &v }

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际 nil", code)
	}
	def, ok := errs.AsDef(err)
	if !ok || def.Code != code {
		t.Fatalf("期望错误码 %s，实际 %v", code, err)
	}
}

var orderSeq int

// seedOrder 造一笔引用套餐的订单。状态与软删由调用方决定。
func seedOrder(t *testing.T, gdb *gorm.DB, pkgID uint64, status string, softDeleted bool) {
	t.Helper()
	orderSeq++
	o := model.Order{
		UserID: 1, OutTradeNo: fmt.Sprintf("LD-T-%d", orderSeq),
		AmountCent: 21600, Credited: 16500000, Status: status,
		PackageID: pkgID, Subject: "充值-测试",
	}
	if err := gdb.Create(&o).Error; err != nil {
		t.Fatalf("造订单失败: %v", err)
	}
	if softDeleted {
		if err := gdb.Delete(&model.Order{}, o.ID).Error; err != nil {
			t.Fatalf("软删订单失败: %v", err)
		}
	}
}

// TestCreateZeroValues 钉死 GORM default 坑：新建「停用 + 排序 0 + 无角标」必须原样落库。
// 若只写 Create(&p)，Enabled 会被 default:true 回填成 true，套餐会直接上架到门户。
func TestCreateZeroValues(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	it, _, err := svc.Create(context.Background(), Input{
		Name: "停用包", PriceCent: 7200, Bonus: 0,
		Enabled: bptr(false), SortOrder: iptr(0), Badge: sptr(""),
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if it.Enabled || it.SortOrder != 0 || it.Badge != "" {
		t.Fatalf("零值被吃掉: enabled=%v sort=%d badge=%q", it.Enabled, it.SortOrder, it.Badge)
	}
	var row model.Package
	if err := gdb.First(&row, it.ID).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if row.Enabled || row.SortOrder != 0 || row.Badge != "" {
		t.Fatalf("库中零值不对: enabled=%v sort=%d badge=%q", row.Enabled, row.SortOrder, row.Badge)
	}
}

// TestCreateSortOrderAppend 未传排序时追加到末尾（空表 10，已有 40 则 50）。
func TestCreateSortOrderAppend(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	first, _, err := svc.Create(ctx, Input{Name: "A", PriceCent: 7200})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if first.SortOrder != 10 {
		t.Fatalf("空表应得 10，实际 %d", first.SortOrder)
	}
	second, _, err := svc.Create(ctx, Input{Name: "B", PriceCent: 7200})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if second.SortOrder != 20 {
		t.Fatalf("应追加为 20，实际 %d", second.SortOrder)
	}
	// 显式置顶：0 必须生效（不被 max+10 覆盖）
	third, _, err := svc.Create(ctx, Input{Name: "C", PriceCent: 7200, SortOrder: iptr(0)})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if third.SortOrder != 0 {
		t.Fatalf("显式 0 应生效，实际 %d", third.SortOrder)
	}
}

// TestNameUniqueAndReuse 同名拒绝；软删后允许复用（与部分唯一索引口径一致）。
func TestNameUniqueAndReuse(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	it, _, err := svc.Create(ctx, Input{Name: "标准包", PriceCent: 21600})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	_, _, err = svc.Create(ctx, Input{Name: "标准包", PriceCent: 21600})
	wantCode(t, err, errs.CodePackageNameExists)

	if err := svc.Delete(ctx, it.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, _, err := svc.Create(ctx, Input{Name: "标准包", PriceCent: 21600}); err != nil {
		t.Fatalf("软删后应可复用同名，实际 %v", err)
	}
}

// TestConcurrentSameName 并发建同名：必须恰好一个成功、另一个 409（不能是 500）。
func TestConcurrentSameName(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	const n = 6
	errsCh := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errsCh[i] = svc.Create(context.Background(), Input{Name: "抢注包", PriceCent: 7200})
		}(i)
	}
	wg.Wait()

	okCount, conflictCount := 0, 0
	for _, err := range errsCh {
		if err == nil {
			okCount++
			continue
		}
		def, ok := errs.AsDef(err)
		if !ok || (def.Code != errs.CodePackageNameExists && def.Code != errs.CodeInternal) {
			t.Fatalf("出现非预期错误: %v", err)
		}
		if def.Code == errs.CodePackageNameExists {
			conflictCount++
		} else {
			t.Fatalf("并发同名落到了 500（唯一索引冲突未映射）: %v", err)
		}
	}
	if okCount != 1 || conflictCount != n-1 {
		t.Fatalf("期望 1 成功 / %d 冲突，实际 %d / %d", n-1, okCount, conflictCount)
	}
}

// TestUpdatePartialAndZero 未传字段不变；显式零值必须写进去。
func TestUpdatePartialAndZero(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	it, _, err := svc.Create(ctx, Input{
		Name: "标准包", PriceCent: 21600, Bonus: 1500096,
		SortOrder: iptr(20), Badge: sptr("recommend"),
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	// 只改名字
	upd, _, err := svc.Update(ctx, it.ID, Input{Name: "标准包-新", PriceCent: 21600, Bonus: 1500096})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if upd.SortOrder != 20 || upd.Badge != "recommend" {
		t.Fatalf("未传字段被改动: sort=%d badge=%q", upd.SortOrder, upd.Badge)
	}
	// 显式清零
	upd2, _, err := svc.Update(ctx, it.ID, Input{
		Name: "标准包-新", PriceCent: 21600, Bonus: 1500096,
		SortOrder: iptr(0), Badge: sptr(""),
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if upd2.SortOrder != 0 || upd2.Badge != "" {
		t.Fatalf("零值未写入: sort=%d badge=%q", upd2.SortOrder, upd2.Badge)
	}
}

// TestValidate 表驱动：每条都必须被拒，且 code 是 invalid_param。
func TestValidate(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	long := strings.Repeat("包", 33)
	cases := []struct {
		name string
		in   Input
	}{
		{"空名称", Input{PriceCent: 7200}},
		{"空白名称", Input{Name: "   ", PriceCent: 7200}},
		{"超长名称", Input{Name: long, PriceCent: 7200}},
		{"含百分号", Input{Name: "包%", PriceCent: 7200}},
		{"含 emoji", Input{Name: "标准包🎉", PriceCent: 7200}},
		{"以标点结尾", Input{Name: "标准包-", PriceCent: 7200}},
		{"价格过低", Input{Name: "包", PriceCent: 99}},
		{"价格超硬上限", Input{Name: "包", PriceCent: MaxPriceCent + 1}},
		{"赠送为负", Input{Name: "包", PriceCent: 7200, Bonus: -1}},
		{"赠送超上限", Input{Name: "包", PriceCent: 7200, Bonus: MaxBonusAmount + 1}},
		{"角标非法", Input{Name: "包", PriceCent: 7200, Badge: sptr("sale")}},
		{"排序为负", Input{Name: "包", PriceCent: 7200, SortOrder: iptr(-1)}},
		{"排序超上限", Input{Name: "包", PriceCent: 7200, SortOrder: iptr(MaxSortOrder + 1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := svc.Create(context.Background(), c.in)
			wantCode(t, err, errs.CodeInvalidParam)
		})
	}
}

// TestCreditAndWarning 到账复算 = 售价（分→微）+ 赠送；超上限只告警不拒绝。
func TestCreditAndWarning(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	it, warn, err := svc.Create(ctx, Input{Name: "标准包", PriceCent: 21600, Bonus: 1500096})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	// 21600 分 = 216.00 元 → 216,000,000 微；赠送 1,500,096 微 原样加上。
	if it.Base != 216_000_000 || it.Credited != 217_500_096 {
		t.Fatalf("金额复算不符: base=%d credited=%d", it.Base, it.Credited)
	}
	if warn != nil {
		t.Fatalf("未超上限不应告警: %+v", warn)
	}
	// 把上限压到 $20，再存 ¥216（到账 $33）→ 保存成功 + 告警
	conf.System().Set("balance_limit", json.RawMessage(`10000000`))
	defer conf.System().Set("balance_limit", json.RawMessage(`500000000`))
	it2, warn2, err := svc.Create(ctx, Input{Name: "专业包", PriceCent: 43200})
	if err != nil {
		t.Fatalf("超上限不应拒绝保存: %v", err)
	}
	if warn2 == nil || warn2.Code != "credit_over_upper_limit" {
		t.Fatalf("应返回超上限告警，实际 %+v", warn2)
	}
	if it2.Credited != 432000000 {
		t.Fatalf("到账金额不符: %d", it2.Credited)
	}
}

// TestDeleteProtected 任意状态、含软删订单都算引用 → 拒绝删除。
func TestDeleteProtected(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	for _, c := range []struct {
		status string
		soft   bool
	}{{"paid", false}, {"refunded", false}, {"expired", true}} {
		it, _, err := svc.Create(ctx, Input{Name: "包-" + c.status, PriceCent: 7200})
		if err != nil {
			t.Fatalf("创建失败: %v", err)
		}
		seedOrder(t, gdb, it.ID, c.status, c.soft)
		err = svc.Delete(ctx, it.ID)
		wantCode(t, err, errs.CodePackageInUse)
		if !strings.Contains(err.Error(), "笔订单") {
			t.Fatalf("文案应含笔数: %v", err)
		}
	}
}

// TestDeleteUnused 无引用可软删；列表不再返回，Unscoped 仍可读。
func TestDeleteUnused(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	it, _, err := svc.Create(ctx, Input{Name: "临时包", PriceCent: 7200})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if err := svc.Delete(ctx, it.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	items, total, err := svc.List(ctx, ListInput{Limit: 20})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("软删后不应出现在列表: total=%d", total)
	}
	var row model.Package
	if err := gdb.Unscoped().First(&row, it.ID).Error; err != nil || !row.DeletedAt.Valid {
		t.Fatalf("软删行应可 Unscoped 读取: %v", err)
	}
	// 重复删除 → 404（已不在默认 scope 内）
	wantCode(t, svc.Delete(ctx, it.ID), errs.CodeNotFound)
}

// TestListSortAndFilter 排序（并列按价格、再按 id）与筛选。
func TestListSortAndFilter(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	mk := func(name string, priceCent int64, sort int, enabled bool) *Item {
		it, _, err := svc.Create(ctx, Input{
			Name: name, PriceCent: priceCent, SortOrder: iptr(sort), Enabled: bptr(enabled),
		})
		if err != nil {
			t.Fatalf("创建失败: %v", err)
		}
		return it
	}
	mk("便宜包", 7200, 20, true)
	mk("贵包", 43200, 10, true)
	mk("同价A", 21600, 20, true)
	mk("同价B", 21600, 20, true)
	mk("停用包", 10000, 30, false)

	items, total, err := svc.List(ctx, ListInput{Limit: 20})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 5 {
		t.Fatalf("total 应为 5，实际 %d", total)
	}
	// 期望顺序：贵包(10) → 便宜包(20,¥72) → 同价A(20,¥216,id 小) → 同价B(20,¥216) → 停用包(30)
	// 同 sort_order 时按价格升序，再按 id —— 与门户 ORDER BY 完全一致。
	got := []string{items[0].Name, items[1].Name, items[2].Name, items[3].Name, items[4].Name}
	want := []string{"贵包", "便宜包", "同价A", "同价B", "停用包"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序不符: got %v want %v", got, want)
		}
	}
	// 只取启用的
	on, _, err := svc.List(ctx, ListInput{Enabled: "1", Limit: 20})
	if err != nil || len(on) != 4 {
		t.Fatalf("启用筛选不符: n=%d err=%v", len(on), err)
	}
	// 关键词
	kw, _, err := svc.List(ctx, ListInput{Keyword: "同价", Limit: 20})
	if err != nil || len(kw) != 2 {
		t.Fatalf("关键词筛选不符: n=%d err=%v", len(kw), err)
	}
}

// TestOrderCountsIncludesDeleted 引用计数必须含软删订单（与删除保护同口径）。
func TestOrderCountsIncludesDeleted(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	it, _, err := svc.Create(ctx, Input{Name: "计数包", PriceCent: 7200})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	seedOrder(t, gdb, it.ID, "paid", false)
	seedOrder(t, gdb, it.ID, "pending", true)

	counts, err := svc.OrderCounts(ctx, []uint64{it.ID})
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if counts[it.ID] != 2 {
		t.Fatalf("应计 2 笔（含软删），实际 %d", counts[it.ID])
	}
	items, _, err := svc.List(ctx, ListInput{Limit: 20})
	if err != nil || items[0].OrderCount != 2 {
		t.Fatalf("列表 order_count 应与删除保护同口径: %+v err=%v", items[0].OrderCount, err)
	}
}

// TestSetEnabledIdempotent 启停幂等；不存在的 id → 404。
func TestSetEnabledIdempotent(t *testing.T) {
	gdb := withTestDB(t)
	svc := New(gdb)
	ctx := context.Background()

	it, _, err := svc.Create(ctx, Input{Name: "启停包", PriceCent: 7200})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	for i := 0; i < 2; i++ {
		got, err := svc.SetEnabled(ctx, it.ID, false)
		if err != nil {
			t.Fatalf("第 %d 次停用失败: %v", i+1, err)
		}
		if got.Enabled {
			t.Fatalf("应已停用")
		}
	}
	wantCode(t, func() error { _, err := svc.SetEnabled(ctx, 999999, false); return err }(), errs.CodeNotFound)
}

// TestCreateDoesNotStampCurrency 套餐**不**带币种：全站只有一个锁定币种。
//
// 旧实现给 packages 存 currency 并据此过滤「异币种套餐」，前提是"同一套餐可能
// 以不同币种售卖"。现在币种是站点级锁定值，price_cent 天然就是该币种的金额 ——
// 再存一份 currency 只会造出「套餐标 USD、站点已切 CNY」这种自相矛盾的行，
// 门户按币种过滤后整排套餐静默消失，而管理台显示"上架中"。
func TestCreateDoesNotStampCurrency(t *testing.T) {
	gdb := withTestDB(t)
	restore := conf.SetBillingForTest(conf.CurrencyUSD, conf.DefaultCnyPerUsd)
	defer restore()
	svc := New(gdb)
	// 套餐名 ≤32 字符：拼 t.Name() 会超长，被名称校验先拦下（与币种无关）。
	it, _, err := svc.Create(context.Background(), Input{Name: "美元包", PriceCent: 1000})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	// 断言列不存在：这才是「币种不进套餐」的硬证据（值相同说服不了人）。
	if gdb.Migrator().HasColumn(&model.Package{}, "currency") {
		t.Fatal("packages 不该有 currency 列：币种是站点级唯一值")
	}
	// 金额换算与币种无关：1000 分 = 10.00 → 10,000,000 微。
	if it.Base != 10_000_000 {
		t.Fatalf("到账基数应为 10,000,000 微，实际 %d", it.Base)
	}
}
