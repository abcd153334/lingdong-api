package mfa

import (
	"context"
	"os"
	"testing"

	"lingdong-api/internal/db"
)

// 本文件守的是「2FA 码不能无限试」这条不变式。
//
// 历史缺陷：Verify 没有任何次数上限，只剩 `user_sess` 的 300/min 兜着，
// 而 `mfa_code_invalid` 与成功可区分 —— 等于给了一个可测量的试码探针。
// 6 位动态码、30 秒一换，300/min 的额度足以在窗口内打出有意义的命中率。

func mfaTestRedis(t *testing.T) {
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
		t.Skip("Redis 不可用，跳过 2FA 失败计数用例")
	}
	t.Cleanup(func() {
		if db.RDB != nil {
			_ = db.RDB.FlushDB(context.Background()).Err()
			_ = db.RDB.Close()
		}
		db.RDB = prev
	})
}

func TestMFAFailCounterBlocksAfterMax(t *testing.T) {
	mfaTestRedis(t)
	ctx := context.Background()
	const uid uint64 = 42

	clearMFAFails(ctx, uid)
	if got := mfaFails(ctx, uid); got != 0 {
		t.Fatalf("初始计数应为 0，实际 %d", got)
	}

	for i := 0; i < maxVerifyFails; i++ {
		bumpMFAFail(ctx, uid)
	}
	if got := mfaFails(ctx, uid); got != maxVerifyFails {
		t.Fatalf("失败 %d 次后计数应为 %d，实际 %d", maxVerifyFails, maxVerifyFails, got)
	}
	// 达到上限后必须被 Verify 的前置检查拦住（这里断言判据本身）。
	if mfaFails(ctx, uid) < maxVerifyFails {
		t.Fatal("达到上限时应判定为已锁定")
	}

	// 成功一次必须清零：否则用户手滑几次后即使输对也要等 15 分钟。
	clearMFAFails(ctx, uid)
	if got := mfaFails(ctx, uid); got != 0 {
		t.Fatalf("成功后应清零，实际 %d", got)
	}
}

func TestMFAFailCounterHasTTL(t *testing.T) {
	mfaTestRedis(t)
	ctx := context.Background()
	const uid uint64 = 43

	bumpMFAFail(ctx, uid)
	ttl, err := db.RDB.TTL(ctx, mfaFailKey(uid)).Result()
	if err != nil {
		t.Fatalf("读取 TTL 失败: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("失败计数必须有 TTL（ttl=%v），否则一次锁定永久有效", ttl)
	}
}

// 失败计数按 uid 而不是按 IP：换 IP 不该重置额度（否则挂代理即可绕过）。
func TestMFAFailCounterKeyedByUID(t *testing.T) {
	mfaTestRedis(t)
	ctx := context.Background()

	bumpMFAFail(ctx, 100)
	if got := mfaFails(ctx, 101); got != 0 {
		t.Fatalf("别的账号不该受影响，实际 %d", got)
	}
	if got := mfaFails(ctx, 100); got != 1 {
		t.Fatalf("本账号计数应为 1，实际 %d", got)
	}
}
