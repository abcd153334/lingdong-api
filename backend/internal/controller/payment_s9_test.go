package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
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
	"lingdong-api/internal/service/order"
	"lingdong-api/internal/service/payment"
)

// 本文件覆盖 HTTP 层的回调接线：按渠道验签、入账、金额严格校验、重放幂等。
// service 层等价路径由 service/order/order_test.go 覆盖，此处验证路由与应答体。

const notifyTestSchema = "ld_test_s9_notify"

// withNotifyDB 在独立 schema 中准备回调所需表，并把 db.DB 指向该连接（controller 读取 db.DB）。
func withNotifyDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LD_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 LD_TEST_DSN，跳过 HTTP 回调集成用例")
	}
	silent := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), silent)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + notifyTestSchema).Error; err != nil {
		t.Fatalf("创建测试 schema 失败: %v", err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	gdb, err := gorm.Open(postgres.Open(dsn+sep+"search_path="+notifyTestSchema), silent)
	if err != nil {
		t.Fatalf("连接测试 schema 失败: %v", err)
	}
	if err := gdb.AutoMigrate(&model.User{}, &model.Package{}, &model.Order{},
		&model.Transaction{}, &model.PaymentDedup{}, &model.PayOrphanNotify{}); err != nil {
		t.Fatalf("迁移测试表失败: %v", err)
	}
	prev := db.DB
	db.DB = gdb
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec("DROP SCHEMA IF EXISTS " + notifyTestSchema + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
		db.DB = prev
	})
	return gdb
}

func setNotifyCfg(key string, v any) {
	raw, _ := json.Marshal(v)
	conf.System().Set(key, raw)
}

// notifyCfg 写入一份可用的支付/钱包配置（一个启用的易支付渠道，密钥 secret）。
//
// 渠道 ID 必须是 "epay"：下单时前端会传 `epay:微信支付`，而 FindChannel 按 ID 精确匹配。
func notifyCfg() {
	setNotifyCfg("pay_channels", []payment.Channel{{
		ID: "epay", Name: "易支付", Protocol: "epay", Enabled: true, Methods: []string{"微信支付"},
		Params: payment.ChannelParams{PayAddress: "https://pay.example.com", PID: "1001", Key: "secret"},
	}})
	// 余额上限也是**微**：5e8 微 = ¥500，必须大于下面套餐的到账额（¥72.50）。
	// 旧口径写 5e7 时等于 ¥50，套餐一入账就越限 —— 限额不是"宽松与否"的问题，
	// 而是单位换了之后数字没跟着换。
	setNotifyCfg("balance_limit", int64(500_000_000))
	setNotifyCfg("site", map[string]any{"api_base_urls": []string{"https://api.example.com"}, "site_url": "https://www.example.com"})
}

