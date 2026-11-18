package keyhealth

import (
	"context"
	"testing"
	"time"

	"lingdong-api/internal/db"
)

// withRedis 尝试连接本机 Redis（独立 DB 15，避免污染业务键）；不可用时跳过用例。
// 注意：**不在这里关闭客户端**——db.RDB 是进程级单例，其他用例还会用它；
// 关闭后 RedisReady() 仍是 true，后续调用会静默失败（fail-open 口径），导致用例假阴性。
func withRedis(t *testing.T) {
	t.Helper()
	if db.RedisReady() {
		return
	}
	db.InitRedis("127.0.0.1:6379", "", 15)
	if !db.RedisReady() {
		t.Skip("未找到可用 Redis，跳过密钥冷却集成用例")
	}
}

func cleanupKeys(t *testing.T, chID uint64, ids ...string) {
	t.Helper()
	ctx := context.Background()
	keys := []string{errKey(chID)}
	for _, id := range ids {
		keys = append(keys, coolKey(chID, id), failKey(chID, id))
	}
	t.Cleanup(func() { db.RDB.Del(ctx, keys...) })
}

// TestBackoffSequences 断言退避序列按原因分底数、按 2 倍递增、封顶 10 分钟。
//
// 这是「坏 key 日命中从 1920 次降到 148 次」的全部依据，序列错一位就等于
// 冷却时长错一倍（第一次就把好 key 雪藏 90 秒，或坏 key 永远只等 45 秒）。
func TestBackoffSequences(t *testing.T) {
	auth := []time.Duration{45 * time.Second, 90 * time.Second, 180 * time.Second,
		360 * time.Second, 600 * time.Second, 600 * time.Second}
	for i, want := range auth {
		if got := Backoff(ReasonAuth, i+1); got != want {
			t.Errorf("auth 第 %d 次退避应为 %v，实际 %v", i+1, want, got)
		}
	}
	rl := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second,
		120 * time.Second, 240 * time.Second, 480 * time.Second, 600 * time.Second, 600 * time.Second}
	for i, want := range rl {
		if got := Backoff(ReasonRateLimit, i+1); got != want {
			t.Errorf("rl 第 %d 次退避应为 %v，实际 %v", i+1, want, got)
		}
	}
	// 非法入参不许产生 0 或负数（那会让 key 立刻"冷却到期"）
	if d := Backoff("", 0); d != CooldownRateLimit {
		t.Errorf("n<1 时按首次处理，实际 %v", d)
	}
	if d := Backoff(ReasonAuth, 99); d != CooldownMax {
		t.Errorf("溢出应封顶，实际 %v", d)
	}
}

// TestCoolClear 覆盖「冷却→命中→清除→不再命中」的完整生命周期。
func TestCoolClear(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90001
	cleanupKeys(t, chID, "k0", "k1")

	if got := CoolingState(ctx, chID, []string{"k0", "k1"}); got != nil {
		t.Fatalf("初始不应有冷却：%v", got)
	}
	Cool(ctx, chID, "k1", ReasonRateLimit, "")
	got := CoolingState(ctx, chID, []string{"k0", "k1"})
	if len(got) != 1 || got["k1"] != ReasonRateLimit {
		t.Fatalf("k1 应以限流原因处于冷却：%v", got)
	}
	Clear(ctx, chID, "k1")
	if got := CoolingState(ctx, chID, []string{"k0", "k1"}); got != nil {
		t.Fatalf("清除后不应再有冷却：%v", got)
	}
}

// TestCoolKeepsReason 断言冷却原因被写入 value。
//
// 这不是「存个备注」：整池冷却时对外是 429（等一会）还是 5xx（key 坏了）完全由它决定，
// 存错就是撒谎 —— 告诉用户「过几秒再来」而实际上永远不会好。
func TestCoolKeepsReason(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90003
	cleanupKeys(t, chID, "a", "b")

	Cool(ctx, chID, "a", ReasonAuth, "401|auth|1")
	Cool(ctx, chID, "b", ReasonRateLimit, "429|rl|2")
	got := CoolingState(ctx, chID, []string{"a", "b"})
	if got["a"] != ReasonAuth || got["b"] != ReasonRateLimit {
		t.Fatalf("原因应原样保留，实际 %v", got)
	}
	// 现场只进「最近错误」，不参与判定
	if m := LastErrors(ctx, chID, []string{"a", "b"}); m["a"] != "401|auth|1" || m["b"] != "429|rl|2" {
		t.Fatalf("最近错误应可批量读回，实际 %v", m)
	}
}

