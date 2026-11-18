package affiliate

import (
	"sync"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
)

// 真并发的三条用例。
//
// 为什么必须有：`RewardTx` 的幂等与次数闸在顺序重放里永远是对的，
// 只有并发才暴露「COUNT 用的是加锁前的快照」这类缺陷。而且它是**唯一的**
// 验证手段 —— 去掉被邀请人行锁时，第二条用例必须变红。

// TestRewardTxConcurrentSameOrder 同一订单被并发发放，只允许落 1 单 2 行。
func TestRewardTxConcurrentSameOrder(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	setAff(t, baseCfg())
	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errsCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errsCh <- gdb.Transaction(func(tx *gorm.DB) error {
				return RewardTx(tx, RewardInput{InviteeID: invitee.ID, OrderID: 9801, AmountCent: 10_000})
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("并发发放不应报错: %v", err)
		}
	}

	// 100 元 × 10% = 10 元；赠金 5 元。重复的那几次必须被 ON CONFLICT 吃掉。
	if n := countRewards(t, gdb, "order_id = ?", 9801); n != 2 {
		t.Fatalf("同一订单应只落 2 行（邀请人+被邀请人），实际 %d", n)
	}
	if got := legOf(t, gdb, 9801, model.AffRoleInviter).Amount; got != 10_000_000 {
		t.Fatalf("邀请人腿应是单份 10 元，实际 %d", got)
	}
	if got := legOf(t, gdb, 9801, model.AffRoleInvitee).Amount; got != 5_000_000 {
		t.Fatalf("被邀请人腿应是单份 5 元，实际 %d", got)
	}
}

// TestRewardTxConcurrentRespectsMaxRewards 两个不同订单并发、N=1 → 只发 1 单。
//
// 这条是「用户行锁是次数闸的串行化点」的直接证据：去掉 RewardTx 里的
// `FOR UPDATE` 后，两个事务会各自读到 count=0 并各插两行。
func TestRewardTxConcurrentRespectsMaxRewards(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	cfg := baseCfg()
	cfg.MaxRewards = 1
	setAff(t, cfg)
	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)

	orders := []uint64{9811, 9812}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errsCh := make(chan error, len(orders))
	for _, oid := range orders {
		wg.Add(1)
		go func(orderID uint64) {
			defer wg.Done()
			<-start
			errsCh <- gdb.Transaction(func(tx *gorm.DB) error {
				return RewardTx(tx, RewardInput{InviteeID: invitee.ID, OrderID: orderID, AmountCent: 10_000})
			})
		}(oid)
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("并发发放不应报错: %v", err)
		}
	}

	var liveOrders int64
	if err := gdb.Model(&model.AffiliateReward{}).
		Where("invitee_id = ? AND NOT voided", invitee.ID).
		Distinct("order_id").Count(&liveOrders).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if liveOrders != 1 {
		t.Fatalf("N=1 时并发两单只应发 1 单，实际 %d（用户行锁可能失效）", liveOrders)
	}
}

// TestRewardTxConcurrentVoidDoesNotOverspend 作废与发放并发时，名额不会被算多。
//
// 覆盖「作废不占名额」在并发下的保守失败方向：并发作废可能让计数偏大，
// 结果是**少发**（可接受），绝不允许超发。
func TestRewardTxConcurrentVoidDoesNotOverspend(t *testing.T) {
	gdb := withTestDB(t)
	migrateAffTables(t, gdb)
	cfg := baseCfg()
	cfg.MaxRewards = 1
	setAff(t, cfg)
	inviter := seedUser(t, gdb)
	invitee := invite(t, gdb, inviter)

	reward(t, gdb, invitee.ID, 9821, 10_000) // 占满 1 个名额（冷却中）

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, fn := range []func() error{
		func() error {
			return gdb.Transaction(func(tx *gorm.DB) error {
				_, e := VoidTx(tx, 9821)
				return e
			})
		},
		func() error {
			return gdb.Transaction(func(tx *gorm.DB) error {
				return RewardTx(tx, RewardInput{InviteeID: invitee.ID, OrderID: 9822, AmountCent: 10_000})
			})
		},
	} {
		wg.Add(1)
		go func(f func() error) {
			defer wg.Done()
			<-start
			if err := f(); err != nil {
				t.Errorf("并发操作报错: %v", err)
			}
		}(fn)
	}
	close(start)
	wg.Wait()

	// 不变量：未作废的订单数永不超过 N。允许 1 或 2 单（作废先成功则新单能发），
	// 但绝不能出现 3 单。
	var live int64
	if err := gdb.Model(&model.AffiliateReward{}).
		Where("invitee_id = ? AND NOT voided", invitee.ID).
		Distinct("order_id").Count(&live).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if live > 1 {
		t.Fatalf("未作废订单数超过 N=1：%d", live)
	}
}
