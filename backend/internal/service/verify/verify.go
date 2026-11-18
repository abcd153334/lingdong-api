// Package verify 是「一次性验证码」的唯一实现：发码、校验、消费、以及
// 密码重置用的短时凭据（ticket）。
//
// 为什么从 service/auth 里搬出来：原实现把「发码」放在 auth 包、把「校验」放在
// ResetPassword 里、把「消费」放在 BindTarget 里，三处各有一套键名与规则
// （例如绑定验证码没有尝试次数上限，重置验证码有），于是同一个 6 位码在两条路径上
// 的强度完全不同。现在只有一套键、一套规则、一套限流。
//
// 键名前缀 vc:（verify code），与旧键 verify:code:* 不共用：
// 旧键的 target 是**明文**邮箱/手机号，属于「用户输入直接进键名」，
// 一旦有超长输入就能把 Redis 撑爆。这里一律 hash。
package verify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/authcfg"
	"lingdong-api/internal/service/notify"
)

// 渠道。
const (
	ChannelEmail = "email"
	ChannelSMS   = "sms"
)

const (
	// codeTTL 是验证码有效期（10 分钟）。
	codeTTL = 10 * time.Minute
	// sendCooldown 是同一收件人同一场景的重发冷却。
	sendCooldown = 60 * time.Second
	// maxCheckAttempts 是单个验证码允许的错误次数。6 位数字 90 万种，
	// 5 次之后作废，命中概率被压到可忽略。
	maxCheckAttempts = 5
	// ipHourlyLimit 是同一 IP 同一场景每小时的发码上限。
	ipHourlyLimit = 20
	// ticketTTL 是「验证码已通过」凭据的有效期（够用户想一个新密码）。
	ticketTTL = 5 * time.Minute
)

// 场景。复用 authcfg 的常量，避免两套字面量。
const (
	SceneRegister = authcfg.SceneRegister
	SceneReset    = authcfg.ScenePasswordReset
	SceneContact  = authcfg.SceneContactBind
)

// Normalize 归一化收件人。
//
// 唯一实现，注册/登录/找回/换绑四处都必须调它：原实现注册按原文比较、
// 失败计数按 lower+trim 计算，于是 A@x.com 与 a@x.com 能注册出两个账号，
// 而它们的失败计数共用一个桶（一方被爆破，另一方登录也要验证码）。
func Normalize(channel, raw string) string {
	s := strings.TrimSpace(raw)
	if channel == ChannelEmail {
		// 邮箱地址在 DNS 层不区分大小写（本地部分理论上区分，但所有主流邮件
		// 服务商都按不区分处理），统一小写是唯一能让唯一索引与人眼判断一致的做法。
		return strings.ToLower(s)
	}
	// 手机号去掉常见分隔符：用户从通讯录复制时常带空格或连字符。
	replacer := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "")
	s = replacer.Replace(s)
	// 前导 `+` 也去掉：它是 E.164 的**显示形态**，不构成号码身份的一部分。
	//
	// 不去掉的后果有三个，且都不报错：
	//   1. `+8613800138000` 与 `8613800138000` 是两个唯一值 → 同一号码可注册两个账号；
	//   2. 发码冷却 `vc:cool:<hash(target)>` 也分两个键 → 60 秒冷却被绕过；
	//   3. 用户注册时写 `138…`、找回密码时写 `+86…` → `WHERE phone = ?` 查不到，
	//      而找回端点对"账号不存在"恒回 200（防枚举）→ 用户永远收不到码、也没有任何提示。
	// 必须与 db/indexes.go 的 uq_users_phone_norm（ltrim(phone,'+')）逐字一致。
	//
	// 用 TrimLeft 而不是 TrimPrefix：双加号是真实存在的粘贴形态（复制两遍、
	// 号簿导出带 `+` 而用户又自己敲了一个），去一个会留下一个 `+`，
	// 于是归一化**不幂等** —— 存进库的值与再归一化一次的值不同，查询就永远查不到。
	return strings.TrimLeft(s, "+")
}

