package order

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/service/payment"
)

// ---------- 无 DB 的纯函数用例 ----------

// ---------- 数据库集成用例的公共设施（需 LD_TEST_DSN；未设置则跳过） ----------

const testSchema = "ld_test_order"

// withTestDB 在独立 schema 里建表并接管 db.DB，结束后还原。
//
// 用独立 schema 而不是清表：order 包的用例会并发建订单/退款流水，
// 清表会让"上一个用例的残留"变成"下一个用例的脏数据"，症状是偶发唯一键冲突。
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
	// 支付三张辅助表（去重/事件/孤儿）必须一起迁：settle 在事务里写 payment_dedup，
	// 少一张表时回调会整体回滚并以 NotifyRetry 结束 —— 表现成「回调失败」。
	// affiliate_rewards 同理：退款成功时会回收该订单发出的邀请奖励，
	// 少这张表会让退款事务在回收那一步报错、整笔回滚（看上去像「退款失败」）。
	if err := gdb.AutoMigrate(&model.User{}, &model.APIKey{}, &model.Order{},
		&model.Package{}, &model.Transaction{}, &model.Usage{},
		&model.PaymentDedup{}, &model.PaymentEvent{}, &model.PayOrphanNotify{},
		&model.AffiliateReward{}); err != nil {
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

// setCfg 写一行系统配置（值按 JSON 编码，与 conf.System().Set 的入参一致）。
func setCfg(key string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("setCfg: " + err.Error())
	}
	conf.System().Set(key, raw)
}

// setupWalletConfig 固定结算币种为 CNY。
//
// 「充值倍率 / 刻度」已随计费重构删除，这里只剩币种与汇率：
// 断言里写死 ¥ 与 scale，就必须先把口径钉住，否则开发机配成 USD 会一片红。
// setupWalletConfig 准备「能真的下单」的最小配置。
//
// 必须同时备好支付渠道：钱包配置只决定币种与汇率，而 Create 要先 resolveChannel
// 才能生成收银台地址。只配币种时下单直接报「当前无可用的支付方式」，
// 而这个失败看起来像"钱包坏了"，其实是测试环境少了一张渠道表。
func setupWalletConfig(t *testing.T) {
	t.Helper()
	conf.SetBillingForTest(conf.CurrencyCNY, 7.2)
	// 渠道密钥固定为 "secret"：下面各用例的验签都按它签名，
	// 用 chanRow 的默认 "k" 会让回调永远验签失败（表现成「回调失败」而不是「配置不对」）。
	ch := chanRow("epay", "易支付", "epay", true, "微信支付")
	ch["params"] = map[string]any{
		"pay_address": "https://pay.example.com", "pid": "1001", "key": "secret",
	}
	setPayChannels(t, []map[string]any{ch})
	// 回调地址由 site.api_base_url 拼出来；缺了它 payURLs 直接回「支付方式配置有误」。
	raw, err := json.Marshal(map[string]any{
		"site_url": "https://www.example.com", "api_base_urls": []string{"https://api.example.com"},
	})
	if err != nil {
		t.Fatalf("序列化 site 配置失败: %v", err)
	}
	prev, had := conf.System().Get("site")
	conf.System().Set("site", raw)
	t.Cleanup(func() {
		if had {
			conf.System().Set("site", prev)
		}
	})
}

// seedUser 造一个可下单的用户（用户名/邀请码唯一，避免用例之间撞唯一键）。
func seedUser(t *testing.T, gdb *gorm.DB, balance int64) *model.User {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	u := &model.User{
		Username: "order-" + suffix, PasswordHash: "x",
		Balance: balance, Group: "default", AffCode: "oa" + suffix,
	}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	return u
}

// TestWalletConfigShape 校验钱包页配置的对外口径。
//
// 「充值倍率 / 刻度」这两个概念已删除：现在到账就是 分×1e4 + 赠送，
// 前端只需要 scale 一个换算常量。这条用例守的是"该下发的三个常量都在且自洽"，
// 而不是某个可配倍率的默认值（那个概念已不存在）。
func TestWalletConfigShape(t *testing.T) {
	cfg := Config(0)
	if cfg.Scale != money.Scale {
		t.Fatalf("Scale 必须是 money.Scale(%d)，实际 %d", money.Scale, cfg.Scale)
	}
	if cfg.Currency == "" || cfg.CurrencySymbol == "" {
		t.Fatalf("币种与符号不能为空：%+v", cfg)
	}
	if cfg.CNYPerUSD <= 0 {
		t.Fatalf("汇率必须为正：%v", cfg.CNYPerUSD)
	}
	// 余额上限 0 = 不限；传 0 时必须回落到全局配置而不是恒 0。
	if cfg.BalanceLimit < 0 {
		t.Fatalf("余额上限不能为负：%d", cfg.BalanceLimit)
	}
}

