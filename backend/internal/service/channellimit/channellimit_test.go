package channellimit

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// withRedis 连接本机 Redis（DB 15，与业务键隔离）；不可用时跳过。
// 不关闭客户端：db.RDB 是进程级单例，关闭会让后续用例静默走降级分支。
func withRedis(t *testing.T) {
	t.Helper()
	if db.RedisReady() {
		return
	}
	db.InitRedis("127.0.0.1:6379", "", 15)
	if !db.RedisReady() {
		t.Skip("未找到可用 Redis，跳过渠道限流集成用例")
	}
}

func ch(id uint64, rpm, conc int) *model.Channel {
	return &model.Channel{ID: id, RPMLimit: rpm, MaxConcurrency: conc}
}

// TestConcurrencyLimit 断言并发槽位：拿满即拒绝，释放后可再拿。
func TestConcurrencyLimit(t *testing.T) {
	resetSlots()
	t.Cleanup(resetSlots)
	ctx := context.Background()
	c := ch(91001, 0, 2)

	r1, err := Acquire(ctx, c)
	if err != nil {
		t.Fatalf("第 1 次获取应成功：%v", err)
	}
	r2, err := Acquire(ctx, c)
	if err != nil {
		t.Fatalf("第 2 次获取应成功：%v", err)
	}

	_, err = Acquire(ctx, c)
	if !IsRateLimited(err) {
		t.Fatalf("超出并发上限应返回限流错误，实际 %v", err)
	}
	var le *LimitError
	if !errors.As(err, &le) || le.Scope != "concurrency" {
		t.Fatalf("应标记 scope=concurrency，实际 %+v", le)
	}

	r1()
	if _, err := Acquire(ctx, c); err != nil {
		t.Fatalf("释放后应可再获取：%v", err)
	}

	// 重复释放不得把额度放大（否则并发上限形同虚设）。
	r1()
	r2()
	got := 0
	for i := 0; i < 5; i++ {
		if _, err := Acquire(ctx, c); err == nil {
			got++
			continue
		}
		break
	}
	if got > 3 {
		t.Fatalf("重复释放不应增加额度，实际又拿到了 %d 次", got)
	}
}

// TestConcurrencyZeroMeansUnlimited 断言 0 = 不限（与 UI「∞」一致）。
func TestConcurrencyZeroMeansUnlimited(t *testing.T) {
	resetSlots()
	ctx := context.Background()
	c := ch(91002, 0, 0)
	for i := 0; i < 100; i++ {
		if _, err := Acquire(ctx, c); err != nil {
			t.Fatalf("limit=0 不应限流，第 %d 次失败：%v", i, err)
		}
	}
}

// TestConcurrencyCapacityChange 断言改配置后新信号量生效（在飞请求归还到旧信号量，不泄漏）。
func TestConcurrencyCapacityChange(t *testing.T) {
	resetSlots()
	t.Cleanup(resetSlots)
	ctx := context.Background()

	old, err := Acquire(ctx, ch(91003, 0, 1))
	if err != nil {
		t.Fatalf("首获取失败：%v", err)
	}
	// 运营把并发上限从 1 改成 2：新配置必须立刻生效，不能等旧槽位释放。
	if _, err := Acquire(ctx, ch(91003, 0, 2)); err != nil {
		t.Fatalf("容量变更后应可获取：%v", err)
	}
	if _, err := Acquire(ctx, ch(91003, 0, 2)); err != nil {
		t.Fatalf("容量 2 的第二个槽位应可获取：%v", err)
	}
	old()
}

// TestRPMLimit 断言固定窗口计数（需要 Redis）。
func TestRPMLimit(t *testing.T) {
	withRedis(t)
	resetSlots()
	t.Cleanup(resetSlots)
	ctx := context.Background()
	const id uint64 = 91004
	key := rpmKeyPrefix + strconv.FormatUint(id, 10) + ":" + strconv.FormatInt(time.Now().Unix()/60, 10)
	t.Cleanup(func() { db.RDB.Del(context.Background(), key) })

	c := ch(id, 2, 0)
	for i := 0; i < 2; i++ {
		rel, err := Acquire(ctx, c)
		if err != nil {
			t.Fatalf("第 %d 次应放行：%v", i+1, err)
		}
		rel()
	}
	_, err := Acquire(ctx, c)
	if !IsRateLimited(err) {
		t.Fatalf("第 3 次应被限流，实际 %v", err)
	}
	var le *LimitError
	if !errors.As(err, &le) || le.Scope != "rpm" {
		t.Fatalf("应标记 scope=rpm，实际 %+v", le)
	}
	if le.RetryAfter < 1 || le.RetryAfter > 60 {
		t.Fatalf("Retry-After 应在 1..60 秒，实际 %d", le.RetryAfter)
	}
}

