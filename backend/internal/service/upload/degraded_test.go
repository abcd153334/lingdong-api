package upload

import (
	"testing"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/memlimit"
)

// TestUploadConcurrencyFailLocal 锁死「Redis 挂了上传并发也要限流」。
//
// 回归的真实缺口：旧实现 `if !db.RedisReady() { return func(){}, true }` ——
// Redis 一挂，上传并发闸直接放行，任何一个登录用户都能开无限并发上传，
// 把存储与带宽打满。取向必须与 Key 维度两道闸一致（fail-local，不是 fail-open）。
//
// 降级后仍是**并发数**限制（进程内信号量），语义与 Redis 版一致：
// limit=3 时第 4 个同时在传必须被拒；而"连着传多个、任意时刻只有 1 个"不受影响。
func TestUploadConcurrencyFailLocal(t *testing.T) {
	memlimit.Reset()
	forceRedisDown(t)

	const uid = uint64(998877)
	var rels []func()
	for i := 0; i < 3; i++ {
		rel, ok := acquireConcurrency(3, uid)
		if !ok {
			t.Fatalf("降级期第 %d 个并发应放行", i+1)
		}
		rels = append(rels, rel)
	}
	if _, ok := acquireConcurrency(3, uid); ok {
		t.Fatal("降级期超出并发上限必须拒绝：放行等于对任何人敞开上传并发")
	}
	// 释放一个后应能再拿：证明降级路径是并发语义而不是速率语义。
	rels[0]()
	if _, ok := acquireConcurrency(3, uid); !ok {
		t.Fatal("释放后应可再次获取：降级路径必须是并发信号量，不是每分钟计数")
	}
}

// forceRedisDown 把进程级 Redis 置为不可用，并在用例结束后恢复。
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
