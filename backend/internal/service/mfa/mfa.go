// Package mfa 是 TOTP 二次验证（绑定 / 校验 / 登录挑战）的唯一实现点。
//
// 这一层刻意不碰 gin：登录第二步（挑战兑换）也要在这里完成，
// 如果校验逻辑散在 controller 里，登录链路与「关闭 2FA」就会各自实现一遍校验，
// 而它们的差异（要不要防重放、备份码能不能用）正是最容易错的地方。
package mfa

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/crypto"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/totp"
	"lingdong-api/internal/service/authcfg"
)

// 策略取值（与 auth_mfa.policy 一致）。
const (
	PolicyOff              = "off"
	PolicyOptional         = "optional"
	PolicyRequiredForAdmin = "required_for_admin"
)

const (
	// challengeTTL 是挑战令牌有效期：够慢手输 6 位码，又不至于长期可爆破。
	challengeTTL = 5 * time.Minute
	// maxChallengeTries 是同一挑战允许的错的次数，超过即作废（必须重新登录）。
	maxChallengeTries = 5
	// backupCodeCount 是每次生成的备份码数量。
	backupCodeCount = 10
	// backupCodeLen 是单个备份码的字符数（归一化后，不含短横线）。
	backupCodeLen = 10
	// usedStepTTL 覆盖 ±1 步的校验窗口：窗口内的同一步只接受一次。
	usedStepTTL = 90 * time.Second

	challengePrefix = "mfa:ch:"
	usedStepPrefix  = "mfa:used:"
)

// backupAlphabet 去掉了 0/O/1/I：备份码是给人抄在纸上的。
const backupAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// Service 是二次验证服务。
type Service struct{ gdb *gorm.DB }

// New 构造服务。
func New(gdb *gorm.DB) *Service { return &Service{gdb: gdb} }

// Status 是 GET /portal/auth/mfa/status 的响应体。
type Status struct {
	Enabled         bool   `json:"enabled"`
	Policy          string `json:"policy"`
	BackupCodesLeft int    `json:"backup_codes_left"`
}

// IsEnabled 判断用户是否已开启二次验证。
func (s *Service) IsEnabled(ctx context.Context, uid uint64) (bool, error) {
	var row model.UserMFA
	err := s.gdb.WithContext(ctx).Where("uid = ?", uid).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return row.Enabled, nil
}

// Status 汇总当前用户的二次验证状态。
func (s *Service) Status(ctx context.Context, uid uint64) (Status, error) {
	out := Status{Policy: authPolicy()}
	var row model.UserMFA
	err := s.gdb.WithContext(ctx).Where("uid = ?", uid).First(&row).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, err
	}
	out.Enabled = err == nil && row.Enabled
	if out.Enabled {
		var left int64
		if err := s.gdb.WithContext(ctx).Model(&model.UserMFABackupCode{}).
			Where("uid = ? AND used_at IS NULL", uid).Count(&left).Error; err != nil {
			return out, err
		}
		out.BackupCodesLeft = int(left)
	}
	return out, nil
}

