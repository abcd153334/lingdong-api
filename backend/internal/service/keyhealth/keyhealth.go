// Package keyhealth 是渠道密钥级冷却闸（审计项 E2）。
//
// 背景：一个渠道的多枚密钥通常属于同一上游账号池。单枚 key 返回 401/429 只说明「这枚 key
// 坏了 / 被限流」，不代表渠道整体不可用。原实现把这类失败直接计入渠道滑窗
// （channelhealth.Record），结果是「3 枚 key 坏 2」的渠道在攒够样本后被整条熔断（status=3），
// 而实际上换一枚 key 就能成功——这就是 E2 描述的误熔断。
//
// 语义：
//   - Cool  ：把某枚密钥移出可选池（Redis TTL 到期自动恢复，无需人工干预），
//     同时记录冷却原因（鉴权失败 / 限流），整池冷却时据此决定对外是 429 还是 5xx；
//   - Clear ：成功调用后清除冷却与失败计数（一次抖动不该让好 key 长期不可用）；
//   - CoolingState / MinCoolingTTL：批量查询冷却集合与最短剩余时长，供 relay.PickKey 用。
//
// **键是 keyID（明文 HMAC 指纹），不是数组下标**：
// 下标会随「编辑保存整体替换密钥池」而指向另一把 key —— 冷却套错人不会报任何错，
// 只会表现为「明明是好的 key 一直不被使用 / 坏 key 一直在打」。
//
// 降级口径（故意 fail-open）：Redis 不可用时全部视为「未冷却」。宁可让请求再试一枚坏 key
// 拿到真实的上游错误，也不能因为缓存不可用就让整个网关拒绝服务。
package keyhealth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"lingdong-api/internal/db"
)

// 冷却底数与上限：按原因分开，而不是统一一个数。
//
//   - 鉴权失败意味着这枚 key 已失效，只有人工换 key 才会恢复，给足 45s 避免连续打脸；
//   - 429 是上游限流，15s 后大概率可用。
//
// 连续失败按 2 倍退避（`base << (n-1)`），封顶 10 分钟：一把彻底失效的 key
// 从「每 45s 被打一次」变成「每 10min 探一次」，日命中量下降约 92%，
// 又不会像"永久禁用"那样必须人工救活。
const (
	CooldownAuth      = 45 * time.Second
	CooldownRateLimit = 15 * time.Second
	CooldownMax       = 10 * time.Minute
)

// 冷却原因的取值是**写进 value** 的，不是两套键。
//
// 为什么需要区分：整池冷却时对外必须给出不同答案 ——
//   - 全部因限流冷却 → 429 + Retry-After（「等一会儿就好」）；
//   - 有一枚是因鉴权失败冷却 → 我们的 key 坏了，等多久都不会好，报 429 就是撒谎。
//
// 只有一份冷却集合（MGet 一次拿全），原因只是 value 的内容，
// 因此不会出现「索引表和原因表不同步」这种第二真相源。
const (
	ReasonAuth      = "auth"
	ReasonRateLimit = "rl"
)

// failWindow 是连续失败计数的存活窗口：窗口内没有成功就一直累加退避，
// 超过窗口自然归零（等价于"再给一次 45s/15s 的机会"）。
const failWindow = 24 * time.Hour

// errWindow 是「最近一次失败」记录的保留时长（面板展示用，与冷却无关）。
const errWindow = 7 * 24 * time.Hour

// Backoff 返回第 n 次连续失败应冷却多久（n 从 1 开始）。
func Backoff(reason string, n int) time.Duration {
	base := CooldownRateLimit
	if reason == ReasonAuth {
		base = CooldownAuth
	}
	if n < 1 {
		n = 1
	}
	d := base << (n - 1)
	if d > CooldownMax || d <= 0 { // 移位溢出保护：d<=0 说明已经绕回负数
		d = CooldownMax
	}
	return d
}

// coolScript 原子完成：判冷却 → 计数 → 封顶 → 写冷却 → 记最近错误。
//
// 为什么必须是脚本而不是几条命令：
//  1. PTTL 判断与 INCR 不原子时，5 个并发 401 会各自看到"未冷却"，
//     把退避从 45s 一路顶到 10 分钟（好 key 被雪藏）；
//  2. INCR 与 EXPIRE 不原子时，进程崩在两者之间会留下**永久计数键**，退避永不归零。
var coolScript = redis.NewScript(`
local ttl = redis.call('PTTL', KEYS[2])
if ttl > 0 then
  redis.call('HSET', KEYS[3], ARGV[5], ARGV[6])
  redis.call('EXPIRE', KEYS[3], ARGV[7])
  return ttl
end
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('EXPIRE', KEYS[1], ARGV[4]) end
local ms = tonumber(ARGV[2]) * (2 ^ (n - 1))
local cap = tonumber(ARGV[3])
if ms > cap then ms = cap end
redis.call('SET', KEYS[2], ARGV[1], 'PX', ms)
redis.call('HSET', KEYS[3], ARGV[5], ARGV[6])
redis.call('EXPIRE', KEYS[3], ARGV[7])
return ms
`)

// Cool 让某枚密钥进入退避冷却；reason 记录原因（ReasonAuth / ReasonRateLimit）。
//
// state 是「这次失败的现场」（如 `401|auth|1710000000`），只用于面板展示，
// 与冷却时长无关；传空串表示不更新现场。
func Cool(ctx context.Context, channelID uint64, keyID, reason, state string) {
	if channelID == 0 || keyID == "" || !db.RedisReady() {
		return
	}
	if reason == "" {
		reason = ReasonRateLimit
	}
	base := CooldownRateLimit
	if reason == ReasonAuth {
		base = CooldownAuth
	}
	_ = coolScript.Run(ctx, db.RDB,
		[]string{failKey(channelID, keyID), coolKey(channelID, keyID), errKey(channelID)},
		reason, base.Milliseconds(), CooldownMax.Milliseconds(),
		int(failWindow.Seconds()), keyID, state, int(errWindow.Seconds()),
	).Err()
}