// ValidateFormat 校验收件人格式。
//
// 手机号走宽松的 E.164 形态（可带 +，6–15 位数字）：刻意不按国内号段硬校验，
// 海外号与服务商回填的号码都要能用，这里只负责挡掉「根本不是号码」的输入。
func ValidateFormat(channel, target string) error {
	switch channel {
	case ChannelEmail:
		if strings.ContainsAny(target, " \t\r\n") || strings.Count(target, "@") != 1 {
			return errs.InvalidParam.WithMessage("邮箱格式不正确")
		}
		at := strings.LastIndex(target, "@")
		if at <= 0 || at == len(target)-1 {
			return errs.InvalidParam.WithMessage("邮箱格式不正确")
		}
		// 域名必须是「至少两级、每级非空」。
		//
		// 只判「域名里含一个点」会放过 `a@.com`、`a@b.`、`a@b..com` ——
		// 它们结构上就是坏地址，而本函数是注册与换绑的准入闸：放过的代价是
		// 用户注册成功、验证码永远收不到，从用户视角看就是「系统坏了」。
		labels := strings.Split(target[at+1:], ".")
		if len(labels) < 2 {
			return errs.InvalidParam.WithMessage("邮箱格式不正确")
		}
		for _, lb := range labels {
			if lb == "" {
				return errs.InvalidParam.WithMessage("邮箱格式不正确")
			}
		}
		return nil
	case ChannelSMS:
		body := strings.TrimPrefix(target, "+")
		if len(body) < 6 || len(body) > 15 {
			return errs.InvalidParam.WithMessage("手机号格式不正确")
		}
		for _, r := range body {
			if r < '0' || r > '9' {
				return errs.InvalidParam.WithMessage("手机号格式不正确")
			}
		}
		return nil
	}
	return errs.InvalidParam
}

// Ready 判断渠道是否具备真实发送能力（SMTP / 短信服务已配置并启用）。
//
// 这是「配置是否可用」的判据，与「策略是否允许用这个渠道」（authcfg.MethodEnabled）
// 是两件事：策略允许但通道没配好，发码必然失败，必须提前拦掉。
func Ready(channel string) bool {
	switch channel {
	case ChannelEmail:
		return notify.EmailReady()
	case ChannelSMS:
		return notify.SMSReady()
	}
	return false
}

// IssueInput 是发码入参。
type IssueInput struct {
	Scene   string
	Channel string
	Target  string
	// UID 为 0 表示未登录场景（注册 / 找回密码）。
	UID uint64
	// IP 用于每小时发码上限（防「换收件人刷」）。
	IP string
}

// Issue 生成并下发验证码，返回有效期秒数。
func Issue(ctx context.Context, in IssueInput) (int, error) {
	if !db.RedisReady() {
		return 0, errs.AuthUnavailable
	}
	target := Normalize(in.Channel, in.Target)
	if err := ValidateFormat(in.Channel, target); err != nil {
		return 0, err
	}
	if !Ready(in.Channel) {
		return 0, errs.BindMethodDisabled
	}
	if err := reserveSend(ctx, in, target); err != nil {
		return 0, err
	}

	code, err := randomCode()
	if err != nil {
		// 不再回退到 time.Now()：那让验证码变成可预测的（同秒同值），
		// 而「熵源坏了」本来就该是一次明确的失败。
		return 0, errs.VerifyUnavailable
	}
	codeKey := keyOf("code", in.Scene, target)
	if err := db.RDB.Set(ctx, codeKey, code, codeTTL).Err(); err != nil {
		return 0, errs.AuthUnavailable
	}
	// 尝试计数与码同寿命：码过期后计数自然消失。
	if err := db.RDB.Del(ctx, keyOf("try", in.Scene, target)).Err(); err != nil {
		logger.L().Warn("清空验证码尝试计数失败", zap.Error(err))
	}

	if err := notify.SendCode(ctx, sendScene(in.Channel), target, code); err != nil {
		// 发送失败必须**回滚冷却键并删除码**：否则用户什么都没收到却要等 60 秒，
		// 而库里躺着一个他永远不知道的码。
		_ = db.RDB.Del(ctx, keyOf("cool", in.Scene, target)).Err()
		_ = db.RDB.Del(ctx, codeKey).Err()
		logger.L().Warn("验证码下发失败",
			zap.String("scene", in.Scene), zap.String("channel", in.Channel), zap.Error(err))
		return 0, errs.VerifyUnavailable
	}
	return int(codeTTL.Seconds()), nil
}

