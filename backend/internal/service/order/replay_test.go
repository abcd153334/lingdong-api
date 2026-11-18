package order

// 本文件守的是**开放面下单的幂等分支**（`CreateForClient` -> `replayByClientOrderNo`）。
//
// 为什么单独一个文件而不是塞进 order_test.go：这条分支的每一条出口都对应用户的钱，
// 而且三条出口**看起来**都能"返回一个 200"，只有断言具体字段才能区分它们：
//
//	pending + 有支付信息 -> 必须带回原 pay_url（否则客户端拿不到链接也不会重试）
//	pending + 无支付信息 -> 必须尝试 RetryPay（否则空链接永远修不好）
//	非 pending           -> 必须带回真实 status（否则已付的单被当成待付再付一次）
//
// 这里**不打上游**：第 3 条用 pay_attempts 打满的方式让 RetryPay 在触网前就返回，
// 用例才是确定性的。真正的上游重试由 RetryPay 自己的用例覆盖。

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// seedClientOrder 造一笔带幂等号的订单。
func seedClientOrder(t *testing.T, gdb *gorm.DB, userID uint64, no, status, payURL string) *model.Order {
	t.Helper()
	expire := time.Now().Add(10 * time.Minute)
	o := &model.Order{
		UserID: userID, OutTradeNo: "LD" + no,
		AmountCent: 1000, Credited: 10_000_000, Status: status,
		Currency: "CNY", ClientOrderNo: no,
		PayChannel: "易支付", PayChannelID: "epay", PayMethod: "微信支付", PayProtocol: "epay",
		PayURL: payURL, ExpireAt: &expire,
	}
	if err := gdb.Create(o).Error; err != nil {
		t.Fatalf("造订单失败: %v", err)
	}
	return o
}

// TestReplayPendingWithPayInfo 已有支付信息的 pending 单必须原样带回支付链接。
//
// 这是"响应丢了重发"最常见的一种：首次下单其实成功了，客户端没收到。
// 此时如果重新向上游建单，用户会看到两个支付页、付任意一个都可能重复到账。
func TestReplayPendingWithPayInfo(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	setupWalletConfig(t)

	u := seedUser(t, gdb, 0)
	o := seedClientOrder(t, gdb, u.ID, "ci-pending-pay", "pending", "https://pay.example.com/cashier/abc")

	res, err := New(gdb).replayByClientOrderNo(ctx, u.ID, 0, "ci-pending-pay")
	if err != nil {
		t.Fatalf("回查不应失败: %v", err)
	}
	if res.ID != o.ID || res.OutTradeNo != o.OutTradeNo {
		t.Fatalf("必须返回原单（id=%d out_trade_no=%s），实际 id=%d out_trade_no=%s",
			o.ID, o.OutTradeNo, res.ID, res.OutTradeNo)
	}
	if res.Status != "pending" {
		t.Errorf("status 应为 pending，实际 %q", res.Status)
	}
	if res.PayURL != o.PayURL {
		t.Errorf("pay_url 必须原样带回，期望 %q，实际 %q", o.PayURL, res.PayURL)
	}
}

// TestReplayPaidReturnsRealStatus 已支付的单必须回报 paid。
//
// 客户端拿到 paid 才会停止支付流程。若这里因为"只返回支付信息"而丢掉状态，
// 用户会对一笔已到账的充值再付一次 —— 多付的钱要么人工退，要么变成 need_manual。
func TestReplayPaidReturnsRealStatus(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	setupWalletConfig(t)

	u := seedUser(t, gdb, 0)
	seedClientOrder(t, gdb, u.ID, "ci-paid", "paid", "")

	res, err := New(gdb).replayByClientOrderNo(ctx, u.ID, 0, "ci-paid")
	if err != nil {
		t.Fatalf("回查不应失败: %v", err)
	}
	if res.Status != "paid" {
		t.Fatalf("status 应为 paid，实际 %q", res.Status)
	}
	if res.PayURL != "" {
		t.Errorf("已支付订单不应再给支付链接，实际 %q", res.PayURL)
	}
}

