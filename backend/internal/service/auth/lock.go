package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"lingdong-api/internal/db"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/verify"
)

// fails 是一次尝试前的失败计数快照。
type fails struct {
	Account int
	IP      int
}

// max 返回两个维度里更大的那个（验证码是否触发由它决定）。
func (f fails) max() int {
	if f.Account > f.IP {
		return f.Account
	}
	return f.IP
}

// 失败计数键（两个维度分开）。
//
// 账号维度用来「转人机验证」（不能锁死账号，否则是 DoS 开关）；
// IP 维度用来「硬性停手」（429），这才是真正抬高高频爆破成本的那一道。
//
// 改造前只有一个账号维度、阈值 3、TTL 60 秒，结果两头都错：
// 攻击者 3 次错密码就能把任意已知用户名锁在门外，而真正的高频爆破
// 换个账号就绕过去了。
//
// acctFailKey 生成账号维度的失败计数键。
//
// 键名一律 hash：账号是用户输入，直接拼进键名时一个 1MB 的"用户名"
// 就能在 Redis 写一个 1MB 的键。
//
// **必须先归一账号形态**：邮箱大小写、手机号里的空格/连字符都不改变
// 「这是同一个人」，不归一的话 `a@b.com` / `A@B.com` / `138-0000-0000`
// 各自算一个键，账号维度的阈值形同虚设（换个写法就重新开始计数）。
// 用户名大小写敏感（登录按原样匹配），但这里仍统一小写：
// 计数合并只会更严格，不会放过爆破。
// failNS 是失败计数的**命名空间**。
//
// 登录与改密必须分开计数。原先改密复用同一个 ipFailKey，而它从不检查
// IPFailLimit —— 已登录用户（或拿到一张泄露票的人）连错 20 次旧密码，
// 就能把这个出口 IP 上所有人的登录打成 429，同时把整栋楼推进图形验证码
// （fails.max() 会把 IP 维度算进账号维度）。这不是安全防护，是可用性炸弹。
type failNS string

const (
	// failNSLogin 是登录（含 2FA 前）的失败计数。
	failNSLogin failNS = "login"
	// failNSPassword 是已登录改密的失败计数，与登录完全隔离。
	failNSPassword failNS = "pwchange"
)

func acctFailKey(ns failNS, account string) string {
	a := strings.TrimSpace(account)
	switch {
	case strings.Contains(a, "@"):
		a = verify.Normalize(verify.ChannelEmail, a)
	case isPhoneShape(a):
		a = verify.Normalize(verify.ChannelSMS, a)
	}
	return string(ns) + ":f:" + shortHash(strings.ToLower(a))
}

func ipFailKey(ns failNS, ip string) string {
	return string(ns) + ":i:" + shortHash(ip)
}

// failBumpScript 原子地把计数 +1 并设置 TTL。
//
// 为什么必须原子：原先 `INCR` 与 `EXPIRE` 是两次往返，第二步失败（连接抖动、
// 命令被丢弃）会留下一个**永不过期**的键 —— 该 IP 从此永久 429、该账号永久
// 要求验证码，且没有任何自愈路径，只能人工去 Redis 删键。
//
// ARGV 是 TTL 阶梯（毫秒）：第 n 次失败取第 n 档、超出取最后一档，
// 与 authcfg.LoginPolicy.BackoffFor 的语义逐字一致。IP 键只传一个值，
// 于是任意 n 都取到同一个固定窗口。
var failBumpScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
local idx = n
if idx > #ARGV then idx = #ARGV end
redis.call('PEXPIRE', KEYS[1], ARGV[idx])
return n
`)

// bumpFail 用脚本累加一个键并返回新计数（Redis 不可用时返回 0）。
func bumpFail(ctx context.Context, key string, ladder []time.Duration) int {
	if len(ladder) == 0 {
		return 0
	}
	args := make([]any, 0, len(ladder))
	for _, d := range ladder {
		args = append(args, d.Milliseconds())
	}
	n, err := failBumpScript.Run(ctx, db.RDB, []string{key}, args...).Int()
	if err != nil {
		return 0
	}
	return n
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// readFails 读取当前失败计数（Redis 不可用时返回零值：认证写路径已 fail-closed，
// 这里只是尽量给出准确数字，不做二次拦截）。
func readFails(ctx context.Context, ns failNS, account, ip string) fails {
	var out fails
	if !db.RedisReady() {
		return out
	}
	if n, err := db.RDB.Get(ctx, acctFailKey(ns, account)).Int(); err == nil {
		out.Account = n
	}
	if n, err := db.RDB.Get(ctx, ipFailKey(ns, ip)).Int(); err == nil {
		out.IP = n
	}
	return out
}

// recordFail 累加一次失败并返回累加后的计数。
//
// 两个键的 TTL 语义不同：
//   - 账号键用**退避阶梯**（60s → 300s → 900s，按第 n 次失败选档），
//     越试越久，正常用户输错一次只需等一分钟；
//   - IP 键用固定窗口（默认 10 分钟），因为它统计的是「短时间内的高频尝试」。
func recordFail(ctx context.Context, ns failNS, account, ip string) fails {
	pol := authcfg.Login()
	var out fails
	if !db.RedisReady() {
		return out
	}
	// 账号键：退避阶梯（60s → 300s → 900s），越试越久。
	out.Account = bumpFail(ctx, acctFailKey(ns, account), pol.Backoff)
	// IP 键：固定窗口，统计「短时间内的高频尝试」。
	out.IP = bumpFail(ctx, ipFailKey(ns, ip), []time.Duration{pol.IPFailWindow})
	return out
}

// clearFails 在成功登录后清空**账号维度**。
//
// 刻意不清 IP 维度：IP 桶统计的是"这个出口的失败速率"，一次成功登录
// 不能为它清零 —— 否则攻击者用自己注册的账号"19 次打受害者 + 1 次登自己"
// 就能无限续命，ip_fail_limit（唯一返回 429 的一档）形同虚设，
// 单 IP 每天可试的账号-密码组从 2880 涨到 86400（30 倍）。
// 正常用户不需要它：10 分钟的固定窗口自己会过期。
func clearFails(ctx context.Context, ns failNS, account string) {
	if !db.RedisReady() {
		return
	}
	_ = db.RDB.Del(ctx, acctFailKey(ns, account)).Err()
}

// randomDelay 返回 [0, max) 内的随机时长（crypto/rand；出错时取 max/2）。
//
// 用随机而不是固定延迟：固定延迟对攻击者是可预测的，而随机延迟让
// 「每次尝试的响应时间」不再提供稳定信号，同时也拖慢自动化爆破。
func randomDelay(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return max / 2
	}
	return time.Duration(n.Int64())
}