// Inflight 只报告**本进程**的实时占用。多副本时它不是全局值，
// 管理台必须按「本机」展示，否则运营会把本机已满误读成整条渠道满了。
func TestInflightReportsLocalOccupancy(t *testing.T) {
	resetSlots()
	t.Cleanup(resetSlots)

	ch := &model.Channel{ID: 9911, MaxConcurrency: 2}
	r1, err := Acquire(context.Background(), ch)
	if err != nil {
		t.Fatalf("第一次取槽失败：%v", err)
	}
	r2, err := Acquire(context.Background(), ch)
	if err != nil {
		t.Fatalf("第二次取槽失败：%v", err)
	}
	if cur, limit := Inflight(9911); cur != 2 || limit != 2 {
		t.Fatalf("占用 = %d/%d，期望 2/2", cur, limit)
	}
	// 幂等释放不能偷走别人的额度，也不能让计数变负。
	r1()
	r1()
	if cur, _ := Inflight(9911); cur != 1 {
		t.Fatalf("重复释放后占用 = %d，期望 1", cur)
	}
	r2()
	if cur, _ := Inflight(9911); cur != 0 {
		t.Fatalf("全部释放后占用 = %d，期望 0", cur)
	}
	// 未使用过的渠道：不应当凭空创建一个信号量。
	if cur, limit := Inflight(9912); cur != 0 || limit != 0 {
		t.Fatalf("未知渠道 = %d/%d，期望 0/0", cur, limit)
	}
}

/* -------------------- 多副本：并发上限必须跨实例共享 -------------------- */

// useCluster 把当前用例切到「多副本」模式，结束后恢复。
func useCluster(t *testing.T, on bool) {
	t.Helper()
	prev := clusterCounting
	clusterCounting = func() bool { return on }
	t.Cleanup(func() { clusterCounting = prev })
}

// TestConcurrencySharedAcrossReplicas 锁死「多副本下并发上限不放大」。
//
// 回归的真实事故：进程内信号量在多副本下的真实上限 = 配置 × 副本数。
// 运营按 3 配置、3 个副本就是 9 并发 —— 上游配额被打爆，而管理台显示一切正常。
//
// 判定方式：拿满后**清掉本进程的信号量**（等价于换到另一个副本），
// 仍然必须被拒 —— 说明计数确实在 Redis 里，不在进程内存里。
func TestConcurrencySharedAcrossReplicas(t *testing.T) {
	withRedis(t)
	useCluster(t, true)
	resetSlots()
	t.Cleanup(resetSlots)
	ctx := context.Background()

	const id uint64 = 91005
	key := ccKeyPrefix + strconv.FormatUint(id, 10)
	db.RDB.Del(ctx, key)
	t.Cleanup(func() { db.RDB.Del(context.Background(), key) })

	c := ch(id, 0, 3)
	var releases []func()
	for i := 0; i < 3; i++ {
		rel, err := Acquire(ctx, c)
		if err != nil {
			t.Fatalf("第 %d 次应放行：%v", i+1, err)
		}
		releases = append(releases, rel)
	}

	// 换「另一个副本」：本进程信号量清空。共享计数在 Redis，不受影响。
	resetSlots()
	_, err := Acquire(ctx, c)
	if !IsRateLimited(err) {
		t.Fatalf("多副本下第 4 次必须被拒（计数应在 Redis 共享），实际 %v", err)
	}

	if ttl := db.RDB.PTTL(ctx, key).Val(); ttl <= 0 {
		t.Fatalf("并发计数键必须带 TTL（否则进程被 kill 后该渠道永久少一个额度），实际 %v", ttl)
	}

	// 释放是异步的：轮询等待计数回落到 0。
	releases[0]()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := db.RDB.Get(ctx, key).Int(); n <= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n, _ := db.RDB.Get(ctx, key).Int(); n > 2 {
		t.Fatalf("释放后计数应回落，实际 %d", n)
	}
	for _, r := range releases {
		r()
	}
}

// TestConcurrencyDegradesToLocal 锁死「Redis 不可用也不能放开并发」。
//
// 与 Key 维度两道闸同一取向（fail-local，不是 fail-open）：Redis 挂了要变慢/变保守，
// 不能变成「渠道无限并发」——那正好在最脆弱的时候打爆上游。
func TestConcurrencyDegradesToLocal(t *testing.T) {
	useCluster(t, true)
	resetSlots()
	t.Cleanup(resetSlots)

	// 指向一个必定连不通的端口，让 RedisReady() 为 false。
	prev := db.RDB
	db.InitRedis("127.0.0.1:1", "", 0)
	t.Cleanup(func() { db.RDB = prev })
	if db.RedisReady() {
		t.Skip("无法把 Redis 置为不可用，跳过降级用例")
	}

	ctx := context.Background()
	c := ch(91006, 0, 2)
	var rels []func()
	for i := 0; i < 2; i++ {
		rel, err := Acquire(ctx, c)
		if err != nil {
			t.Fatalf("降级期第 %d 次应放行（走进程内兜底）：%v", i+1, err)
		}
		rels = append(rels, rel)
	}
	if _, err := Acquire(ctx, c); !IsRateLimited(err) {
		t.Fatalf("Redis 不可用时仍必须限流（fail-local），实际 %v", err)
	}
	for _, r := range rels {
		r()
	}
}

// TestInflightClusterReadsSharedCounter 多副本下管理台读到的必须是共享计数。
func TestInflightClusterReadsSharedCounter(t *testing.T) {
	withRedis(t)
	useCluster(t, true)
	ctx := context.Background()

	const id uint64 = 91007
	key := ccKeyPrefix + strconv.FormatUint(id, 10)
	db.RDB.Set(ctx, key, 5, time.Minute)
	t.Cleanup(func() { db.RDB.Del(context.Background(), key) })

	cur, limit := Inflight(id)
	if cur != 5 {
		t.Fatalf("多副本下应读共享计数 5，实际 %d", cur)
	}
	if limit != 0 {
		t.Fatalf("上限不在计数键里，应由调用方用配置补，实际 %d", limit)
	}
}