// TestReplayPendingWithoutPayInfoFallsBack 上游建单失败过的 pending 单：
// 必须尝试重试，且重试失败时仍然返回订单本身（而不是抛错）。
//
// pay_attempts 打到上限后 RetryPay 在**触网之前**就返回 PayGatewayError，
// 用例因此是确定性的。此时期望：不报错、status 仍是 pending、pay_error 可见。
// 客户端据此判断"该换新号重下"，错误码做不到这一点。
func TestReplayPendingWithoutPayInfoFallsBack(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	setupWalletConfig(t)

	u := seedUser(t, gdb, 0)
	seedClientOrder(t, gdb, u.ID, "ci-nopay", "pending", "")
	if err := gdb.Model(&model.Order{}).
		Where("client_order_no = ?", "ci-nopay").
		Updates(map[string]any{
			"pay_attempts": maxPayAttempts,
			"pay_error":    "upstream: connect timeout",
		}).Error; err != nil {
		t.Fatalf("设置重试次数失败: %v", err)
	}

	res, err := New(gdb).replayByClientOrderNo(ctx, u.ID, 0, "ci-nopay")
	if err != nil {
		t.Fatalf("重试失败也必须返回订单而不是错误: %v", err)
	}
	if res.Status != "pending" {
		t.Errorf("status 应为 pending，实际 %q", res.Status)
	}
	if res.PayError == "" {
		t.Errorf("应带回 pay_error，否则客户端无法判断该换新单号: %+v", res)
	}
}

// TestReplayUnknownOrderReturnsError 幂等号查不到时必须报错，而不是编一张空单。
//
// 这条分支理论上不可达（唯一冲突刚发生，行必然存在）。真出现说明行被物理删除，
// 此时**必须**暴露成错误：返回一张 amount=0 的空单会让客户端以为下单成功。
func TestReplayUnknownOrderReturnsError(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	setupWalletConfig(t)

	u := seedUser(t, gdb, 0)
	if _, err := New(gdb).replayByClientOrderNo(ctx, u.ID, 0, "ci-absent"); err == nil {
		t.Fatal("查不到订单时必须返回错误")
	}
}

// TestReplayIsScopedToUser 幂等号必须按 (user_id, client_order_no) 命中。
//
// 只按号查会让 A 用户拿到 B 用户的订单（含支付链接与金额）—— 幂等号是
// 调用方自己编的，两个应用很容易撞同一个 "order-1"。
func TestReplayIsScopedToUser(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	setupWalletConfig(t)

	owner := seedUser(t, gdb, 0)
	other := seedUser(t, gdb, 0)
	seedClientOrder(t, gdb, owner.ID, "ci-shared", "pending", "https://pay.example.com/o")

	if _, err := New(gdb).replayByClientOrderNo(ctx, other.ID, 0, "ci-shared"); err == nil {
		t.Fatal("跨用户同号必须查不到（否则会泄露他人订单与支付链接）")
	}
}

// TestReplayIsScopedToApp 幂等号必须按应用隔离。
//
// 幂等号是**调用方自己编的**，两个应用很容易都用 "order-1"。只按 user 回查时，
// 应用 A 提交一个应用 B 用过的号会撞唯一索引，然后拿回 B 的订单
// （金额、状态、pay_url）—— 这是跨应用信息泄露，而不是"幂等"。
func TestReplayIsScopedToApp(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	setupWalletConfig(t)

	u := seedUser(t, gdb, 0)
	o := seedClientOrder(t, gdb, u.ID, "ci-app-shared", "pending", "https://pay.example.com/b")
	if err := gdb.Model(&model.Order{}).Where("id = ?", o.ID).Update("app_id", 2).Error; err != nil {
		t.Fatalf("设置订单归属应用失败: %v", err)
	}

	// 同用户的另一个应用：必须查不到（否则会把应用 B 的订单返回给应用 A）。
	if _, err := New(gdb).replayByClientOrderNo(ctx, u.ID, 3, "ci-app-shared"); err == nil {
		t.Fatal("跨应用同号必须查不到（否则会泄露另一应用的订单与支付链接）")
	}
	// 本应用仍要能命中，幂等语义不能因此退化。
	res, err := New(gdb).replayByClientOrderNo(ctx, u.ID, 2, "ci-app-shared")
	if err != nil {
		t.Fatalf("本应用的回查不应失败: %v", err)
	}
	if res.ID != o.ID {
		t.Fatalf("应返回本应用的原单 id=%d，实际 %d", o.ID, res.ID)
	}
}
