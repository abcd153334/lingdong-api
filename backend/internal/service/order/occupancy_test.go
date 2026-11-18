package order

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
)

// TestPlaceOrderCountsPendingOccupancy 在途未支付订单必须占额。
//
// 旧实现只算「当前余额 + 本单」：用户可以连下 N 笔各接近上限的订单再一起支付，
// 每笔下单都已通过校验，而支付回调里后面的全部命中「到账将超出余额上限」→
// 转 need_manual：**钱收了、额度不到账**，只能人工一单单退。
//
// 这条用例只覆盖「拒绝」分支（在下单闸就返回，不触上游），
// 目的就是把「在途占额」这个语义钉住。
func TestPlaceOrderCountsPendingOccupancy(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	setupXunhuChannel()
	setCfg("site", map[string]any{
		"api_base_urls": []string{"https://api.example.com"},
		"site_url":      "https://example.com",
	})
	setCfg("custom_topup_enabled", true)

	const unit = int64(1_000_000) // 1 元
	const capLimit = 100 * unit   // 账户余额上限 100 元

	u := seedUser(t, gdb, 0)
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).
		Update("balance_limit", capLimit).Error; err != nil {
		t.Fatalf("设置用户上限失败: %v", err)
	}

	// 一笔在途订单占掉 80 元
	pending := &model.Order{
		UserID: u.ID, OutTradeNo: "LDPEND" + time.Now().Format("150405.000000"),
		AmountCent: 8000, Credited: 80 * unit, Status: "pending",
		PayChannelID: "xunhupay", PayProtocol: "xunhupay",
	}
	if err := gdb.Create(pending).Error; err != nil {
		t.Fatalf("造在途订单失败: %v", err)
	}

	svc := New(gdb)
	// 80（在途）+ 30（本单）> 100 → 必须拒绝
	_, err := svc.Create(ctx, CreateInput{
		UserID: u.ID, Amount: 30 * unit, ChannelID: "xunhupay", Method: "聚合支付",
	})
	if err == nil {
		t.Fatal("在途占额应导致下单被拒，实际成功")
	}
	if !isErr(err, errs.BalanceLimitExceeded) {
		t.Fatalf("应返回余额上限错误，实际 %v", err)
	}

	// 关键：被拒的单不能落库（否则每次失败都留一条 pending 噪点，越拦越多）
	var n int64
	if err := gdb.Model(&model.Order{}).
		Where("user_id = ? AND status = ?", u.ID, "pending").Count(&n).Error; err != nil {
		t.Fatalf("统计订单失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("被拒的下单不应落库，pending 单数应为 1，实际 %d", n)
	}
}

// TestPendingOccupancyWindowIgnoresStale 超过窗口的僵尸 pending 不得永久锁死用户。
//
// 关单 worker 每 5 分钟跑一次，正常 pending 寿命远小于 2 小时；
// 超过窗口还挂着的单说明 worker 出问题了，此时继续占额会让用户**永远充不进来**。
func TestPendingOccupancyWindowIgnoresStale(t *testing.T) {
	gdb := withTestDB(t)
	setupXunhuChannel()
	setCfg("site", map[string]any{
		"api_base_urls": []string{"https://api.example.com"},
		"site_url":      "https://example.com",
	})
	setCfg("custom_topup_enabled", true)

	const unit = int64(1_000_000)
	u := seedUser(t, gdb, 0)
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).
		Update("balance_limit", 100*unit).Error; err != nil {
		t.Fatalf("设置上限失败: %v", err)
	}

	stale := &model.Order{
		UserID: u.ID, OutTradeNo: "LDSTALE" + time.Now().Format("150405.000000"),
		AmountCent: 9000, Credited: 90 * unit, Status: "pending",
		PayChannelID: "xunhupay", PayProtocol: "xunhupay",
	}
	if err := gdb.Create(stale).Error; err != nil {
		t.Fatalf("造僵尸订单失败: %v", err)
	}
	// 把它推到窗口外（3 小时前）
	if err := gdb.Model(&model.Order{}).Where("id = ?", stale.ID).
		Update("created_at", time.Now().Add(-3*time.Hour)).Error; err != nil {
		t.Fatalf("改创建时间失败: %v", err)
	}

	// 这里只断言「不被在途占额拦住」：用 cap 恰好留出 90 元空间，
	// 若僵尸单仍占额则必然报 BalanceLimitExceeded。
	if err := gdb.Model(&model.User{}).Where("id = ?", u.ID).
		Update("balance_limit", 100*unit).Error; err != nil {
		t.Fatalf("设置上限失败: %v", err)
	}
	svc := New(gdb)
	_, err := svc.Create(context.Background(), CreateInput{
		UserID: u.ID, Amount: 95 * unit, ChannelID: "xunhupay", Method: "聚合支付",
	})
	if err != nil && isErr(err, errs.BalanceLimitExceeded) {
		t.Fatalf("窗口外的僵尸订单不应继续占额: %v", err)
	}
	// 其它错误（上游不可达 / 未配置）是允许的：本用例只关心上限闸。
}

// isErr 判断错误链里是否含目标错误定义。
func isErr(err error, def errs.Def) bool {
	got, ok := errs.AsDef(err)
	return ok && got.Code == def.Code
}