// TestCoolNoopWhileCooling 断言冷却期内再次失败**不重复计数**。
//
// 不加这条判断时，5 个并发请求同时拿到 401 会把退避从 45s 直接顶到 10 分钟，
// 好 key 被雪藏 —— 这是"看起来更严格、实际更糟"的典型。
func TestCoolNoopWhileCooling(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90006
	cleanupKeys(t, chID, "k")

	Cool(ctx, chID, "k", ReasonAuth, "")
	first, err := db.RDB.PTTL(ctx, coolKey(chID, "k")).Result()
	if err != nil || first <= 0 {
		t.Fatalf("首次冷却应写入 TTL：%v %v", first, err)
	}
	n, _ := db.RDB.Get(ctx, failKey(chID, "k")).Int()
	if n != 1 {
		t.Fatalf("首次失败计数应为 1，实际 %d", n)
	}

	Cool(ctx, chID, "k", ReasonAuth, "")
	second, _ := db.RDB.PTTL(ctx, coolKey(chID, "k")).Result()
	if second > first {
		t.Fatalf("冷却期内不应延长 TTL：first=%v second=%v", first, second)
	}
	if n, _ := db.RDB.Get(ctx, failKey(chID, "k")).Int(); n != 1 {
		t.Fatalf("冷却期内不应增加计数，实际 %d", n)
	}
}

// TestCoolDoublesAfterExpiry 断言冷却到期后再次失败会翻倍（模拟"一直坏"的 key）。
func TestCoolDoublesAfterExpiry(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90007
	cleanupKeys(t, chID, "k")

	Cool(ctx, chID, "k", ReasonAuth, "")
	// 手动删掉冷却键 = 模拟 TTL 自然到期（计数键保留）
	db.RDB.Del(ctx, coolKey(chID, "k"))
	Cool(ctx, chID, "k", ReasonAuth, "")
	ttl, _ := db.RDB.PTTL(ctx, coolKey(chID, "k")).Result()
	if ttl < 80*time.Second || ttl > 91*time.Second {
		t.Fatalf("第二次应退避到 ≈90s，实际 %v", ttl)
	}
}

// TestClearResetsBackoff 断言成功一次即清零（否则偶发抖动会累积成越来越长的冷却）。
func TestClearResetsBackoff(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90008
	cleanupKeys(t, chID, "k")

	Cool(ctx, chID, "k", ReasonAuth, "")
	db.RDB.Del(ctx, coolKey(chID, "k"))  // 模拟到期
	Cool(ctx, chID, "k", ReasonAuth, "") // 现在计数=2
	Clear(ctx, chID, "k")
	if n, _ := db.RDB.Exists(ctx, failKey(chID, "k")).Result(); n != 0 {
		t.Fatal("成功后必须清掉失败计数")
	}
	Cool(ctx, chID, "k", ReasonAuth, "")
	ttl, _ := db.RDB.PTTL(ctx, coolKey(chID, "k")).Result()
	if ttl > 46*time.Second {
		t.Fatalf("清零后应回到首次 45s，实际 %v", ttl)
	}
}

// TestCoolLegacyValueTreatedAsRateLimit 断言改造前写下的 "1" 仍被认作冷却。
//
// 滚动发布期间新旧进程会同时读写同一个 Redis：老进程写 "1"，新进程必须理解它，
// 否则老进程冷却的 key 会在新进程里被当成「未冷却」立刻重选，冷却窗直接被绕过。
func TestCoolLegacyValueTreatedAsRateLimit(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90004
	cleanupKeys(t, chID, "legacy")

	db.RDB.Set(ctx, coolKey(chID, "legacy"), "1", time.Minute)
	got := CoolingState(ctx, chID, []string{"legacy"})
	if got["legacy"] != ReasonRateLimit {
		t.Fatalf("历史值 \"1\" 应按限流原因处理，实际 %v", got)
	}
}