// Clear 清除冷却与失败计数（成功调用后调用）。保留「最近错误」历史供面板查看。
func Clear(ctx context.Context, channelID uint64, keyID string) {
	if channelID == 0 || keyID == "" || !db.RedisReady() {
		return
	}
	pipe := db.RDB.Pipeline()
	pipe.Del(ctx, coolKey(channelID, keyID))
	pipe.Del(ctx, failKey(channelID, keyID))
	_, _ = pipe.Exec(ctx)
}

// CoolingState 返回冷却中的密钥 keyID → 冷却原因（Redis 不可用返回 nil）。
//
// 一次 MGet 拿全：调用方既要知道「哪些不能用」，也要在整池冷却时知道「为什么」。
// 分成两次查询就会有两份可能不一致的真相源。
func CoolingState(ctx context.Context, channelID uint64, keyIDs []string) map[string]string {
	if channelID == 0 || len(keyIDs) == 0 || !db.RedisReady() {
		return nil
	}
	keys := make([]string, len(keyIDs))
	for i, id := range keyIDs {
		keys[i] = coolKey(channelID, id)
	}
	vals, err := db.RDB.MGet(ctx, keys...).Result()
	if err != nil || len(vals) != len(keyIDs) {
		return nil
	}
	out := make(map[string]string, len(keyIDs))
	for i, v := range vals {
		if v == nil {
			continue
		}
		// 历史键的 value 可能是 "1"（改造前只存标记）：原因未知按限流处理，
		// 它是最常见的冷却来源，且 429 比 5xx 更安全（会退避而不是立刻重试）。
		s, _ := v.(string)
		if s == "" || s == "1" {
			s = ReasonRateLimit
		}
		out[keyIDs[i]] = s
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// CoolingTTLs 返回每枚冷却中密钥的**剩余时长**（keyID → TTL，只含 >0 的）。
//
// 为什么要逐行：管理台原来用 MinCoolingTTL 给所有行填同一个数（全池最短），
// 「还剩 12s」与「还剩 9min」显示得一模一样 —— 而这两个数决定的是完全相反的动作
// （等一等 vs 换掉）。一次 pipeline 拿全，不逐行 HGET。
func CoolingTTLs(ctx context.Context, channelID uint64, keyIDs []string) map[string]time.Duration {
	if channelID == 0 || len(keyIDs) == 0 || !db.RedisReady() {
		return nil
	}
	pipe := db.RDB.Pipeline()
	cmds := make([]*redis.DurationCmd, 0, len(keyIDs))
	for _, id := range keyIDs {
		cmds = append(cmds, pipe.PTTL(ctx, coolKey(channelID, id)))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil
	}
	out := make(map[string]time.Duration, len(keyIDs))
	for i, cmd := range cmds {
		// -1 = 无过期时间（不该出现：Cool 一定带 TTL，但历史键可能残留）；
		// -2 = 键已不存在（冷却刚好到期）。两者都不算"冷却中"。
		if d := cmd.Val(); d > 0 {
			out[keyIDs[i]] = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MinCoolingTTL 返回整池密钥里**最短**的剩余冷却时长。
//
// 只在「整池都在冷却」时才需要调用（先 MGet 判满，再按需补这一次 PTTL）：
// 调用方要用它填 Retry-After —— 全冷却时继续挑一枚冷却中的 key 去打，
// 只会把账号级限流越拖越久，对外应该明确说「等几秒」，而不是让人立刻重试再撞一次。
//
// 读法与 CoolingTTLs 共用同一个 PTTL 原语，这里只取 min，不新增第二套读法。
func MinCoolingTTL(ctx context.Context, channelID uint64, keyIDs []string) (time.Duration, bool) {
	var min time.Duration
	for _, d := range CoolingTTLs(ctx, channelID, keyIDs) {
		if min == 0 || d < min {
			min = d
		}
	}
	return min, min > 0
}

// LastErrors 批量返回「最近一次失败」的现场（keyID → `status|reason|unix`）。
//
// 只用于面板展示：它不参与任何判定。一次 HMGet 拿全 —— 每把 key 单独 HGet
// 会让面板一次查询变成 N 次往返，而面板恰恰是在出事时才被打开。
func LastErrors(ctx context.Context, channelID uint64, keyIDs []string) map[string]string {
	if channelID == 0 || len(keyIDs) == 0 || !db.RedisReady() {
		return nil
	}
	vals, err := db.RDB.HMGet(ctx, errKey(channelID), keyIDs...).Result()
	if err != nil || len(vals) != len(keyIDs) {
		return nil
	}
	out := make(map[string]string, len(keyIDs))
	for i, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			out[keyIDs[i]] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// 键前缀：冷却与计数都以 (渠道, keyID) 为维度。
func coolKey(channelID uint64, keyID string) string {
	return fmt.Sprintf("ld:ch:kcd:%d:%s", channelID, keyID)
}

func failKey(channelID uint64, keyID string) string {
	return fmt.Sprintf("ld:ch:kfail:%d:%s", channelID, keyID)
}

// errKey 是「最近错误」hash（field=keyID）。按渠道一个 hash：
// 每把 key 一个键会让面板一次查询变成 N 次往返。
func errKey(channelID uint64) string {
	return fmt.Sprintf("ld:ch:kerr:%d", channelID)
}
