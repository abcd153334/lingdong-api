// Package totp 实现 RFC 6238 的 TOTP（时间步长一次性口令），零依赖。
//
// 为什么自己写而不是引库：整个需求就是「HMAC-SHA1 + 动态截断 + base32」约 60 行，
// 而第三方库往往顺带引入二维码、图片、时区处理等一串依赖，还要跟着版本升级。
//
// 约定（与主流 Authenticator 一致，改一项就等于让所有已绑定的用户失效）：
//   - 步长 30 秒，6 位数字，HMAC-SHA1；
//   - 校验允许 ±1 步（时钟漂移 30 秒内仍可用，再宽就等于放大爆破窗口）；
//   - 密钥是 base32 无 padding、大写；解码前先把用户可能输入的 `=` 和小写容错掉。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const (
	// Step 是时间步长（秒）。
	Step = 30
	// Digits 是口令位数。
	Digits = 6
	// Skew 是允许的步数偏差（±1 步）。
	Skew = 1
	// secretBytes 是密钥熵（160 位，RFC 4226 推荐）。
	secretBytes = 20
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret 生成一个 base32（无 padding、大写）的随机密钥。
func NewSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("totp: 生成密钥失败: %w", err)
	}
	return b32.EncodeToString(raw), nil
}

// Normalize 归一化密钥输入：去掉空白与 padding，转大写。
//
// 用户从 Authenticator 或后台复制密钥时常带空格/换行/padding，
// 与 base32 解码器的严格模式不一致，会在「绑定」这一步就失败。
func Normalize(secret string) string {
	s := strings.ToUpper(strings.TrimSpace(secret))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	return strings.TrimRight(s, "=")
}

// DecodeSecret 解码 base32 密钥。
func DecodeSecret(secret string) ([]byte, error) {
	s := Normalize(secret)
	if s == "" {
		return nil, fmt.Errorf("totp: 密钥为空")
	}
	raw, err := b32.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("totp: 密钥不是合法的 base32: %w", err)
	}
	return raw, nil
}

// StepOf 返回 t 所在的步序号（UTC Unix 时间 / 步长）。
func StepOf(t time.Time) int64 { return t.Unix() / Step }

// CodeAt 计算第 step 步的 6 位口令。
func CodeAt(secret string, step int64) (string, error) {
	key, err := DecodeSecret(secret)
	if err != nil {
		return "", err
	}
	return codeWithKey(key, step), nil
}

// Code 是 CodeAt(secret, StepOf(t)) 的便捷形式。
func Code(secret string, t time.Time) (string, error) {
	return CodeAt(secret, StepOf(t))
}

func codeWithKey(key []byte, step int64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	// 动态截断（RFC 4226 §5.4）：取最后一字节低 4 位作为偏移。
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off])&0x7f)<<24 |
		uint32(sum[off+1])<<16 |
		uint32(sum[off+2])<<8 |
		uint32(sum[off+3])
	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", Digits, v%mod)
}

// Validate 校验口令，允许 ±Skew 步；命中时返回对应的**步序号**。
//
// 返回 step 是为了让调用方做防重放：同一个 step 只接受一次
// （否则嗅到一个码就能在 30 秒内反复使用）。
func Validate(secret, code string, t time.Time) (int64, bool) {
	// 容忍空格：不少 Authenticator 把码显示成「123 456」，用户会连着空格一起粘贴。
	// 去掉空格不削弱安全性（仍然是 6 个数字），但不做就会被「看着对却提示错误」坑到。
	c := strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	// 只接受纯数字且长度正好 6：常量时间比较也依赖长度一致。
	if len(c) != Digits {
		return 0, false
	}
	for i := 0; i < len(c); i++ {
		if c[i] < '0' || c[i] > '9' {
			return 0, false
		}
	}
	key, err := DecodeSecret(secret)
	if err != nil {
		return 0, false
	}
	cur := StepOf(t)
	for delta := int64(-Skew); delta <= Skew; delta++ {
		step := cur + delta
		want := codeWithKey(key, step)
		if subtle.ConstantTimeCompare([]byte(want), []byte(c)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// URI 生成 otpauth:// 绑定地址（供前端渲染二维码）。
//
// account 通常是用户名；issuer 是站点名。两处的 issuer 都要写：
// 老版本 Authenticator 只读 query 参数，新版只读 label 前缀。
func URI(issuer, account, secret string) string {
	label := issuer + ":" + account
	q := "secret=" + Normalize(secret) +
		"&issuer=" + queryEscape(issuer) +
		"&algorithm=SHA1&digits=" + fmt.Sprint(Digits) + "&period=" + fmt.Sprint(Step)
	return "otpauth://totp/" + queryEscape(label) + "?" + q
}

// queryEscape 只转义 URI 里真正有歧义的字符，保留可读性（`灵动 API` 这类中文原样输出，
// 前端 encodeURI 风格一致即可）。
func queryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', ':', '/', '?', '&', '=', '#', '+', '%':
			b.WriteString("%")
			b.WriteString(fmt.Sprintf("%02X", r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
