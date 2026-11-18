package billing

import (
	"context"
	"errors"
	"sync"
	"testing"

	"lingdong-api/internal/model"
)

/*
本文件锁死「同步链路的幂等键」：同一个 Idempotency-Key 在同一個端点上只建一次计费记录。

回归的真实资损路径：同步端点（图片/音频/对话）此前完全没有幂等 —— 客户端 SDK 超时
重试一次就是一次新的冻结与扣费。请求耗时 20–60 秒的同步出图尤其容易踩：客户端
等不到响应就重发，两次都成功、两笔都扣，而用户认为"我只发了一次"。

判据：第二次必须返回 `*ErrIdemReplay`，余额只动一次，usages 只有一行。
并发那条还要验证**唯一索引**兜底（两个请求同时通过预查时，只放行一个）。
*/

// idemTestSnapshot 是这次用例的价目（1 张 5 万微）。
func idemTestSnapshot() Snapshot {
	return Snapshot{
		ModelID: "nano-banana", BillingMode: model.BillingModeSpec,
		SpecPrice: model.SpecPrices{Kind: SpecKindPerImage, TierPrice: map[string]int64{"1K": 50_000}},
	}
}

func idemTestInput(userID, keyID uint64, idem string) FreezeInput {
	return FreezeInput{
		UserID: userID, KeyID: keyID, Model: "nano-banana", Modality: "image",
		Snapshot: idemTestSnapshot(), Amount: 50_000,
		Estimate:  Usage{ImageSpec: "1K", ImageN: 1},
		IdemKey:   idem,
		IdemScope: "/v1/images/generations",
	}
}

// TestFreezeIdempotentReplay 同一幂等键第二次不扣钱，返回首次的记录号。
func TestFreezeIdempotentReplay(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	u := model.User{Username: "idem-replay", Balance: 1_000_000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-idem-replay", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	// 生产里这条索引由 M64 建；测试 schema 是 AutoMigrate 出来的，这里补上，
	// 否则并发用例验证不到"唯一索引兜底"那一层。
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_usages_idem
	                      ON usages (key_id, idem_scope, idem_key) WHERE idem_key <> ''`).Error; err != nil {
		t.Fatalf("建幂等索引失败: %v", err)
	}

	svc := New(gdb)
	first, err := svc.Freeze(ctx, idemTestInput(u.ID, k.ID, "k-1"))
	if err != nil {
		t.Fatalf("首次冻结应成功: %v", err)
	}

	_, err = svc.Freeze(ctx, idemTestInput(u.ID, k.ID, "k-1"))
	var replay *ErrIdemReplay
	if !errors.As(err, &replay) {
		t.Fatalf("同键第二次应返回 ErrIdemReplay，实际 %v", err)
	}
	if replay.UsageID != first.UsageID {
		t.Fatalf("重复提交应指向首次记录 #%d，实际 #%d", first.UsageID, replay.UsageID)
	}

	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查用户失败: %v", err)
	}
	if after.Balance != 1_000_000-50_000 {
		t.Fatalf("余额只应扣一次 50000，实际 %d", after.Balance)
	}
	var n int64
	if err := gdb.Model(&model.Usage{}).Where("idem_key = ?", "k-1").Count(&n).Error; err != nil {
		t.Fatalf("查 usage 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("同键只应有一行 usage，实际 %d", n)
	}
}

// TestFreezeIdempotentConcurrent 并发同键：只放行一个，另一个拿到首次记录号。
//
// 这条用例验证的是"预查通过、插入撞唯一索引"那条路径 —— 没有唯一索引时
// 两个请求都会成功，用户被扣两次，而没有任何地方会报错。
func TestFreezeIdempotentConcurrent(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	u := model.User{Username: "idem-race", Balance: 1_000_000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-idem-race", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_usages_idem
	                      ON usages (key_id, idem_scope, idem_key) WHERE idem_key <> ''`).Error; err != nil {
		t.Fatalf("建幂等索引失败: %v", err)
	}

	svc := New(gdb)
	const n = 2
	var wg sync.WaitGroup
	oks := make([]bool, n)
	replays := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := svc.Freeze(ctx, idemTestInput(u.ID, k.ID, "k-race"))
			if err == nil {
				oks[i] = true
				return
			}
			var replay *ErrIdemReplay
			if errors.As(err, &replay) {
				replays[i] = true
			}
		}(i)
	}
	wg.Wait()

	okCount, replayCount := 0, 0
	for i := 0; i < n; i++ {
		if oks[i] {
			okCount++
		}
		if replays[i] {
			replayCount++
		}
	}
	if okCount != 1 || replayCount != 1 {
		t.Fatalf("并发同键应恰好 1 次成功 + 1 次重放，实际 success=%d replay=%d", okCount, replayCount)
	}
	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查用户失败: %v", err)
	}
	if after.Balance != 1_000_000-50_000 {
		t.Fatalf("并发下余额只应扣一次 50000，实际 %d", after.Balance)
	}
}

