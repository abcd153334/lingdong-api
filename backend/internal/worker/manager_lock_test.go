package worker

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/db"
)

// withRedis 连接本机 Redis（DB 15）用于 leader 锁语义断言；不可用时跳过。
func withRedis(t *testing.T) {
	t.Helper()
	if db.RedisReady() {
		return
	}
	db.InitRedis("127.0.0.1:6379", "", 15)
	if !db.RedisReady() {
		t.Skip("未找到可用 Redis，跳过 leader 锁用例")
	}
}

// TestRenewFailsWhenLockHeldByOthers 验证 fail-closed（P2-11）：
// 锁在别的实例手里时，本实例必须立刻认为自己不是 leader，而不是继续跑定时任务。
func TestRenewFailsWhenLockHeldByOthers(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	if err := db.RDB.Set(ctx, lockKey, "another-instance", time.Minute).Err(); err != nil {
		t.Fatalf("写入锁失败: %v", err)
	}
	t.Cleanup(func() { db.RDB.Del(ctx, lockKey) })

	m := &Manager{instanceID: "me"}
	if m.renew(ctx) {
		t.Fatal("锁不属于本实例时 renew 必须返回 false")
	}
	// 关键：别人的锁不能被续期/覆盖
	if v, _ := db.RDB.Get(ctx, lockKey).Result(); v != "another-instance" {
		t.Fatalf("不应改动他人持有的锁，实际 %q", v)
	}
}

// TestRenewExtendsOwnLock 验证持有者能续期，且 TTL 被刷新。
func TestRenewExtendsOwnLock(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	if err := db.RDB.Set(ctx, lockKey, "me", 2*time.Second).Err(); err != nil {
		t.Fatalf("写入锁失败: %v", err)
	}
	t.Cleanup(func() { db.RDB.Del(ctx, lockKey) })

	m := &Manager{instanceID: "me"}
	if !m.renew(ctx) {
		t.Fatal("持有者续期应成功")
	}
	ttl, err := db.RDB.PTTL(ctx, lockKey).Result()
	if err != nil {
		t.Fatalf("读取 TTL 失败: %v", err)
	}
	if ttl < lockTTL/2 {
		t.Fatalf("续期后 TTL 应接近 %s，实际 %s", lockTTL, ttl)
	}
}

// TestReleaseDoesNotDeleteOthersLock 验证 CAS-DEL：释放只删自己的锁。
func TestReleaseDoesNotDeleteOthersLock(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	if err := db.RDB.Set(ctx, lockKey, "another-instance", time.Minute).Err(); err != nil {
		t.Fatalf("写入锁失败: %v", err)
	}
	t.Cleanup(func() { db.RDB.Del(ctx, lockKey) })

	(&Manager{instanceID: "me"}).release(ctx)
	if n, _ := db.RDB.Exists(ctx, lockKey).Result(); n != 1 {
		t.Fatal("释放时不应删除他人持有的锁（否则会出现第三个实例抢锁的空窗）")
	}

	// 自己的锁可以被释放
	if err := db.RDB.Set(ctx, lockKey, "me", time.Minute).Err(); err != nil {
		t.Fatalf("写入锁失败: %v", err)
	}
	(&Manager{instanceID: "me"}).release(ctx)
	if n, _ := db.RDB.Exists(ctx, lockKey).Result(); n != 0 {
		t.Fatal("持有者释放后锁应被删除")
	}
}

// TestRenewFailsClosedOnRedisError 验证 Redis 出错时按「失去 leader」处理（fail-closed）。
func TestRenewFailsClosedOnRedisError(t *testing.T) {
	withRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消的 ctx 会让脚本执行失败
	if (&Manager{instanceID: "me"}).renew(ctx) {
		t.Fatal("Redis 操作失败时必须返回 false（宁可少跑一轮，不能双跑）")
	}
}
