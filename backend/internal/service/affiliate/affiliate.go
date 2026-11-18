// Package affiliate 实现邀请码绑定、按充值笔数发放返利、以及返利转入余额。
//
// # 资金模型
//
// 返利额度**不存在 users 上**：可转出 / 冷却中 / 累计全部由 affiliate_rewards
// 的行派生（见 Info）。行就是钱，没有第二个累加器。
//
// 一行 = 一个受益人因一笔订单拿到的钱（role = inviter / invitee）。为什么不是
// 「一行装两个人」：那样只有一个 transferred_at，任何一方先转出都会把另一方的
// 份额永久锁死（转不出、退款也不作废）。详见 model.AffiliateReward 的注释。
//
// 返利按「被邀请人的前 N 笔充值」逐笔发放：邀请人拿每笔实付的比例，
// 被邀请人拿每笔固定额（且不超过该笔实付）。
//
// 冷却期内（hold_hours）返利不可转出；此期间订单退款成功则该行**作废**
// （钱从未到用户手里，不倒扣余额、不记欠款）。到期后归用户，之后再退款不追回。
package affiliate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/affcfg"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/service/billing"
)

// 返利行的状态（对外下发，前端据此渲染标签；服务端算好，前端不碰时钟）。
const (
	StateCooling     = "cooling"     // 冷却中，不可转出
	StateAvailable   = "available"   // 已到期，可转出
	StateTransferred = "transferred" // 已转入余额
	StateVoided      = "voided"      // 已随退款作废
)

// rewardStateSQL 是「一行处于什么状态」的**唯一实现**。
//
// Info、Records、对账都从这里取。分开各写一遍的代价不是重复代码，而是三处判据
// 会慢慢漂移：某一处把 `>` 写成 `>=`，用户在到期那一刻看到可转出、点进去却报
// 「未达最低转出额度」。调用方必须提供别名 `r`。
const rewardStateSQL = `CASE WHEN r.voided THEN 'voided'
                             WHEN r.transferred_at IS NOT NULL THEN 'transferred'
                             WHEN r.available_at > now() THEN 'cooling'
                             ELSE 'available' END`

// Config 是邀请返利的运行时配置（值语义，缺键已补齐）。
type Config struct {
	affcfg.Settings
	// Degraded 表示这份配置不能按原样使用（缺键 / 脏 JSON / 越界 / 双 0）。
	// 必须下发给门户并打日志：静默按零值工作就是「开关开着、一分不发、没人知道」。
	Degraded bool `json:"degraded"`
}

// degradedLogged 保证同一种坏配置在每个进程里只告警一次。
//
// 用内容摘要而不是布尔标记：配置被改成另一种坏形态时要再报一次，
// 否则第二次改坏就彻底静默了。
var degradedLogged sync.Map

// ConfigNow 读取当前邀请返利配置。
//
// 三种失败一律 Degraded=true 并返回安全值：
//   - 键不存在 / 脏 JSON → 按**关闭**处理（不能因为「没配」就开始发钱）；
//   - 值越界 → 按 affcfg.Normalize 收敛（可解释），仍标记 Degraded 让人知道被改过。
func ConfigNow() Config {
	raw, ok := conf.System().Get("aff")
	if !ok {
		markDegraded("missing", "邀请返利配置键 aff 不存在，已按关闭处理")
		return Config{Settings: affcfg.Defaults(), Degraded: true}
	}
	s, degraded, err := affcfg.Resolve(raw)
	if err != nil {
		markDegraded(fmt.Sprintf("decode:%x", sha256.Sum256(raw)), "邀请返利配置无法解析，已按关闭处理")
		s.Enabled = false
		return Config{Settings: s, Degraded: true}
	}
	if degraded {
		markDegraded(fmt.Sprintf("value:%x", sha256.Sum256(raw)), "邀请返利配置存在缺失或越界，已按收敛值生效")
	}
	return Config{Settings: s, Degraded: degraded}
}

// markDegraded 记录一次降级（同内容只报一次 + 指标常驻）。
func markDegraded(key, msg string) {
	metrics.Set("affiliate_config_degraded", 1)
	if _, loaded := degradedLogged.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	logger.L().Error(msg)
}

