package task

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/service/billing"
)

// seedPendingSessionForTask 造一条 **pending** 计费会话并绑到任务上。
//
// 这正是历史缺陷的现场：失败路径的释放是独立事务，可能在 CAS 之后、Release 之前
// 崩溃（或 DB 抖动），于是旧 usage 停在 pending、冻结额还挂在用户身上。
// 重试时若不先释放，就会再冻结一份 —— 用户被扣两次，旧的那份要等回收任务兜底。
func seedPendingSessionForTask(t *testing.T, gdb *gorm.DB, uid, kid uint64, modelID string, taskID uint64, price int64) uint64 {
	t.Helper()
	ctx := context.Background()
	svc := billing.New(gdb)
	p := price
	sess, err := svc.Freeze(ctx, billing.FreezeInput{
		UserID: uid, KeyID: kid, Model: modelID, Modality: "video", Group: "default",
		Snapshot: billing.Snapshot{
			ModelID: modelID, BillingMode: model.BillingModeSpec,
			SpecPrice: model.SpecPrices{Kind: billing.SpecKindPerTask, PerTaskPrice: &p},
		},
		Amount:   price,
		CallKind: billing.CallKindAsync,
	})
	if err != nil {
		t.Fatalf("造 pending 会话失败: %v", err)
	}
	if err := svc.BindTask(ctx, sess.UsageID, taskID); err != nil {
		t.Fatalf("绑定 usage↔task 失败: %v", err)
	}
	return sess.UsageID
}

// TestRetryReleasesPreviousPendingBeforeRefreeze 是「二次冻结」的回归锁。
//
// 断言两件事：
//  1. 旧 pending 会话被释放（status=released，钱退回）；
//  2. 重试后用户只被冻结**一份**（frozen == price，不是 2×price）。
//
// 没有修复时会看到 frozen=2000 —— 余额够的话 Freeze 会成功，于是问题不在报错，
// 而在"用户被默默扣了两份"。
func TestRetryReleasesPreviousPendingBeforeRefreeze(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 5000)
	price := int64(1000)
	seedRoutableChannel(t, gdb, "vid-pend")
	if err := relay.RegisterAdapter(fakeTaskAdapter{}); err != nil {
		t.Fatalf("注册测试适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-async") })

	row := seedFailedTask(t, gdb, uid, kid, "vid-pend", price)
	// 把任务指到一个**仍然 pending** 的会话上（覆盖 seedFailedTask 造的已释放行）。
	oldUsage := seedPendingSessionForTask(t, gdb, uid, kid, "vid-pend", row.ID, price)
	if err := gdb.Model(&model.Task{}).Where("id = ?", row.ID).Update("usage_id", oldUsage).Error; err != nil {
		t.Fatalf("改写任务会话指针失败: %v", err)
	}
	// 冻结即占用 KEY 额度：预扣阶段 api_keys.used 就已经加上去了（释放时回冲）。
	assertQuota(t, gdb, uid, kid, 4000, 1000, price)

	if _, err := Retry(ctx, row.ID, AllScope()); err != nil {
		t.Fatalf("重试失败: %v", err)
	}

	var got model.Task
	if err := gdb.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.UsageID == oldUsage {
		t.Fatal("重试应挂载新会话")
	}
	var old model.Usage
	if err := gdb.First(&old, oldUsage).Error; err != nil {
		t.Fatal(err)
	}
	if old.Status != model.UsageStatusReleased {
		t.Fatalf("旧会话应被释放，实际 status=%s", old.Status)
	}
	// 关键断言：frozen 仍然只有一份，KEY 额度也只剩新会话那一份。
	assertQuota(t, gdb, uid, kid, 4000, price, price)
}

// TestRetryConcurrentFreezesOnlyOnce：并发重试只允许一次冻结。
//
// CAS 是唯一互斥点；这条用例守的是「释放 + 冻结」这对动作不会因为并发而重复。
func TestRetryConcurrentFreezesOnlyOnce(t *testing.T) {
	gdb := withTestDB(t)
	ctx := context.Background()
	uid, kid := seedUserAndKey(t, gdb, 5000)
	price := int64(1000)
	seedRoutableChannel(t, gdb, "vid-race")
	if err := relay.RegisterAdapter(fakeTaskAdapter{}); err != nil {
		t.Fatalf("注册测试适配器失败: %v", err)
	}
	t.Cleanup(func() { relay.UnregisterAdapter("fake-async") })

	row := seedFailedTask(t, gdb, uid, kid, "vid-race", price)
	oldUsage := seedPendingSessionForTask(t, gdb, uid, kid, "vid-race", row.ID, price)
	if err := gdb.Model(&model.Task{}).Where("id = ?", row.ID).Update("usage_id", oldUsage).Error; err != nil {
		t.Fatalf("改写任务会话指针失败: %v", err)
	}

	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Retry(ctx, row.ID, AllScope()); err != nil {
				if !errors.Is(err, ErrNotRetryable) {
					t.Errorf("非幂等拒绝的错误：%v", err)
				}
				return
			}
			mu.Lock()
			okCount++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("并发重试应只有 1 次成功，实际 %d", okCount)
	}
	// 无论并发顺序如何，用户身上只应有一份冻结。
	var u model.User
	if err := gdb.First(&u, uid).Error; err != nil {
		t.Fatal(err)
	}
	if u.Frozen != price {
		t.Fatalf("并发重试后冻结额应为 %d，实际 %d", price, u.Frozen)
	}
}
