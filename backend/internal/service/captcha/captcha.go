// Package captcha 是人机验证的唯一实现：图形验证码（5 位数字）。
//
// 职责边界只有两块：
//   - Required：这次要不要验证（策略，唯一裁决点）
//   - New/Verify：出一张图、校验一次（实现，唯一路径）
//
// 为什么不用「自建 PoW / Turnstile」：
//   - Turnstile 官方不支持中国大陆，且必须配 site key/secret；
//   - PoW 虽然无感，但用户看不到任何东西，"启用验证码却什么都没显示"是必然的困惑。
//
// 为什么自己管存储、不实现库的 Store 接口：
//
//	dchest 的 Store 没有 error 返回值，Redis 故障会被退化成"答案错误"（400），
//	而存储层故障必须是 503——否则真故障会被埋进"用户一直在输错验证码"的指标里。
package captcha

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"time"

	// 包名与本包同名，必须起别名（直接 import 会报重名）。
	imgcaptcha "github.com/dchest/captcha"
	"go.uber.org/zap"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/authcfg"
)

// 场景（与 authcfg 同一套字面量）。
const (
	SceneRegister      = authcfg.SceneRegister
	SceneLogin         = authcfg.SceneLogin
	ScenePasswordReset = authcfg.ScenePasswordReset
)

const (
	// digitsLen 是图片上的数字位数。
	// 5 位 = 10 万种组合；配合「一次性 + 5 分钟 + IP 限流」，脚本暴力猜中的概率可忽略。
	digitsLen = 5
	// capTTL 是答案在 Redis 里的存活时间。用户看图、慢慢输入通常几十秒，
	// 5 分钟是给"填到一半去接电话"留的余量。
	capTTL = 5 * time.Minute
	// capPrefix 是 Redis 键前缀（与其它一次性票据 vc:* 区分开，便于排查）。
	capPrefix = "cap:"
	// answerLen 是答案字符串长度（与 digitsLen 必须一致）。
	answerLen = digitsLen
)

// Required 判断某场景这次是否需要人机验证。
//
// 这是**唯一裁决点**：controller / service 里不许再出现 `if scene == ...`。
// fails 是该账号的连续失败次数（只有登录会用到其余两个场景传 0）。
//
// 注册与找回恒为 true（不读配置）：
//   - 注册是不需要凭据的建号入口，关掉就等于开放批量注册；
//   - 找回密码的发码端点每条都是一封真实邮件/短信（就是钱）。
//
// 登录可配：always / after_fails / off。after_fails 的阈值
// **只认 authcfg.Login().FailThreshold**——历史上这里另有一个
// `max_fails_before_captcha`，两个阈值不一致时会出现"要求验证但不告诉前端"，
// 前端不亮图、用户提交必失败。
func Required(scene string, fails int) bool {
	switch scene {
	case SceneRegister, ScenePasswordReset:
		return true
	case SceneLogin:
		switch authcfg.Captcha().Login {
		case authcfg.SceneOff:
			return false
		case authcfg.SceneAlways:
			return true
		default: // after_fails
			return fails >= authcfg.Login().FailThreshold
		}
	}
	// 未知场景按"要验证"处理：未来新增入口忘了登记时，结果是多要一次，
	// 而不是静默放开。
	return true
}

// New 生成一张图形验证码，返回图片 id 与 PNG 字节。
//
// 顺序是固定的：先画图，再写 Redis，最后返回。
// 反过来会留下"有答案但没有图"的孤儿键——用户看到的图与答案永远对不上。
func New(ctx context.Context) (id string, png []byte, err error) {
	if !db.RedisReady() {
		return "", nil, errs.CaptchaUnavailable
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, errs.CaptchaUnavailable
	}
	id = hex.EncodeToString(raw)

	// 注意：RandomDigits 返回的是 0-9 的**数值字节**（如 {3,7,1,0,5}），
	// 不是 ASCII。它只能喂给画图函数；存进 Redis 前必须逐字节 +'0'。
	digits := imgcaptcha.RandomDigits(digitsLen)

	var buf bytes.Buffer
	// 尺寸写死标准值，不开放配置：NewImage 内部用 rng.Int(border, maxx-border)，
	// 宽高过小会让参数反转直接 panic。
	if _, err := imgcaptcha.NewImage(id, digits,
		imgcaptcha.StdWidth, imgcaptcha.StdHeight).WriteTo(&buf); err != nil {
		return "", nil, errs.CaptchaUnavailable
	}

	answer := make([]byte, len(digits))
	for i, d := range digits {
		answer[i] = '0' + d
	}
	if err := db.RDB.Set(ctx, capPrefix+id, answer, capTTL).Err(); err != nil {
		logger.L().Warn("验证码写入失败", zap.Error(err))
		return "", nil, errs.CaptchaUnavailable
	}
	return id, buf.Bytes(), nil
}

// Verify 校验一次图形验证码，成功即消费（一张图只能用一次）。
//
// 「取不到」覆盖三种情况：不存在 / 已用过 / 已过期。服务端无法可靠区分，
// 对用户也是同一件事——刷新重填。
func Verify(ctx context.Context, id, answer string) error {
	id = strings.TrimSpace(id)
	answer = strings.TrimSpace(answer)
	if id == "" || answer == "" {
		return errs.CaptchaRequired
	}
	// 长度不对直接拒，不查 Redis：省一次往返，也挡住超长输入。
	if len(answer) != answerLen {
		return errs.CaptchaInvalid
	}
	// dev/test 的固定码（e2e 无法 OCR）。生产下 TestCode() 恒空，
	// 且 conf.ValidateSecrets 会拒绝带该变量的进程启动。
	if tc := TestCode(); tc != "" && subtle.ConstantTimeCompare([]byte(answer), []byte(tc)) == 1 {
		return nil
	}

	want, ok, err := db.TakeOnce(ctx, capPrefix+id) // Lua EVAL，兼容 Redis 5.0
	if err != nil {
		// 存储故障不是「你答错了」。折成 invalid 会让所有人都在"输错验证码"，
		// 而真实原因（Redis 不可用）在监控上完全看不见。
		logger.L().Warn("验证码存储不可用", zap.Error(err))
		return errs.CaptchaUnavailable
	}
	if !ok {
		return errs.CaptchaInvalid
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(answer)) != 1 {
		return errs.CaptchaInvalid
	}
	return nil
}

// TestCode 返回「测试环境固定验证码」。仅 APP_ENV=dev|test（或未设置）时非空。
//
// 生产必须为空：它等于一个万能答案。conf.ValidateSecrets 会在非 dev/test
// 拒绝设置 LD_CAPTCHA_TEST_CODE，这里是第二道闸（防止绕过 conf 直接注入）。
//
// 为什么「未设置」也算 dev：APP_ENV 的空值只可能出现在单测/未走启动校验的进程，
// 真实的 prod 进程在 main 里已被 ValidateSecrets 拒绝启动。这里与
// middleware.devLike 保持同一口径（它也把空值按 dev 处理），否则会出现
// 「CSRF 放宽了、验证码却按生产校验」的半套开发态，单测里表现为莫名其妙的 400。
func TestCode() string {
	switch conf.EnvConfig().AppEnv {
	case "", "dev", "test":
		return strings.TrimSpace(conf.EnvConfig().CaptchaTestCode)
	}
	return ""
}
