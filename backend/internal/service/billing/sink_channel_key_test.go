package billing

import (
	"context"
	"testing"

	"lingdong-api/internal/model"
)

// TestSettleWritesChannelKeyID 结算路径必须记下「这一单走了哪把上游密钥」。
//
// 为什么要落库：同一个渠道里有多把 key 时，「渠道 A 出了 401」这句话没有排障价值——
// 要定位的是渠道 A 里**具体哪一把**被吊销了。归属只能由出站那一刻的选中者知道，
// 事后无法反推，所以必须在结算的同一个 UPDATE 里写下来。
func TestSettleWritesChannelKeyID(t *testing.T) {
	base := context.Background()
	svc, sess := seedSession(t, base)

	const keyID = "0123456789abcdef0123456789abcdef"
	if _, err := svc.Settle(base, sess, SettleInput{
		StatusCode: 200, Usage: Usage{PromptTokens: 100},
		UsageSource: "upstream", ChannelKeyID: keyID, Attempts: 2,
	}); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	var u model.Usage
	if err := svc.db.First(&u, sess.UsageID).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	if u.ChannelKeyID != keyID {
		t.Fatalf("上游密钥归属未落库: %q", u.ChannelKeyID)
	}
}

// TestReleaseChannelKeyIDEmptyDoesNotOverwrite 释放路径的两条相反要求。
//
// ① 带了 key 就要写（失败行同样是「有终态的实际调用」，只记成功的等于少一半线索）；
// ② 没带 key 时**一个字节都不能动**。后者是真会踩的：迟到补结、回收重算、
// worker 侧对账都拿不到当次 key，若无条件写空串，第一次结算记下的归属就被抹平，
// 表现是「排障时这单的密钥列突然空了，而且只在超时/重试的请求上出现」。
func TestReleaseChannelKeyIDEmptyDoesNotOverwrite(t *testing.T) {
	base := context.Background()
	svc, usageID := seedPending(t, base, 1000, 100, tokenSnap())

	const first = "ffffffffffffffffffffffffffffffff"
	if err := svc.db.Model(&model.Usage{}).Where("id = ?", usageID).
		UpdateColumn("channel_key_id", first).Error; err != nil {
		t.Fatalf("预置归属失败: %v", err)
	}

	// 迟到路径：不带 key 释放 → 保留原归属。
	if _, err := svc.ReleaseByUsageID(base, usageID, SettleInput{
		StatusCode: 502, ErrorMsg: "upstream 502", UsageSource: "none",
	}); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	var got model.Usage
	if err := svc.db.First(&got, usageID).Error; err != nil {
		t.Fatalf("读 usage 失败: %v", err)
	}
	if got.ChannelKeyID != first {
		t.Fatalf("空值把已有归属抹掉了: %q", got.ChannelKeyID)
	}

	// 正常路径：带 key 释放 → 落库（失败行也必须能回答「哪把 key 挂的」）。
	const second = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	u2 := model.Usage{
		UserID: got.UserID, KeyID: got.KeyID, Model: got.Model,
		Status: "pending", PricingSnapshot: got.PricingSnapshot,
	}
	if err := svc.db.Create(&u2).Error; err != nil {
		t.Fatalf("建第二条 usage 失败: %v", err)
	}
	if _, err := svc.ReleaseByUsageID(base, u2.ID, SettleInput{
		StatusCode: 401, ChannelKeyID: second, UsageSource: "none",
	}); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	var got2 model.Usage
	if err := svc.db.First(&got2, u2.ID).Error; err != nil {
		t.Fatalf("读第二条 usage 失败: %v", err)
	}
	if got2.ChannelKeyID != second {
		t.Fatalf("失败行的密钥归属未落库: %q", got2.ChannelKeyID)
	}
}
