package controller

import (
	"testing"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/memlimit"
)

// TestMediaProxyFailLocal 锁死「Redis 挂了媒体代理也要限流」。
//
// 这个端点是匿名可打的（HMAC 能力 URL），又真读存储与上游带宽：
// 旧实现 Redis 一挂就 `return true`，等于在最脆弱的时候对公网无限敞开。
// 降级额度按 limit/6 打折（600 → 100），并打 ratelimit_degraded 指标。
func TestMediaProxyFailLocal(t *testing.T) {
	memlimit.Reset()
	forceRedisDown(t)

	ip := "203.0.113.9"
	for i := 0; i < 100; i++ {
		if !allowMediaProxy(ip) {
			t.Fatalf("降级期第 %d 次应在额度内放行", i+1)
		}
	}
	if allowMediaProxy(ip) {
		t.Fatal("降级期超过额度仍必须拒绝：放行等于对公网敞开下载")
	}
}

func forceRedisDown(t *testing.T) {
	t.Helper()
	wasReady := db.RedisReady()
	prev := db.RDB
	db.InitRedis("127.0.0.1:1", "", 0)
	if db.RedisReady() {
		t.Skip("无法把 Redis 置为不可用，跳过降级用例")
	}
	t.Cleanup(func() {
		if wasReady {
			db.InitRedis("127.0.0.1:6379", "", 15)
			return
		}
		db.RDB = prev
	})
}
