package auth

import (
	"context"
	"os"
	"testing"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
)

// 本文件守的是失败计数的**两个不变式**：
//
//  1. 登录成功只清账号维度，不清 IP 维度；
//  2. 登录与改密各用各的命名空间，互不影响。
//
// 两条都是真实缺陷的回归用例：
//   - 原先 clearFails 同时 Del 了 ipFailKey，而 ip_fail_limit 是唯一返回 429 的
//     分支 → 攻击者用自己账号登一次就能把 IP 桶清零，单 IP 每天可试的
//     账号-密码组从 2880 涨到 86400。
//   - 原先改密复用同一个 ipFailKey 且不判 IP 上限 → 已登录用户连错 20 次旧密码
//     就能把同出口所有人的登录打成 429。

func lockTestRedis(t *testing.T) {
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
		t.Skip("Redis 不可用，跳过失败计数用例")
	}
	if conf.System() == nil {
		t.Setenv("LD_APP_ENV", "dev")
		conf.Load()
	}
	t.Cleanup(func() {
		if db.RDB != nil {
			_ = db.RDB.FlushDB(context.Background()).Err()
			_ = db.RDB.Close()
		}
		db.RDB = prev
	})
}

// TestClearFailsKeepsIPBucket 登录成功**不得**清零 IP 桶。
func TestClearFailsKeepsIPBucket(t *testing.T) {
	lockTestRedis(t)
	ctx := context.Background()

	const account, ip = "alice@example.com", "203.0.113.9"
	for i := 0; i < 3; i++ {
		if got := recordFail(ctx, failNSLogin, account, ip); got.IP != i+1 {
			t.Fatalf("第 %d 次失败后 IP 计数应为 %d，实际 %d", i+1, i+1, got.IP)
		}
	}

	clearFails(ctx, failNSLogin, account)

	f := readFails(ctx, failNSLogin, account, ip)
	if f.Account != 0 {
		t.Fatalf("登录成功应清零账号维度，实际 %d", f.Account)
	}
	if f.IP < 3 {
		t.Fatalf("IP 维度不得被登录成功清零（否则 ip_fail_limit 形同虚设），实际 %d", f.IP)
	}
}

// TestFailNamespacesAreIsolated 改密失败不能污染登录的 IP 桶。
func TestFailNamespacesAreIsolated(t *testing.T) {
	lockTestRedis(t)
	ctx := context.Background()

	const ip = "198.51.100.7"
	for i := 0; i < 5; i++ {
		recordFail(ctx, failNSPassword, "bob", ip)
	}

	if f := readFails(ctx, failNSLogin, "bob", ip); f.IP != 0 {
		t.Fatalf("改密失败不应计入登录 IP 桶，实际 %d", f.IP)
	}
	if f := readFails(ctx, failNSPassword, "bob", ip); f.IP != 5 {
		t.Fatalf("改密 IP 桶应为 5，实际 %d", f.IP)
	}
}

// TestRecordFailSetsTTL 计数键必须有 TTL。
//
// 历史上 INCR 与 EXPIRE 是两次往返，第二步失败会留下**永不过期**的键：
// 该 IP 从此永久 429、该账号永久要验证码，且没有任何自愈路径。
func TestRecordFailSetsTTL(t *testing.T) {
	lockTestRedis(t)
	ctx := context.Background()

	recordFail(ctx, failNSLogin, "carol", "192.0.2.5")

	for _, key := range []string{
		acctFailKey(failNSLogin, "carol"),
		ipFailKey(failNSLogin, "192.0.2.5"),
	} {
		ttl, err := db.RDB.TTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("读取 %s 的 TTL 失败: %v", key, err)
		}
		if ttl <= 0 {
			t.Fatalf("%s 没有 TTL（ttl=%v），会永久锁死", key, ttl)
		}
	}
}
