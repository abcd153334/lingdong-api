// Package apikey 是 API KEY 的生成、存储与校验单一入口（v2.0 §4.14.1 / step-03）。
// 明文只在创建/重置/回显时短暂出现：落库仅存 key_cipher（AES-GCM 密文）与 key_hash（SHA-256 索引）。
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/crypto"
)

// 明文格式对齐原型 keys.html：`ld-` + 32 位 base62，key_prefix = 明文前 11 位。
const (
	// PlainPrefix 是 API KEY 明文前缀。
	//
	// 导出它只为**凭证形态分派**（`/api/user/*` 的 Bearer 可能是会话票也可能是
	// API KEY，前缀是唯一无歧义的分流判据）。判断一把 KEY 是否可用仍然只能靠
	// Authenticate（哈希比对 + 状态 + 有效期），前缀本身不构成任何授权。
	PlainPrefix = "ld-"
	bodyLen     = 32
	prefixLen   = 11
	alphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// IsKeyToken 判断给定凭证是否为本包签发的 API KEY 形态。
func IsKeyToken(s string) bool { return strings.HasPrefix(s, PlainPrefix) }

// 校验失败原因。
var (
	ErrKeyDisabled = errors.New("apikey: 密钥已禁用")
	ErrKeyExpired  = errors.New("apikey: 密钥已过期")
	// ErrKeyRetired 表示这把明文**曾经有效**，已被轮换取代且重叠窗口已结束。
	//
	// 必须与 invalid_api_key 分开：接入方看到"密钥无效"会去检查是不是抄错了、
	// 是不是环境变量没注入；而真实原因只有一个 —— 该换新钥了。两者要做的
	// 事完全不同，折成同一个码等于把一次有序的运维动作变成一次排障。
	ErrKeyRetired = errors.New("apikey: 密钥已被轮换取代")
	// ErrAuthUnavailable 表示认证依赖（数据库/写续期）不可用，**不是**凭证有问题。
	//
	// 必须与「密钥非法」分开：折成 401 invalid_api_key 会让接入方以为密钥失效，
	// 可能触发自动轮换或人工换钥；而真实原因只是存储层抖动，重试即可恢复。
	// 铁律：存储层错误必须 503，不许折成「你填错了」。
	ErrAuthUnavailable = errors.New("apikey: 认证依赖不可用")
)

// Generate 生成一个新的明文密钥及其索引三元组。
func Generate() (plain, hash, prefix string, err error) {
	buf := make([]byte, bodyLen)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("apikey: 生成随机字节失败: %w", err)
	}
	b := make([]byte, bodyLen)
	for i, v := range buf {
		b[i] = alphabet[int(v)%len(alphabet)]
	}
	plain = PlainPrefix + string(b)
	hash = Hash(plain)
	prefix = plain[:prefixLen]
	return plain, hash, prefix, nil
}

// Hash 计算明文密钥的 SHA-256 十六进制索引。
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Encrypt 用进程级加密密钥对明文做 AES-GCM 加密。
func Encrypt(plain string) (string, error) {
	return crypto.Encrypt([]byte(conf.EnvConfig().AppEncryptionKey), []byte(plain))
}

// Decrypt 解密 key_cipher 还原明文。
func Decrypt(cipherText string) (string, error) {
	b, err := crypto.Decrypt([]byte(conf.EnvConfig().AppEncryptionKey), cipherText)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Authenticate 按明文密钥查找并校验状态/有效期（供 S5 网关使用）。
// Authenticate 按明文验证 API KEY，是 KEY 状态/过期的**唯一判定点**。
//
// 过期语义分两种：
//   - 自建 KEY：硬过期，到期即拒，绝不续期（用户自己的凭证，自己负责轮换）。
//   - 托管 KEY（hosted）：闲置过期——expired_at 到期时先看闲置基准
//     COALESCE(last_used_at, created_at)，闲置未超 ExpireDays 就续期放行
//     （应用在跑就永不过期，真闲置才失效）；ExpireDays<=0 表示过期功能整体关闭。
//
// 轮换窗口：主查询（key_hash）未命中时再按 prev_key_hash 查一跳 ——
// 那是"上一代明文"，在 prev_key_expires_at 之前仍然有效。窗口判断在应用层做
// （而不是写进 SQL 条件），为的是窗口结束后还能返回 key_retired 而不是
// invalid_api_key：前者让接入方知道该换钥了，后者会让它去排查配置。
//
// 顺带节流落库 last_used_at（touchKey），并为临期托管密钥预续。
func Authenticate(plain string) (*model.APIKey, error) {
	now := time.Now()
	h := Hash(plain)
	var k model.APIKey
	err := db.DB.Where("key_hash = ?", h).First(&k).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
		}
		// 第二跳：按上一代明文查（轮换重叠窗口）。
		err = db.DB.Where("prev_key_hash = ?", h).First(&k).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err // 查无此 KEY：调用方映射为「密钥非法」，语义正确
			}
			return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
		}
		if k.PrevKeyExpiresAt == nil || !k.PrevKeyExpiresAt.After(now) {
			return nil, ErrKeyRetired
		}
	}
	if err := checkUsable(&k, now); err != nil {
		return nil, err
	}
	touchKey(&k, now)
	return &k, nil
}