// MarkSent 只写限流键、不发码。
//
// 用于「账号不存在」的分支：找回密码时必须让「存在」与「不存在」两条路径
// 的响应与限流行为完全一致，否则「第二次请求是否被限流」就成了账号枚举通道。
func MarkSent(ctx context.Context, in IssueInput) error {
	if !db.RedisReady() {
		return errs.AuthUnavailable
	}
	target := Normalize(in.Channel, in.Target)
	return reserveSend(ctx, in, target)
}

// reserveSend 占用三层配额：收件人冷却、同账号冷却、同 IP 每小时。
func reserveSend(ctx context.Context, in IssueInput, target string) error {
	// 1) 收件人维度：挡「用多个账号轰炸同一个邮箱/手机号」。
	ok, err := db.RDB.SetNX(ctx, keyOf("cool", in.Scene, target), "1", sendCooldown).Result()
	if err != nil {
		return errs.AuthUnavailable
	}
	if !ok {
		return errs.VerifyRateLimited
	}
	// 2) 账号维度：登录用户换绑时，同一账号每分钟一条。
	if in.UID != 0 {
		uidKey := fmt.Sprintf("vc:uid:%d:%s", in.UID, in.Scene)
		ok, err := db.RDB.SetNX(ctx, uidKey, "1", sendCooldown).Result()
		if err != nil {
			return errs.AuthUnavailable
		}
		if !ok {
			return errs.VerifyRateLimited
		}
	}
	// 3) IP 维度：换收件人刷码的兜底。
	if in.IP != "" {
		bucket := time.Now().Unix() / 3600
		ipKey := fmt.Sprintf("vc:ip:%s:%s:%d", in.Scene, hashKey(in.IP), bucket)
		n, err := db.RDB.Incr(ctx, ipKey).Result()
		if err != nil {
			return errs.AuthUnavailable
		}
		if n == 1 {
			_ = db.RDB.Expire(ctx, ipKey, 3700*time.Second).Err()
		}
		if n > ipHourlyLimit {
			return errs.VerifyRateLimited
		}
	}
	return nil
}

// Check 校验验证码但**不消费**，失败累加尝试次数。
//
// 找回密码第 2 步用它：用户还要在第 3 步设新密码，若这一步就消费掉，
// 密码不合规时用户必须重新等 60 秒收码。
func Check(ctx context.Context, scene, channel, target, code string) error {
	if !db.RedisReady() {
		return errs.AuthUnavailable
	}
	t := Normalize(channel, target)
	return check(ctx, scene, t, strings.TrimSpace(code))
}

// Consume 原子消费验证码：值相等才删除。
//
// 不能先 GetDel 再比对：输错一次就会烧掉正确验证码，用户必须重发，
// 而这个行为还能被用来「刷掉别人正在填的码」。
func Consume(ctx context.Context, scene, channel, target, code string) error {
	if !db.RedisReady() {
		return errs.AuthUnavailable
	}
	t := Normalize(channel, target)
	if err := check(ctx, scene, t, strings.TrimSpace(code)); err != nil {
		return err
	}
	n, err := consumeScript.Run(ctx, db.RDB, []string{keyOf("code", scene, t)},
		strings.TrimSpace(code)).Int()
	if err != nil {
		return errs.AuthUnavailable
	}
	if n != 1 {
		// 极小概率：两次并发同时通过 check，一方消费成功后另一方删不到。
		return errs.VerifyCodeInvalid
	}
	_ = db.RDB.Del(ctx, keyOf("try", scene, t)).Err()
	return nil
}