// TestCoolExpires 断言冷却自动到期（TTL 生效，不需要人工清理）。
func TestCoolExpires(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90002
	cleanupKeys(t, chID, "k")

	// 直接写一个 300ms 的冷却键：Cool 的时长由退避决定（最短 15s），
	// 等 15 秒的用例没有价值，这里验的是"TTL 到期就会被 CoolingState 视为未冷却"。
	db.RDB.Set(ctx, coolKey(chID, "k"), ReasonRateLimit, 300*time.Millisecond)
	if got := CoolingState(ctx, chID, []string{"k"}); got["k"] != ReasonRateLimit {
		t.Fatalf("刚冷却应命中：%v", got)
	}
	time.Sleep(400 * time.Millisecond)
	if got := CoolingState(ctx, chID, []string{"k"}); got != nil {
		t.Fatalf("到期后应自动恢复：%v", got)
	}
}

// TestMinCoolingTTL 断言返回最短剩余冷却（决定 Retry-After 的那个数）。
func TestMinCoolingTTL(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90005
	cleanupKeys(t, chID, "auth", "rl")

	Cool(ctx, chID, "auth", ReasonAuth, "")    // 45s
	Cool(ctx, chID, "rl", ReasonRateLimit, "") // 15s
	ttl, ok := MinCoolingTTL(ctx, chID, []string{"auth", "rl"})
	if !ok {
		t.Fatal("应取到剩余冷却时长")
	}
	// 取最小值：让客户端等待最早恢复的那枚，而不是最晚的。
	if ttl <= 0 || ttl > 16*time.Second {
		t.Fatalf("应返回最短剩余时长（≈15s），实际 %v", ttl)
	}
}

// TestCoolIgnoresInvalidInput 断言非法入参不会写入脏键（无渠道 / 空 keyID）。
func TestCoolIgnoresInvalidInput(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	Cool(ctx, 0, "k", ReasonRateLimit, "")
	Cool(ctx, 1, "", ReasonRateLimit, "")
	if got := CoolingState(ctx, 1, []string{"k", ""}); got != nil {
		t.Fatalf("非法入参不应产生冷却：%v", got)
	}
}

// TestKeysIncludeChannelAndKeyID 断言键含渠道与 keyID 两个维度（不同渠道不互相干扰）。
func TestKeysIncludeChannelAndKeyID(t *testing.T) {
	if coolKey(1, "a") == coolKey(2, "a") || coolKey(1, "a") == coolKey(1, "b") {
		t.Fatal("冷却键必须同时区分渠道与 keyID")
	}
	if coolKey(1, "abc") != "ld:ch:kcd:1:abc" {
		t.Fatalf("冷却键格式被改动: %s", coolKey(1, "abc"))
	}
}

// TestCoolingTTLsPerKey 断言逐行给的是**本行**剩余时长，而不是全池最小值。
//
// 面板原来把所有冷却中的行都填成 MinCoolingTTL（最短的那个），于是
// "还剩 12s（等一会儿就好）"与"还剩 9min（该换掉了）"看起来一模一样，
// 运维按错误的时间做决定。
func TestCoolingTTLsPerKey(t *testing.T) {
	withRedis(t)
	ctx := context.Background()
	const chID uint64 = 90012
	cleanupKeys(t, chID, "slow", "fast")

	Cool(ctx, chID, "slow", ReasonAuth, "")      // 45s
	Cool(ctx, chID, "fast", ReasonRateLimit, "") // 15s

	ttls := CoolingTTLs(ctx, chID, []string{"slow", "fast"})
	if len(ttls) != 2 {
		t.Fatalf("两把都在冷却，应返回两条，实际 %v", ttls)
	}
	if ttls["slow"] <= ttls["fast"] {
		t.Fatalf("长的应比短的更长：slow=%v fast=%v", ttls["slow"], ttls["fast"])
	}
	if ttl := ttls["fast"]; ttl <= 0 || ttl > 16*time.Second {
		t.Fatalf("fast 应≈15s，实际 %v", ttl)
	}
	if ttl := ttls["slow"]; ttl <= 30*time.Second || ttl > 46*time.Second {
		t.Fatalf("slow 应≈45s，实际 %v", ttl)
	}

	// 未冷却的 key 不出现在结果里（否则面板会给它画一个 0s 的冷却徽标）。
	only := CoolingTTLs(ctx, chID, []string{"never"})
	if len(only) != 0 {
		t.Fatalf("未冷却的 key 不该出现：%v", only)
	}
}