// authPair 是「一次 JOIN 同时取回 KEY 与用户」的扫描目标。
//
// 为什么用 embeddedPrefix 而不是手写别名：`users` 有 30+ 列，手写列名一定会随
// 模型漂移（新增列后鉴权拿到零值，症状是"用户被当成禁用/余额为 0"，而不是报错）。
// 这里让 GORM 自己按字段生成 `k_` / `u_` 前缀列名，Select 列表也用同一份 schema
// 生成 —— 两边不可能对不上。
type authPair struct {
	Key  model.APIKey `gorm:"embedded;embeddedPrefix:k_"`
	User model.User   `gorm:"embedded;embeddedPrefix:u_"`
}

// authPairSelect 生成 `k.id AS k_id, u.status AS u_status, ...`。
//
// 只算一次：schema 解析有成本，而列名在一次进程生命周期里不会变。
var authPairSelect = sync.OnceValues(func() (string, error) {
	stmt := &gorm.Statement{DB: db.DB}
	if err := stmt.Parse(&authPair{}); err != nil {
		return "", err
	}
	var b strings.Builder
	first := true
	for _, f := range stmt.Schema.Fields {
		// 头像（data URL，最大 24KB）不进鉴权查询。
		//
		// 它只出现在门户资料页（controller/auth.go 的 /me 与 service/auth 的登录回执），
		// `/v1` 的任何一个出口都不读它。把它带进每请求的 JOIN，等于按请求量
		// 复制一份图片 —— 7k rps 时是几十 MB/s 的 PG 带宽，白花。
		// 需要头像的路径用的是会话鉴权（另一条链路），不受影响。
		if f.DBName == "u_avatar" {
			continue
		}
		alias, col := "k", strings.TrimPrefix(f.DBName, "k_")
		if strings.HasPrefix(f.DBName, "u_") {
			alias, col = "u", strings.TrimPrefix(f.DBName, "u_")
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(alias + "." + col + " AS " + f.DBName)
	}
	return b.String(), nil
})

// loadAuthPair 用一条 JOIN 查询取回 KEY 与用户（这是 `/v1` 每请求的**唯一** DB 往返）。
func loadAuthPair(where string, arg any) (*authPair, error) {
	sel, err := authPairSelect()
	if err != nil {
		return nil, err
	}
	var p authPair
	// Unscoped 是**必需**的：embeddedPrefix 让字段的 DBName 变成了 `k_deleted_at`，
	// GORM 的软删条件会据此生成 `k.k_deleted_at IS NULL`（不存在的列）。
	// 代价是软删过滤要自己写 —— 所以下面显式加上，且**只能**加在这里：
	// 少了它，被删的 KEY 会继续通过鉴权（这是安全缺陷，不是显示问题）。
	err = db.DB.Unscoped().Table("api_keys AS k").
		Select(sel).
		Joins("JOIN users AS u ON u.id = k.user_id").
		Where(where, arg).
		Where("k.deleted_at IS NULL AND u.deleted_at IS NULL").
		Limit(1).
		Take(&p).Error
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// AuthenticateWithUser 是 Authenticate 的「顺带取用户」版本：网关每请求都要用到
// 用户（状态、余额、分组），分两次查库等于把 DB 事务翻倍。
//
// 压测实测：`/v1` 每请求 1.84 个 DB 事务，10 VU 就把 PG 打到 9.3k txn/s 饱和，
// 20 VU 吞吐从 5,047 rps 掉到 1,565 rps。合并成一条后事务/请求 ≈0.92。
//
// 语义与 Authenticate **完全一致**（同一份 checkUsable + touchKey），只有查询形态不同；
// 两条路径的一致性由 TestAuthenticateWithUserMatchesTwoStep 守着。
func AuthenticateWithUser(plain string) (*model.APIKey, *model.User, error) {
	now := time.Now()
	h := Hash(plain)
	p, err := loadAuthPair("k.key_hash = ?", h)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
		}
		// 第二跳同样走 JOIN：轮换窗口内仍要拿到用户，否则调用方还得再查一次。
		p, err = loadAuthPair("k.prev_key_hash = ?", h)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil, err
			}
			return nil, nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
		}
		if p.Key.PrevKeyExpiresAt == nil || !p.Key.PrevKeyExpiresAt.After(now) {
			return nil, nil, ErrKeyRetired
		}
	}
	if err := checkUsable(&p.Key, now); err != nil {
		return nil, nil, err
	}
	touchKey(&p.Key, now)
	return &p.Key, &p.User, nil
}

