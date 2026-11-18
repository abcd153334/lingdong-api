package billing

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"lingdong-api/internal/model"
)

/*
本文件锁死「回收路径也必须按条免单」。

回归的真实资损路径：正常结算走 `applyArtifactActuals` → `BillableClips`（0 条 = 0 元），
而回收路径 `ReclaimSettle` 无条件调 `BillableImageN`（0 = 保持意图）。
同一个 per_clip 孤儿单，在结算路径免单、在回收路径按 max_n 收 2 条 —— 用户可举证。

根因是入参用一个 `0` 同时表达两件事：
  - 「本次确实交付 0 条」（应免单）；
  - 「回收期拿不到交付事实」（应保持意图，按冻结收）。
修复方式：`<0 = 未知`、`>=0 = 事实`。
*/

// perClipSnap 造一份按条计价的快照（¥0.1/条、最多 2 条）。
func perClipSnap(unit, maxN int64) Snapshot {
	return Snapshot{
		ModelID:     "suno",
		BillingMode: model.BillingModeSpec,
		SpecPrice:   model.SpecPrices{Kind: SpecKindPerClip, PerClipPrice: &unit, MaxN: &maxN},
	}
}

// seedClipUsage 造一个 per_clip 的 pending 会话：冻结 = 单价 × max_n。
func seedClipUsage(t *testing.T, unit, maxN int64, username string) (*Service, uint64, uint64) {
	t.Helper()
	gdb := withTestDB(t)
	frozen := unit * maxN
	snap := perClipSnap(unit, maxN)
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("序列化快照失败: %v", err)
	}

	// 冻结已经把余额扣走了：这里直接造「冻结后」的状态（balance 已是扣后值）。
	u := model.User{Username: username, Balance: 1_000_000 - frozen, Frozen: frozen}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-" + username, Status: 1, Used: frozen}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	usage := model.Usage{
		UserID: u.ID, KeyID: k.ID, Model: "suno", Status: model.UsageStatusPending,
		PricingSnapshot: raw, ImageN: maxN,
	}
	if err := gdb.Create(&usage).Error; err != nil {
		t.Fatalf("建 usage 失败: %v", err)
	}
	if err := gdb.Create(&model.Transaction{
		UserID: u.ID, Type: model.TxnFreeze, Amount: -frozen, BalanceAfter: 1_000_000 - frozen,
		FrozenAfter: frozen, RefType: model.RefTypeUsage,
		RefID: strconv.FormatUint(usage.ID, 10),
	}).Error; err != nil {
		t.Fatalf("建冻结流水失败: %v", err)
	}
	return New(gdb), usage.ID, u.ID
}

// TestReclaimPerClipZeroDeliveredIsFree 确定交付 0 条 → 免单，冻结额全退。
func TestReclaimPerClipZeroDeliveredIsFree(t *testing.T) {
	ResetLimitsForTest()
	const unit, maxN = int64(100_000), int64(2)
	svc, usageID, userID := seedClipUsage(t, unit, maxN, "reclaim-clip-zero")

	charge, err := svc.ReclaimSettle(context.Background(), usageID, model.AnomalyReclaimedAfterSuccess, 0)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if charge != 0 {
		t.Fatalf("确定交付 0 条应收 0 元（与正常结算的 BillableClips 同口径），实际 %d", charge)
	}

	var got model.Usage
	if err := svc.db.First(&got, usageID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.UsageStatusSettled || got.Charge != 0 {
		t.Fatalf("终态应为 settled/charge=0，实际 %s/%d", got.Status, got.Charge)
	}

	var rel model.Transaction
	if err := svc.db.Where("ref_type = ? AND ref_id = ? AND type = ?", model.RefTypeUsage,
		strconv.FormatUint(usageID, 10), model.TxnRelease).First(&rel).Error; err != nil {
		t.Fatalf("应有一条全额释放流水: %v", err)
	}
	if want := unit * maxN; rel.Amount != want {
		t.Fatalf("释放应退 %d，实际 %d", want, rel.Amount)
	}

	var after model.User
	if err := svc.db.First(&after, userID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Frozen != 0 || after.Used != 0 || after.Balance != 1_000_000 {
		t.Fatalf("账目不一致：frozen=%d used=%d balance=%d（期望 0 / 0 / 1000000）",
			after.Frozen, after.Used, after.Balance)
	}
}

// TestReclaimPerClipUnknownKeepsFrozen 「拿不到交付事实」不能变成免单 —— 那是回滚开关，
// 也是「无证据不多收不少收」的基线。
func TestReclaimPerClipUnknownKeepsFrozen(t *testing.T) {
	ResetLimitsForTest()
	const unit, maxN = int64(100_000), int64(2)
	svc, usageID, _ := seedClipUsage(t, unit, maxN, "reclaim-clip-unknown")

	charge, err := svc.ReclaimSettle(context.Background(), usageID, model.AnomalyReclaimedOrphan, -1)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if want := unit * maxN; charge != want {
		t.Fatalf("交付量未知时应按冻结额 %d 入账，实际 %d（不能凭未知就免单）", want, charge)
	}
}
