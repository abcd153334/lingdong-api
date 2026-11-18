package worker

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/model"
)

// TestRefundHorizonOverdueIsReported 守住「超窗悬挂退款单不能静默」。
//
// 巡检有第 3 条硬约束（超过 24h 不再查），但那些单子仍然欠着用户钱。
// 过去它们既不查也不报，只有人工翻订单列表才会发现。这里只统计与告警，
// **不自动重发** —— 重复退款是真实资金损失（见 order_refund.go 头注释）。
func TestRefundHorizonOverdueIsReported(t *testing.T) {
	gdb := workerTestDB(t)
	if err := gdb.AutoMigrate(&model.Order{}); err != nil {
		t.Fatalf("迁移订单表失败: %v", err)
	}

	now := time.Now()
	old := now.Add(-30 * time.Hour)  // 窗口外
	fresh := now.Add(-1 * time.Hour) // 窗口内

	rows := []model.Order{
		{UserID: 1, OutTradeNo: "OLD1", AmountCent: 100, Status: model.OrderRefunding,
			RefundMode: "gateway", RefundStatus: "pending", RefundAt: &old},
		{UserID: 1, OutTradeNo: "NEW1", AmountCent: 100, Status: model.OrderRefunding,
			RefundMode: "gateway", RefundStatus: "pending", RefundAt: &fresh},
		// balance 模式不算：它没有出网动作，停在这里是另一类问题。
		{UserID: 1, OutTradeNo: "BAL1", AmountCent: 100, Status: model.OrderRefunding,
			RefundMode: "balance", RefundStatus: "pending", RefundAt: &old},
		// 已终结的也不算。
		{UserID: 1, OutTradeNo: "DONE1", AmountCent: 100, Status: model.OrderRefunded,
			RefundMode: "gateway", RefundStatus: "success", RefundAt: &old},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatalf("造单失败: %v", err)
	}

	n, err := countRefundHorizonOverdue(context.Background())
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("超窗悬挂单应为 1（只有 OLD1），实际 %d", n)
	}
}