// checkUsable 是「拿到行之后」的全部判定：状态、过期、托管密钥的闲置续期。
//
// 抽出来给两条查询路径共用：各写一遍的话，迟早会出现"合并查询那条忘了续期"，
// 症状是托管密钥在合并路径上永远过期（或永远不落 status=2）。
func checkUsable(k *model.APIKey, now time.Time) error {
	if k.Status != KeyStatusEnabled {
		// 闲置超期也会落成 status=2（见 markExpired），但它不是"被封禁"：
		// 一把钥因为太久没调用而被标记，处置动作是"在门户轮换一次"；而
		// admin_block 的处置动作是"联系平台"。两者若共用一个码，应用会在
		// 该找用户点轮换的时候被告知"去联系平台"，或者反过来。
		//
		// 这也是"码会变脸"的修法：改造前第一次请求返回 key_expired（当时还没落状态），
		// 之后每次都是 key_disabled —— 同一个故障出现两种语义，排障文档对不上。
		if k.DisabledReason == model.KeyDisabledExpired {
			return ErrKeyExpired
		}
		return ErrKeyDisabled
	}
	if k.ExpiredAt != nil && k.ExpiredAt.Before(now) {
		if !k.IsHosted() {
			return ErrKeyExpired
		}
		pol := hostedPolicyOf()
		if pol.ExpireDays > 0 {
			// 闲置判定基准与截止线：基准取 COALESCE(last_used_at, created_at)，
			// 截止线 = now - ExpireDays。算一次、传下去 —— 落库谓词用**同一个**
			// 截止线（`COALESCE(...) < cutoff`），不再各写一条等价不等式。
			base := k.CreatedAt
			if k.LastUsedAt != nil {
				base = *k.LastUsedAt
			}
			cutoff := now.AddDate(0, 0, -pol.ExpireDays)
			if base.Before(cutoff) {
				// 真闲置超限：把"名存实亡"落成显式状态（status=2 + reason=expired）。
				//
				// 只判不写的话，门户会一直显示"启用中"，而应用每次调用都 401 ——
				// 用户与运营都只能看到一条错误码，无法统计也无法解释。
				// 写失败不影响本次判定（本次仍然拒绝），下次请求会重试。
				markExpired(k, now, cutoff)
				return ErrKeyExpired // 救回无意义
			}
			// 闲置未超限：续期放行。ahead=0 = 只对已过期的钥生效。
			// 写库失败按 503 处理（fail-closed 且语义正确）——
			// 不静默放行一把过期 KEY，也不让接入方误以为凭证失效。
			if _, err := renewExpiredAt(k, now, pol, 0); err != nil {
				return fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
			}
		}
	}
	return nil
}

// AllowsModel 校验双白名单：models_allowed 与 modalities_allowed 均为空时放行全部。
// 供 S5 网关在转发前调用（S3 冻结并单测契约）。
func AllowsModel(k *model.APIKey, modelID, modality string) bool {
	if models := decodeStrings(k.ModelsAllowed); len(models) > 0 && !contains(models, modelID) {
		return false
	}
	if mods := decodeStrings(k.ModalitiesAllowed); len(mods) > 0 && !contains(mods, modality) {
		return false
	}
	return true
}

