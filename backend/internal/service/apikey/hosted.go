package apikey

// 本文件是**应用托管密钥**的签发实现。
//
// 托管密钥与用户自建密钥共用 api_keys 表（见 model.APIKey 的注释），差别在三点：
//
//	kind='hosted'        —— 唯一能被 ScopeGuard 放行写操作的凭证类型
//	app_id>0             —— 归因锚点：usages.key_id -> api_keys.app_id
//	scopes=契约派生集合  —— 由 apicontract.HostedScopes() 唯一决定，不由调用方传
//
// 第三条是刻意的：如果签发时允许调用方指定 scopes，接入方就能给自己要"删文件"
// 权限；而它是否需要删文件，平台无从判断。权限集只跟随契约里 AuthWriter 端点的
// 并集，契约变了它自动跟着变（文档与权限因此不会分叉）。

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"

	"lingdong-api/internal/apicontract"
	"lingdong-api/internal/conf"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/pgerr"
)

// HostedPolicy 是托管密钥的默认约束，签发时全额生效。
// rotate 只续期 expired_at、刷新 last_used_at、按契约∩策略重算 scopes、并留下
// 上一代的重叠窗口；**不改**金额上限（由 Freeze 的自动提额跟随余额上调），
// status 是否解禁由 disabled_reason 决定（见 CanRotateRecover）。
//
// 为什么必须有：托管密钥是平台权限最高的长期凭证 —— 能调全部在售模型。
// 应用侧泄露后，损失上限由金额上限封顶，且轮换/续期/提额都有唯一出口。
type HostedPolicy struct {
	// ExpireDays 是签发/轮换时的有效期（0 = 过期功能整体关闭，永不过期）。
	ExpireDays int
	// AmountLimitFloor 是金额上限的下限（微）：取 max(用户当前余额, 该值)，
	// 保证"余额为 0 的新用户"也能拿到一把能用的密钥，但不会无限额。
	AmountLimitFloor int64
	// RPMLimit 是默认每分钟请求数（0 = 不限制）。
	RPMLimit int
	// AllowBillingWrite 是否给新密钥下发 billing:write（下单 / 兑换码核销）。
	// 默认 false：应用侧替用户充值并不常见，而它是唯一能"动钱"的写权限。
	AllowBillingWrite bool
	// AllowAutoRaise 是否允许 Freeze 顶额时跟随余额自动上调金额上限。
	// 硬顶 = 用户当前余额（与签发承诺一致，敞口不超余额本身）；
	// 显式配 false 可热关闭，回退到"顶额即拒、人工处理"。
	AllowAutoRaise bool
	// RotateOverlapHours 是轮换后**旧明文**的存活时长（0 = 立即失效）。
	//
	// 这个值直接决定「轮换会不会造成断服」：窗口足够覆盖一次发布（含灰度）
	// 就是零中断；窗口 0 只有在「密钥已确认泄露」时才该用。
	RotateOverlapHours int
}