// TestFreezeIdemKeyFreedAfterRelease 释放必须交还幂等键。
//
// 回归的真实路径（2026-11-16 真机复现）：带 Idempotency-Key 发一次
// `size=99999x99999`，上游 400、全额退款、usage 进 released；随后改对参数
// 用同一个键重试，得到 409「该键已用于上一次相同请求」——
// 一个从未产生任何费用的失败请求，把客户端永久钉在"必须换键"上。
//
// 判据：释放后同键可以再建一条记录，且余额只在**第二次**扣一次。
func TestFreezeIdemKeyFreedAfterRelease(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	u := model.User{Username: "idem-release", Balance: 1_000_000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-idem-release", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_usages_idem
	                      ON usages (key_id, idem_scope, idem_key) WHERE idem_key <> ''`).Error; err != nil {
		t.Fatalf("建幂等索引失败: %v", err)
	}

	svc := New(gdb)
	first, err := svc.Freeze(ctx, idemTestInput(u.ID, k.ID, "k-retry"))
	if err != nil {
		t.Fatalf("首次冻结应成功: %v", err)
	}
	if err := svc.Release(ctx, first, SettleInput{StatusCode: 400, ErrorMsg: "上游拒绝参数"}); err != nil {
		t.Fatalf("释放应成功: %v", err)
	}

	second, err := svc.Freeze(ctx, idemTestInput(u.ID, k.ID, "k-retry"))
	if err != nil {
		t.Fatalf("失败请求释放后，同键重试必须放行，实际 %v", err)
	}
	if second.UsageID == first.UsageID {
		t.Fatalf("重试应建新记录，不能复用已释放的 #%d", first.UsageID)
	}
	var after model.User
	if err := gdb.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查用户失败: %v", err)
	}
	// 第一次冻结后全额退回，第二次冻结留下 50,000。
	if after.Balance != 1_000_000-50_000 {
		t.Fatalf("余额应只剩第二次的冻结 50000，实际 %d", after.Balance)
	}
}

// TestFreezeIdemKeyKeptAfterSettle 收过钱的行必须**保留**幂等键。
//
// 这是同一个开关的另一半：结算成功说明这次调用真的产生了费用，
// 同键再来一次就是重复提交，必须拦（否则幂等等于没做）。
// 两条用例合起来才说明白"释放交还、结算保留"这条判据。
func TestFreezeIdemKeyKeptAfterSettle(t *testing.T) {
	ResetLimitsForTest()
	gdb := withTestDB(t)
	ctx := context.Background()

	u := model.User{Username: "idem-settle", Balance: 1_000_000}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	k := model.APIKey{UserID: u.ID, KeyHash: "hash-idem-settle", Status: 1}
	if err := gdb.Create(&k).Error; err != nil {
		t.Fatalf("建 KEY 失败: %v", err)
	}
	if err := gdb.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_usages_idem
	                      ON usages (key_id, idem_scope, idem_key) WHERE idem_key <> ''`).Error; err != nil {
		t.Fatalf("建幂等索引失败: %v", err)
	}

	svc := New(gdb)
	first, err := svc.Freeze(ctx, idemTestInput(u.ID, k.ID, "k-settled"))
	if err != nil {
		t.Fatalf("首次冻结应成功: %v", err)
	}
	if _, err := svc.Settle(ctx, first, SettleInput{
		StatusCode: 200, Usage: Usage{ImageN: 1, ImageSpec: "1K"},
	}); err != nil {
		t.Fatalf("结算应成功: %v", err)
	}

	_, err = svc.Freeze(ctx, idemTestInput(u.ID, k.ID, "k-settled"))
	var replay *ErrIdemReplay
	if !errors.As(err, &replay) {
		t.Fatalf("已结算的键再提交必须判重放，实际 %v", err)
	}
	if replay.UsageID != first.UsageID {
		t.Fatalf("重放应指向已结算的 #%d，实际 #%d", first.UsageID, replay.UsageID)
	}
}