// Setup 生成（或覆盖）一套未启用的 TOTP 密钥，返回明文密钥与 otpauth 地址。
//
// 允许覆盖未完成态：用户点了「绑定」但没扫完码就关页面，下次再进来必须能重新生成，
// 否则他会永远卡在「已生成但没启用」的中间态里（而且他自己看不到那串密钥）。
//
// **这里不生成备份码**：没启用就发 10 个可用于登录的码，等于凭空多出一批凭证。
func (s *Service) Setup(ctx context.Context, uid uint64, account, issuer string) (string, string, error) {
	if enabled, err := s.IsEnabled(ctx, uid); err != nil {
		return "", "", err
	} else if enabled {
		return "", "", errs.MFAAlreadyEnabled
	}
	secret, err := totp.NewSecret()
	if err != nil {
		return "", "", errs.Internal
	}
	enc, err := crypto.Encrypt([]byte(conf.EnvConfig().AppEncryptionKey), []byte(secret))
	if err != nil {
		return "", "", errs.Internal
	}
	// 先查再写：存在就覆盖（把 enabled 显式拉回 false、清掉 enabled_at），不存在就插入。
	//
	// 不用 `ON CONFLICT` 也不用 `Updates(struct)`：后者会忽略零值，
	// 而这里的「覆盖」恰恰需要把 enabled=false、enabled_at=NULL 写进去。
	now := time.Now()
	var existing model.UserMFA
	err = s.gdb.WithContext(ctx).Where("uid = ?", uid).First(&existing).Error
	switch {
	case err == nil:
		if uerr := s.gdb.WithContext(ctx).Model(&model.UserMFA{}).Where("uid = ?", uid).
			Updates(map[string]any{"secret_enc": enc, "enabled": false, "enabled_at": nil}).Error; uerr != nil {
			return "", "", uerr
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		row := model.UserMFA{UID: uid, SecretEnc: enc, Enabled: false, CreatedAt: now}
		if cerr := s.gdb.WithContext(ctx).Create(&row).Error; cerr != nil {
			return "", "", cerr
		}
	default:
		return "", "", err
	}
	return secret, totp.URI(issuer, account, secret), nil
}

// Enable 用一次正确动态码确认绑定，返回**仅此一次**可见的备份码明文。
func (s *Service) Enable(ctx context.Context, uid uint64, code string) ([]string, error) {
	var row model.UserMFA
	err := s.gdb.WithContext(ctx).Where("uid = ?", uid).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 没 setup 就直接 enable：按「先绑定」处理，不要 500。
			return nil, errs.MFANotEnabled
		}
		return nil, err
	}
	if row.Enabled {
		return nil, errs.MFAAlreadyEnabled
	}
	secret, err := s.decryptSecret(row.SecretEnc)
	if err != nil {
		return nil, err
	}
	if err := s.verifyTOTP(ctx, uid, secret, code); err != nil {
		return nil, err
	}
	now := time.Now()
	// 必须用 map：`enabled=false` 这种零值会被 GORM 的 Updates(struct) 忽略 ——
	// 这里正好是反向（要把 false 写成 true），但表结构里 enabled 带 default:false，
	// 用 struct 更新会在一半场景静默不生效。
	if err := s.gdb.WithContext(ctx).Model(&model.UserMFA{}).Where("uid = ?", uid).
		Updates(map[string]any{"enabled": true, "enabled_at": now}).Error; err != nil {
		return nil, err
	}
	return s.issueBackupCodes(ctx, uid)
}

// Disable 关闭二次验证：删除密钥与全部备份码。
//
// 必须同时校验一次动态码/备份码：只凭会话就能关闭 2FA，
// 等于「拿到一个已登录的浏览器」就能永久摘掉第二因素。
func (s *Service) Disable(ctx context.Context, uid uint64, code string) error {
	if err := s.Verify(ctx, uid, code); err != nil {
		return err
	}
	err := s.gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("uid = ?", uid).Delete(&model.UserMFABackupCode{}).Error; err != nil {
			return err
		}
		return tx.Where("uid = ?", uid).Delete(&model.UserMFA{}).Error
	})
	return err
}

// Regenerate 重新生成备份码（旧码立即作废）。
func (s *Service) Regenerate(ctx context.Context, uid uint64, code string) ([]string, error) {
	if err := s.Verify(ctx, uid, code); err != nil {
		return nil, err
	}
	return s.issueBackupCodes(ctx, uid)
}