// Info 是邀请返利面板数据。
//
// 三个金额全部是派生值，不是累加器：
//   - AffAvailable 已到期且未转出、未作废；
//   - AffCooling   未到期、未转出、未作废；
//   - AffHistory   累计已获得（含已转出与冷却中，不含已作废）。
type Info struct {
	AffCode       string `json:"aff_code"`
	AffCount      int64  `json:"aff_count"`
	AffAvailable  int64  `json:"aff_available"`
	AffCooling    int64  `json:"aff_cooling"`
	AffHistory    int64  `json:"aff_history"`
	Enabled       bool   `json:"enabled"`
	Degraded      bool   `json:"degraded"`
	MinTransfer   int64  `json:"min_transfer"`
	InviterRate   int64  `json:"inviter_rate"`
	InviteeAmount int64  `json:"invitee_amount"`
	MaxRewards    int64  `json:"max_rewards"`
	HoldHours     int64  `json:"hold_hours"`
}

// Record 是单条返利记录（订单粒度，对方用户名脱敏）。
//
// 一行一受益人后，被邀请人也能在同一个列表里看到自己拿到的赠金：
// Role 说明这笔钱是哪种身份拿到的，Counterpart 是交易对手（邀请人 / 被邀请人）。
type Record struct {
	Counterpart string    `json:"counterpart"`
	Role        string    `json:"role"`
	OrderID     uint64    `json:"order_id"`
	BaseAmount  int64     `json:"base_amount"`
	Reward      int64     `json:"reward"`
	State       string    `json:"state"`
	AvailableAt time.Time `json:"available_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// Service 是邀请返利域服务。
type Service struct{ db *gorm.DB }

// New 构造邀请返利服务。
func New(gdb *gorm.DB) *Service { return &Service{db: gdb} }

// Info 返回当前用户的邀请码、受邀人数与三项派生额度。
func (s *Service) Info(ctx context.Context, userID uint64) (*Info, error) {
	var u model.User
	if err := s.db.WithContext(ctx).Select("id", "aff_code").First(&u, userID).Error; err != nil {
		return nil, err
	}
	// 邀请码懒生成：注册时若那一步失败（撞码 5 次、进程中断），用户就再也拿不到码，
	// 而邀请码只有这一个写入口 —— 没有自愈就是「这个账号永远不能邀请人」。
	if u.AffCode == "" {
		code, err := s.repairCode(ctx, userID)
		if err != nil {
			logger.L().Warn("补生成邀请码失败", zapUint("user_id", userID), zapErr(err))
		} else {
			u.AffCode = code
		}
	}
	var count int64
	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("inviter_id = ?", userID).Count(&count).Error; err != nil {
		return nil, err
	}
	var sums struct {
		Available int64
		Cooling   int64
		History   int64
	}
	// 一次扫描算出三项：命中 idx_aff_reward_beneficiary。
	// 旧实现用 UNION ALL 把两侧金额拼成一张临时表，既有重复 CASE 又依赖
	// 「inviter_id <> invitee_id」这种特例来防翻倍 —— 一行一受益人后都不需要了。
	if err := s.db.WithContext(ctx).Raw(`
		SELECT COALESCE(SUM(amount) FILTER (WHERE state = 'available'), 0) AS available,
		       COALESCE(SUM(amount) FILTER (WHERE state = 'cooling'), 0)   AS cooling,
		       COALESCE(SUM(amount), 0)                                    AS history
		  FROM (SELECT r.amount, `+rewardStateSQL+` AS state
		          FROM affiliate_rewards r WHERE r.beneficiary_id = ?) x
		 WHERE x.state <> 'voided'`, userID).Scan(&sums).Error; err != nil {
		return nil, err
	}
	cfg := ConfigNow()
	return &Info{
		AffCode: u.AffCode, AffCount: count,
		AffAvailable: sums.Available, AffCooling: sums.Cooling, AffHistory: sums.History,
		Enabled: cfg.Enabled, Degraded: cfg.Degraded, MinTransfer: cfg.MinTransfer,
		InviterRate: cfg.InviterRate, InviteeAmount: cfg.InviteeAmount,
		MaxRewards: cfg.MaxRewards, HoldHours: cfg.HoldHours,
	}, nil
}

// OnRegister 在注册成功后调用：为新用户生成邀请码；邀请码合法且返利开启时绑定邀请关系。
//
// 注册阶段**只绑定关系，不发任何额度**：返利由被邀请人后续充值时逐笔发放，
// 把刷号成本抬到「真金白银」。同时限制单邀请人 24h 内最多绑定 DailyInviteCap 个新用户。
//
// 锁顺序：先把「自己」与「邀请人」两个 id 排序，再按升序加行锁。
// 不排序的话，A 填 B 的码、B 填 A 的码同时注册会以相反顺序取锁 → 死锁，
// 表现为两边注册成功但邀请关系都丢了（只留一条 WARN，没人会发现）。
func (s *Service) OnRegister(ctx context.Context, userID uint64, inviteCode string) error {
	code := strings.ToUpper(strings.TrimSpace(inviteCode))
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		cfg := ConfigNow()
		// 先无锁解析邀请人：这一步只是查 id，真正的串行化在下面的行锁。
		var inviterID uint64
		var skipReason string
		if code != "" {
			if !cfg.Enabled {
				skipReason = "disabled"
			} else {
				var inv struct{ ID uint64 }
				err := tx.Model(&model.User{}).Select("id").Where("aff_code = ?", code).Take(&inv).Error
				switch {
				case errors.Is(err, gorm.ErrRecordNotFound):
					skipReason = "invalid_code"
				case err != nil:
					return err
				default:
					inviterID = inv.ID
				}
				if inviterID == userID {
					skipReason, inviterID = "self", 0
				}
			}
		}

		ids := []uint64{userID}
		if inviterID != 0 {
			ids = append(ids, inviterID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

		var me model.User
		for _, id := range ids {
			var u model.User
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("id", "aff_code", "inviter_id").First(&u, id).Error; err != nil {
				return err
			}
			if id == userID {
				me = u
			}
		}
		if err := s.ensureCodeTx(tx, &me); err != nil {
			return err
		}
		if skipReason != "" {
			logBindSkipped(userID, skipReason)
			return nil
		}
		if me.InviterID != nil {
			// 已经绑过：注册只会发生一次，但把幂等判据写进 SQL 语义，
			// 将来若加换绑入口也不会把关系覆盖成别人的。
			logBindSkipped(userID, "already_bound")
			return nil
		}
		// 24h 窗口用**数据库** now()：应用与库的时钟偏差会让「刚绑的」被算成过期，
		// 直接绕过每日上限。
		var bound int64
		if err := tx.Model(&model.User{}).
			Where("inviter_id = ? AND created_at > now() - interval '24 hours'", inviterID).
			Count(&bound).Error; err != nil {
			return err
		}
		if bound >= cfg.DailyInviteCap {
			logBindSkipped(userID, "cap")
			return nil
		}
		return tx.Model(&model.User{}).
			Where("id = ? AND inviter_id IS NULL", userID).
			Update("inviter_id", inviterID).Error
	})
}

// ensureCodeTx 在当前事务内补齐邀请码（已加行锁，不会撞并发）。
func (s *Service) ensureCodeTx(tx *gorm.DB, me *model.User) error {
	if me.AffCode != "" {
		return nil
	}
	own, err := newAffCode(tx)
	if err != nil {
		return err
	}
	if err := tx.Model(&model.User{}).Where("id = ? AND aff_code = ''", me.ID).
		Update("aff_code", own).Error; err != nil {
		return err
	}
	me.AffCode = own
	return nil
}

// repairCode 在门户读取时补码（不在事务里，靠唯一索引兜并发）。
func (s *Service) repairCode(ctx context.Context, userID uint64) (string, error) {
	for i := 0; i < 3; i++ {
		own, err := randCode()
		if err != nil {
			return "", err
		}
		res := s.db.WithContext(ctx).Model(&model.User{}).
			Where("id = ? AND aff_code = ''", userID).Update("aff_code", own)
		if res.Error != nil {
			if pgerr.IsUnique(res.Error) {
				continue // 撞码：换一个再试
			}
			return "", res.Error
		}
		if res.RowsAffected == 1 {
			return own, nil
		}
		// 0 行 = 已被别的路径写好，直接读回来。
		var cur struct{ AffCode string }
		if err := s.db.WithContext(ctx).Model(&model.User{}).
			Select("aff_code").Where("id = ?", userID).Take(&cur).Error; err != nil {
			return "", err
		}
		return cur.AffCode, nil
	}
	return "", errors.New("affiliate: 邀请码补生成冲突")
}

// logBindSkipped 记录「这次注册没有建立邀请关系」及其原因。
//
// 以前四个分支全是静默 return nil：用户注册成功、关系没建、双方都不知道，
// 客诉时唯一能查的只有「邀请记录是空的」。现在至少留下原因与计数。
func logBindSkipped(userID uint64, reason string) {
	metrics.Inc("affiliate_bind_skipped_total", "reason", reason)
	logger.L().Warn("邀请关系未绑定（注册已成功）",
		zapUint("user_id", userID), zapStr("reason", reason))
}

// RewardInput 是一次返利发放请求。
//
// 用结构体而不是位置参数：AmountCent 的名字就是单位断言 —— 旧签名第三个参数传的是
// o.Credited（微，且含套餐赠送），改成实付时分/微混用会让金额静默错 100 万倍或多算赠送。
type RewardInput struct {
	InviteeID  uint64
	OrderID    uint64
	AmountCent int64 // 订单实付（分）
}

// RewardTx 在被邀请人某笔订单支付成功后发放返利（必须与入账在同一事务内调用）。
//
// 幂等三件套：
//  1. 同单预查（省一次无谓插入；人工补单在「已入账」分支仍会调用本函数）；
//  2. 被邀请人行锁 —— 次数闸的串行化点；
//  3. (order_id, role) 唯一索引 —— 最后一道闸，撞上按幂等成功处理。
//
// 返回值只有「真的出错了」才是 error：未绑定邀请人、已达笔数上限、开关关闭、
// 重复订单都是正常分支。
func RewardTx(tx *gorm.DB, in RewardInput) error {
	cfg := ConfigNow()
	// Degraded 时不发：配置都读不明白就算不出正确金额，宁可漏发（可对账补发），
	// 也不能按错的比例发出去（发出去就收不回来）。
	if cfg.Degraded || !cfg.Enabled || in.OrderID == 0 || in.InviteeID == 0 {
		return nil
	}
	var dup int64
	if err := tx.Model(&model.AffiliateReward{}).Where("order_id = ?", in.OrderID).
		Count(&dup).Error; err != nil {
		return err
	}
	if dup > 0 {
		return nil
	}
	// 一条语句同时读关系和加锁：把次数闸的正确性建立在「调用顺序恰好如此」上是
	// 不可接受的 —— 多一条 SELECT 的代价远小于「并发两单各发一笔」。
	var u model.User
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "inviter_id").First(&u, in.InviteeID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if u.InviterID == nil || *u.InviterID == in.InviteeID {
		return nil
	}
	// 必须是**锁之后**的独立语句：READ COMMITTED 下才看得到别人已提交的行。
	// 一单两行，所以必须按 order_id 去重计数。
	var used int64
	if err := tx.Raw(`
		SELECT COUNT(DISTINCT order_id) FROM affiliate_rewards
		 WHERE invitee_id = ? AND NOT voided`, in.InviteeID).Scan(&used).Error; err != nil {
		return err
	}
	if used >= cfg.MaxRewards {
		return nil
	}

	base, err := money.FromCents(in.AmountCent)
	if err != nil {
		return err
	}
	if base <= 0 {
		return nil // 实付为 0 的异常订单：不发、不占名额、不落行
	}
	inviterAmt, err := money.MulDiv(base, cfg.InviterRate, affcfg.MaxRate)
	if err != nil {
		return err
	}
	// 赠金不超过本单实付：否则「最低充值 1 元、赠金 5 元」就是纯套利。
	// 邀请人侧按比例天然不可能超过实付（比例上限 100%），被邀请人侧必须显式对齐。
	inviteeAmt := cfg.InviteeAmount
	if inviteeAmt > base {
		inviteeAmt = base
	}
	if inviterAmt <= 0 && inviteeAmt <= 0 {
		return nil // 比例与赠金都为 0：不落 0 元凭据白占名额
	}

	// available_at 用数据库 now()：应用与库的时钟偏差会让「刚发的行」被判成已到期
	// （或把到期的判成冷却中），而这条边界就是钱的分界线。
	//
	// ON CONFLICT DO NOTHING 而不是「插库失败再判 23505」：23505 会把整个事务置为
	// aborted（25P02），而这个函数跑在充值入账的大事务里 —— 一旦走那条分支，
	// 后面的「把订单改成 paid」就全废了。冲突即 0 行，天然幂等。
	// 逐行插入（一单最多两行）。两条语句都在同一事务里，原子性由事务保证；
	// 相比拼一条多值 SQL，这里更容易看出「哪一侧被跳过」。
	type leg struct {
		role        string
		beneficiary uint64
		amount      int64
		rate        int64
	}
	legs := make([]leg, 0, 2)
	if inviterAmt > 0 {
		legs = append(legs, leg{model.AffRoleInviter, *u.InviterID, inviterAmt, cfg.InviterRate})
	}
	if inviteeAmt > 0 {
		legs = append(legs, leg{model.AffRoleInvitee, in.InviteeID, inviteeAmt, 0})
	}
	for _, r := range legs {
		if err := tx.Exec(`INSERT INTO affiliate_rewards
			(order_id, invitee_id, beneficiary_id, role, amount, base_amount, rate,
			 available_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, now() + make_interval(hours => ?::int), now())
			ON CONFLICT (order_id, role) DO NOTHING`,
			in.OrderID, in.InviteeID, r.beneficiary, r.role, r.amount, base, r.rate,
			cfg.HoldHours).Error; err != nil {
			return err
		}
	}
	metrics.Add("affiliate_reward_rows_total", float64(len(legs)))
	return nil
}

// RewardBestEffort 尽力发放：返利失败**不阻断充值入账**。
//
// 必须是嵌套事务（GORM 里就是 SAVEPOINT）：裸 Exec 报错会把外层事务置为 aborted
// （25P02），此后「把订单改成 paid」也会失败，最终整笔回滚 —— 那就变成
// 「一个营销模块的故障让全部充值都收不到额度」。包一层之后，返利失败只回滚到
// 存档点，充值照常提交，缺的返利由对账扫描 + 补发通道补齐。
func RewardBestEffort(tx *gorm.DB, in RewardInput) {
	err := tx.Transaction(func(ntx *gorm.DB) error { return RewardTx(ntx, in) })
	if err == nil {
		return
	}
	metrics.Inc("affiliate_reward_failed_total")
	logger.L().Error("邀请返利发放失败（充值已入账，不回滚；可按对账补发）",
		zapUint("order_id", in.OrderID), zapUint("invitee_id", in.InviteeID), zapErr(err))
}

// Voided 是作废结果。
type Voided struct {
	// Rows 是本次真正作废的行数（0 表示无需作废：已转出或本就没发过）。
	Rows int64
}

// VoidTx 在退款成功时作废该订单发出的返利（必须与退款在同一事务内调用）。
//
// 两个条件各有职责：
//   - NOT voided：幂等，重复退款不会二次处理；
//   - transferred_at IS NULL：只作废「还没到手」的钱。
//
// **不再判 available_at**：旧实现要求「未到期才作废」，于是 hold_hours=0 时
// （签发即到期）退款永远不作废，返利还能继续转出。判定「钱有没有出去」只需要
// 看 transferred_at —— 到期但没转出，钱仍在平台手里，必须作废。
func VoidTx(tx *gorm.DB, orderID uint64) (Voided, error) {
	if orderID == 0 {
		return Voided{}, nil
	}
	res := tx.Exec(`UPDATE affiliate_rewards
		SET voided = true, voided_at = now()
		WHERE order_id = ? AND NOT voided AND transferred_at IS NULL`, orderID)
	if res.Error != nil {
		return Voided{}, res.Error
	}
	return Voided{Rows: res.RowsAffected}, nil
}

// Transfer 把当前用户所有「已到期且未转出」的返利转入余额（写 invite 流水）。
//
// 关闭开关**只停新增**（绑定与发放），存量返利照常可转出：钱已经记在用户名下，
// 冻结它只会产生客诉，不会省下任何成本。
//
// 顺序：锁用户 → 取候选并锁行 → 判门槛 → 标记 → 入账。
// 门槛必须在标记之前判：低于门槛时不该留下任何已标记的行。
func (s *Service) Transfer(ctx context.Context, userID uint64) (int64, error) {
	cfg := ConfigNow()
	var credited int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, userID).Error; err != nil {
			return err
		}
		// FOR UPDATE 必须在 ORDER BY 之后：同一用户的两次转入并发时，
		// 后到的那个等锁、重新判定谓词，看到 transferred_at 已非空就自动排除。
		type row struct {
			ID     uint64
			Amount int64
		}
		var rows []row
		if err := tx.Raw(`
			SELECT r.id AS id, r.amount AS amount
			  FROM affiliate_rewards r
			 WHERE r.beneficiary_id = ?
			   AND NOT r.voided AND r.transferred_at IS NULL AND r.available_at <= now()
			 ORDER BY r.id
			   FOR UPDATE`, userID).Scan(&rows).Error; err != nil {
			return err
		}
		ids := make([]uint64, 0, len(rows))
		var sum int64
		for _, r := range rows {
			sum += r.Amount
			ids = append(ids, r.ID)
		}
		if len(ids) == 0 || sum <= 0 {
			return errs.AffNothing
		}
		if sum < cfg.MinTransfer {
			return errs.AffBelowMin
		}
		// 用具体 id 数组标记，不按条件重匹配：避免把并发期间新出现的行捎上。
		if err := tx.Model(&model.AffiliateReward{}).Where("id IN ?", ids).
			Update("transferred_at", gorm.Expr("now()")).Error; err != nil {
			return err
		}
		if err := billing.CreditTx(tx, billing.CreditInput{
			UserID: userID, Amount: sum, Type: model.TxnInvite,
			RefType: model.RefTypeAffiliate, RefID: strconv.FormatUint(userID, 10),
			Reason: "邀请奖励转入余额",
			// 余额上限必须生效：这里是系统内第二个入账入口，漏掉就绕过了 balance_limit。
			Cap: conf.BalanceLimitFor(u.BalanceLimit),
		}); err != nil {
			// 余额上限是**业务拒绝**，不是服务故障：不映射就会变成 500
			// 「服务器内部错误」，用户和运营都看不出真正原因。
			if errors.Is(err, billing.ErrBalanceLimitExceeded) {
				return errs.BalanceLimitExceeded
			}
			return err
		}
		credited = sum
		return nil
	})
	if err != nil {
		return 0, err
	}
	return credited, nil
}

// Records 返回当前用户的返利明细（订单粒度，分页，对方用户名脱敏）。
//
// 金额一律读行上的快照，**不读配置**：旧实现用的是当前配置值，
// 运营改一次比例，历史记录的金额就全变了 —— 那是第二真相源。
//
// 列表按 beneficiary_id 查（不是 inviter_id）：被邀请人拿到的赠金同样要能看到，
// 否则会出现「可转出有金额、明细一行都没有」。
func (s *Service) Records(ctx context.Context, userID uint64, offset, limit int) ([]Record, int64, error) {
	var total int64
	if err := s.db.WithContext(ctx).Model(&model.AffiliateReward{}).
		Where("beneficiary_id = ?", userID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	items := make([]Record, 0, limit)
	if limit > 0 {
		// LEFT JOIN + deleted_at 过滤：对方被软删后，那笔钱仍计入 Info 的派生额，
		// 列表里却消失会让用户以为平台吞了钱。用户名取不到时由 maskUsername 兜底。
		//
		// counterpart 的取法：role=inviter 时对方是付款方（r.invitee_id）；
		// role=invitee 时对方是付款方的邀请人（iu.inviter_id）。
		if err := s.db.WithContext(ctx).Raw(`
			SELECT COALESCE(CASE WHEN r.role = 'inviter' THEN iu.username ELSE vu.username END, '') AS counterpart,
			       r.role, r.order_id, r.base_amount, r.amount AS reward,
			       `+rewardStateSQL+` AS state,
			       r.available_at, r.created_at
			  FROM affiliate_rewards r
			  LEFT JOIN users iu ON iu.id = r.invitee_id AND iu.deleted_at IS NULL
			  LEFT JOIN users vu ON vu.id = iu.inviter_id AND vu.deleted_at IS NULL
			 WHERE r.beneficiary_id = ?
			 ORDER BY r.id DESC LIMIT ? OFFSET ?`, userID, limit, offset).Scan(&items).Error; err != nil {
			return nil, 0, err
		}
	}
	for i := range items {
		items[i].Counterpart = maskUsername(items[i].Counterpart)
	}
	return items, total, nil
}

// BackfillResult 是一次补发的结果。
type BackfillResult struct {
	Scanned  int64 `json:"scanned"`
	Rewarded int64 `json:"rewarded"`
	Skipped  int64 `json:"skipped"`
}

// 补发扫描的硬上限：一次最多处理这么多单，避免一次调用扫全表。
const (
	backfillMaxLimit = 500
	backfillMaxDays  = 90
)

// BackfillMissing 找出「已入账但没有任何返利行」的订单并补发。
//
// 为什么需要它：返利失败现在是「不回滚充值 + 记 ERROR」，那批订单不会自己长回来。
// 没有补发通道，一次故障就等于永久少发 —— 而且对账只能告诉你缺了多少，修不了。
//
// 只扫 paid 且未退款的订单：已退款的订单补发出去就是「钱退了还能提返利」。
func (s *Service) BackfillMissing(ctx context.Context, since time.Time, limit int, dryRun bool) (BackfillResult, error) {
	var rep BackfillResult
	if limit <= 0 || limit > backfillMaxLimit {
		limit = backfillMaxLimit
	}
	type cand struct {
		ID         uint64
		UserID     uint64
		AmountCent int64
	}
	var list []cand
	// 判据用「有 recharge 流水 + 无返利行」：流水才是入账事实，订单状态可能被人改过。
	if err := s.db.WithContext(ctx).Raw(`
		SELECT o.id, o.user_id, o.amount_cent
		  FROM orders o
		 WHERE o.created_at >= ?
		   AND o.status = ?
		   AND (o.refund_status IS NULL OR o.refund_status = '')
		   AND EXISTS (SELECT 1 FROM transactions t
		                WHERE t.ref_type = 'order' AND t.ref_id = o.id::text AND t.type = ?)
		   AND NOT EXISTS (SELECT 1 FROM affiliate_rewards r WHERE r.order_id = o.id)
		 ORDER BY o.id
		 LIMIT ?`, since, model.OrderPaid, model.TxnRecharge, limit).Scan(&list).Error; err != nil {
		return rep, err
	}
	rep.Scanned = int64(len(list))
	if dryRun {
		return rep, nil
	}
	for _, c := range list {
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return RewardTx(tx, RewardInput{InviteeID: c.UserID, OrderID: c.ID, AmountCent: c.AmountCent})
		})
		if err != nil {
			// 单笔失败不中断整批：其余订单仍应补发。
			logger.L().Error("补发邀请返利失败",
				zapUint("order_id", c.ID), zapUint("invitee_id", c.UserID), zapErr(err))
			continue
		}
		var n int64
		if err := s.db.WithContext(ctx).Model(&model.AffiliateReward{}).
			Where("order_id = ?", c.ID).Count(&n).Error; err != nil {
			return rep, err
		}
		if n > 0 {
			rep.Rewarded++
		} else {
			rep.Skipped++
		}
	}
	return rep, nil
}

// BackfillSince 把「天数」换算成扫描起点（供 controller 用，避免它自己算时间）。
func BackfillSince(days int) time.Time {
	if days <= 0 || days > backfillMaxDays {
		days = 7
	}
	return time.Now().AddDate(0, 0, -days)
}

// newAffCode 生成唯一邀请码（8 位大写字母数字，冲突重试）。
func newAffCode(tx *gorm.DB) (string, error) {
	for i := 0; i < 5; i++ {
		c, err := randCode()
		if err != nil {
			return "", err
		}
		var n int64
		if err := tx.Model(&model.User{}).Where("aff_code = ?", c).Count(&n).Error; err != nil {
			return "", err
		}
		if n == 0 {
			return c, nil
		}
	}
	return "", errors.New("affiliate: 邀请码生成冲突")
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randCode() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

// maskUsername 做展示层脱敏；空值（对方已注销或从未发放）给一个可读文案。
func maskUsername(name string) string {
	r := []rune(name)
	switch {
	case len(r) == 0:
		return "已注销"
	case len(r) <= 3:
		return string(r[:1]) + "***"
	default:
		return string(r[:3]) + "***"
	}
}

// 小包装：避免为了两三个字段把 zap 拉进整个文件的调用点。
func zapUint(k string, v uint64) zap.Field { return zap.Uint64(k, v) }
func zapStr(k, v string) zap.Field         { return zap.String(k, v) }
func zapErr(err error) zap.Field           { return zap.Error(err) }
