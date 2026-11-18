package db

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestRedisHealthProbeMarksUnavailable 验证 P2-18 的「连续失败才降级」：
// Redis 连不上时，探测必须在若干次失败后把 RedisReady 置为 false（否则每个请求都去等一个死连接）。
func TestRedisHealthProbeMarksUnavailable(t *testing.T) {
	prevRDB, prevReady := RDB, redisReady.Load()
	t.Cleanup(func() {
		RDB = prevRDB
		redisReady.Store(prevReady)
	})

	// 指向必然拒绝连接的地址（127.0.0.1:1），并关闭重试以保证快速失败
	RDB = redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1,
	})
	redisReady.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRedisHealthProbe(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !RedisReady() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("连续探测失败后 RedisReady 应变为 false")
}

// TestRedisHealthProbeRecovers 验证恢复路径：启动时因 Redis 未就绪而永久降级是错误的，
// 探测一旦成功必须立刻把标志恢复（否则限流/leader 锁永远不生效）。
func TestRedisHealthProbeRecovers(t *testing.T) {
	prevRDB, prevReady := RDB, redisReady.Load()
	t.Cleanup(func() {
		RDB = prevRDB
		redisReady.Store(prevReady)
	})

	client := InitRedis("127.0.0.1:6379", "", 15)
	if !RedisReady() {
		t.Skip("未找到可用 Redis，跳过恢复用例")
	}
	redisReady.Store(false) // 模拟「启动时没连上，之后 Redis 起来了」

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartRedisHealthProbe(ctx, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if RedisReady() {
			t.Cleanup(func() { _ = client.Close() })
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Redis 可用时探测应把 RedisReady 恢复为 true")
}
