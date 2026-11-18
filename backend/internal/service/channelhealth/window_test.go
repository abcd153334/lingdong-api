package channelhealth

import (
	"context"
	"math"
	"testing"

	"github.com/redis/go-redis/v9"

	"lingdong-api/internal/db"
)

// withWindowRedis 连接本机 Redis（DB 15，与业务键隔离）；不可用时跳过。
// 不关闭客户端：db.RDB 是进程级单例。
func withWindowRedis(t *testing.T) {
	t.Helper()
	if db.RedisReady() {
		return
	}
	db.InitRedis("127.0.0.1:6379", "", 15)
	if !db.RedisReady() {
		t.Skip("未找到可用 Redis，跳过熔断滑窗用例")
	}
}

// seedWindow 直接写滑窗 ZSET（键与 Record 完全一致），避免依赖 conf/DB。
func seedWindow(t *testing.T, channelID uint64, total, fails int) {
	t.Helper()
	ctx := context.Background()
	reqKey, failKey := windowKeys(channelID)
	db.RDB.Del(ctx, reqKey, failKey)
	t.Cleanup(func() { db.RDB.Del(context.Background(), reqKey, failKey) })
	for i := 0; i < total; i++ {
		member := string(rune('a'+i%26)) + string(rune('0'+i/26))
		db.RDB.ZAdd(ctx, reqKey, redis.Z{Score: float64(i), Member: member})
		if i < fails {
			db.RDB.ZAdd(ctx, failKey, redis.Z{Score: float64(i), Member: "f" + member})
		}
	}
}

// TestWindowStats 断言滑窗样本的读取口径：
// 有流量的渠道给出 total/fails/fail_rate；**无流量不返回**（0 样本 ≠ 0% 失败）。
func TestWindowStats(t *testing.T) {
	withWindowRedis(t)
	ctx := context.Background()

	const a, b, c uint64 = 92001, 92002, 92003
	seedWindow(t, a, 5, 2)  // 40%
	seedWindow(t, b, 0, 0)  // 无流量
	seedWindow(t, c, 12, 7) // 58.3%

	got := WindowStats(ctx, []uint64{a, b, c})
	if _, ok := got[b]; ok {
		t.Fatal("无流量的渠道不应出现在结果里（0 样本不等于 0% 失败）")
	}
	wa, ok := got[a]
	if !ok {
		t.Fatal("渠道 a 应有滑窗数据")
	}
	if wa.Total != 5 || wa.Fails != 2 {
		t.Fatalf("渠道 a 应为 5 次 / 2 失败，实际 %d / %d", wa.Total, wa.Fails)
	}
	if math.Abs(wa.FailRate-0.4) > 1e-9 {
		t.Fatalf("渠道 a 失败率应为 0.4，实际 %v", wa.FailRate)
	}
	wc, ok := got[c]
	if !ok || wc.Total != 12 || wc.Fails != 7 {
		t.Fatalf("渠道 c 应为 12 次 / 7 失败，实际 %+v", wc)
	}
}

// TestWindowStatsEmptyInput 空入参不应打 Redis（返回空 map，不报错）。
func TestWindowStatsEmptyInput(t *testing.T) {
	withWindowRedis(t)
	if got := WindowStats(context.Background(), nil); len(got) != 0 {
		t.Fatalf("空入参应返回空 map，实际 %v", got)
	}
}

// TestWindowStatsMatchesEvaluate 断言「界面显示的滑窗」与「熔断判定用的数据」是同一份：
// 同一组样本喂给 Evaluate，结论必须与窗口数值一致。
func TestWindowStatsMatchesEvaluate(t *testing.T) {
	withWindowRedis(t)
	ctx := context.Background()
	pol := Policy{AutoDisable: true, FailThreshold: 0.5, FailWindow: 10}

	const hot, cold uint64 = 92004, 92005
	seedWindow(t, hot, 20, 12) // 60% > 50%，样本 20 ≥ 10 → 应熔断
	seedWindow(t, cold, 8, 5)  // 62.5% 但样本 8 < 10 → 不熔断

	got := WindowStats(ctx, []uint64{hot, cold})
	if !Evaluate(got[hot].Total, got[hot].Fails, pol) {
		t.Fatalf("滑窗 20/12 应判定熔断，实际未熔断")
	}
	if Evaluate(got[cold].Total, got[cold].Fails, pol) {
		t.Fatalf("滑窗 8/5 样本不足，不应判定熔断（这正是界面上要解释的场景）")
	}
}