// postNotify 以表单编码 POST 到带渠道的回调地址。
func postNotify(r *gin.Engine, channel string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/pay/"+channel+"/notify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func signedForm(params map[string]string, key string) url.Values {
	params["sign"] = payment.Sign(params, key)
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	return form
}

func notifyUserQuota(t *testing.T, gdb *gorm.DB, uid uint64) int64 {
	t.Helper()
	var u model.User
	if err := gdb.First(&u, uid).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	return u.Balance
}

func newNotifyUser(t *testing.T, gdb *gorm.DB) *model.User {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	u := &model.User{Username: "n" + suffix, PasswordHash: "x", Balance: 0, Group: "default", AffCode: "aff" + suffix}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	return u
}

// TestPayNotifyHTTP 覆盖：篡改签名 → fail 且不入账；少付 1 分 → success 但转人工；
// 正确签名 → success 且入账；同表单重放 → 仍 success 且不重复入账。
func TestPayNotifyHTTP(t *testing.T) {
	gdb := withNotifyDB(t)
	notifyCfg()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/pay/:channel/notify", PayNotifyChannel)

	p := &model.Package{Name: "体验包", PriceCent: 7200, Bonus: 500000, Enabled: true}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("造套餐失败: %v", err)
	}
	u := newNotifyUser(t, gdb)

	res, err := order.New(gdb).Create(context.Background(), order.CreateInput{UserID: u.ID, PackageID: p.ID})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	// 基础 = 7200 分 × 1e4 = 72,000,000 微；到账 = 基础 + 赠送 500000。
	// （旧口径 72×69444 那套"刻度倍率"已删除，现在是精确的 分→微 换算。）
	if res.Credited != 72_500_000 {
		t.Fatalf("到账金额不符: %d", res.Credited)
	}
	if res.PayChannelID != "epay" || res.PayProtocol != "epay" {
		t.Fatalf("下单未记录稳定渠道标识: %+v", res)
	}

	// 1) 篡改签名（用错误密钥计算）：回 fail，且不入账
	bad := signedForm(map[string]string{
		"out_trade_no": res.OutTradeNo, "trade_no": "N-bad",
		"trade_status": "SUCCESS", "money": "72.00",
	}, "wrong-key")
	if got := postNotify(r, "epay", bad).Body.String(); got != "fail" {
		t.Fatalf("篡改签名应回 fail，实际 %q", got)
	}
	if q := notifyUserQuota(t, gdb, u.ID); q != 0 {
		t.Fatalf("验签失败不应入账，quota=%d", q)
	}

	// 2) 少付 1 分：严格相等，回 success（止血）但转 need_manual 且不入账。
	//    这是原实现的定价漏洞：±1 分容差让「少付 1 分」也算付清。
	res2, err := order.New(gdb).Create(context.Background(), order.CreateInput{UserID: u.ID, PackageID: p.ID})
	if err != nil {
		t.Fatalf("第二单下单失败: %v", err)
	}
	short := signedForm(map[string]string{
		"out_trade_no": res2.OutTradeNo, "trade_no": "N-short",
		"trade_status": "SUCCESS", "money": "71.99",
	}, "secret")
	if got := postNotify(r, "epay", short).Body.String(); got != "success" {
		t.Fatalf("金额不符应回 success（已转人工），实际 %q", got)
	}
	var o2 model.Order
	if err := gdb.First(&o2, res2.ID).Error; err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if o2.Status != "need_manual" {
		t.Fatalf("金额不符应转 need_manual，实际 %s", o2.Status)
	}
	if q := notifyUserQuota(t, gdb, u.ID); q != 0 {
		t.Fatalf("金额不符不应入账，quota=%d", q)
	}

	// 3) 正确签名：回 success，入账 + 订单转 paid + recharge 流水恰 1 条
	//    这里用 TRADE_SUCCESS 而不是 SUCCESS：那是易支付系的真实取值，
	//    用它端到端跑通才能守住「回调状态映射」不被改回只认 SUCCESS。
	good := signedForm(map[string]string{
		"out_trade_no": res.OutTradeNo, "trade_no": "N-ok",
		"trade_status": "TRADE_SUCCESS", "money": "72.00",
	}, "secret")
	if got := postNotify(r, "epay", good).Body.String(); got != "success" {
		t.Fatalf("正确签名应回 success，实际 %q", got)
	}
	var o model.Order
	if err := gdb.First(&o, res.ID).Error; err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if o.Status != "paid" || o.PaidAt == nil {
		t.Fatalf("订单未转 paid: status=%s paid_at=%v", o.Status, o.PaidAt)
	}
	if o.NotifyCount < 1 {
		t.Fatalf("回调次数未记录: %d", o.NotifyCount)
	}
	// 到账 = 售价 7200 分 → 72,000,000 微 + 赠送 500,000 微 = 72,500,000 微。
	// 旧值的 5,499,968 是「72 × 69444 + 赠送」的刻度产物，刻度已删除。
	if q := notifyUserQuota(t, gdb, u.ID); q != 72_500_000 {
		t.Fatalf("到账金额不符: %d", q)
	}
	var txCount int64
	if err := gdb.Model(&model.Transaction{}).Where("user_id = ? AND type = ?", u.ID, "recharge").Count(&txCount).Error; err != nil {
		t.Fatalf("统计流水失败: %v", err)
	}
	if txCount != 1 {
		t.Fatalf("recharge 流水应恰 1 条，实际 %d", txCount)
	}

	// 4) 同表单重放：仍回 success 且不重复入账（订单终态 + payment_dedup 双保险）
	if got := postNotify(r, "epay", good).Body.String(); got != "success" {
		t.Fatalf("重放应回 success，实际 %q", got)
	}
	if q := notifyUserQuota(t, gdb, u.ID); q != 72_500_000 {
		t.Fatalf("重放导致重复入账: balance=%d", q)
	}
	if err := gdb.Model(&model.Transaction{}).Where("user_id = ? AND type = ?", u.ID, "recharge").Count(&txCount).Error; err != nil {
		t.Fatalf("统计流水失败: %v", err)
	}
	if txCount != 1 {
		t.Fatalf("重放后流水应仍为 1 条，实际 %d", txCount)
	}
	// 重放也要计数：这是「上游仍在重试」的唯一可观测信号
	if err := gdb.First(&o, res.ID).Error; err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if o.NotifyCount < 2 {
		t.Fatalf("重复回调也应计数，实际 %d", o.NotifyCount)
	}
}

// TestPayNotifyOrphan 验签通过但订单不存在：必须回 success 并落孤儿记录。
//
// 回 fail 会让网关重试 6 次后放弃，而本地根本没有订单可变出来 —— 真正的止血是「应答 success +
// 留下可查的证据」，否则我方丢单完全不可观测。
func TestPayNotifyOrphan(t *testing.T) {
	gdb := withNotifyDB(t)
	notifyCfg()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/pay/:channel/notify", PayNotifyChannel)

	form := signedForm(map[string]string{
		"out_trade_no": "LD-NOT-EXIST", "trade_no": "N-orphan",
		"trade_status": "SUCCESS", "money": "72.00",
	}, "secret")
	if got := postNotify(r, "epay", form).Body.String(); got != "success" {
		t.Fatalf("订单不存在应回 success（止住重试），实际 %q", got)
	}
	var n int64
	if err := gdb.Model(&model.PayOrphanNotify{}).Where("out_trade_no = ?", "LD-NOT-EXIST").Count(&n).Error; err != nil {
		t.Fatalf("统计孤儿记录失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应落 1 条孤儿记录，实际 %d", n)
	}
}

// TestPayNotifyUnknownChannel 渠道不存在时不能验签，必须回 fail 且不落任何记录。
func TestPayNotifyUnknownChannel(t *testing.T) {
	gdb := withNotifyDB(t)
	notifyCfg()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/pay/:channel/notify", PayNotifyChannel)

	form := signedForm(map[string]string{
		"out_trade_no": "LD-ANY", "trade_no": "N-any",
		"trade_status": "SUCCESS", "money": "1.00",
	}, "secret")
	if got := postNotify(r, "nope", form).Body.String(); got != "fail" {
		t.Fatalf("未知渠道应回 fail，实际 %q", got)
	}
	var n int64
	_ = gdb.Model(&model.PayOrphanNotify{}).Count(&n).Error
	if n != 0 {
		t.Fatalf("未验签的请求不得写孤儿表，实际 %d 条", n)
	}
}