// Verify 校验一次「第二因素」（动态码或备份码），用于开启/关闭/重生成与登录第二步。
//
// 顺序是先 TOTP 再备份码：TOTP 是常态，备份码是兜底，
// 而备份码校验要写 used_at（一次性），先试它会把一次正常的登录变成一次备份码消耗。
func (s *Service) Verify(ctx context.Context, uid uint64, code string) error {
	// 失败计数：关闭二次验证 / 重新生成备份码 / 开启二次验证都走这里，
	// 原先没有任何次数上限 —— 只剩 `user_sess` 的 300/min 兜着，
	// 而 `mfa_code_invalid` 与成功可区分，等于给了一个可测量的试码探针。
	if mfaFails(ctx, uid) >= maxVerifyFails {
		// 统一回「码不正确」而不是单开一个"被限流"的码：后者会告诉攻击者
		// "这个 uid 确实开了 2FA 且我打到上限了"，反而成了确认信号。
		return errs.MFACodeInvalid
	}
	var row model.UserMFA
	err := s.gdb.WithContext(ctx).Where("uid = ?", uid).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.MFANotEnabled
		}
		return err
	}
	if !row.Enabled {
		return errs.MFANotEnabled
	}
	secret, err := s.decryptSecret(row.SecretEnc)
	if err != nil {
		return err
	}
	if err := s.verifyTOTP(ctx, uid, secret, code); err == nil {
		clearMFAFails(ctx, uid)
		return nil
	} else if !errors.Is(err, errs.MFACodeInvalid) {
		// Redis 不可用这类错误要原样上抛：退回「试备份码」会把可用性问题伪装成「码错了」。
		return err
	}
	if err := s.consumeBackupCode(ctx, uid, code); err != nil {
		bumpMFAFail(ctx, uid)
		return err
	}
	clearMFAFails(ctx, uid)
	return nil
}

const (
	// maxVerifyFails 是同一个账号连续验错的容忍次数。
	//
	// 5 次足够覆盖"手滑 + 时钟偏差"，又不足以对 6 位动态码做有意义的枚举
	// （10^6 空间、30 秒一换，5 次/15 分钟等于没有胜率）。
	maxVerifyFails = 5
	// mfaFailPrefix 是失败计数键前缀（按 uid，不按 IP：换 IP 不该重置额度）。
	mfaFailPrefix = "mfa:fail:"
	// mfaFailTTL 是计数窗口：15 分钟足以让人等到下一个时间片，也不至于长期锁人。
	mfaFailTTL = 15 * time.Minute
)

func mfaFailKey(uid uint64) string { return fmt.Sprintf("%s%d", mfaFailPrefix, uid) }

// mfaFails 读取当前失败次数（Redis 不可用时返回 0：让校验本身去 fail-closed，
// 不要在这里造一个"看起来被限流"的假象）。
func mfaFails(ctx context.Context, uid uint64) int {
	if !db.RedisReady() {
		return 0
	}
	n, err := db.RDB.Get(ctx, mfaFailKey(uid)).Int()
	if err != nil {
		return 0
	}
	return n
}

func bumpMFAFail(ctx context.Context, uid uint64) {
	if !db.RedisReady() {
		return
	}
	key := mfaFailKey(uid)
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		return
	}
	// 每次都刷新窗口：连续试错会持续延长，停手 15 分钟自动恢复。
	_ = db.RDB.Expire(ctx, key, mfaFailTTL).Err()
	_ = n
}

func clearMFAFails(ctx context.Context, uid uint64) {
	if !db.RedisReady() {
		return
	}
	_ = db.RDB.Del(ctx, mfaFailKey(uid)).Err()
}

// verifyTOTP 校验动态码并做防重放（同一步只接受一次）。
func (s *Service) verifyTOTP(ctx context.Context, uid uint64, secret, code string) error {
	step, ok := totp.Validate(secret, code, time.Now())
	if !ok {
		return errs.MFACodeInvalid
	}
	// 没有这一步，嗅到一个码就能在 30 秒窗口内反复使用。
	// Redis 不可用时**拒绝**（fail-closed）：放行等于关掉防重放。
	if !db.RedisReady() {
		return errs.AuthUnavailable
	}
	key := fmt.Sprintf("%s%d:%d", usedStepPrefix, uid, step)
	set, err := db.RDB.SetNX(ctx, key, 1, usedStepTTL).Result()
	if err != nil {
		return errs.AuthUnavailable
	}
	if !set {
		return errs.MFACodeInvalid
	}
	return nil
}