func check(ctx context.Context, scene, target, code string) error {
	if code == "" {
		return errs.VerifyCodeInvalid
	}
	tryKey := keyOf("try", scene, target)
	n, err := db.RDB.Incr(ctx, tryKey).Result()
	if err != nil {
		return errs.AuthUnavailable
	}
	if n == 1 {
		_ = db.RDB.Expire(ctx, tryKey, codeTTL).Err()
	}
	if int(n) > maxCheckAttempts {
		// 超限即作废该码：只返回「无效」，不告诉攻击者「你已经用完次数」——
		// 两者对合法用户是同一件事（重发），对攻击者少一条信息。
		_ = db.RDB.Del(ctx, keyOf("code", scene, target)).Err()
		return errs.VerifyCodeInvalid
	}
	got, err := db.RDB.Get(ctx, keyOf("code", scene, target)).Result()
	if err != nil {
		return errs.VerifyCodeInvalid
	}
	// 恒定时间比较：代价为零，省掉「计时差可被利用」这类讨论。
	if subtle.ConstantTimeCompare([]byte(got), []byte(code)) != 1 {
		return errs.VerifyCodeInvalid
	}
	return nil
}

// IssueTicket 在验证码通过后签发一次性凭据，返回凭据与有效期秒数。
//
// 为什么不直接把「已验证」状态挂在目标地址上：那样任何知道邮箱的人
// 都能在验证通过后的 5 分钟内抢先用它改密码。凭据绑定 uid 且只存在服务端。
func IssueTicket(ctx context.Context, uid uint64) (string, int, error) {
	if !db.RedisReady() {
		return "", 0, errs.AuthUnavailable
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", 0, errs.AuthUnavailable
	}
	ticket := "ldr_" + hex.EncodeToString(raw)
	if err := db.RDB.Set(ctx, "vc:ticket:"+hashKey(ticket), fmt.Sprintf("%d", uid), ticketTTL).Err(); err != nil {
		return "", 0, errs.AuthUnavailable
	}
	return ticket, int(ticketTTL.Seconds()), nil
}

// RedeemTicket 消费凭据并返回 uid（一次性：GETDEL）。
func RedeemTicket(ctx context.Context, ticket string) (uint64, error) {
	if !db.RedisReady() {
		return 0, errs.AuthUnavailable
	}
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return 0, errs.ResetTicketInvalid
	}
	// 用 db.TakeOnce 而不是 RDB.GetDel：GETDEL 要 Redis ≥ 6.2，旧版会回
	// unknown command —— 找回密码第 3 步会永久 503，而且看起来像「凭据已失效」。
	val, ok, err := db.TakeOnce(ctx, "vc:ticket:"+hashKey(ticket))
	if err != nil {
		return 0, errs.AuthUnavailable
	}
	if !ok {
		return 0, errs.ResetTicketInvalid
	}
	var uid uint64
	if _, err := fmt.Sscanf(val, "%d", &uid); err != nil || uid == 0 {
		return 0, errs.ResetTicketInvalid
	}
	return uid, nil
}

// consumeScript 是「值相等才删除」的原子脚本（Redis 内单线程执行）。
var consumeScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`)

// keyOf 生成场景键。target 一律 hash：用户输入不能直接进键名。
func keyOf(kind, scene, target string) string {
	return "vc:" + kind + ":" + scene + ":" + hashKey(target)
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// randomCode 生成 6 位数字验证码（crypto/rand）。
func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// sendScene 把渠道映射成通知模板的场景名。
//
// 沿用既有模板（email_bind / phone_bind 就是「验证码」模板），
// 不新增模板名：新增而未配置模板时，发送会静默失败，排查要翻 SMTP 日志。
func sendScene(channel string) string {
	if channel == ChannelSMS {
		return "phone_bind"
	}
	return "email_bind"
}