// TestPackageAmountsExact 锁定「分 → 微」是**精确整数换算**，全程没有倍率与浮点。
//
// 旧实现走 quota.Basis（倍率 × 刻度）再四舍五入，同一笔钱在不同倍率下会差 1 分；
// 现在只剩一条算式：base = 分 × 1e4，credit = base + bonus（bonus 本身就是微）。
func TestPackageAmountsExact(t *testing.T) {
	for _, priceCent := range []int64{1, 100, 7200, 21600, 43200, 216000, 1234567, 9_999_999} {
		for _, bonus := range []int64{0, 96, 500000} {
			base, err := money.FromCents(priceCent)
			if err != nil {
				t.Fatalf("FromCents(%d) 报错: %v", priceCent, err)
			}
			if base != priceCent*money.MicroPerCent {
				t.Fatalf("分→微 不是精确换算：%d 分 → %d 微", priceCent, base)
			}
			credit, err := money.Sum(base, bonus)
			if err != nil {
				t.Fatalf("Sum 报错: %v", err)
			}
			if credit != base+bonus {
				t.Fatalf("到账应为 本金+赠送：%d+%d → %d", base, bonus, credit)
			}
		}
	}
	// 越界必须报错，绝不回绕成负数或小数。
	if _, err := money.FromCents(money.MaxAmount); err == nil {
		t.Fatal("分→微 超上限应报错")
	}
}

// TestExpirePendingProtocolSets 是「关单策略按协议集合分支」的回归。
//
// 三个必须同时成立的结论：
//  1. 没有查单能力的协议（epay / 空协议）到期即关 —— 否则永久 pending；
//  2. 有查单能力的协议（wxpay）到期**不能**立即关 —— 否则「用户刚付完、订单已过期」；
//  3. 同一笔 wxpay 过了 30 分钟补查窗口后仍要被关 —— 否则僵尸单永远挂着。
func TestExpirePendingProtocolSets(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	ctx := context.Background()
	svc := New(gdb)
	u := seedUser(t, gdb, 0)

	now := time.Now()
	past := now.Add(-10 * time.Minute)
	insert := func(no, protocol string, expire time.Time) *model.Order {
		o := &model.Order{
			UserID: u.ID, OutTradeNo: no, AmountCent: 100, Credited: 100,
			Status: "pending", PayProtocol: protocol, ExpireAt: &expire,
		}
		if err := gdb.Create(o).Error; err != nil {
			t.Fatalf("插入订单失败: %v", err)
		}
		return o
	}

	empty := insert("LD-T-EMPTY", "", past)
	epay := insert("LD-T-EPAY", string(payment.ProtocolEpay), past)
	wxFresh := insert("LD-T-WX-FRESH", string(payment.ProtocolWxpay), past)

	if _, err := svc.ExpirePending(ctx); err != nil {
		t.Fatalf("关单失败: %v", err)
	}
	status := func(id uint64) string {
		var o model.Order
		if err := gdb.Select("status").First(&o, id).Error; err != nil {
			t.Fatalf("读取订单失败: %v", err)
		}
		return o.Status
	}
	if status(empty.ID) != "expired" {
		t.Fatalf("空协议订单到期应即关（否则永久 pending），实际 %s", status(empty.ID))
	}
	if status(epay.ID) != "expired" {
		t.Fatalf("易支付订单到期应即关，实际 %s", status(epay.ID))
	}
	if status(wxFresh.ID) != "pending" {
		t.Fatalf("微信订单到期后仍在补查窗口内，不能被关，实际 %s", status(wxFresh.ID))
	}

	// 超过补查窗口（30 分钟）后才允许强关。
	if err := gdb.Model(&model.Order{}).Where("id = ?", wxFresh.ID).
		Update("expire_at", now.Add(-40*time.Minute)).Error; err != nil {
		t.Fatalf("回拨失败: %v", err)
	}
	if _, err := svc.ExpirePending(ctx); err != nil {
		t.Fatalf("关单失败: %v", err)
	}
	if status(wxFresh.ID) != "expired" {
		t.Fatalf("超过补查窗口的微信订单必须被关，实际 %s", status(wxFresh.ID))
	}
}