// consumeBackupCode 消耗一个未使用的备份码。
func (s *Service) consumeBackupCode(ctx context.Context, uid uint64, code string) error {
	norm := normalizeBackupCode(code)
	if len(norm) != backupCodeLen {
		return errs.MFACodeInvalid
	}
	hash := backupHash(uid, norm)
	now := time.Now()
	// 条件更新（used_at IS NULL）而不是「先查再改」：并发的两次提交里
	// 只能有一个把 used_at 写进去，另一个影响 0 行 —— 备份码必须是**一次性**的。
	res := s.gdb.WithContext(ctx).Model(&model.UserMFABackupCode{}).
		Where("uid = ? AND code_hash = ? AND used_at IS NULL", uid, hash).
		Update("used_at", now)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.MFACodeInvalid
	}
	return nil
}

// issueBackupCodes 作废旧码并生成一批新的（明文只在返回值里出现一次）。
func (s *Service) issueBackupCodes(ctx context.Context, uid uint64) ([]string, error) {
	codes := make([]string, 0, backupCodeCount)
	rows := make([]model.UserMFABackupCode, 0, backupCodeCount)
	now := time.Now()
	for i := 0; i < backupCodeCount; i++ {
		raw, err := randomBackupCode()
		if err != nil {
			return nil, errs.Internal
		}
		codes = append(codes, raw[:backupCodeLen/2]+"-"+raw[backupCodeLen/2:])
		rows = append(rows, model.UserMFABackupCode{
			UID: uid, CodeHash: backupHash(uid, raw), CreatedAt: now,
		})
	}
	err := s.gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("uid = ?", uid).Delete(&model.UserMFABackupCode{}).Error; err != nil {
			return err
		}
		return tx.Create(&rows).Error
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func (s *Service) decryptSecret(enc string) (string, error) {
	plain, err := crypto.Decrypt([]byte(conf.EnvConfig().AppEncryptionKey), enc)
	if err != nil {
		// 密钥轮换/数据损坏：属于服务端配置问题，不能当成「用户输错码」。
		return "", errs.AuthUnavailable
	}
	return string(plain), nil
}

// ------------------------------ 登录挑战 ------------------------------

// CreateChallenge 为已开 MFA 的用户生成一次性挑战令牌（登录第一步之后）。
//
// supersededID 是第一步请求携带的旧会话 id（属于本人时非 0），随挑战一起存下，
// 第二步建会话时再撤销它。**不能只存票据明文**（Redis 泄露即会话劫持），
// 也不能在第一步就撤销：那样用户「密码对、动态码输错」会被顺手登出，
// 多设备场景下表现为「另一台设备突然掉线」。
func (s *Service) CreateChallenge(ctx context.Context, uid uint64, ip string, supersededID uint64) (string, int, error) {
	if !db.RedisReady() {
		return "", 0, errs.AuthUnavailable
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", 0, errs.Internal
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	key := challengePrefix + token
	// 用 hash 存而不是 JSON：Lua 里要逐字段比对 IP，hash 更直接。
	err := db.RDB.HSet(ctx, key, map[string]any{
		"uid":   fmt.Sprint(uid),
		"ip":    ipHash(ip),
		"tries": 0,
		"sup":   fmt.Sprint(supersededID),
	}).Err()
	if err != nil {
		return "", 0, errs.AuthUnavailable
	}
	if err := db.RDB.Expire(ctx, key, challengeTTL).Err(); err != nil {
		return "", 0, errs.AuthUnavailable
	}
	return token, int(challengeTTL.Seconds()), nil
}

// claimScript 原子地「校验 IP + 累加试错次数」。
//
// 为什么必须用 Lua：先 GET 再 INCR 之间有窗口，并发请求可以把它变成
// 「试 100 次但计数器只涨 1」，5 次的限额就形同虚设。
//
// 返回：{uid, tries}；-1 不存在，-2 IP 不匹配，-3 超过试错上限（已删除）。
var claimScript = redis.NewScript(`
local v = redis.call('HGETALL', KEYS[1])
if #v == 0 then return {-1, 0, 0} end
local uid, ip, tries, sup = '0', '', 0, 0
for i = 1, #v, 2 do
  if v[i] == 'uid' then uid = v[i+1]
  elseif v[i] == 'ip' then ip = v[i+1]
  elseif v[i] == 'tries' then tries = tonumber(v[i+1]) or 0
  elseif v[i] == 'sup' then sup = tonumber(v[i+1]) or 0 end
end
if ip ~= ARGV[1] then return {-2, 0, 0} end
if tries >= tonumber(ARGV[2]) then
  redis.call('DEL', KEYS[1])
  return {-3, 0, 0}
end
redis.call('HINCRBY', KEYS[1], 'tries', 1)
return {tonumber(uid), tries + 1, sup}
`)

// RedeemChallenge 校验挑战 + 动态码，成功返回 uid 并作废挑战。
//
// 所有失败都收敛成同一个错误码（mfa_challenge_expired）：区分「不存在 / IP 变了 /
// 试错超限」等于给攻击者一个判断挑战是否存在的探针。
func (s *Service) RedeemChallenge(ctx context.Context, token, ip, code string) (uid uint64, supersededID uint64, err error) {
	if !db.RedisReady() {
		return 0, 0, errs.AuthUnavailable
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, 0, errs.MFAChallengeExpired
	}
	res, rerr := claimScript.Run(ctx, db.RDB, []string{challengePrefix + token}, ipHash(ip), maxChallengeTries).Result()
	if rerr != nil {
		return 0, 0, errs.AuthUnavailable
	}
	arr, ok := res.([]any)
	if !ok || len(arr) < 2 {
		return 0, 0, errs.MFAChallengeExpired
	}
	n, _ := arr[0].(int64)
	if n <= 0 {
		return 0, 0, errs.MFAChallengeExpired
	}
	if len(arr) >= 3 {
		if sup, ok := arr[2].(int64); ok && sup > 0 {
			supersededID = uint64(sup)
		}
	}
	if verr := s.Verify(ctx, uint64(n), code); verr != nil {
		return 0, 0, verr
	}
	// 成功后立刻作废：同一挑战不能换出第二个会话。
	_ = db.RDB.Del(ctx, challengePrefix+token).Err()
	return uint64(n), supersededID, nil
}

// ------------------------------ 内部工具 ------------------------------

// authPolicy 读当前策略。走 authcfg 而不是自己 Unmarshal，
// 保证「策略的唯一读取点」不被绕过（多一处读取就多一处漏改）。
func authPolicy() string { return authcfg.MFAPolicy() }

func ipHash(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:8])
}

