package billing

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"lingdong-api/internal/model"
)

/*
本文件锁死 A1：**回收补结必须按实际交付量收费**。

回归的是这条真实资损路径：组图模型按 max_n 冻 10 张的额度，上游只出了 2 张，
任务标记成功但 Settle 没跑完（进程重启 / 上游超时 / 客户端断流）。
改造前 reclaimAmounts 对 spec 直接返回冻结额 —— 用户为没拿到的 8 张付钱，
而且账单上只有 reclaimed 一个标记，看不出「少交付」这件事。
*/

// TestReclaimSpecChargesActualArtifacts 10 张的冻结、实交 2 张 → 只收 2 张。
func TestReclaimSpecChargesActualArtifacts(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	const unit = int64(50_000)
	const declared = int64(10)
	frozen := unit * declared

	snap := Snapshot{
		ModelID: "seedream", BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{Kind: SpecKindPerImage, TierPrice: map[string]int64{"1K": unit}},
	}
	raw, _ := json.Marshal(snap)

	u := model.User{Username: "reclaim-img", Balance: 1_000_000, Frozen: frozen}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-reclaim-img", Status: 1, Used: frozen}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	usage := model.Usage{
		UserID: u.ID, KeyID: k.ID, Model: "seedream", Status: model.UsageStatusPending,
		PricingSnapshot: raw, ImageN: declared, ImageSpec: "1K",
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

	// artifact_count = 2：只交付 2 张。
	charge, err := New(gdb).ReclaimSettle(ctx, usage.ID, model.AnomalyReclaimedAfterSuccess, 2)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if want := unit * 2; charge != want {
		t.Fatalf("charge=%d，期望 %d（按实交 2 张，不是冻结的 10 张）", charge, want)
	}

	var got model.Usage
	if err := gdb.First(&got, usage.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.UsageStatusSettled || got.Charge != charge {
		t.Fatalf("终态应为 settled/charge=%d，实际 %s/%d", charge, got.Status, got.Charge)
	}
	if !model.HasAnomaly(got.Anomaly, model.AnomalyReclaimedAfterSuccess) {
		t.Fatalf("anomaly 应含 %s，实际 %q", model.AnomalyReclaimedAfterSuccess, got.Anomaly)
	}

	// 差额退回：400_000 一条 release 流水，余额加上它、冻结清零、累计消费只记实收。
	var rel model.Transaction
	if err := gdb.Where("ref_type = ? AND ref_id = ? AND type = ?", model.RefTypeUsage,
		strconv.FormatUint(usage.ID, 10), model.TxnRelease).First(&rel).Error; err != nil {
		t.Fatalf("应有一条 release 差额流水: %v", err)
	}
	if want := frozen - charge; rel.Amount != want {
		t.Fatalf("release 应退 %d，实际 %d", want, rel.Amount)
	}
	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Frozen != 0 || after.Used != charge || after.Balance != 1_000_000+frozen-charge {
		t.Fatalf("账目不一致：frozen=%d used=%d balance=%d（期望 0 / %d / %d）",
			after.Frozen, after.Used, after.Balance, charge, 1_000_000+frozen-charge)
	}
	var keyAfter model.APIKey
	if err := gdb.First(&keyAfter, k.ID).Error; err != nil {
		t.Fatal(err)
	}
	if keyAfter.Used != charge {
		t.Fatalf("KEY 已用额应回到实收 %d，实际 %d", charge, keyAfter.Used)
	}
}

// TestReclaimSpecWithoutArtifactCountKeepsFrozen 孤立 usage（拿不到 artifact_count）
// 必须退回「按冻结额」这条老口径 —— 那是回滚开关，也是「无证据不多收不少收」的基线。
func TestReclaimSpecWithoutArtifactCountKeepsFrozen(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	const unit = int64(50_000)
	const declared = int64(4)
	frozen := unit * declared

	snap := Snapshot{
		ModelID: "seedream", BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{Kind: SpecKindPerImage, TierPrice: map[string]int64{"1K": unit}},
	}
	raw, _ := json.Marshal(snap)

	u := model.User{Username: "reclaim-orphan", Balance: 1_000_000, Frozen: frozen}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-reclaim-orphan", Status: 1, Used: frozen}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	usage := model.Usage{
		UserID: u.ID, KeyID: k.ID, Model: "seedream", Status: model.UsageStatusPending,
		PricingSnapshot: raw, ImageN: declared, ImageSpec: "1K",
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

	// 拿不到 artifact_count = 未知（-1），不是"确定交付 0 张"。
	charge, err := New(gdb).ReclaimSettle(ctx, usage.ID, model.AnomalyReclaimedOrphan, -1)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if charge != frozen {
		t.Fatalf("无交付证据时应按冻结额 %d 收，实际 %d", frozen, charge)
	}
}

// TestReclaimSpecDirtyTierFallsBack 脏行（按秒计费但没有分辨率档位）必须回落冻结额
// 并留痕，不能把整单算成免费、也不能卡在 pending 让 worker 每 5 分钟重试同一个必败运算。
//
// 注意不能拿「图片缺档位」当脏行：imageCharge 对空档位会回落到**最高价档**
// （与正常结算同一口径），那是合法行为，不是算不出。
func TestReclaimSpecDirtyTierFallsBack(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	const frozen = int64(30_000)
	// per_video：videoCharge 取 SecondPrice[u.VideoResolution]，档位为空 → ErrInvalidTier。
	snap := Snapshot{
		ModelID: "vid", BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{
			Kind:        SpecKindPerVideo,
			SecondPrice: map[string]int64{"720p": 1_000},
		},
	}
	raw, _ := json.Marshal(snap)

	u := model.User{Username: "reclaim-dirty", Balance: 1_000_000, Frozen: frozen}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-reclaim-dirty", Status: 1, Used: frozen}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	usage := model.Usage{
		UserID: u.ID, KeyID: k.ID, Model: "vid", Status: model.UsageStatusPending,
		PricingSnapshot: raw, VideoResolution: "", VideoSeconds: 5,
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

	charge, err := New(gdb).ReclaimSettle(ctx, usage.ID, model.AnomalyReclaimedOrphan, 1)
	if err != nil {
		t.Fatalf("脏行不应让回收报错: %v", err)
	}
	if charge != frozen {
		t.Fatalf("算不出金额时应按冻结额 %d 收，实际 %d", frozen, charge)
	}
	var got model.Usage
	if err := gdb.First(&got, usage.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !model.HasAnomaly(got.Anomaly, model.AnomalyChargeCalcFailed) {
		t.Fatalf("anomaly 应含 %s，实际 %q", model.AnomalyChargeCalcFailed, got.Anomaly)
	}
}
