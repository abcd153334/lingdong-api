package billing

import (
	"context"
	"errors"
	"math"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/money"
)

// 本文件覆盖 Adjust（管理台人工调账）的边界。
//
// 为什么必须有：2026-10-29 审计发现 `POST /api/admin/users/{id}/quota` 传
// `2^63-1` 返回 500 internal_error。真实原因不是溢出（money.Check 已经拦住了），
// 而是 controller 的错误映射漏了两个 case —— 而 Adjust 本身**一条用例都没有**，
// 所以"它到底会返回哪些 sentinel"从来没被锁定过。

func seedUser(t *testing.T, gdb *gorm.DB, balance int64, limit int64) *model.User {
	t.Helper()
	u := &model.User{
		Username: "adjust-target", PasswordHash: "x", Status: 1,
		Balance: balance, BalanceLimit: limit,
	}
	if err := gdb.Create(u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return u
}

func TestAdjustRejectsOutOfRangeAmount(t *testing.T) {
	gdb := withTestDB(t)
	u := seedUser(t, gdb, 1_000_000, 0)

	cases := []struct {
		name   string
		amount int64
	}{
		{"MaxInt64（审计实测值）", math.MaxInt64},
		{"超单笔上限 1 微", money.MaxAmount + 1},
		{"负向超单笔上限", -(money.MaxAmount + 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := New(gdb).Adjust(context.Background(), u.ID, tc.amount, "边界测试")
			if !errors.Is(err, ErrAmountTooLarge) {
				t.Fatalf("期望 ErrAmountTooLarge，实际 %v", err)
			}
			// 拒绝必须发生在写库之前：余额与流水都不能动。
			var after model.User
			if err := gdb.First(&after, u.ID).Error; err != nil {
				t.Fatalf("回读用户失败: %v", err)
			}
			if after.Balance != 1_000_000 {
				t.Fatalf("余额被改动了：%d", after.Balance)
			}
			var n int64
			gdb.Model(&model.Transaction{}).Count(&n)
			if n != 0 {
				t.Fatalf("越界调账不应写流水，实际 %d 条", n)
			}
		})
	}
}

func TestAdjustRejectsNegativeBalance(t *testing.T) {
	gdb := withTestDB(t)
	u := seedUser(t, gdb, 1_000_000, 0)

	err := New(gdb).Adjust(context.Background(), u.ID, -2_000_000, "扣成负数")
	if !errors.Is(err, ErrNegativeBalance) {
		t.Fatalf("期望 ErrNegativeBalance，实际 %v", err)
	}
}

func TestAdjustRejectsOverBalanceLimit(t *testing.T) {
	gdb := withTestDB(t)
	// balance_limit = 5 元（5e6 微），已有 4 元，再加 2 元应被拒。
	u := seedUser(t, gdb, 4_000_000, 5_000_000)

	err := New(gdb).Adjust(context.Background(), u.ID, 2_000_000, "超上限")
	if !errors.Is(err, ErrBalanceLimitExceeded) {
		t.Fatalf("期望 ErrBalanceLimitExceeded，实际 %v", err)
	}
}

func TestAdjustWritesLedgerAndBalance(t *testing.T) {
	gdb := withTestDB(t)
	u := seedUser(t, gdb, 1_000_000, 0)

	if err := New(gdb).Adjust(context.Background(), u.ID, 2_500_000, "人工补偿"); err != nil {
		t.Fatalf("正向调账失败: %v", err)
	}
	if err := New(gdb).Adjust(context.Background(), u.ID, -500_000, "扣回"); err != nil {
		t.Fatalf("负向调账失败: %v", err)
	}

	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if after.Balance != 3_000_000 {
		t.Fatalf("余额 = %d，期望 3000000", after.Balance)
	}

	var rows []model.Transaction
	if err := gdb.Where("user_id = ? AND type = ?", u.ID, model.TxnAdjust).Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("查流水失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 条 adjust 流水，实际 %d", len(rows))
	}
	if rows[0].Amount != 2_500_000 || rows[1].Amount != -500_000 {
		t.Fatalf("流水金额不符：%d, %d", rows[0].Amount, rows[1].Amount)
	}
	// 链式一致性：第二条的 balance_after 必须等于两条之和 + 初始余额。
	if rows[1].BalanceAfter != 3_000_000 {
		t.Fatalf("balance_after 链断了：%d", rows[1].BalanceAfter)
	}
}

func TestAdjustUnknownUser(t *testing.T) {
	gdb := withTestDB(t)
	err := New(gdb).Adjust(context.Background(), 99999, 1_000_000, "不存在")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("期望 ErrRecordNotFound，实际 %v", err)
	}
}