// normalizeBackupCode 归一化备份码：去空格与短横线、转大写。
func normalizeBackupCode(code string) string {
	s := strings.ToUpper(strings.TrimSpace(code))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}

// backupHash = hex(HMAC-SHA256(key=APP_ENCRYPTION_KEY, msg="uid|code"))。
//
// 为什么不能只做 sha256：备份码只有 10 位、字母表 32 个字符，熵约 50 bit ——
// 拖库之后用 GPU 把整张码表算出来是分钟级的事，而"加 uid 当盐"完全挡不住
// （uid 是明文列，攻击者按 uid 逐个建表即可）。加上服务端密钥之后，
// 拿到库但没拿到 APP_ENCRYPTION_KEY 就一条都算不出来。
//
// uid 仍要进消息：不同用户的同一个码不能撞成同一行（否则一个人的码能解另一个人的锁）。
func backupHash(uid uint64, code string) string {
	mac := hmac.New(sha256.New, []byte(conf.EnvConfig().AppEncryptionKey))
	fmt.Fprintf(mac, "%d|%s", uid, code)
	return hex.EncodeToString(mac.Sum(nil))
}

// randomBackupCode 生成 10 位备份码（去掉易混字符的 32 字母表）。
//
// 长度是安全参数：32^10 ≈ 1.1e15，配合「连续错 5 次锁 15 分钟」才有意义；
// 原先是 8 位（≈1.1e12），在线爆破的边际成本差了三个数量级。
func randomBackupCode() (string, error) {
	buf := make([]byte, backupCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, backupCodeLen)
	for i, b := range buf {
		// 32 整除 256，取模不引入偏置。
		out[i] = backupAlphabet[int(b)%len(backupAlphabet)]
	}
	return string(out), nil
}