// TestSettleCurrencyMismatchRejected 覆盖 H1 币种比对：金额相同但币种不符的回调必须转人工，
// 不得入账（¥72 与 $72 是 7.2 倍差异）。
func TestSettleCurrencyMismatchRejected(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	ctx := context.Background()
	svc := New(gdb)
	u := seedUser(t, gdb, 0)

	o := &model.Order{
		UserID: u.ID, OutTradeNo: "LD-CUR-MISMATCH", AmountCent: 7200,
		Credited: 4999968, Currency: "CNY", Status: "pending",
		PayChannelID: "epay", PayProtocol: string(payment.ProtocolEpay),
	}
	if err := gdb.Create(o).Error; err != nil {
		t.Fatalf("插入订单失败: %v", err)
	}

	// 回调上报 USD（金额数字一致），必须被币种比对拦下转人工。
	ch := payment.Channel{ID: "epay", Protocol: "epay"}
	n := &payment.Notification{
		OutTradeNo: o.OutTradeNo, TradeNo: "T-CUR", AmountCent: 7200, Currency: "USD",
		Kind: payment.NotifyPaid, Raw: map[string]string{"trade_no": "T-CUR"},
	}
	if r := svc.settle(ctx, ch, n, "notify"); r != NotifyNeedManual {
		t.Fatalf("币种不符回调应转人工，实际 %v", r)
	}
	var after model.Order
	_ = gdb.First(&after, o.ID)
	if after.Status != "need_manual" {
		t.Fatalf("币种不符回调不应入账，status=%s", after.Status)
	}
	var user model.User
	_ = gdb.First(&user, u.ID)
	if user.Balance != 0 {
		t.Fatalf("币种不符回调不得入账，balance=%d", user.Balance)
	}
}

// TestReplayDoesNotOverwriteNotifyRaw 覆盖 H2：同一订单的重复支付回调必须幂等短路，
// 不得走 dedup 转 need_manual、不得覆盖原始支付证据 notify_raw。
func TestReplayDoesNotOverwriteNotifyRaw(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	ctx := context.Background()
	svc := New(gdb)
	u := seedUser(t, gdb, 0)

	p := &model.Package{Name: "体验包", PriceCent: 7200, Bonus: 0, Enabled: true}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("造套餐失败: %v", err)
	}
	res, err := svc.Create(ctx, CreateInput{UserID: u.ID, PackageID: p.ID})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	// 首次回调：带 trade_no，正常入账，notify_raw 落原始证据。
	params := map[string]string{"out_trade_no": res.OutTradeNo, "trade_no": "T-REPLAY",
		"trade_status": "SUCCESS", "money": "72.00"}
	params["sign"] = payment.Sign(params, "secret")
	if r := svc.HandleNotify(ctx, "epay", params); r != NotifyDone {
		t.Fatalf("首次回调失败: %v", r)
	}
	var first model.Order
	_ = gdb.First(&first, res.ID)
	if first.Status != "paid" || first.NotifyRaw == "" {
		t.Fatalf("首次入账后状态/证据不符: status=%s raw=%q", first.Status, first.NotifyRaw)
	}
	rawBefore := first.NotifyRaw

	// 重复回调（同 trade_no）：必须幂等短路，不覆盖 notify_raw、不转 need_manual。
	if r := svc.HandleNotify(ctx, "epay", params); r != NotifyDone {
		t.Fatalf("重复回调应幂等短路，实际 %v", r)
	}
	var after model.Order
	_ = gdb.First(&after, res.ID)
	if after.Status != "paid" {
		t.Fatalf("重复回调不得改状态，status=%s", after.Status)
	}
	if after.NotifyRaw != rawBefore {
		t.Fatalf("重复回调不得覆盖原始证据: before=%q after=%q", rawBefore, after.NotifyRaw)
	}
}
