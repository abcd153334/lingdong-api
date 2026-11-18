package billing

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

/*
本文件锁死 A2：**冻结期必须把计费维度落库**，否则回收只能按 1 张保底收。

回归的真实资损路径（本轮真机排查发现）：
Freeze 只写 frozen 与 est_tokens，`usages.image_n / image_spec / image_ratio /
ref_images` 全留在 0；`ReclaimSettle` 用 `usageOfRow` 从这些列还原计费入参，
拿回一个 `ImageN=0` 的 Usage，而 `imageCharge` 对 0 的处理是 `n = max(1, 0)`。
于是「组图按 15 张冻结、实际交付 2 张」的会话被回收时只收 1 张，
差额既不在账单上、也没有任何异常标记 —— 只能靠对账偶然发现。

修复前这两条用例必红：第一条的列全为 0，第二条 charge 等于 1×单价。
*/

// imageFreezeEstimate 是一次组图请求的计费维度（冻结期算出来的那份）。
func imageFreezeEstimate() Usage {
	return Usage{
		ImageSpec: "2K", ImageRatio: "16:9", ImageN: 15, ImageSpecSource: "user",
		RefImages: 2, ImageQuality: "high",
	}
}

// TestFreezePersistsImageSnapshot 冻结后，usages 行必须能完整还原这次请求的计费维度。
func TestFreezePersistsImageSnapshot(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	const unit = int64(50_000)
	est := imageFreezeEstimate()
	frozen := unit * est.ImageN

	snap := Snapshot{
		ModelID: "seedream", BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{Kind: SpecKindPerImage, TierPrice: map[string]int64{"2K": unit}},
	}
	u := model.User{Username: "freeze-cols", Balance: 1_000_000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-freeze-cols", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}

	sess, err := New(gdb).Freeze(ctx, FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "seedream", Modality: "image",
		Snapshot: snap, Amount: frozen, Estimate: est,
	})
	if err != nil {
		t.Fatalf("冻结失败: %v", err)
	}

	var row model.Usage
	if err := gdb.First(&row, sess.UsageID).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	if row.ImageSpec != "2K" {
		t.Errorf("image_spec 没落库：got %q want %q（回收期只能读到 0）", row.ImageSpec, "2K")
	}
	if row.ImageN != est.ImageN {
		t.Errorf("image_n 没落库：got %d want %d（回收会按 1 张保底）", row.ImageN, est.ImageN)
	}
	if row.ImageRatio != "16:9" {
		t.Errorf("image_ratio 没落库：got %q", row.ImageRatio)
	}
	if row.ImageSpecSource != "user" {
		t.Errorf("image_spec_source 没落库：got %q", row.ImageSpecSource)
	}
	if row.RefImages != 2 {
		t.Errorf("ref_images 没落库：got %d want 2", row.RefImages)
	}
	if row.ImageQuality != "high" {
		t.Errorf("image_quality 没落库：got %q", row.ImageQuality)
	}
}

// TestReclaimUsesPersistedIntent 冻结 15 张、实交 2 张 → 只收 2 张（不是 1 张、也不是全冻）。
func TestReclaimUsesPersistedIntent(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	const unit = int64(50_000)
	est := imageFreezeEstimate()
	frozen := unit * est.ImageN

	snap := Snapshot{
		ModelID: "seedream", BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{Kind: SpecKindPerImage, TierPrice: map[string]int64{"2K": unit}},
	}
	u := model.User{Username: "reclaim-intent", Balance: 2_000_000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-reclaim-intent", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}

	sess, err := New(gdb).Freeze(ctx, FreezeInput{
		UserID: u.ID, KeyID: k.ID, Model: "seedream", Modality: "image",
		Snapshot: snap, Amount: frozen, Estimate: est,
	})
	if err != nil {
		t.Fatalf("冻结失败: %v", err)
	}

	got, err := New(gdb).ReclaimSettle(ctx, sess.UsageID, "", 2)
	if err != nil {
		t.Fatalf("回收结算失败: %v", err)
	}
	if want := unit * 2; got != want {
		t.Fatalf("回收金额 = %d，期望 %d（2 张×单价）。收到 %d 说明 ImageN 在 pending 行上是 0，"+
			"imageCharge 按 n=max(1,0) 保底；收到 %d 说明没按实际交付下调",
			got, want, unit, frozen)
	}

	var row model.Usage
	if err := gdb.First(&row, sess.UsageID).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	if row.Charge != unit*2 {
		t.Errorf("usages.charge = %d，期望 %d", row.Charge, unit*2)
	}
	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatalf("读用户失败: %v", err)
	}
	if after.Frozen != 0 {
		t.Errorf("回收后 frozen 应为 0，实际 %d", after.Frozen)
	}
	if want := int64(2_000_000) - unit*2; after.Balance != want {
		t.Errorf("余额 = %d，期望 %d（只扣 2 张）", after.Balance, want)
	}
}

// TestReclaimUnknownActualKeepsIntent 没有交付证据（actual=0）时保持意图张数。
//
// 这条是「回滚开关」：回收期拿不到 artifact_count 时行为必须与改造前一致，
// 不能因为新公式把未知当 0 张。
func TestReclaimUnknownActualKeepsIntent(t *testing.T) {
	if got := BillableImageN(15, 0); got != 15 {
		t.Errorf("BillableImageN(15,0) = %d，期望 15（未知交付不等于 0 交付）", got)
	}
	if got := BillableImageN(15, 3); got != 3 {
		t.Errorf("BillableImageN(15,3) = %d，期望 3", got)
	}
	if got := BillableImageN(15, 20); got != 15 {
		t.Errorf("BillableImageN(15,20) = %d，期望 15（多交不向用户收费）", got)
	}
	if got := BillableImageN(0, 3); got != 0 {
		t.Errorf("BillableImageN(0,3) = %d，期望 0（不按张计价的单子不能被改成 3）", got)
	}
}

// TestUpstreamSpecColumnMatchesConstant 锁死列宽与常量一致。
//
// 这两者曾经不一致（列 16 / 常量 32）：上游回报 17–32 字符档位时，
// Settle 的 UPDATE 会因 varchar(16) 报错 → 整个结算事务回滚 → usage 停 pending。
// 用反射读 tag 而不是查库：老库上列宽由 M61 迁移负责，这里守的是"代码侧不再漂移"。
func TestUpstreamSpecColumnMatchesConstant(t *testing.T) {
	f, ok := reflect.TypeOf(model.Usage{}).FieldByName("UpstreamSpec")
	if !ok {
		t.Fatal("model.Usage 没有 UpstreamSpec 字段")
	}
	tag := f.Tag.Get("gorm")
	i := strings.Index(tag, "size:")
	if i < 0 {
		t.Fatalf("UpstreamSpec 缺 size tag：%s", tag)
	}
	rest := tag[i+len("size:"):]
	end := strings.IndexAny(rest, ";, ")
	if end > 0 {
		rest = rest[:end]
	}
	if want := itoa(upstreamSpecMaxLen); rest != want {
		t.Fatalf("usages.upstream_spec 列宽 = %s，但 billing.upstreamSpecMaxLen = %s："+
			"两者必须一致（写入侧按常量判长，列更短会让结算事务整体回滚）", rest, want)
	}
}

// itoa 是 strconv.Itoa 的薄封装（本文件只用于上面那条断言，避免多一个 import）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
