package middleware

import (
	"testing"

	"lingdong-api/internal/model"
)

// 这一层只测**进程内兜底**的两个原语：上限是否真的生效、释放是否幂等。
//
// 为什么不在这里测"Redis 挂掉时的整体行为"：`db.RedisReady()` 是进程级标志，
// 单测里没有不侵入的入口去翻它。整体行为由隔离环境的 F1（停 Redis 后仍应出现
// 429）验收 —— 那是端到端证据，比在单测里造一个假 Redis 更有说服力。

func TestLocalConcurrencyRespectsLimit(t *testing.T) {
	resetLocalLimitsForTest()
	const keyID, limit = uint64(7), 2

	r1, ok1 := localConcurrency(keyID, limit)
	r2, ok2 := localConcurrency(keyID, limit)
	if !ok1 || !ok2 {
		t.Fatalf("前两次获取应成功：ok1=%v ok2=%v", ok1, ok2)
	}
	if _, ok3 := localConcurrency(keyID, limit); ok3 {
		t.Fatal("第三次获取应被拒（limit=2）")
	}
	r1()
	if _, ok4 := localConcurrency(keyID, limit); !ok4 {
		t.Fatal("释放一次后应能再获取")
	}
	r2()
}

func TestLocalConcurrencyKeysAreIndependent(t *testing.T) {
	resetLocalLimitsForTest()
	a, okA := localConcurrency(1, 1)
	if !okA {
		t.Fatal("key 1 首次应成功")
	}
	if _, ok := localConcurrency(2, 1); !ok {
		t.Fatal("key 2 不应受 key 1 占用影响")
	}
	// key 1 已经到上限，再要应被拒
	if _, ok := localConcurrency(1, 1); ok {
		t.Fatal("key 1 到上限后应被拒")
	}
	a()
}

// 释放幂等：迟到的第二次释放不许把计数减成负数，否则下一次会"白送"额度。
func TestLocalConcurrencyReleaseIsIdempotent(t *testing.T) {
	resetLocalLimitsForTest()
	const keyID, limit = uint64(9), 1
	rel, ok := localConcurrency(keyID, limit)
	if !ok {
		t.Fatal("首次应成功")
	}
	rel()
	rel() // 重复释放

	r1, ok1 := localConcurrency(keyID, limit)
	if !ok1 {
		t.Fatal("释放后应能再获取")
	}
	if _, ok2 := localConcurrency(keyID, limit); ok2 {
		t.Fatal("重复释放不应额外增加额度")
	}
	r1()
}

func TestLocalKeyRateAllowCap(t *testing.T) {
	resetLocalLimitsForTest()
	const keyID, limit = uint64(11), 3
	for i := 1; i <= limit; i++ {
		if !localKeyRateAllow(keyID, limit) {
			t.Fatalf("第 %d 次应放行", i)
		}
	}
	if localKeyRateAllow(keyID, limit) {
		t.Fatalf("第 %d 次应被拒（limit=%d）", limit+1, limit)
	}
	// 另一把 KEY 不受影响
	if !localKeyRateAllow(12, limit) {
		t.Fatal("不同 KEY 应各自计数")
	}
	if !localKeyRateAllow(keyID, 0) {
		t.Fatal("limit<=0 表示不限，必须放行")
	}
}

// TestAcquireConcurrencyDefaultIsBounded 守住「未配置也必须有限」。
//
// 改造前 `concurrency_limit=0` 意味着**不限**：一把 KEY 开 2000 并发就能占满
// 连接池，全站一起排队。现在 0 走进程内默认 32，-1 才是显式关闭。
func TestAcquireConcurrencyDefaultIsBounded(t *testing.T) {
	resetLocalLimitsForTest()
	k := &model.APIKey{ID: 101}
	rels := make([]func(), 0, keyInFlightDefault)
	for i := 0; i < keyInFlightDefault; i++ {
		rel, ok := acquireConcurrency(k)
		if !ok {
			t.Fatalf("第 %d 个在途应被允许（默认上限 %d）", i+1, keyInFlightDefault)
		}
		rels = append(rels, rel)
	}
	if _, ok := acquireConcurrency(k); ok {
		t.Fatalf("超过默认上限 %d 后应返回 429 语义", keyInFlightDefault)
	}
	rels[0]()
	if _, ok := acquireConcurrency(k); !ok {
		t.Fatal("释放一个在途后应能再进一个")
	}

	// -1 = 显式关闭：老部署要"不限"时必须能配出来。
	off := &model.APIKey{ID: 102, ConcurrencyLimit: -1}
	for i := 0; i < keyInFlightDefault*2; i++ {
		if _, ok := acquireConcurrency(off); !ok {
			t.Fatalf("concurrency_limit=-1 应完全不限，第 %d 个被拒", i+1)
		}
	}
}
