package order

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/service/payment"
)

/*
本文件守住 D4：退款回调必须**认领到具体哪一笔退款**，且金额按「本笔退款额」核对。

改造前两个口径都不对：
  - out_refund_no 只写进留痕、从不校验 → 同一订单的第二笔退款（或旧单重放）
    会直接改掉第一笔的结论；
  - 金额拿 amount.total（订单总额）比 → 全额退款成立，部分退款必然被判「金额不符」
    转人工，而那条提示本身是错的（钱其实退对了）。
*/

// paidOrderForNotify 下一笔单并用 epay 回调付掉，返回 (订单主键, 商户单号)。
func paidOrderForNotify(t *testing.T, gdb *gorm.DB, svc *Service, name string, priceCent int64) (uint64, string) {
	t.Helper()
	u := seedUser(t, gdb, 0)
	p := &model.Package{Name: name, PriceCent: priceCent, Bonus: 0, Enabled: true}
	if err := gdb.Create(p).Error; err != nil {
		t.Fatalf("造套餐失败: %v", err)
	}
	res, err := svc.Create(context.Background(), CreateInput{UserID: u.ID, PackageID: p.ID})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	params := map[string]string{
		"out_trade_no": res.OutTradeNo, "trade_no": "T-" + name,
		"trade_status": "SUCCESS", "money": "72.00",
	}
	params["sign"] = payment.Sign(params, "secret")
	if r := svc.HandleNotify(context.Background(), "epay", params); r != NotifyDone {
		t.Fatalf("支付回调失败: %v", r)
	}
	return res.ID, res.OutTradeNo
}

// markRefunding 把订单置成「已发起退款、结果待确认」。
func markRefunding(t *testing.T, gdb *gorm.DB, id uint64, refundNo string, refundAmountCent int64) {
	t.Helper()
	if err := gdb.Model(&model.Order{}).Where("id = ?", id).Updates(map[string]any{
		"status":             model.OrderRefunding,
		"refund_status":      RefundStatePending,
		"refund_no":          refundNo,
		"refund_amount_cent": refundAmountCent,
	}).Error; err != nil {
		t.Fatalf("置退款中失败: %v", err)
	}
}

// TestRefundNotifyIgnoresOtherRefundNo 单号不符的退款回调不得改订单状态。
func TestRefundNotifyIgnoresOtherRefundNo(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	svc := New(gdb)
	id, outTradeNo := paidOrderForNotify(t, gdb, svc, "认领包", 7200)
	markRefunding(t, gdb, id, "RF-A", 7200)

	ch, ok := payment.FindChannel("epay")
	if !ok {
		t.Fatal("测试前提不成立：epay 渠道未配置")
	}
	n := refundNotifyOf(outTradeNo, payment.NotifyRefunded)
	n.AmountCent = 7200
	n.OutRefundNo = "RF-B" // 不是本单发起的那一笔
	if r := svc.settle(context.Background(), ch, n, "notify"); r != NotifyDone {
		t.Fatalf("非本单的退款通知应按已完成应答（不能让网关无限重试），实际 %v", r)
	}

	var after model.Order
	if err := gdb.First(&after, id).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != model.OrderRefunding || after.RefundStatus != RefundStatePending {
		t.Fatalf("单号不符不得推进状态：status=%s refund_status=%s", after.Status, after.RefundStatus)
	}
	if !strings.Contains(metrics.Render(), `refund_no_mismatch`) {
		t.Fatal("必须计数：否则「退款回调被忽略」永远只有日志")
	}
}

// TestRefundNotifyMatchesRefundNo 单号一致时正常推进到 refunded。
func TestRefundNotifyMatchesRefundNo(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	svc := New(gdb)
	id, outTradeNo := paidOrderForNotify(t, gdb, svc, "认领包2", 7200)
	markRefunding(t, gdb, id, "RF-C", 7200)

	ch, _ := payment.FindChannel("epay")
	n := refundNotifyOf(outTradeNo, payment.NotifyRefunded)
	n.AmountCent = 7200
	n.OutRefundNo = "RF-C"
	if r := svc.settle(context.Background(), ch, n, "notify"); r != NotifyDone {
		t.Fatalf("本单退款通知应处理成功，实际 %v", r)
	}
	var after model.Order
	if err := gdb.First(&after, id).Error; err != nil {
		t.Fatal(err)
	}
	if after.Status != model.OrderRefunded || after.RefundStatus != RefundStateSuccess {
		t.Fatalf("应推进为 refunded/success，实际 %s/%s", after.Status, after.RefundStatus)
	}
}

// TestRefundNotifyPartialAmountAccepted 部分退款按「本笔退款额」核对，不能被判金额不符。
func TestRefundNotifyPartialAmountAccepted(t *testing.T) {
	gdb := withTestDB(t)
	setupWalletConfig(t)
	svc := New(gdb)
	id, outTradeNo := paidOrderForNotify(t, gdb, svc, "部分退款包", 7200)
	markRefunding(t, gdb, id, "RF-P", 3000) // 只退 30 元

	ch, _ := payment.FindChannel("epay")
	n := refundNotifyOf(outTradeNo, payment.NotifyRefunded)
	n.AmountCent = 3000
	n.OutRefundNo = "RF-P"
	if r := svc.settle(context.Background(), ch, n, "notify"); r != NotifyDone {
		t.Fatalf("部分退款应按本笔退款额核对通过，实际 %v", r)
	}
	var after model.Order
	if err := gdb.First(&after, id).Error; err != nil {
		t.Fatal(err)
	}
	if after.RefundStatus != RefundStateSuccess {
		t.Fatalf("部分退款也应推进成功，实际 %s", after.RefundStatus)
	}
}