// hostedPolicyOf 读取 system_config.hosted_key，缺省值见上。
func hostedPolicyOf() HostedPolicy {
	p := HostedPolicy{
		ExpireDays: 180, AmountLimitFloor: 100_000_000, RPMLimit: 600,
		AllowAutoRaise: true,
		// 72h：覆盖"改配置 → 滚动发布 → 观察"一个完整周期，且留出跨周末的余量。
		// 再长就等于让两把长期密钥并存，泄露处置的意义被稀释。
		RotateOverlapHours: 72,
	}
	raw, ok := conf.System().Get("hosted_key")
	if !ok {
		return p
	}
	var v struct {
		// ExpireDays / RotateOverlapHours 用指针：0 都是合法语义
		// （关掉过期 / 立即失效），必须与「没配」区分。
		ExpireDays         *int  `json:"expire_days"`
		RotateOverlapHours *int  `json:"rotate_overlap_hours"`
		AmountLimitFloor   int64 `json:"amount_limit_floor"`
		RPMLimit           int   `json:"rpm_limit"`
		AllowBillingWrite  bool  `json:"allow_billing_write"`
		AllowAutoRaise     *bool `json:"allow_auto_raise"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return p
	}
	if v.ExpireDays != nil && *v.ExpireDays >= 0 {
		p.ExpireDays = *v.ExpireDays
	}
	// 上限 30 天：超过它就等于"轮换后旧钥长期可用"，与不轮换没有区别。
	if v.RotateOverlapHours != nil && *v.RotateOverlapHours >= 0 {
		p.RotateOverlapHours = *v.RotateOverlapHours
		if p.RotateOverlapHours > 720 {
			p.RotateOverlapHours = 720
		}
	}
	if v.AmountLimitFloor > 0 {
		p.AmountLimitFloor = v.AmountLimitFloor
	}
	if v.RPMLimit > 0 {
		p.RPMLimit = v.RPMLimit
	}
	p.AllowBillingWrite = v.AllowBillingWrite
	// 指针判定：缺字段=保持默认 true；显式 false 才关（避免"没配就静默关闭自愈"）。
	if v.AllowAutoRaise != nil {
		p.AllowAutoRaise = *v.AllowAutoRaise
	}
	return p
}

// HostedKeyPolicy 供 billing 等外部包只读托管策略（进程内配置，无 DB 开销）。
// 单一真相源：托管策略解析只有 hostedPolicyOf 一份，外部一律经这里。
func HostedKeyPolicy() HostedPolicy { return hostedPolicyOf() }

// scopesForNewHostedKey 按策略裁剪权限集（只影响新签发的密钥）。
func scopesForNewHostedKey(all []string, p HostedPolicy) []string {
	if p.AllowBillingWrite {
		return all
	}
	out := make([]string, 0, len(all))
	for _, s := range all {
		if s == apicontract.ScopeBillingWrite {
			continue
		}
		out = append(out, s)
	}
	return out
}

// hostedScopesForIssue 是托管密钥权限集的**唯一出口**：契约派生 ∩ 策略裁剪。
//
// 签发与轮换都必须经它。曾出现过"insert 走裁剪、rotate 直接传原始契约集"的分叉，
// 后果是 allow_billing_write=false 时，轮换一次就把 billing:write 加回去 ——
// 一个配置项被一次运维动作静默推翻，且没有任何日志能看出来。
func hostedScopesForIssue() []string {
	return scopesForNewHostedKey(apicontract.HostedScopes(), hostedPolicyOf())
}

// HostedInput 是签发/轮换托管密钥的入参。
type HostedInput struct {
	AppID  uint64
	UserID uint64
	// Name 只在首次创建时生效（轮换不改名，避免接入方的自定义名被冲掉）。
	Name string
	// Rotate 表达"想换一把新的"这个**意图**，不参与任何授权判定。
	//
	// 是否属于"首次接入"由库中是否已存在该 (app,user) 的托管密钥决定
	// （见 controller 的 Existing 预查）—— 用请求参数做授权锚点，等于让调用方
	// 自己声明"我不是新用户"，一个 {"rotate":true} 就能绕过 allow_self_issue。
	Rotate bool
	// Existing 是 controller 预查到的既有托管密钥（nil = 首次接入）。
	//
	// 由调用方传入而不是本函数自己查，是为了让"授权判定"与"业务动作"用**同一次**
	// 读取的结果：否则授权时看到的是旧行、插入时又查一次，中间存在时序缝隙。
	Existing *model.APIKey
}

// HostedResult 是签发结果。
type HostedResult struct {
	Key *model.APIKey
	// Plaintext 仅在"本次真的生成了新密钥"时非空（首次签发或轮换）。
	// 已存在且未轮换时为 ""，调用方必须据此判断能不能展示一次性明文。
	Plaintext string
	// Created 表示本次是否新建了记录（false = 命中已有密钥）。
	Created bool
	// PreviousExpiresAt 是**上一把明文**的最晚可用时间（重叠窗口结束点）。
	// nil = 没有上一代可用（首次签发，或窗口配置为 0 = 立即失效）。
	//
	// 必须回给调用方：接入方据此得知"还有多久必须完成替换"，
	// 否则它只能靠猜，或者在旧钥失效时才从 401 上发现。
	PreviousExpiresAt *time.Time
}

// CreateHosted 为一个 (应用, 用户) 签发或轮换托管密钥。
//
// 唯一性由 uq_api_keys_hosted 保证：同一 (app_id, user_id) 只允许一把未删除的
// 托管密钥。并发签发时后到的那个会撞唯一索引，此时**回查并复用**已存在的行
// （与用户自建 KEY 的处理相反 —— 那里撞了就重试新随机值，因为两把独立密钥
// 本来就是合法的）。
//
// 为什么必须只允许一把：应用侧调用 /v1 时用的是它自己存的那把。如果同一
// (应用, 用户) 存在两把，用量会随机落到其中一把，两把的 used 各自累计、
// KEY 级金额限额各自计算 —— 账单上表现为"同一个用户的应用消费被拆成两份"，
// 而任何单点日志都看不出问题。
func CreateHosted(ctx context.Context, in HostedInput) (*HostedResult, error) {
	if in.AppID == 0 || in.UserID == 0 {
		return nil, ErrInvalidHostedInput
	}
	// 调用方已预查：复用它，不再重复查一次（否则授权与动作读的是两个时刻的行）。
	if in.Existing != nil {
		if !in.Rotate {
			return &HostedResult{Key: in.Existing}, nil
		}
		return rotateHosted(ctx, in.Existing, false)
	}

	scopes := hostedScopesForIssue()
	if len(scopes) == 0 {
		// 契约里没有任何 AuthWriter 端点时不允许签发：一把不能写任何东西的
		// "托管密钥"只会让接入方以为拿到了可用凭证，然后在第一个写请求上 403。
		return nil, ErrNoHostedScopes
	}
	res, cerr := insertHosted(db.DB.WithContext(ctx), in, scopes)
	if cerr == nil {
		return res, nil
	}
	if !pgerr.IsUnique(cerr) {
		return nil, cerr
	}
	// 并发：另一个请求刚插入且**已经拿到了明文**。
	//
	// 这里绝不能接着轮换 —— 那会把对方刚交付的明文挤进 prev，窗口 0 时当场失效，
	// 而它没有任何办法察觉。Plaintext="" + Created=false 是既有契约
	// （"没有新明文"），调用方据此就知道"已有钥，不是我拿到的"。
	var existing model.APIKey
	if err := db.DB.WithContext(ctx).
		Where("app_id = ? AND user_id = ? AND kind = ?", in.AppID, in.UserID, model.KeyKindHosted).
		First(&existing).Error; err != nil {
		return nil, err
	}
	return &HostedResult{Key: &existing}, nil
}

// hostedExpiry 是 expired_at 的唯一计算点（签发与轮换共用）。
// ExpireDays<=0 表示过期功能整体关闭（永不过期 → nil）。
func hostedExpiry(pol HostedPolicy, now time.Time) *time.Time {
	if pol.ExpireDays <= 0 {
		return nil
	}
	t := now.AddDate(0, 0, pol.ExpireDays)
	return &t
}

// generateKeyMaterial 生成一整套密钥物料（明文 / 密文 / 哈希 / 前缀）。
//
// 唯一实现：签发与轮换必须产出**同构**的凭证 —— 任何一边漏一步，都会造出一把
// "能存不能查"（缺 hash）或"能查不能解"（缺 cipher）的坏钥，而后者要到用户点
// 回显时才暴露。
func generateKeyMaterial() (plain, cipher, hash, prefix string, err error) {
	plain, hash, prefix, err = Generate()
	if err != nil {
		return "", "", "", "", err
	}
	if cipher, err = Encrypt(plain); err != nil {
		return "", "", "", "", err
	}
	return plain, cipher, hash, prefix, nil
}

// insertHosted 生成并落库一把新托管密钥。
func insertHosted(conn *gorm.DB, in HostedInput, scopes []string) (*HostedResult, error) {
	plain, cipher, hash, prefix, err := generateKeyMaterial()
	if err != nil {
		return nil, err
	}
	scopesJSON, err := model.NewJSON(scopes)
	if err != nil {
		return nil, err
	}
	name := in.Name
	if name == "" {
		name = "应用托管密钥"
	}
	// 签发时的默认约束：有效期 / 金额上限 / RPM。
	// 金额下限取 max(用户当前余额, floor)：余额为 0 的新用户也要能拿到一把能用的密钥，
	// 但不会出现"连上限都没有"的长期凭证；后续由 Freeze 的自动提额跟随余额上调。
	//
	// now 只取一次：expired_at 与 created_at 若有微秒差，"闲置 180 天"的判定基准
	// 与"有效期起算点"就会指向两个时刻，排障时对不上。
	pol := hostedPolicyOf()
	now := time.Now()
	amountLimit := pol.AmountLimitFloor
	var owner model.User
	if err := conn.Select("balance").First(&owner, in.UserID).Error; err == nil && owner.Balance > amountLimit {
		amountLimit = owner.Balance
	}
	expiresAt := hostedExpiry(pol, now)
	k := model.APIKey{
		UserID: in.UserID,
		AppID:  in.AppID,
		Kind:   model.KeyKindHosted,
		Scopes: scopesJSON,
		// 新签发一律带三重默认约束（有效期 / 金额上限 / RPM），见 HostedPolicy。
		//
		// 历史注释曾写"这些都是应用该自己管的事"，但托管密钥是平台权限最高的
		// 长期凭证：应用侧泄露时，用户自己都无法在门户禁用它（只能轮换）。
		// 没有平台侧的硬闸，"损失上限"就是用户全部余额。
		ExpiredAt:           expiresAt,
		AmountLimit:         amountLimit,
		RPMLimit:            pol.RPMLimit,
		KeyCipher:           cipher,
		KeyHash:             hash,
		KeyPrefix:           prefix,
		Name:                name,
		Status:              1,
		AllowUserQuotaQuery: true,
		CreatedAt:           now,
	}
	if err := conn.Create(&k).Error; err != nil {
		return nil, err
	}
	return &HostedResult{Key: &k, Plaintext: plain, Created: true}, nil
}

// rotateHosted 为已有托管密钥换一把新的，并按 viaPrev 决定"上一代"留给谁。
//
// 为什么不是"换完即失效"：新明文要经过接入方的配置系统、发布流水线、灰度实例
// 才真正生效，这个时间差以小时计。立即失效等于把每次轮换都变成一次可控故障
// ——旧实现的注释写着"旧明文立即失效，应用必须同时更新配置"，但"同时"在
// 多实例部署里不可能做到。
//
// 窗口内旧明文仍可鉴权（见 Authenticate 的第二跳查询），窗口结束后返回
// key_retired（而不是 invalid_api_key）：让接入方能区分"我配错了"与
// "我该换钥了"，前者要排查，后者是运维动作。
//
// 走 map 更新而不是 Save：Save 会把结构体的零值写回（例如 Used 归零），
// 那会静默抹掉累计消费额，直接影响 KEY 级金额限额的判断。
//
// CAS 必须同时绑定 key_hash / status / disabled_reason —— 三者任一变化都意味着
// "有人在我读取之后做了裁决"，此时按旧快照写回就是覆盖别人的处置：
//   - key_hash 变 = 另一个请求已轮换成功 → 再换一次会把它刚交付的明文挤掉
//   - status/disabled_reason 变 = 管理员禁用或解禁 → 按旧快照写 status 会绕过处置
func rotateHosted(ctx context.Context, k *model.APIKey, viaPrev bool) (*HostedResult, error) {
	// 权限集在函数内部取：不设参数，就没有"调用方传错"这个可能。
	// 上一轮 `billing:write` 回归正是从"insert 走裁剪、rotate 传原始契约集"来的。
	scopes := hostedScopesForIssue()
	if len(scopes) == 0 {
		return nil, ErrNoHostedScopes
	}
	scopesJSON, err := model.NewJSON(scopes)
	if err != nil {
		return nil, err
	}
	pol := hostedPolicyOf()
	// 首次读取到的哈希：CAS 谓词与"别人是否已换钥"的比对都以它为准。
	// 循环内 k 会被重读替换，这个值不能跟着变。
	origHash := k.KeyHash

	for attempt := 0; attempt < 2; attempt++ {
		if k.Status != KeyStatusEnabled && !k.CanRotateRecover() {
			// admin_block：人认定这把凭证不可信。**不生成、不返回明文**。
			return nil, ErrKeyDisabled
		}
		plain, cipher, hash, prefix, err := generateKeyMaterial()
		if err != nil {
			return nil, err
		}
		now := time.Now()
		// 轮换 = 重建信任，有效期重新拉满（与签发同一 helper）。
		renewedExp := hostedExpiry(pol, now)
		updates := map[string]any{
			"key_cipher": cipher,
			"key_hash":   hash,
			"key_prefix": prefix,
			// 权限集 = 当前契约 ∩ 当前策略，与签发**同一个出口**。
			// 曾经这里直接传契约全集，于是 allow_billing_write=false 被一次轮换
			// 静默推翻（billing:write 又回来了）。
			"scopes":          scopesJSON,
			"reset_secret_at": &now,
			"expired_at":      renewedExp,
			"last_used_at":    &now,
			"updated_at":      now,
			// 先清空上一代，只有确实要开窗口时才写回：避免"窗口 0"时
			// 残留一把上一个周期的旧钥（那会让立即失效的语义失效）。
			"prev_key_hash":       "",
			"prev_key_expires_at": nil,
		}
		var prevExp *time.Time
		switch {
		case viaPrev:
			// 调用方手里是**窗口期旧钥**：明文只回发起者，它拿不到别人轮换出的
			// 新明文，所以必须允许自救。但窗口不能延长 —— 否则反复用旧钥轮换
			// 就等于让旧钥永不失效。故 prev 指向**原主钥**（先前轮换者拿到的那把），
			// 终点沿用原值：两个"上一代"候选里只有它能被保留，而调用方已换新。
			if k.PrevKeyExpiresAt != nil && k.PrevKeyExpiresAt.After(now) {
				t := *k.PrevKeyExpiresAt
				updates["prev_key_hash"] = origHash
				updates["prev_key_expires_at"] = &t
				prevExp = &t
			}
		case pol.RotateOverlapHours > 0:
			t := now.Add(time.Duration(pol.RotateOverlapHours) * time.Hour)
			updates["prev_key_hash"] = origHash
			updates["prev_key_expires_at"] = &t
			prevExp = &t
		}
		// 禁用语义：只有"闲置超期"这类**不带安全判断**的禁用能被轮换解除。
		// admin_block 保持禁用 —— 否则管理员处置可被一次 rotate 绕过。
		if k.Status != KeyStatusEnabled && k.CanRotateRecover() {
			updates["status"] = KeyStatusEnabled
			updates["disabled_reason"] = ""
		}

		res := db.DB.WithContext(ctx).Model(&model.APIKey{}).
			Where("id = ? AND key_hash = ? AND status = ? AND disabled_reason = ?",
				k.ID, origHash, k.Status, k.DisabledReason).
			Updates(updates)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 1 {
			k.KeyCipher, k.KeyHash, k.KeyPrefix, k.Scopes = cipher, hash, prefix, scopesJSON
			k.ResetSecretAt = &now
			k.ExpiredAt = renewedExp
			k.LastUsedAt = &now
			k.PrevKeyHash, k.PrevKeyExpiresAt = "", nil
			if v, ok := updates["prev_key_hash"].(string); ok && v != "" {
				k.PrevKeyHash, k.PrevKeyExpiresAt = v, prevExp
			}
			if v, ok := updates["status"]; ok {
				k.Status = v.(int16)
				k.DisabledReason = ""
			}
			return &HostedResult{Key: k, Plaintext: plain, PreviousExpiresAt: prevExp}, nil
		}

		// 0 行：读取之后有人改了这一行。用**干净 dest** 重读 ——
		// 复用已填充的 dest 会被 GORM 把主键追加成条件（项目踩过的坑）。
		var fresh model.APIKey
		if err := db.DB.WithContext(ctx).First(&fresh, k.ID).Error; err != nil {
			return nil, err
		}
		if fresh.KeyHash != origHash {
			// 别人已经换过钥：再换一次只会把它刚交付的明文挤掉（窗口 0 时当场死），
			// 而它无从得知。退出竞争，让调用方拿到 409。
			return nil, ErrRotateConflict
		}
		k = &fresh
	}
	return nil, ErrRotateConflict
}

// RotateSelf 让一把**已通过鉴权**的托管密钥换新明文（自举轮换）。
//
// 与 CreateHosted(rotate=true) 的区别：直接锁 key_id，不再按 (app,user) 查 ——
// 同一用户在不同应用下各有一把托管密钥，按 (app,user) 查会命中错行，把另一个
// 应用的凭证换掉。
//
// viaPrev 表示本次鉴权走的是重叠窗口里的上一代明文（由中间件判定后传入），
// 语义见 rotateHosted 的分支说明。
func RotateSelf(ctx context.Context, keyID uint64, viaPrev bool) (*HostedResult, error) {
	var k model.APIKey
	err := db.DB.WithContext(ctx).
		Where("id = ? AND kind = ?", keyID, model.KeyKindHosted).
		First(&k).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyGone
		}
		return nil, err
	}
	return rotateHosted(ctx, &k, viaPrev)
}

// ErrRotateConflict 表示并发轮换超过了内部重试次数（同一密钥被两个请求同时换）。
//
// 必须显式报错而不是"随便返回一把"：调用方拿到明文后就会覆盖自己存的凭证，
// 返回一把已经被覆盖的钥等于给它一个必然 401 的配置。
var ErrRotateConflict = errors.New("apikey: 密钥正在被并发轮换，请重试")

// ErrKeyGone 表示自举轮换时目标行已不存在（鉴权后到轮换前被拆除）。
//
// 与 ErrKeyDisabled 分开：后者是「还在，但不可用」（401 key_disabled，
// 语义上等人工处置）；这里是「已经没了」，对客应表现为 invalid_api_key，
// 让应用去查自己的配置而不是等平台解锁。
var ErrKeyGone = errors.New("apikey: 密钥已不存在")

// ErrInvalidHostedInput 表示签发入参缺少 app_id / user_id。
var ErrInvalidHostedInput = errInvalid("apikey: 托管密钥缺少应用或用户")

// ErrNoHostedScopes 表示契约里没有任何写端点，托管密钥无意义。
var ErrNoHostedScopes = errInvalid("apikey: 契约未声明任何写端点，无法签发托管密钥")

// errInvalid 是包内简单错误类型（哨兵用，不需要 errors.Is 之外的能力）。
type errInvalid string

func (e errInvalid) Error() string { return string(e) }

// 唯一冲突判据只有 pkg/pgerr 一份（曾在本包留一份同名函数，判据漂移过一次）。
