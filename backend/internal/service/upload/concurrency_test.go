package upload

import (
	"context"
	"fmt"
	"os"
	"testing"

	"lingdong-api/internal/db"
)

// withTestRedis 连接测试 Redis（DB15，与其它 Redis 用例同一约定）；不可用则跳过。
func withTestRedis(t *testing.T) {
	t.Helper()
	addr := os.Getenv("LD_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	prev := db.RDB
	db.InitRedis(addr, "", 15)
	if !db.RedisReady() {
		if prev != nil {
			db.RDB = prev
		}
		t.Skip("Redis 不可用，跳上传并发闸用例")
	}
	t.Cleanup(func() { db.RDB = prev })
}

// TestUploadConcurrencyHasTTLAndClamps 守住两条：并发键必须有 TTL、归还不得减成负数。
//
// 为什么这两条必须有用例：它俩失效时症状极隐蔽 —— 前者是"进程被杀后该用户
// **永久**上传 429"（只有人工 DEL 能救），后者是"每次 TTL 到期后白送一个并发额度"。
// 两者都不报错、都不影响其它用户，靠人眼看代码很容易漏。
func TestUploadConcurrencyHasTTLAndClamps(t *testing.T) {
	withTestRedis(t)
	ctx := context.Background()
	const uid = uint64(9999991)
	key := fmt.Sprintf("%s%d", concurrencyKeyPrefix, uid)
	db.RDB.Del(ctx, key)

	r1, ok1 := acquireConcurrency(2, uid)
	r2, ok2 := acquireConcurrency(2, uid)
	_, ok3 := acquireConcurrency(2, uid)
	if !ok1 || !ok2 {
		t.Fatalf("前两次应放行：ok1=%v ok2=%v", ok1, ok2)
	}
	if ok3 {
		t.Fatal("超过上限的第三次必须拒绝")
	}

	// 被拒的那次已经归还，计数应回到 2
	if v := db.RDB.Get(ctx, key).Val(); v != "2" {
		t.Fatalf("拒绝后计数应归还回 2，实际 %q", v)
	}
	if pttl := db.RDB.PTTL(ctx, key).Val(); pttl <= 0 {
		t.Fatalf("并发键必须带 TTL（否则进程被 SIGKILL 后该用户永久 429），pttl=%v", pttl)
	}

	r1()
	r2()
	// 多还一次：钳零，不能出现 -1
	r1()
	if v := db.RDB.Get(ctx, key).Val(); v != "0" {
		t.Fatalf("多归还后应钳到 0，实际 %q（负数会让下一次 INCR 从 0 开始，白送额度）", v)
	}
}

// TestUploadConcurrencyRepairsLeakedKey 守住"旧泄漏键原子自愈"。
//
// 只在 n==1 时挂 TTL 是不够的：升级前泄漏的键已经存在且没有 TTL，
// 它们永远不会再满足 n==1 —— 那些用户会一直 429。所以每次 acquire 都要补 TTL。
func TestUploadConcurrencyRepairsLeakedKey(t *testing.T) {
	withTestRedis(t)
	ctx := context.Background()
	const uid = uint64(9999992)
	key := fmt.Sprintf("%s%d", concurrencyKeyPrefix, uid)
	db.RDB.Del(ctx, key)
	// 模拟旧版本/SIGKILL 留下的计数：有值、无 TTL
	if err := db.RDB.Set(ctx, key, 5, 0).Err(); err != nil {
		t.Fatalf("准备泄漏键失败：%v", err)
	}
	if pttl := db.RDB.PTTL(ctx, key).Val(); pttl > 0 {
		t.Fatalf("前置条件不成立：键应无 TTL，pttl=%v", pttl)
	}

	_, ok := acquireConcurrency(3, uid)
	if ok {
		t.Fatal("5 已超过上限 3，必须拒绝")
	}
	if pttl := db.RDB.PTTL(ctx, key).Val(); pttl <= 0 {
		t.Fatalf("旧泄漏键在下一次 acquire 时必须补上 TTL，pttl=%v", pttl)
	}
}
