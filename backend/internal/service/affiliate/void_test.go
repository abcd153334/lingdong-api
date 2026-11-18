package affiliate

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// TestVoidIgnoresExpiry 作废只看「钱有没有出去」，不看是否到期。
//
// 旧实现要求 `available_at > now()` 才作废，于是 hold_hours=0（签发即到期）时
// 退款永远不作废，返利还能继续转出 —— 冷却期配置越激进，保护越弱。
// 到期但没转出，钱仍在平台手里，必须作废。
func TestVoidIgnoresExpiry(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	now := time.Now()

	u := seedUser(t, gdb)
	// 已到期（1 小时前）且未转出
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9701, InviteeID: u.ID, BeneficiaryID: u.ID, Role: model.AffRoleInvitee,
		Amount: 100, BaseAmount: 1000, AvailableAt: now.Add(-time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写到期行失败: %v", err)
	}
	// 同单的邀请人腿
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9701, InviteeID: u.ID, BeneficiaryID: u.ID + 1, Role: model.AffRoleInviter,
		Amount: 200, BaseAmount: 1000, Rate: 100_000, AvailableAt: now.Add(-time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写邀请人腿失败: %v", err)
	}

	var res Voided
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		var e error
		res, e = VoidTx(tx, 9701)
		return e
	}); err != nil {
		t.Fatalf("作废失败: %v", err)
	}
	if res.Rows != 2 {
		t.Fatalf("到期未转出的两行都应作废，实际 %d", res.Rows)
	}
	var rows []model.AffiliateReward
	if err := gdb.Where("order_id = ?", 9701).Find(&rows).Error; err != nil {
		t.Fatalf("查行失败: %v", err)
	}
	for _, r := range rows {
		if !r.Voided || r.VoidedAt == nil {
			t.Fatalf("行应已作废: %+v", r)
		}
	}

	// 作废**不动钱**：余额不变、无流水。这是「不追债」的核心断言。
	if got := reload(t, gdb, u.ID); got.Balance != 0 {
		t.Fatalf("作废不应改动余额: %d", got.Balance)
	}
	var n int64
	if err := gdb.Model(&model.Transaction{}).Where("user_id = ?", u.ID).Count(&n).Error; err != nil {
		t.Fatalf("统计流水失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("作废不应写资金流水: %d", n)
	}
}

// TestVoidAfterTransferIsNoop 已转出的不追回：钱已经进余额，退款不再动它。
func TestVoidAfterTransferIsNoop(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()

	u := seedUser(t, gdb)
	now := time.Now()
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9711, InviteeID: u.ID, BeneficiaryID: u.ID, Role: model.AffRoleInvitee,
		Amount: 200_000, BaseAmount: 200_000, AvailableAt: now.Add(-time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写返利行失败: %v", err)
	}
	if _, err := svc.Transfer(ctx, u.ID); err != nil {
		t.Fatalf("转出应成功: %v", err)
	}

	var res Voided
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		var e error
		res, e = VoidTx(tx, 9711)
		return e
	}); err != nil {
		t.Fatalf("作废失败: %v", err)
	}
	if res.Rows != 0 {
		t.Fatalf("已转出的行不该被作废，实际 %d", res.Rows)
	}
	if got := reload(t, gdb, u.ID); got.Balance != 200_000 {
		t.Fatalf("余额不应被扣回: %d", got.Balance)
	}
}

// TestVoidTxIdempotent 重复退款只作废一次，voided_at 不被覆盖。
func TestVoidTxIdempotent(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	now := time.Now()

	u := seedUser(t, gdb)
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9721, InviteeID: u.ID, BeneficiaryID: u.ID, Role: model.AffRoleInvitee,
		Amount: 100, BaseAmount: 1000, AvailableAt: now.Add(time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写返利行失败: %v", err)
	}

	var first Voided
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		var e error
		first, e = VoidTx(tx, 9721)
		return e
	}); err != nil {
		t.Fatalf("首次作废失败: %v", err)
	}
	if first.Rows != 1 {
		t.Fatalf("首次应作废 1 行，实际 %d", first.Rows)
	}
	var before model.AffiliateReward
	if err := gdb.Where("order_id = ?", 9721).First(&before).Error; err != nil {
		t.Fatalf("查行失败: %v", err)
	}

	var second Voided
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		var e error
		second, e = VoidTx(tx, 9721)
		return e
	}); err != nil {
		t.Fatalf("重复作废不应报错: %v", err)
	}
	if second.Rows != 0 {
		t.Fatalf("重复作废应 0 行，实际 %d", second.Rows)
	}
	var after model.AffiliateReward
	if err := gdb.Where("order_id = ?", 9721).First(&after).Error; err != nil {
		t.Fatalf("查行失败: %v", err)
	}
	if after.VoidedAt == nil || before.VoidedAt == nil || !after.VoidedAt.Equal(*before.VoidedAt) {
		t.Fatalf("voided_at 被覆盖: before=%v after=%v", before.VoidedAt, after.VoidedAt)
	}
}

// TestTransferSelectsOnlyAvailable 转出只取「已到期」的行，冷却中的原样留着。
func TestTransferSelectsOnlyAvailable(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	svc := New(gdb)
	ctx := context.Background()

	u := seedUser(t, gdb)
	now := time.Now()
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9731, InviteeID: u.ID, BeneficiaryID: u.ID, Role: model.AffRoleInvitee,
		Amount: 120_000, BaseAmount: 120_000, AvailableAt: now.Add(-time.Minute), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写到期的行失败: %v", err)
	}
	if err := gdb.Create(&model.AffiliateReward{
		OrderID: 9732, InviteeID: u.ID, BeneficiaryID: u.ID, Role: model.AffRoleInvitee,
		Amount: 300_000, BaseAmount: 300_000, AvailableAt: now.Add(time.Hour), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写冷却的行失败: %v", err)
	}

	got, err := svc.Transfer(ctx, u.ID)
	if err != nil {
		t.Fatalf("转出应成功: %v", err)
	}
	if got != 120_000 {
		t.Fatalf("只应转出到期部分 120000，实际 %d", got)
	}
	var cooling model.AffiliateReward
	if err := gdb.Where("order_id = ?", 9732).First(&cooling).Error; err != nil {
		t.Fatalf("查行失败: %v", err)
	}
	if cooling.TransferredAt != nil {
		t.Fatal("冷却中的行不该被标记转出")
	}
	info, err := svc.Info(ctx, u.ID)
	if err != nil {
		t.Fatalf("面板查询失败: %v", err)
	}
	if info.AffCooling != 300_000 || info.AffAvailable != 0 {
		t.Fatalf("派生额不符: cooling=%d available=%d", info.AffCooling, info.AffAvailable)
	}
	if info.AffHistory != 420_000 {
		t.Fatalf("累计应含已转出与冷却中: %d", info.AffHistory)
	}
}