// DecodeStrings 解析 api_keys 的 JSONB 字符串数组列（空/非法一律返回**空切片**）。
//
// 空切片而不是 nil：这些值会直接进管理台/门户的 JSON 响应（`models_allowed` 等），
// 而 nil slice 序列化成 `null` → 前端 `.length` 抛 TypeError → 整页被 ErrorBoundary
// 换成 "Something went wrong!"（HTTP 却还是 200，接口层完全看不出问题）。
// 判空仍然用 len()，两种写法语义一致，只有序列化结果不同。
func DecodeStrings(raw model.JSON) []string { return decodeStrings(raw) }

func decodeStrings(raw model.JSON) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// 密钥状态（落库值）。历史上用字面量 1/2 散在各处，这里给新代码一个具名入口；
// 已有代码暂不批量替换（改动面大且无收益），但**新写的**一律用常量。
const (
	KeyStatusEnabled  int16 = 1
	KeyStatusDisabled int16 = 2
)

// revokeKeysTx 是 KEY 止血的唯一实现：禁用该用户全部启用中的密钥。
// includeHosted=false 时排除托管密钥（kind='hosted'）——排除式而非枚举式：
// 未来新增 kind 默认仍会被止血禁用（fail-closed），不会因为漏加枚举而留下漏网之鱼。
//
// 为什么托管密钥可以被改密豁免：密码泄露 ≠ 托管密钥泄露，两者威胁模型正交；
// 托管密钥自有轮换 + 金额上限 + 有效期三重止损，误禁反而制造"应用断供且无法自愈"的死局。
// 管理员强制重置密码（账号认定失陷）仍然全禁，见 setPassword 的 revokeHosted 参数。
func revokeKeysTx(tx *gorm.DB, uid uint64, includeHosted bool) (int64, error) {
	q := tx.Model(&model.APIKey{}).
		Where("user_id = ? AND status = ?", uid, KeyStatusEnabled)
	if !includeHosted {
		q = q.Where("kind <> ?", model.KeyKindHosted)
	}
	// disabled_reason 必须与 status 同写：这是"账号可能已失陷"的判定，
	// 属于最严格的一类禁用 —— 若留空串，轮换路径会把它当成可自愈，
	// 于是强制重置密码后应用只要换一次钥就绕过了处置。
	res := q.Updates(map[string]any{
		"status":          KeyStatusDisabled,
		"disabled_reason": model.KeyDisabledAdminBlock,
	})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// RevokeAllTx 在给定事务里禁用该用户的**全部启用中**密钥，返回受影响行数。
//
// 为什么必须有这一步：改密与换绑是"账号可能已被接管"的标准止血动作。
// 在此之前两者只撤 user_sessions —— 攻击者用会话建的自建 KEY（或已泄露的
// KEY）不受影响，会继续按 user_id 计费直到过期，止血只止了一半。
//
// 放在事务里是因为它必须与"改密/换绑"同生共死：先改密后撤 KEY 中间失败，
// 会留下"密码已换、旧 KEY 仍能花钱"的窗口，而那正是用户最需要它生效的时刻。
//
// 返回 0 是正常情况（用户没有 KEY）。
func RevokeAllTx(tx *gorm.DB, uid uint64) (int64, error) {
	return revokeKeysTx(tx, uid, true)
}

// RevokeSelfBuiltTx 只禁自建 KEY（托管密钥不动）。改密/换绑的豁免入口。
func RevokeSelfBuiltTx(tx *gorm.DB, uid uint64) (int64, error) {
	return revokeKeysTx(tx, uid, false)
}

// RevokeAll 是 RevokeAllTx 的非事务版本（用于没有现成事务的调用点）。
func RevokeAll(ctx context.Context, uid uint64) (int64, error) {
	return RevokeAllTx(db.DB.WithContext(ctx), uid)
}

// RevokeSelfBuilt 是 RevokeSelfBuiltTx 的非事务版本。
func RevokeSelfBuilt(ctx context.Context, uid uint64) (int64, error) {
	return RevokeSelfBuiltTx(db.DB.WithContext(ctx), uid)
}
