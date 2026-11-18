package order

import (
	"context"
	"strings"
	"testing"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/payment"
)

// seedPendingOrder 造的订单走真实下单路径（币种/金额/渠道都是生产口径），
// 见各用例里的 svc.Create。

// refundNotifyOf 见 refund_test.go：退款类回调允许不带金额。

// TestRefundNotifyKeepsPaymentRaw 退款回调不得覆盖支付原文。
//
// notify_raw 是「钱怎么进来的」的原始物证。一笔已支付订单随后会收到退款通知，
// 若每次都覆盖，客诉/对账时能看到的只剩那句退款状态，支付报文（渠道、上游交易号、
// 金额、付款方）永久消失。所以只保留首次。
func TestRefundNotifyKeepsPaymentRaw(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	ctx := context.Background()
	svc := New(gdb)
	u := seedUser(t, gdb, 0)

	p := &model.Package{Name: "证据包", PriceCent: 7200, Bonus: 0, Enabled: true}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("造套餐失败: %v", err)
	}
	res, err := svc.Create(ctx, CreateInput{UserID: u.ID, PackageID: p.ID})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	// ① 支付成功：落支付原文。
	params := map[string]string{"out_trade_no": res.OutTradeNo, "trade_no": "T-EVID",
		"trade_status": "SUCCESS", "money": "72.00"}
	params["sign"] = payment.Sign(params, "secret")
	if r := svc.HandleNotify(ctx, "epay", params); r != NotifyDone {
		t.Fatalf("支付回调失败: %v", r)
	}
	var paid model.Order
	_ = gdb.First(&paid, res.ID)
	if paid.NotifyRaw == "" || !strings.Contains(paid.NotifyRaw, "T-EVID") {
		t.Fatalf("支付原文应含上游交易号，实际 %q", paid.NotifyRaw)
	}
	rawBefore := paid.NotifyRaw

	// ② 上游退款回调（我们从未发起退款 → 转人工）：只加告警，不得改原文。
	ch, ok := payment.FindChannel("epay")
	if !ok {
		t.Fatal("测试前提不成立：epay 渠道未配置")
	}
	n := refundNotifyOf(res.OutTradeNo, payment.NotifyRefunded)
	n.Raw = map[string]string{"trade_status": "CD", "out_refund_no": "R-EVID"}
	if r := svc.settle(ctx, ch, n, "notify"); r != NotifyNeedManual {
		t.Fatalf("未发起退款的已支付订单收到退款回调应转人工，实际 %v", r)
	}
	var after model.Order
	_ = gdb.First(&after, res.ID)
	if after.NotifyRaw != rawBefore {
		t.Fatalf("退款回调覆盖了支付原文：before=%q after=%q", rawBefore, after.NotifyRaw)
	}
	if after.RefundStatus != RefundStateUpstreamOnly {
		t.Fatalf("退款事实应另记一列，实际 refund_status=%q", after.RefundStatus)
	}
}

// TestIgnoredNotifyOnPendingIsObservable 覆盖「验签通过、金额对得上、状态不认识」的回调。
//
// 这是最容易静默丢钱的一类：上游新增终态或改了大小写，我们既不认识也不入账，
// 订单永远 pending，钱只在上下游账单里。行为必须可观测（notify_raw 落库 +
// 指标计数），否则只能等客诉。
func TestIgnoredNotifyOnPendingIsObservable(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	ctx := context.Background()
	svc := New(gdb)
	u := seedUser(t, gdb, 0)

	p := &model.Package{Name: "忽略包", PriceCent: 1000, Bonus: 0, Enabled: true}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("造套餐失败: %v", err)
	}
	res, err := svc.Create(ctx, CreateInput{UserID: u.ID, PackageID: p.ID})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	var o model.Order
	_ = gdb.First(&o, res.ID)

	ch, ok := payment.FindChannel("epay")
	if !ok {
		t.Fatal("测试前提不成立：epay 渠道未配置")
	}
	n := &payment.Notification{
		OutTradeNo: o.OutTradeNo, AmountCent: o.AmountCent,
		Kind: payment.NotifyIgnored,
		Raw:  map[string]string{"trade_status": "SUCCESS_V2"},
	}
	// 应答必须是「已完成」：上层不认识状态时不能让它无限重试（重试也不会变认识）。
	if r := svc.settle(ctx, ch, n, "notify"); r != NotifyDone {
		t.Fatalf("不可识别状态的回调应按已完成应答，实际 %v", r)
	}

	var after model.Order
	_ = gdb.First(&after, o.ID)
	if after.Status != "pending" {
		t.Fatalf("不可识别的状态不得推进订单，实际 status=%s", after.Status)
	}
	if after.NotifyCount != 1 {
		t.Fatalf("回调必须计数（否则「上游还在重试」无从发现），实际 %d", after.NotifyCount)
	}
	if !strings.Contains(after.NotifyRaw, "SUCCESS_V2") {
		t.Fatalf("原文必须留痕（这是唯一能还原上游说了什么的证据），实际 %q", after.NotifyRaw)
	}
	var user model.User
	_ = gdb.First(&user, u.ID)
	if user.Balance != 0 {
		t.Fatalf("不可识别的状态不得入账，实际余额 %d", user.Balance)
	}
	if !strings.Contains(metrics.Render(), `pay_notify_dropped_total{channel="epay"`) {
		t.Fatal("必须在 pay_notify_dropped_total 上计数：没有曲线时这类丢单只能靠客诉发现")
	}
}
