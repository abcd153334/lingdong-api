package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/money"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/service/apikey"
	"lingdong-api/internal/service/authevent"
)

// 计费流程错误。
var (
	// ErrInvalidAmount 表示入参金额非法（负数 / 超上限 / 缺失用户或 KEY）。
	ErrInvalidAmount = errors.New("billing: 金额非法")
	// ErrNegativeBalance 表示调账后余额将为负，拒绝写入。
	ErrNegativeBalance = errors.New("billing: 调账后余额不能为负")
	// ErrInsufficientQuota 表示余额或 KEY 限额不足（→ 402 insufficient_balance）。
	ErrInsufficientQuota = errors.New("billing: 余额或限额不足")
	// ErrKeyLimitBalanceCap 表示托管密钥顶额后自动提额仍达"余额硬顶"：
	// 提额目标 max(used+amount, floor) 已超过 max(用户余额, floor)，无额可提。
	// 与 ErrInsufficientQuota 必须分开——前者语义是"充值即可恢复"，
	// 下游据此给用户明确的行动指引；混在一起会把"KEY 上限"误报成"账户没钱"。
	ErrKeyLimitBalanceCap = errors.New("billing: 密钥消费上限已达账户余额上限")
	// ErrSessionClosed 表示会话已结算/释放，重复操作被拒绝。
	ErrSessionClosed = errors.New("billing: 计费会话已关闭")
	// ErrUsageGone 表示 usages 行已不存在（被归档清理）。
	//
	// 与 ErrSessionClosed 必须分开：前者是「查不到这条账」，后者是「账已了结」。
	// 合并成一句会让调用方在「记录被删」时按「已结算」处理 → 静默不记账，
	// 而这笔钱既没有被收、也没有留痕，对账时两个方向都查不出来。
	ErrUsageGone = errors.New("billing: 计费记录不存在")
	// ErrUserGone 表示用户记录物理不存在（连软删的壳都没了）。
	//
	// 注意：**用户软删不算**。软删的账号账要照平（补收照扣），
	// 用 tx.Unscoped() 就能读到；只有物理删除才会走到这里。
	ErrUserGone = errors.New("billing: 用户记录不存在")
	// ErrAlreadySettled 表示该笔已由别处结算（唯一流水已存在）。
	//
	// 用于迟到补收：撞 uq_transactions_usage_ref 说明另一次 LateSettle 抢先完成，
	// 本次不再动账。这不是错误路径，但必须能被调用方识别出来 ——
	// 当成普通失败处理会变成「每轮重试一个必败动作直到复查窗口结束」。
	ErrAlreadySettled = errors.New("billing: 该笔已结算")
	// ErrBalanceLimitExceeded 表示入账后余额将超出余额上限，拒绝写入。
	ErrBalanceLimitExceeded = errors.New("billing: 入账后超出余额上限")
	// ErrAmountTooLarge 表示金额越界（映射 400 amount_too_large）。
	ErrAmountTooLarge = money.ErrAmountTooLarge
)

// Service 是余额唯一写入口。
// 任何模块禁止直写 users.balance / api_keys.used，必须经本服务。
type Service struct {
	db *gorm.DB
}

// New 构造 billing 服务。
func New(db *gorm.DB) *Service { return &Service{db: db} }

// Session 是一次计费会话：Freeze → Settle/Release。
type Session struct {
	UsageID uint64 // 对应 usages 行（一 session 一终态行）
	UserID  uint64
	KeyID   uint64
	Frozen  int64 // 预扣额（微，0 = 免费模型直接放行）
	closed  bool
}

// FreezeInput 是开启计费会话的入参。
type FreezeInput struct {
	UserID    uint64
	KeyID     uint64
	Model     string
	Modality  string
	Group     string
	IP        string
	RequestID string
	IsStream  bool
	Snapshot  Snapshot
	Amount    int64 // 预扣额（微）；0 表示免费模型（跳过两级预扣，仅建 pending 行）
	// PromptEst/CompletionEst 是预扣时的估算 token（落 usages.est_*，供回收与误差分析使用）。
	// 注意：completion 为 max_tokens 最坏值，**不得**用于回收结算。
	PromptEst     int64
	CompletionEst int64
	// StartedAt 是**请求入口时刻**（handler 收到请求时），总耗时的起点。
	//
	// 为什么必须由调用方传入而不是在这里取 time.Now()：Freeze 发生在鉴权、读体、
	// 定价预检、选路预检之后，用它当起点会漏掉这些阶段。零值回落 now()。
	StartedAt time.Time
	// CallKind 标记记录来源（sync/async/plugin/precheck），空串按 sync 落库。
	CallKind string
	// Estimate 是**预扣时算出的计费维度**（档位/画幅/张数/参考素材/秒数）。
	//
	// 必须在冻结期就落库，理由是**回收期只有这一行可读**：`ReclaimSettle` 拿不到
	// 上游用量，只能按 usages 列重算。改造前这些列只在 Settle 写，pending 行全是 0，
	// 于是「冻结 15 张、实际交付 2 张」的会话被回收时按 imageCharge 的 `n=1` 保底
	// 收 1 张 —— 系统性少收，而且账单上看不出原因。
	//
	// 金额仍然只认 freeze 流水（frozenOf），这里只负责**维度**，不参与金额来源。
	Estimate Usage
	// IdemKey 是客户端声明的幂等键（`Idempotency-Key` 头）。空串 = 不启用。
	//
	// 只在**同步**链路上有意义：异步提交有 tasks 行与自己的幂等键（B4-2），
	// 两条链路各记各的，互不复用（一个 id 在两边撞车会让人误以为能互换）。
	IdemKey string
	// IdemScope 是幂等作用域（端点路径）。同一个 Key 在不同端点上互不影响：
	// 客户端给一批请求复用同一个键是常见写法，只按 Key 判重会误伤。
	IdemScope string
}

// ErrIdemReplay 表示「这个幂等键已经用过」，本次请求没有再次扣费。
//
// 它不是错误，而是**重复提交**：调用方超时重试时应该得到与首次一致的结论，
// 而不是被扣第二次钱。UsageID 指向首次那条记录。
type ErrIdemReplay struct {
	// UsageID 是首次请求的计费记录号（排障与对账用）。
	UsageID uint64
}

func (e *ErrIdemReplay) Error() string { return "billing: 幂等键重复，已存在计费记录" }

// SettleInput 是结算/释放的终态信息。
type SettleInput struct {
	StatusCode int
	ChannelID  uint64
	// ChannelKeyID 是实际出站的渠道密钥标识（HMAC 指纹）。只落库溯源，**不参与金额**。
	// 空串 = 不改（迟到补结/回收重算时拿不到当次 key，不能把已有归属抹掉）。
	ChannelKeyID      string
	AdapterID         string
	UpstreamRequestID string
	ErrorMsg          string
	// Cost 是上游自报成本（微，结算币种），仅落 usages.cost 参考，不参与 charge。
	Cost          int64
	RequestDigest string
	Usage         Usage
	// UsageSource 是本次结算的用量来源（upstream/mixed/estimated/none）。
	UsageSource string
	// Anomaly 是逗号分隔的口径异常标记。
	Anomaly string
	// UpstreamSpec 是上游实际交付的图像尺寸原文（拿不到留空，不猜）。
	// 只落库 + 与 ImageSpec 比对打 tier_mismatch，**不参与金额计算**。
	UpstreamSpec string
	// RawUsage 是上游 usage 原文（截断后落库，便于事后复核）。
	RawUsage string

	// ---- 调用记录详情：耗时拆分与分类 ----
	UpstreamMs   int
	Attempts     int
	FirstTokenMs int
	// CallKind 覆盖 Freeze 时写入的分类（空串 = 不改）。
	CallKind string
	// TaskID 非 0 时回填 usages.task_id（0 = 不改，避免把已有的绑定清空）。
	TaskID uint64
	// VideoSecondsSource 说明秒数来源：request / upstream / reclaimed。
	VideoSecondsSource string
}

// SettleResult 是结算的实收结果，供调用方展示（如 tasks.settled）。
//
// 为什么必须回传：调用方若自己再算一遍 charge，会与结算内部的两道降额
// （全局截顶 / 余额下限）不一致 —— 任务详情显示 500、调用记录显示 250，永久对不上。
type SettleResult struct {
	Charge    int64  // 实际入账额（截顶/拦截后）
	BeforeCap int64  // 两级降额之前应收
	CapReason string // global / frozen3x / zero_frozen / balance（逗号分隔，空 = 未降额）
	Frozen    int64  // 本次会话的冻结额
	Diff      int64  // charge − frozen（>0 补扣，<0 退还）
	Anomaly   string // 最终落库的异常标记
}

// Freeze 两级原子预扣：扣用户余额 + 占 KEY 限额 + 建 usages pending 行 + 写 freeze 流水。
// 任一步不满足则整体回滚，session 未开启（不产生任何半态）。
//
// 入口守卫（修历史缺陷）：
//   - 负金额显式拒绝：旧实现只判 `Amount > 0`，负数会写进 frozen 并导致结算多扣、
//     释放跳过回退，负冻结永久留在账上。此前"不可达"只是因为 Estimate 恰好把负数
//     钳成了 0 —— 用另一个函数的巧合保护自己，属结构性缺陷。
//   - 超上限拒绝：预扣前就报错，不让一次异常请求冻结出天文数字。
// Freeze 预扣余额并建立计费会话（幂等入口）。
//
// 幂等是**冻结这一层**的职责，不是适配器或控制器的：重复键要在动钱之前就判掉，
// 否则"先冻再查"仍会多出一条冻结流水（逆序退回又是一次多余的对账噪声）。
func (s *Service) Freeze(ctx context.Context, in FreezeInput) (*Session, error) {
	if strings.TrimSpace(in.IdemKey) != "" {
		if id, ok := s.findIdemUsage(ctx, in); ok {
			return nil, &ErrIdemReplay{UsageID: id}
		}
	}
	sess, err := s.freezeOnce(ctx, in)
	if err != nil && strings.TrimSpace(in.IdemKey) != "" && pgerr.IsUnique(err) {
		// 并发同键：两个请求同时通过预查，唯一索引只放行一个。
		// 输的那个事务已整体回滚（钱没动），补一次查询把首次的记录号带回去。
		if id, ok := s.findIdemUsage(ctx, in); ok {
			return nil, &ErrIdemReplay{UsageID: id}
		}
	}
	return sess, err
}

// findIdemUsage 查这个幂等键是否已经建立过计费记录。
func (s *Service) findIdemUsage(ctx context.Context, in FreezeInput) (uint64, bool) {
	var row struct{ ID uint64 }
	err := s.db.WithContext(ctx).Model(&model.Usage{}).
		Select("id").
		Where("key_id = ? AND idem_scope = ? AND idem_key = ?", in.KeyID, in.IdemScope, strings.TrimSpace(in.IdemKey)).
		Limit(1).Scan(&row).Error
	if err != nil || row.ID == 0 {
		return 0, false
	}
	return row.ID, true
}

func (s *Service) freezeOnce(ctx context.Context, in FreezeInput) (*Session, error) {
	if in.UserID == 0 || in.KeyID == 0 {
		return nil, ErrInvalidAmount
	}
	if in.Amount < 0 {
		return nil, ErrInvalidAmount
	}
	if in.Amount > maxCharge() {
		return nil, ErrChargeOverLimit
	}
	snap, err := model.NewJSON(in.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("billing: 快照序列化失败: %w", err)
	}
	sess := &Session{UserID: in.UserID, KeyID: in.KeyID, Frozen: in.Amount}
	var limitRaisedTo int64 // 事务内 raiseLimit 的提额结果；>0 = 本次发生了自动提额
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, in.UserID).Error; err != nil {
			return err
		}
		var k model.APIKey
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&k, in.KeyID).Error; err != nil {
			return err
		}

		// raiseLimit 是托管密钥顶额时的自动提额（唯一实现，仅 hosted）：
		// 把 amount_limit 上调到 max(need, floor)，硬顶 = max(用户当前余额, floor)
		// ——敞口不超过余额本身，与签发承诺一致；充值后下一请求自动恢复。
		// 自建 KEY 绝不自动提额（用户自己的限额自己改）。
		// 前置：k 行已 FOR UPDATE，同 KEY 并发天然串行，无需条件谓词。
		// 事件在事务提交后记（见函数尾部）：authevent 硬规则是"只在确定结果后入队"。
		raiseLimit := func(need int64) error {
			pol := apikey.HostedKeyPolicy()
			if !k.IsHosted() || !pol.AllowAutoRaise {
				return ErrInsufficientQuota
			}
			newLimit := max(need, pol.AmountLimitFloor)
			if newLimit > max(u.Balance, pol.AmountLimitFloor) {
				return ErrKeyLimitBalanceCap
			}
			if err := tx.Model(&model.APIKey{}).Where("id = ?", k.ID).
				Updates(map[string]any{"amount_limit": newLimit, "updated_at": time.Now()}).Error; err != nil {
				return err
			}
			k.AmountLimit = newLimit
			limitRaisedTo = newLimit
			return nil
		}
		if in.Amount > 0 {
			if u.Balance < in.Amount {
				return ErrInsufficientQuota
			}
			// 限额判定用加法而不是减法：旧写法 `limit - used < amount` 在 used 变负时
			// 会因 int64 溢出得到负数，判定恒真 —— 该 KEY 永久无法冻结。
			if k.AmountLimit > 0 && k.Used+in.Amount > k.AmountLimit {
				if err := raiseLimit(k.Used + in.Amount); err != nil {
					return err
				}
			}
		} else if k.AmountLimit > 0 && k.Used > k.AmountLimit {
			// 免费模型也要判一次：否则"0 元冻结"完全绕过 KEY 限额。
			// hosted 同样走自动提额（0 元请求被旧顶额卡死一样是断供）。
			if err := raiseLimit(k.Used); err != nil {
				return err
			}
		}

		newBalance, newFrozen := u.Balance, u.Frozen
		if in.Amount > 0 {
			newBalance = u.Balance - in.Amount
			newFrozen = u.Frozen + in.Amount
			if err := tx.Model(&model.User{}).Where("id = ?", in.UserID).
				Updates(map[string]any{"balance": newBalance, "frozen": newFrozen}).Error; err != nil {
				return err
			}
			if err := keyUsageDelta(tx, in.KeyID, in.Amount); err != nil {
				return err
			}
		}

		startedAt := in.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		usage := model.Usage{
			UserID: in.UserID, KeyID: in.KeyID, Model: in.Model, Modality: in.Modality,
			Kind: "relay", Status: model.UsageStatusPending, Group: in.Group,
			IP: in.IP, RequestID: in.RequestID, IsStream: in.IsStream,
			PricingSnapshot: snap,
			// Currency 单独落一列（不从快照反解）：列表页要按行内币种展示金额，
			// 每行都去解一次 jsonb 会让分页查询多一次反序列化。
			Currency:        in.Snapshot.CurrencyValue(),
			EstPromptTokens: in.PromptEst, EstCompletionTokens: in.CompletionEst,
			StartedAt: &startedAt,
			Frozen:    in.Amount,
			CallKind:  normalizeCallKind(in.CallKind),
			// 计费维度在冻结期就落库：回收/补结没有第二份来源，见 FreezeInput.Estimate。
			ImageSpec:         in.Estimate.ImageSpec,
			ImageRatio:        in.Estimate.ImageRatio,
			ImageQuality:      in.Estimate.ImageQuality,
			ImageN:            in.Estimate.ImageN,
			ImageSpecSource:   in.Estimate.ImageSpecSource,
			AudioDurationSecs: in.Estimate.AudioDurationSecs,
			AudioChars:        in.Estimate.AudioChars,
			RerankDocs:        in.Estimate.RerankDocs,
			VideoResolution:   in.Estimate.VideoResolution,
			VideoSeconds:      in.Estimate.VideoSeconds,
			RefImages:         in.Estimate.RefImages,
			RefVideos:         in.Estimate.RefVideos,
			RefAudios:         in.Estimate.RefAudios,
			RefVideoSeconds:   in.Estimate.RefVideoSeconds,
			// 幂等键与记录同事务写入：唯一索引在这里拦下并发重复。
			IdemKey:   strings.TrimSpace(in.IdemKey),
			IdemScope: strings.TrimSpace(in.IdemScope),
		}
		if err := tx.Create(&usage).Error; err != nil {
			return err
		}
		sess.UsageID = usage.ID

		if in.Amount > 0 {
			return tx.Create(&model.Transaction{
				UserID: in.UserID, Type: model.TxnFreeze, Amount: -in.Amount,
				BalanceAfter: newBalance, FrozenAfter: newFrozen,
				RefType: model.RefTypeUsage, RefID: strconv.FormatUint(usage.ID, 10), Reason: "计费预扣",
			}).Error
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 事务已提交，自动提额落地成功：留痕（authevent 异步队列，不阻塞热路径）。
	// 必须在提交后——事务回滚时记事件等于伪造一条没发生的提额。
	if limitRaisedTo > 0 {
		authevent.Record(ctx, authevent.Event{
			UID: in.UserID, Event: authevent.EventKeyLimitRaised, Result: authevent.ResultSuccess,
			Detail: map[string]any{"key_id": in.KeyID, "new_limit": limitRaisedTo, "user_id": in.UserID},
		})
	}
	return sess, nil
}

// amountOf 是结算金额的**唯一**取值点：算得出就用公式，算不出就按冻结额入账。
//
// 三条路径（Settle / LateSettle / ReclaimSettle）都从这里取金额，差别只有两处：
//   - 入参 u 的来源：正常结算用上游实测用量，回收只能用 usages 行上的估算列
//     （usageOfRow，再按实际交付张数下调）；
//   - capFrozen：回收为 true（只少收，不得制造超过冻结额的新欠费），
//     正常结算为 false（Settle 自己做两级降额：3×冻结额 / 余额下限）。
//
// 铁律：结算期不能因为「算不出金额」而返回 error。返回 error 的后果是三重的：
//  1. `usages` 停在 pending → 同步请求的冻结**永不归还**；
//  2. 异步任务卡到 deadline → 全额退款，但产物已经交付（白送 + 白付上游）；
//  3. 回收 worker 每 5 分钟重试同一个必败运算，日志被刷满而钱一分没动。
//
// 所以公式类错误（快照损坏 / 价目缺失 / 档位不匹配 / 金额溢出）在这里被**吞掉**，
// 换成「按预扣冻结额入账 + charge_calc_failed 标记」；只有 DB/事务错误才由调用方上抛。
//
// allowZero 用于区分「0 元是算不出」与「0 元是事实」：只有调用方能证明
// 「确实一条都没交付」（per_clip 的 known-zero）时才传 true。不传的话，
// 回收期的 0 会被当成脏行回落到冻结额 —— 那正是 per_clip 免单规则失效的第二层原因。
//
// 为什么按冻结额而不是按 0（退款）：冻结额是用户下单时已经看到并同意的金额，
// 也是平台唯一可信的数字；按 0 入账等于对一次真实消耗的上游调用完全免费。
// 多收/少收的差额靠 anomaly 标记 + 对账人工复核，而不是靠「卡住不落账」。
func amountOf(raw model.JSON, u Usage, frozen int64, scope string, capFrozen, allowZero bool) (snap Snapshot, charge, uncapped int64, flags []string) {
	if err := json.Unmarshal(raw, &snap); err != nil {
		logCalcFallback(scope, err, frozen)
		return Snapshot{}, frozen, frozen, []string{model.AnomalyChargeCalcFailed}
	}
	charged, err := ChargeDetailed(snap, u)
	// 回收期的 0 元默认视为「算不出」：pending 行没有实测列，算出 0 只可能是脏行
	// （档位/秒数缺失被当成 0），按 0 入账等于白送一次真实的上游调用。
	// 例外是调用方明确声明「0 是事实」（allowZero，per_clip 确定零交付）：
	// 这时 0 是合法的应收额，必须免单，而不是回落成冻结额。
	// 正常结算的 0 元本来就是合法结果（免费模型 / 真的没用 token），不能兜底。
	if err != nil || (capFrozen && !allowZero && charged.Amount <= 0) {
		logCalcFallback(scope, err, frozen)
		return snap, frozen, frozen, []string{model.AnomalyChargeCalcFailed}
	}
	if capFrozen && charged.Amount > frozen {
		// 只压金额，不压「本来该收多少」这个事实：charge_before_cap 仍记真实应收。
		return snap, frozen, charged.Uncapped, charged.Flags
	}
	return snap, charged.Amount, charged.Uncapped, charged.Flags
}

// logCalcFallback 记录「按冻结额兜底入账」——这是必须被人看见的资金事件，一律 ERROR。
func logCalcFallback(scope string, err error, frozen int64) {
	logger.L().Error("结算公式失败，已按预扣冻结额入账（需对账复核）",
		zap.Bool("charge_calc_failed", true),
		zap.String("scope", scope),
		zap.Int64("frozen", frozen),
		zap.Error(err))
	metrics.Inc(metrics.NameTaskSettleFailures, "stage", scope+"_fallback")
}

// Settle 结算：按快照复核实际用量 → 计算 charge → 差额 UPDATE + usages 终态 + settle 流水。
// diff == 0 时不写差额流水。允许多扣至余额下限（默认 0，不透支）。
func (s *Service) Settle(ctx context.Context, sess *Session, in SettleInput) (SettleResult, error) {
	if sess == nil || sess.closed {
		return SettleResult{}, ErrSessionClosed
	}
	// 账务写操作必须与请求生命周期解耦：客户端断开后请求 ctx 已取消，
	// 若直接用于事务会导致结算失败、冻结永久滞留。
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	var negativeBalanceAfter, settledCharge, beforeCap int64
	var capReason, finalAnomaly string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var usage model.Usage
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&usage, sess.UsageID).Error; err != nil {
			return err
		}
		if usage.Status != model.UsageStatusPending {
			return ErrSessionClosed
		}
		// 上游交付了更高的档 → 先按交付档抬计费（只升不降），再走统一计费。
		// 必须在 amountOf 之前：金额一旦算完，补收就变成了第二次扣款。
		var preSnap Snapshot
		_ = json.Unmarshal(usage.PricingSnapshot, &preSnap)
		var upFlag string
		in.Usage, upFlag = UpgradeDeliveredTier(preSnap.SpecPrice, in.Usage, in.UpstreamSpec)

		snap, calcCharge, uncapped, calcFlags := amountOf(usage.PricingSnapshot, in.Usage, sess.Frozen, "settle", false, false)
		charge := calcCharge
		// before_cap 记录**两级降额之前**的应收额：否则"冻结额 3 倍截顶"在账单里不可见。
		beforeCap = uncapped
		// **必须并上 usage.Anomaly**：提交期已经写进这一列（appendUsageAnomaly 的
		// weak_param_pricing_conflict / submit_orphaned 等），结算时整列覆盖会把它抹掉。
		// 以前只有 ReclaimSettle / LateSettle 这样做，Settle 与 Release 是覆盖写 ——
		// 于是"为什么这单按兜底口径收"在账单上再也查不到。
		anomaly := joinFlagsLocal(append(append([]string{usage.Anomaly}, in.Anomaly, upFlag), calcFlags...))

		// 第一道降额：上限 = max(3×冻结额, 配置允许的最坏金额)。
		//
		// 只按 3×冻结额会削掉合法金额：ASR 冻结期不知道时长（冻结 0），
		// 图片上游多出图也天然超过冻结额。ceiling 是「这一单按价目最多能收多少」，
		// 只在模型显式配置了 max_seconds / max_n 时才有值——没有配置就退回 3×冻结，
		// 绝不用引擎上限（3600 秒 / 128 张）给上游谎报开口子。
		ceiling, ceilingReason := ChargeCeiling(snap, in.Usage)
		if capped, reason, overflow := chargeCapFor(sess.Frozen, ceiling); charge > capped {
			charge = capped
			capReason = reason
			anomaly = prependFlags(anomaly, model.AnomalyChargeCapped)
			if overflow {
				anomaly = prependFlags(anomaly, model.AnomalyCapOverflow)
			}
			// 指标：截顶是「少收」的直接来源，只有 ERROR 日志时没人能回答
			// 「这个月因截顶少收了多少」。带上 cap_reason 与计费口径，
			// 让「为什么被截」一眼可判（冻结估算偏低 vs 上游谎报 vs 公式错）。
			metrics.Inc(metrics.NameUsageAnomaly, "kind", model.AnomalyChargeCapped)
			logger.L().Error("结算金额被截顶",
				zap.Bool("charge_capped", true),
				zap.Uint64("usage_id", usage.ID),
				zap.Uint64("user_id", sess.UserID),
				zap.Int64("frozen", sess.Frozen),
				zap.Int64("cap", capped),
				zap.String("cap_reason", reason),
				zap.Int64("ceiling", ceiling),
				zap.String("ceiling_reason", ceilingReason),
				zap.String("billing_mode", snap.BillingMode),
				zap.String("usage_source", in.UsageSource),
				zap.Int64("prompt_tokens", in.Usage.PromptTokens),
				zap.Int64("completion_tokens", in.Usage.CompletionTokens),
				zap.Int64("charge_before_cap", uncapped))
		}

		// 兜底入账必须可判：`charge == frozen` 且 cap_reason 为空时，账单上完全
		// 分不清「公式算不出、按冻结额兜底」与「本来就该收这么多」。
		if capReason == "" && model.HasAnomaly(anomaly, model.AnomalyChargeCalcFailed) {
			capReason = "calc_failed"
			metrics.Inc(metrics.NameUsageOverCharge, "reason", "calc_failed")
		}

		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, sess.UserID).Error; err != nil {
			return err
		}

		// 第二道降额：余额下限（默认 0 = 不透支）。
		diff := charge - sess.Frozen
		newBalance := u.Balance - diff
		// 只在**加扣**（diff > 0）时降额。diff < 0 是退款，它只会把余额往上抬，
		// 永远不可能击穿下限；若不判断，用户在透支线下时（人工改库／调低 LD_NEGATIVE_LIMIT）
		// allowedDiff 会被钳成 0，charge 反而被抬回冻结额，退款被静默吞成全额扣费。
		if limit := negativeLimit(); diff > 0 && newBalance < -limit {
			allowedDiff := u.Balance + limit
			if allowedDiff < 0 {
				allowedDiff = 0
			}
			charge = sess.Frozen + allowedDiff
			if charge < 0 {
				charge = 0
			}
			diff = charge - sess.Frozen
			newBalance = u.Balance - diff
			anomaly = prependFlags(anomaly, model.AnomalyNegativeBalanceBlocked)
			// 两个降额原因都要留：只留 balance 会让人以为截顶没生效。
			if capReason == "" {
				capReason = "balance"
			} else {
				capReason += ",balance"
			}
			logger.L().Error("结算会击穿余额下限，已按上限扣减",
				zap.Bool("negative_balance_blocked", true),
				zap.Uint64("usage_id", usage.ID),
				zap.Uint64("user_id", sess.UserID),
				zap.Int64("balance", u.Balance),
				zap.Int64("limit", limit))
		}
		// 退款不得超出用户**实际**冻结额（与 Release 同一条规则，修"凭空造钱"）。
		//
		// 会话记的冻结额是权威值，它是"应该退多少"；`users.frozen` 是"实际冻了多少"。
		// 漂移时（人工改库/历史脏数据）只看前者就会把从没冻过的钱退给用户。
		// 这里必须同时反映到 newBalance 与 newFrozen 上，两条路径的账才对得上。
		if refundWant := sess.Frozen - charge; refundWant > u.Frozen {
			frozenDriftWarn(sess.UserID, sess.KeyID, usage.ID, u.Frozen, refundWant)
			anomaly = prependFlags(anomaly, model.AnomalyFrozenDrift)
			diff = charge - u.Frozen
			newBalance = u.Balance - diff
		}
		settledCharge = charge
		newFrozen := u.Frozen - sess.Frozen
		if newFrozen < 0 {
			// 账务漂移（人工改库/历史脏数据）：钳 0 保证不出现负冻结，但必须留痕。
			frozenDriftWarn(sess.UserID, sess.KeyID, usage.ID, u.Frozen, sess.Frozen)
			newFrozen = 0
			anomaly = prependFlags(anomaly, model.AnomalyFrozenDrift)
		}
		if newBalance < 0 {
			negativeBalanceAfter = newBalance
		}
		if err := tx.Model(&model.User{}).Where("id = ?", sess.UserID).Updates(map[string]any{
			"balance":       newBalance,
			"frozen":        newFrozen,
			"used":          u.Used + charge,
			"request_count": u.RequestCount + 1,
		}).Error; err != nil {
			return err
		}
		if err := keyUsageDelta(tx, sess.KeyID, diff); err != nil {
			return err
		}

		// 耗时与档位：总耗时用「入口时刻 → 现在」；上游耗时由调用方传入。
		finishedAt := time.Now()
		startedAt := usage.CreatedAt
		if usage.StartedAt != nil {
			startedAt = *usage.StartedAt
		}
		tierUnit, tierSource := TierInfo(snap, in.Usage)

		// 上游实际尺寸与计费档位不一致 → 只留痕、不改价。
		//
		// 金额本身完全正常（按请求档位收），所以任何金额校验都发现不了
		// 「按 4K 收、按 2K 交」；这条标记是对账唯一的入口。
		//
		// 只在用户**显式指定**尺寸时比对：没传 size / auto 的请求由上游自己挑档，
		// 拿它去比「上游实际档 vs 平台默认档」会让每一条 auto 请求都变假告警。
		upstreamSpec, specFlags := normalizeUpstreamSpec(in.UpstreamSpec)
		if len(specFlags) > 0 {
			anomaly = prependFlags(anomaly, specFlags...)
		}
		if upstreamSpec != "" && in.Usage.ImageSpecExplicit && in.Usage.ImageSpec != "" {
			if tier, ok := upstreamTier(snap.SpecPrice, upstreamSpec); ok && tier != in.Usage.ImageSpec {
				anomaly = prependFlags(anomaly, model.AnomalyTierMismatch)
			}
			if want := strings.TrimSpace(in.Usage.ImageRatio); want != "" {
				if got := upstreamRatio(upstreamSpec); got != "" && !ratioClose(got, want) {
					anomaly = prependFlags(anomaly, model.AnomalyImageRatioMismatch)
				}
			}
		}

		updates := map[string]any{
			"status": model.UsageStatusSettled, "http_status": in.StatusCode,
			"channel_id": in.ChannelID, "adapter_id": in.AdapterID,
			"prompt_tokens": in.Usage.PromptTokens, "completion_tokens": in.Usage.CompletionTokens,
			// 计费值与实测值同源：正常结算拿到的就是上游实测。
			"bill_prompt_tokens": in.Usage.PromptTokens, "bill_completion_tokens": in.Usage.CompletionTokens,
			"cache_tokens": in.Usage.CacheTokens, "cache_write5m_tokens": in.Usage.CacheWrite5mTokens,
			"cache_write1h_tokens": in.Usage.CacheWrite1hTokens, "image_tokens": in.Usage.ImageTokens,
			"audio_input_tokens": in.Usage.AudioInputTokens, "audio_output_tokens": in.Usage.AudioOutputTokens,
			"image_spec": textx.TruncateRunes(in.Usage.ImageSpec, 32), "image_ratio": textx.TruncateRunes(in.Usage.ImageRatio, 16),
			"image_quality": in.Usage.ImageQuality, "image_n": in.Usage.ImageN,
			"image_spec_source":   textx.TruncateRunes(in.Usage.ImageSpecSource, 16),
			"upstream_spec":       upstreamSpec,
			"audio_duration_secs": in.Usage.AudioDurationSecs, "audio_chars": in.Usage.AudioChars,
			"rerank_docs": in.Usage.RerankDocs,
			"cost":        in.Cost, "charge": charge,
			"error_msg":           textx.TruncateRunes(in.ErrorMsg, 500),
			"upstream_request_id": in.UpstreamRequestID, "request_digest": in.RequestDigest,
			"usage_source": normalizeSource(in.UsageSource),
			"raw_usage":    textx.TruncateRunes(in.RawUsage, rawUsageMaxLen),
			// 必须用本地 anomaly（含截顶/透支/漂移标记与 ChargeDetailed 的口径标记），
			// 不能用 in.Anomaly —— 否则降额标记全部丢失，事后只能看到"少扣了但不知道为什么"。
			"anomaly": textx.TruncateRunes(anomaly, anomalyMaxLen),

			"video_resolution":     in.Usage.VideoResolution,
			"video_seconds":        nonNeg(in.Usage.VideoSeconds),
			"video_seconds_source": textx.TruncateRunes(in.VideoSecondsSource, 16),
			"ref_images":           nonNeg(in.Usage.RefImages),
			"ref_videos":           nonNeg(in.Usage.RefVideos),
			"ref_audios":           nonNeg(in.Usage.RefAudios),
			"ref_video_seconds":    nonNeg(in.Usage.RefVideoSeconds),
			"usage_semantic":       normalizeSemantic(in.Usage.Semantic),
			"tier_price":           tierUnit,
			"tier_source":          tierSource,
			"charge_before_cap":    beforeCap,
			"cap_reason":           textx.TruncateRunes(capReason, 32),
			"frozen":               sess.Frozen,
			"finished_at":          finishedAt,
			"latency_ms":           spanMs(startedAt, finishedAt),
			"upstream_ms":          nonNegInt(in.UpstreamMs),
			"first_token_ms":       nonNegInt(in.FirstTokenMs),
			"attempts":             nonNegInt(in.Attempts),
		}
		// 只有真的知道走了哪把 key 才写：迟到补结/回收重算拿不到当次 key，
		// 写空串会把首次结算记下的归属抹掉。
		if id := strings.TrimSpace(in.ChannelKeyID); id != "" {
			updates["channel_key_id"] = id
		}
		if k := normalizeCallKind(in.CallKind); k != "" {
			updates["call_kind"] = k
		}
		// task_id 只在调用方明确给出时回填：同步调用传 0，
		// 若无条件写 0 会把任务提交时已绑定的 task_id 清掉。
		if in.TaskID > 0 {
			updates["task_id"] = in.TaskID
		}
		if err := tx.Model(&model.Usage{}).Where("id = ?", usage.ID).Updates(updates).Error; err != nil {
			return err
		}
		finalAnomaly = anomaly

		if diff != 0 {
			return tx.Create(&model.Transaction{
				UserID: sess.UserID, Type: model.TxnSettle, Amount: -diff,
				BalanceAfter: newBalance, FrozenAfter: newFrozen,
				RefType: model.RefTypeUsage, RefID: strconv.FormatUint(usage.ID, 10), Reason: "计费结算",
			}).Error
		}
		return nil
	})
	if err != nil {
		// 唯一流水索引兜底：并发/重复结算时第二次会命中 23505。
		// 仅当该 usage 已进入终态才视为幂等成功，其它冲突照常上抛。
		if pgerr.IsUnique(err) && s.usageTerminal(ctx, sess.UsageID) {
			sess.closed = true
			return s.resultOf(ctx, sess), nil
		}
		return SettleResult{}, err
	}
	if negativeBalanceAfter < 0 {
		quotaNegativeWarn(sess.UserID, sess.KeyID, sess.UsageID, settledCharge, sess.Frozen, negativeBalanceAfter)
	}
	sess.closed = true
	return SettleResult{
		Charge:    settledCharge,
		BeforeCap: beforeCap,
		CapReason: capReason,
		Frozen:    sess.Frozen,
		Diff:      settledCharge - sess.Frozen,
		Anomaly:   finalAnomaly,
	}, nil
}

// resultOf 从已终态的 usages 行读回结算结果（重复结算的幂等返回路径）。
// 读失败时只回冻结额：调用方拿到 0 会把展示字段写坏。
func (s *Service) resultOf(ctx context.Context, sess *Session) SettleResult {
	out := SettleResult{Frozen: sess.Frozen}
	var row struct {
		Charge          int64
		ChargeBeforeCap int64
		CapReason       string
		Anomaly         string
	}
	// 显式列名会随列重命名漂移（旧实现硬编码 charge 等 4 列，改名后静默读到 0）。
	if err := s.db.WithContext(ctx).Model(&model.Usage{}).
		Select("charge, charge_before_cap, cap_reason, anomaly").
		Where("id = ?", sess.UsageID).Scan(&row).Error; err != nil {
		out.Charge = sess.Frozen
		return out
	}
	out.Charge, out.BeforeCap = row.Charge, row.ChargeBeforeCap
	out.CapReason, out.Anomaly = row.CapReason, row.Anomaly
	out.Diff = out.Charge - sess.Frozen
	return out
}

// settleDBTimeout 是账务写操作的独立超时（与请求生命周期解耦后仍需上限，避免锁等待无限挂起）。
func settleDBTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LD_SETTLE_DB_TIMEOUT"))
	if raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 && n <= 60 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Second
}

// dbCtx 把任意上下文转换为「不受取消影响 + 有超时」的账务上下文。
// 用途：客户端断开（流式中断/超时）时，结算与释放仍必须落库。
func dbCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleDBTimeout())
}

// normalizeSource 归一计费来源；空值保持空串（对外映射 unknown）。
func normalizeSource(s string) string {
	switch s {
	case "upstream", "mixed", "estimated", "none":
		return s
	default:
		return ""
	}
}

// rawUsageMaxLen 是 usages.raw_usage 的**业务**上限（列已是 text）。
//
// 4000 而不是无限：上游 usage 是给人复核的，正常 usage 对象远小于此；
// 超过 4000 只可能是上游把整段报文塞进了 usage 字段。
const rawUsageMaxLen = 4000

// 调用分类（落 usages.call_kind）。
const (
	CallKindSync     = "sync"
	CallKindAsync    = "async"
	CallKindPlugin   = "plugin"
	CallKindPrecheck = "precheck"
)

// normalizeCallKind 归一调用分类；未知值回落 sync。
func normalizeCallKind(s string) string {
	switch s {
	case CallKindSync, CallKindAsync, CallKindPlugin, CallKindPrecheck:
		return s
	default:
		return CallKindSync
	}
}

// normalizeSemantic 归一 token 语义（空值按 unknown 落库，而不是 openai ——
// 存 unknown 才能区分「确实是 OpenAI 语义」与「当时没识别出来」）。
func normalizeSemantic(s Semantic) string {
	switch s {
	case SemanticOpenAI:
		return string(SemanticOpenAI)
	case SemanticAnthropic:
		return string(SemanticAnthropic)
	default:
		return string(SemanticUnknown)
	}
}

// spanMs 返回两个时刻之间的毫秒数（负值钳 0：系统时钟回拨时不能让耗时变负）。
func spanMs(start, end time.Time) int {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return int(end.Sub(start).Milliseconds())
}

// nonNegInt 把负数归零（耗时/次数列不允许负值）。
func nonNegInt(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// BindTask 把已存在的 usages 行绑定到异步任务（提交成功后调用）。
//
// 为什么要单独一步而不是在 Freeze 时传：任务行是在 Freeze **之后**才创建的主键。
func (s *Service) BindTask(ctx context.Context, usageID, taskID uint64) error {
	if usageID == 0 || taskID == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Model(&model.Usage{}).Where("id = ?", usageID).
		Updates(map[string]any{"task_id": taskID, "call_kind": CallKindAsync}).Error
}

// 唯一索引冲突（transactions 的 usage 级部分唯一索引）判据见 pkg/pgerr。

// usageTerminal 判断 usage 是否已进入终态（用于重复结算的幂等判定）。
func (s *Service) usageTerminal(ctx context.Context, usageID uint64) bool {
	var status string
	if err := s.db.WithContext(ctx).Model(&model.Usage{}).
		Select("status").Where("id = ?", usageID).Scan(&status).Error; err != nil {
		return false
	}
	return status == model.UsageStatusSettled || status == model.UsageStatusReleased
}

// frozenDriftWarn 输出「冻结额扣减为负」的结构化告警（钳 0 后仍须可追溯）。
var frozenDriftWarn = func(userID, keyID, usageID uint64, frozenBefore, deduct int64) {
	logger.L().Warn("冻结额出现负值，已钳 0（账务漂移）",
		zap.Bool("frozen_drift", true),
		zap.Uint64("user_id", userID),
		zap.Uint64("key_id", keyID),
		zap.Uint64("usage_id", usageID),
		zap.Int64("frozen_before", frozenBefore),
		zap.Int64("deduct", deduct),
	)
}

// keyDriftWarn 输出「KEY 已用额回退为负」的结构化告警（钳 0 后仍须可追溯）。
var keyDriftWarn = func(userID, keyID, usageID uint64, usedBefore, deduct int64) {
	logger.L().Warn("KEY 已用额回退出现负值，已钳 0（账务漂移）",
		zap.Bool("key_used_drift", true),
		zap.Uint64("user_id", userID),
		zap.Uint64("key_id", keyID),
		zap.Uint64("usage_id", usageID),
		zap.Int64("used_before", usedBefore),
		zap.Int64("deduct", deduct),
	)
}

// quotaNegativeWarn 输出「结算后余额为负」的结构化告警（允许负额时须强制告警）。
var quotaNegativeWarn = func(userID, keyID, usageID uint64, settled, frozen, balanceAfter int64) {
	logger.L().Warn("结算后余额为负",
		zap.Bool("balance_negative", true),
		zap.Uint64("user_id", userID),
		zap.Uint64("key_id", keyID),
		zap.Uint64("usage_id", usageID),
		zap.Int64("settled", settled),
		zap.Int64("frozen", frozen),
		zap.Int64("balance_after", balanceAfter),
	)
}

// keyUsageDelta 是 api_keys.used 的**唯一**变更入口。
//
// 历史缺陷：同一条"补差"语义在 5 处各写一遍，符号还互相矛盾
// （Settle 是 +diff、Reclaim 是 -diff、LateSettle 完全不动），
// 于是 used 会变成负数，而负 used 让 `limit - used < amount` 恒成立
// → 该 KEY 永久无法冻结，限额被系统性绕过。
//
// 现在统一为「加法 + 钳 0 + 溢出检查」。加法在 Go 里做（要拿到钳位前的值才能
// 区分"正常减到 0"与"被钳到 0"），读的那步带 `FOR UPDATE`，因此仍是事务内的
// 串行读-改-写，不存在丢失更新。
func keyUsageDelta(tx *gorm.DB, keyID uint64, delta int64) error {
	if keyID == 0 || delta == 0 {
		return nil
	}
	var before int64
	if err := tx.Raw(`SELECT used FROM api_keys WHERE id = ? FOR UPDATE`, keyID).
		Scan(&before).Error; err != nil {
		return err
	}
	raw, err := money.Sum(before, delta)
	if err != nil {
		return ErrAmountTooLarge
	}
	after := raw
	if after < 0 {
		after = 0
	}
	if after > money.MaxAmount {
		return ErrAmountTooLarge
	}
	if err := tx.Exec(`UPDATE api_keys SET used = ? WHERE id = ?`, after, keyID).Error; err != nil {
		return err
	}
	if raw < 0 {
		// 钳位发生过：used 本来就不够扣（账务漂移），留痕便于对账。
		//
		// 判据必须是"钳位前的值 < 0"而不是"钳位后的值 == 0"：一次正常调用
		// （冻结 +F、释放 −F）之后 used 必然回到 0，用后者的写法会让**每一次
		// 正常释放**都打一条漂移告警，真正的漂移反而被淹没在噪声里。
		// usedBefore 必须是钳位前的真实值、deduct 必须是本次增量：
		// 曾经两个参数都传 -raw，日志里会显示「used_before=-5, deduct=-5」，
		// 排障时会读成"多扣了 5"，而这是漂移的唯一线索。
		keyDriftWarn(0, keyID, 0, before, delta)
	}
	return nil
}

// Release 释放（失败/取消/超时/路由空转）：冻结全额回退、charge=0、终态 released + release 流水。
func (s *Service) Release(ctx context.Context, sess *Session, in SettleInput) error {
	if sess == nil || sess.closed {
		return ErrSessionClosed
	}
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var usage model.Usage
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&usage, sess.UsageID).Error; err != nil {
			return err
		}
		if usage.Status != model.UsageStatusPending {
			return ErrSessionClosed
		}

		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, sess.UserID).Error; err != nil {
			return err
		}
		// 同 Settle：提交期写入的标记不能被释放动作清空（失败/取消时 in.Anomaly 常为空串）。
		anomaly := joinFlagsLocal([]string{usage.Anomaly, in.Anomaly})
		newBalance, newFrozen := u.Balance, u.Frozen
		refund := sess.Frozen
		if refund > 0 {
			// 漂移防护（修"凭空造钱"）：实际冻结不足以回补时，只按实际值回补，
			// 并把 usage.frozen 改写成实际值，让对账能收敛。
			if u.Frozen < refund {
				frozenDriftWarn(sess.UserID, sess.KeyID, usage.ID, u.Frozen, refund)
				anomaly = prependFlags(anomaly, model.AnomalyFrozenDrift)
				refund = u.Frozen
			}
			newBalance = u.Balance + refund
			newFrozen = u.Frozen - refund
			if err := tx.Model(&model.User{}).Where("id = ?", sess.UserID).
				Updates(map[string]any{"balance": newBalance, "frozen": newFrozen}).Error; err != nil {
				return err
			}
			// KEY 侧必须按**实际退款额**回退，不能按名义冻结额：
			// 漂移时退款已被钳到 u.Frozen，而 KEY 若按 sess.Frozen 多退，
			// 差额会让该 KEY 的 used 偏小 —— 限额被永久放宽，用户白拿额度。
			if err := keyUsageDelta(tx, sess.KeyID, -refund); err != nil {
				return err
			}
		}

		finishedAt := time.Now()
		startedAt := usage.CreatedAt
		if usage.StartedAt != nil {
			startedAt = *usage.StartedAt
		}
		updates := map[string]any{
			"status": model.UsageStatusReleased, "http_status": in.StatusCode,
			"channel_id": in.ChannelID, "adapter_id": in.AdapterID,
			"charge": 0, "cost": 0,
			"error_msg":    textx.TruncateRunes(in.ErrorMsg, 500),
			"usage_source": normalizeSource(in.UsageSource),
			"anomaly":      textx.TruncateRunes(anomaly, anomalyMaxLen),
			// 失败/取消的行同样是"有终态的实际调用"：没有耗时的话，
			// 排障时无法回答「这个 502 是秒回还是跑了 60 秒才超时」。
			"finished_at":    finishedAt,
			"latency_ms":     spanMs(startedAt, finishedAt),
			"upstream_ms":    nonNegInt(in.UpstreamMs),
			"first_token_ms": nonNegInt(in.FirstTokenMs),
			"attempts":       nonNegInt(in.Attempts),
			"frozen":         refund,
			// 释放必须**交还幂等键**：这次调用一分钱没扣（charge=0），
			// 用户修好参数后用同一个 Key 重试是合法的第二次尝试，不是重复提交。
			//
			// 不交还的后果是实测过的：带 Idempotency-Key 发一次 `size=99999x99999`
			// 被上游 400、全额退款，随后改对 size 用同一个键重试，得到 409
			// 「该键已用于上一次相同请求」—— 一个从未产生任何费用的失败请求，
			// 把客户端永久钉在"必须换键"上，而换键就会失去幂等保护。
			//
			// 键只在**真的收过钱**（settled）时占位：那种情况下重试确实会二次扣费，
			// 必须拦。部分交付（reclaim 结算）同理保留键。
			"idem_key":   "",
			"idem_scope": "",
		}
		// 失败行同样要记"哪把 key 失败了"：只记成功的那些等于排障时少一半线索。
		if id := strings.TrimSpace(in.ChannelKeyID); id != "" {
			updates["channel_key_id"] = id
		}
		if k := normalizeCallKind(in.CallKind); k != "" {
			updates["call_kind"] = k
		}
		if in.TaskID > 0 {
			updates["task_id"] = in.TaskID
		}
		// 释放路径同样要落上游单号：这是「上游已经出图、平台却没能交付」的唯一凭据。
		// 仅非空才写，避免把 pending 行上已有的单号误清空。
		if up := strings.TrimSpace(in.UpstreamRequestID); up != "" {
			updates["upstream_request_id"] = textx.TruncateRunes(up, 128)
		}
		if err := tx.Model(&model.Usage{}).Where("id = ?", usage.ID).Updates(updates).Error; err != nil {
			return err
		}

		if refund > 0 {
			return tx.Create(&model.Transaction{
				UserID: sess.UserID, Type: model.TxnRelease, Amount: refund,
				BalanceAfter: newBalance, FrozenAfter: newFrozen,
				RefType: model.RefTypeUsage, RefID: strconv.FormatUint(usage.ID, 10), Reason: "预扣释放",
			}).Error
		}
		return nil
	})
	if err != nil {
		if pgerr.IsUnique(err) && s.usageTerminal(ctx, sess.UsageID) {
			sess.closed = true
			return nil
		}
		return err
	}
	sess.closed = true
	return nil
}

// ReleaseByUsageID 是「按 usage_id 释放」的唯一出口。
//
// 语义约定（调用方不需要再写错误分支）：
//   - released=true          —— 本次真的退了一笔冻结；
//   - released=false, err=nil —— 这笔早已终态/行不存在（幂等，未动任何账）；
//   - err != nil             —— 只有 DB/事务错误。
//
// 为什么不让调用方用自己手里的 Session：Session 是内存对象，可能在调用前
// 已被别的路径（结算/回收/另一个 goroutine）关闭；而冻结额的正确来源是
// freeze 流水（Reopen → frozenOf），用它才能保证"退的是账上真实冻结的那笔"。
//
// 这同时把历史缺陷收敛掉：软删用户（Settle/Release 默认作用域查不到）在
// Reopen 之后仍然走同一条路径，不会再出现"永远 pending"。
func (s *Service) ReleaseByUsageID(ctx context.Context, usageID uint64, in SettleInput) (bool, error) {
	if usageID == 0 {
		return false, nil
	}
	sess, err := s.Reopen(ctx, usageID)
	if err != nil {
		if errors.Is(err, ErrSessionClosed) || errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := s.Release(ctx, sess, in); err != nil {
		if errors.Is(err, ErrSessionClosed) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReleasePendingByTask 释放某个任务名下所有仍 pending 的计费会话（keepUsageID 除外）。
//
// 唯一调用方是 task.Retry：重试会新建一个会话并把 tasks.usage_id 指过去，
// 而旧会话如果因为"失败路径释放没跑成"仍是 pending，用户就被冻结了两份钱。
//
// 失败策略交给调用方：返回 err 时调用方必须中止重试（此时还没冻结，钱没动）。
// 已释放的保持已释放 —— 重试可重入。
func (s *Service) ReleasePendingByTask(ctx context.Context, taskID, keepUsageID uint64, in SettleInput) (int, error) {
	if taskID == 0 {
		return 0, nil
	}
	q := s.db.WithContext(ctx).Model(&model.Usage{}).
		Where("task_id = ? AND status = ?", taskID, model.UsageStatusPending)
	if keepUsageID > 0 {
		q = q.Where("id <> ?", keepUsageID)
	}
	var ids []uint64
	if err := q.Order("id").Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	released := 0
	for _, id := range ids {
		ok, err := s.ReleaseByUsageID(ctx, id, in)
		if err != nil {
			return released, err
		}
		if ok {
			released++
		}
	}
	return released, nil
}

// Reopen 按 usage_id 重建计费会话（异步任务：Worker 在任意时刻恢复结算/释放）。
//
// Session 是内存对象不可跨时间持有，故冻结额从 usages 关联的 freeze 流水读回（唯一真相源）。
// 行已终态返回 ErrSessionClosed —— 已结算/已释放的任务不再重复动账。
func (s *Service) Reopen(ctx context.Context, usageID uint64) (*Session, error) {
	if usageID == 0 {
		return nil, ErrSessionClosed
	}
	// 账务读不该被客户端取消带走：请求断开会留下「冻结了但读不到冻结额」的错觉。
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	var usage model.Usage
	if err := s.db.WithContext(ctx).First(&usage, usageID).Error; err != nil {
		return nil, err
	}
	if usage.Status != model.UsageStatusPending {
		return nil, ErrSessionClosed
	}
	frozen, err := s.frozenOf(s.db.WithContext(ctx), usageID)
	if err != nil {
		return nil, err
	}
	return &Session{UsageID: usage.ID, UserID: usage.UserID, KeyID: usage.KeyID, Frozen: frozen}, nil
}

// UsageState 是按 usage_id 查到的账务现状（**只读视图**，W1 迟到成功的四态分支用）。
//
// 刻意不带 Frozen：冻结额的唯一真相源是从 freeze 流水反推（frozenOf）。
// 把 usages.frozen 也塞进来，调用方迟早会有一处直接读它，
// 于是同一笔钱有了两个来源 —— 而这一列在 Release 之后是历史值，不是当前值。
type UsageState struct {
	Status string
	Charge int64 // Status=settled 时的实际入账额
	UserID uint64
	KeyID  uint64
}

// UsageStateOf 查一次账务现状。行已归档（物理删除）→ ErrUsageGone。
//
// 为什么不能复用 Reopen：Reopen 只把「非 pending」折成 ErrSessionClosed，
// 调用方于是分不清「已释放（该补收）」「已结算（不该重复扣）」「行没了（不能扣）」。
// 迟到成功恰好需要把这三者区分开 —— 合成一个错误就会写成一刀切的错账。
func (s *Service) UsageStateOf(ctx context.Context, usageID uint64) (UsageState, error) {
	if usageID == 0 {
		return UsageState{}, ErrUsageGone
	}
	var usage model.Usage
	err := s.db.WithContext(ctx).
		Select("id", "status", "charge", "user_id", "key_id").
		First(&usage, usageID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return UsageState{}, ErrUsageGone
	}
	if err != nil {
		return UsageState{}, err
	}
	return UsageState{
		Status: usage.Status, Charge: usage.Charge,
		UserID: usage.UserID, KeyID: usage.KeyID,
	}, nil
}

// ReclaimSettle 回收卡在 pending 的会话（结算失败/进程崩溃/客户端断流后未落账）。
//
// 口径（资金正确性关键）：
//   - 按次：预扣额 F 即精确金额（Estimate==Charge），C=F；
//   - 按规格：用 usages 行上的归一列重算，再按实际交付张数下调（只降不升）；
//   - 按量：按估算 token 计费，且 C 不得超过 F（无上游证据时不制造新欠费）。
//
// 资金动作：终态 settled、charge=C、anomaly 追加 reclaimed；退还差额 F−C（一条 release 流水），
// 等式 余额 + (F−C) = 原余额 − C；KEY 已用额同步回退 F−C。
// ReclaimSettle 按估算用量回收结算（上限=冻结额，只少收不多收）。
//
// extraAnomaly 是回收**原因**的标记（孤账 / 成功后补结），与固定的 reclaimed、
// completion_unmeasured 一起落库：只写 reclaimed 的话，事后无法区分
// 「任务成功但结算失败」和「任务行都不见了」——这两种处置完全不同。
//
// actualImageN 是**实际成功落地的产物数**（tasks.artifact_count），只用来**下调**
// 计费数量。类型是有符号的，因为它同时承载「事实」与「没有事实」：
//   - `>= 0`：确实交付了这么多（0 = 确定零交付 → per_clip 免单、per_image 保持意图）；
//   - `< 0`（约定传 -1）：回收期拿不到交付证据（孤儿单 / 清单缺失）→ 保持意图，
//     按冻结额入账，与改造前逐字一致。这是回滚开关。
//
// 曾经用 0 表示「未知」，与 per_clip 的「0 条 = 免单」直接冲突：同一个 0 在
// 结算路径免单、在回收路径按 max_n 收费，用户可举证。
func (s *Service) ReclaimSettle(ctx context.Context, usageID uint64, extraAnomaly string, actualImageN int64) (int64, error) {
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	var charge int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var usage model.Usage
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&usage, usageID).Error; err != nil {
			return err
		}
		if usage.Status != model.UsageStatusPending {
			return ErrSessionClosed
		}
		frozen, ferr := s.frozenOf(tx, usageID)
		if ferr != nil {
			// 事务回滚 → usage 保持 pending → 下一轮回收重试。
			// 绝不允许折成 0 继续算：那会把整单当免费，且不留任何异常标记。
			return ferr
		}
		// 公式失败一律按冻结额入账（见 amountOf 的注释），绝不 return err ——
		// 否则这条 usage 永远 pending，回收 worker 每 5 分钟重试同一个必败运算。
		calcUsage := usageOfRow(&usage)
		// 条数口径与结算同源（唯一实现见 BillableCount）。入参是一个**有符号**的事实：
		//   >=0 = 确实交付了这么多；<0 = 拿不到交付证据（孤儿单/清单缺失）。
		// 为什么不能沿用「0 = 未知」：per_clip 的既定语义是「0 条 = 免单」，
		// 同一个 0 若还表示未知，回收路径就会在「上游明确没给音频」时按 max_n 收 2 条。
		var preSnap Snapshot
		_ = json.Unmarshal(usage.PricingSnapshot, &preSnap)
		calcUsage.ImageN = BillableCount(preSnap.SpecPrice, calcUsage.ImageN, actualImageN, actualImageN >= 0)
		// allowZero 只在「0 是事实」时开：per_clip 确定零交付必须真的免单，
		// 而不是被当成脏行回落到冻结额（那是同一个 0 承载两种含义的老问题）。
		snap, calcCharge, calcUncapped, calcFlags := amountOf(usage.PricingSnapshot, calcUsage, frozen, "reclaim", true, actualImageN == 0)
		charge = calcCharge
		uncapped := calcUncapped

		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, usage.UserID).Error; err != nil {
			return err
		}

		anomaly := appendFlag(usage.Anomaly, model.AnomalyReclaimed, model.AnomalyCompletionUnmeasured)
		if extraAnomaly != "" {
			anomaly = appendFlag(anomaly, extraAnomaly)
		}
		for _, f := range calcFlags {
			anomaly = prependFlags(anomaly, f)
		}
		// 漂移时以「用户实际被冻结的额度」为准，三条口径必须来自同一个数：
		// 余额退还 refund、users.frozen 减 actualFrozen、KEY 增量 charge-actualFrozen。
		// 以前退款钳到 u.Frozen 而 frozen 列与 KEY 仍按名义值 ——
		// 差额既没退给用户也没落任何科目（钱蒸发），同时该 KEY 的限额被永久放宽。
		actualFrozen := frozen
		if actualFrozen > u.Frozen {
			frozenDriftWarn(usage.UserID, usage.KeyID, usageID, u.Frozen, frozen)
			anomaly = prependFlags(anomaly, model.AnomalyFrozenDrift)
			actualFrozen = u.Frozen
		}
		// 可收额同样不能超过实际冻结：否则回收会把用户没冻到的部分也扣走。
		if calcCharge > actualFrozen {
			calcCharge = actualFrozen
			charge = actualFrozen
			uncapped = actualFrozen
		}
		refund := actualFrozen - calcCharge
		newBalance := u.Balance + refund
		newFrozen := u.Frozen - actualFrozen
		if err := tx.Model(&model.User{}).Where("id = ?", usage.UserID).Updates(map[string]any{
			"balance": newBalance, "frozen": newFrozen,
			"used": u.Used + charge, "request_count": u.RequestCount + 1,
		}).Error; err != nil {
			return err
		}
		// KEY 已用额回到实际消费额：delta = charge − actualFrozen（与退款口径同一个数）。
		if err := keyUsageDelta(tx, usage.KeyID, charge-actualFrozen); err != nil {
			return err
		}

		source := "estimated"
		if snap.BillingMode != model.BillingModeTokens {
			source = "none"
		}
		// 回收场景的耗时是**下界**：会话卡在 pending 直到被回收任务捞起来，
		// 真实结束时刻不可知。落 now() 并打 reclaimed 标记，前端据此显示「≥ 10m」。
		reclaimAt := time.Now()
		startedAt := usage.CreatedAt
		if usage.StartedAt != nil {
			startedAt = *usage.StartedAt
		}
		// 只写 bill_*，**绝不写实测列**：prompt_tokens/completion_tokens 是上游实测口径，
		// stats 的 usage_daily 直接 SUM 它们。把估算值写进去会让当日 token 统计
		// 被放大一个数量级，而且 rollup 一旦跑过就不可逆。
		billPrompt := int64(0)
		if snap.BillingMode == model.BillingModeTokens {
			billPrompt = nonNeg(usage.EstPromptTokens)
		}
		if err := tx.Model(&model.Usage{}).Where("id = ?", usage.ID).Updates(map[string]any{
			"status": model.UsageStatusSettled, "charge": charge,
			"charge_before_cap":  uncapped,
			"bill_prompt_tokens": billPrompt, "bill_completion_tokens": int64(0),
			"usage_source": source,
			"anomaly":      textx.TruncateRunes(anomaly, anomalyMaxLen),
			"error_msg":    textx.TruncateRunes("reclaimed: 会话未能正常结算，按估算补收并释放差额", 500),
			"finished_at":  reclaimAt,
			"latency_ms":   spanMs(startedAt, reclaimAt),
		}).Error; err != nil {
			return err
		}
		if refund != 0 {
			return tx.Create(&model.Transaction{
				UserID: usage.UserID, Type: model.TxnRelease, Amount: refund,
				BalanceAfter: newBalance, FrozenAfter: newFrozen,
				RefType: model.RefTypeUsage, RefID: strconv.FormatUint(usage.ID, 10), Reason: "回收释放差额",
			}).Error
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return charge, nil
}

// frozenOf 从 freeze 流水读回冻结额（唯一真相源）。
//
// 只有「根本没有 freeze 流水」（免费模型 / 0 元冻结）才返回 (0, nil)；
// 其余错误**必须上抛**。折成 0 是最坏的安静失败：调用方会把整单当成免费，
// 既不出 anomaly（算的是 0 不是错误），也没有任何日志可追溯。
func (s *Service) frozenOf(tx *gorm.DB, usageID uint64) (int64, error) {
	var txn model.Transaction
	err := tx.Where("ref_type = ? AND ref_id = ? AND type = ?", model.RefTypeUsage,
		strconv.FormatUint(usageID, 10), model.TxnFreeze).Order("id ASC").First(&txn).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if txn.Amount >= 0 {
		return 0, nil
	}
	return -txn.Amount, nil
}

// LateSettle 对「已被释放的会话」补收：异步任务超时/取消后上游迟到成功，
// 冻结已退回给用户，但平台已真实消耗上游成本。这里按快照重算 charge 并补扣。
//
// 资金口径（保守、不制造新欠费）：
//   - charge 上限 = 原冻结额（从 freeze 流水读回）；
//   - charge 受余额下限约束，击穿时降到可扣额度（少收 + 告警）；
//   - charge == 0（免费模型或余额已耗尽）时只推进状态，不写流水。
//
// 幂等：仅 released 状态可补收；settled/rejected 一律 ErrSessionClosed。
//
// 四个边界的口径（每一个都对应一种「钱与状态不一致」的真实情形）：
//   - usages 行已归档 → ErrUsageGone，调用方**不扣款但照常交付产物**。
//     绝不能写「usage 不在就按冻结额硬扣」：那绕过 PricingSnapshot，
//     扣多少全凭一个已经不存在的历史数字，是凭空扣款。
//   - 用户软删 → 用 Unscoped 读出来**照常扣**。账要平，软删只是隐藏账号，
//     不是免除债务；不扣会让「删号」变成一条免费的逃单路径。
//   - 用户物理不存在 → ErrUserGone，不扣款、照常交付（钱已无处可记）。
//   - 流水唯一冲突（uq_transactions_usage_ref）→ ErrAlreadySettled。
//     说明另一次补收抢先完成，本次不再动账；当成普通错误处理会让调用方
//     每轮重试一个必败动作，直到复查窗口耗尽。
func (s *Service) LateSettle(ctx context.Context, usageID uint64, u Usage) (int64, error) {
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	var charge int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var usage model.Usage
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&usage, usageID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUsageGone
			}
			return err
		}
		if usage.Status != model.UsageStatusReleased {
			return ErrSessionClosed
		}
		frozen, ferr := s.frozenOf(tx, usageID)
		if ferr != nil {
			// 与 ReclaimSettle 同口径：读不到冻结额就整笔回滚，让调用方下一轮重试。
			// 折成 0 会让「补收上限 = 0」→ 一分不扣但状态照常推进 → 上游成本白送。
			return ferr
		}
		// 公式失败按冻结额补收（此时冻结已退给用户，补收等于把预扣额重新扣回来），
		// 不 return err：迟到结算的调用方是任务收尾，报错只会让这笔钱永远不落账。
		var preSnap Snapshot
		_ = json.Unmarshal(usage.PricingSnapshot, &preSnap)
		// 迟到结算同样按交付档补收（标记已在主路径落过，这里不重复）。
		u, _ = UpgradeDeliveredTier(preSnap.SpecPrice, u, usage.UpstreamSpec)

		_, calcCharge, calcUncapped, calcFlags := amountOf(usage.PricingSnapshot, u, frozen, "late_settle", false, false)
		charge = calcCharge
		if charge > frozen {
			charge = frozen // 不制造超过原冻结额的新欠费
		}
		uncapped := calcUncapped

		var user model.User
		// Unscoped：软删的用户也要读到（账照平）。只有物理不存在才会 NotFound。
		if err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&user, usage.UserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserGone
			}
			return err
		}
		anomaly := appendFlag(usage.Anomaly, model.AnomalyLateSuccessCharged)
		if charge <= 0 {
			// 「已补收」这个标记在 charge=0 时是误导：账上确实一分钱没进来。
			// 补一个 free 标记，让对账能把「交付了但没收钱」单独筛出来。
			anomaly = appendFlag(anomaly, model.AnomalyLateSuccessFree)
		}
		if user.DeletedAt.Valid {
			// 用户已软删但账照平：这个标记是「删号逃单未遂」的唯一线索。
			anomaly = appendFlag(anomaly, model.AnomalyLateSuccessUserDeleted)
		}
		for _, f := range calcFlags {
			anomaly = prependFlags(anomaly, f)
		}
		newBalance := user.Balance - charge
		if limit := negativeLimit(); newBalance < -limit {
			allowed := user.Balance + limit
			if allowed < 0 {
				allowed = 0
			}
			if allowed < charge {
				charge = allowed
			}
			newBalance = user.Balance - charge
			anomaly = prependFlags(anomaly, model.AnomalyNegativeBalanceBlocked)
			logger.L().Warn("迟到结算会击穿余额下限，已按上限扣减",
				zap.Bool("late_settle_capped", true),
				zap.Uint64("usage_id", usageID),
				zap.Uint64("user_id", usage.UserID),
				zap.Int64("balance", user.Balance),
				zap.Int64("limit", limit))
		}

		if err := tx.Model(&model.User{}).Where("id = ?", usage.UserID).
			Updates(map[string]any{"balance": newBalance, "used": user.Used + charge}).Error; err != nil {
			return err
		}
		// 补写 KEY 已用额：旧实现完全不动这一列，导致该 usage 在 KEY 上净额是 -frozen
		// （Release 退过头）→ used 变负 → 该 KEY 永久无法冻结。
		if err := keyUsageDelta(tx, usage.KeyID, charge); err != nil {
			return err
		}
		finishedAt := time.Now()
		startedAt := usage.CreatedAt
		if usage.StartedAt != nil {
			startedAt = *usage.StartedAt
		}
		if err := tx.Model(&model.Usage{}).Where("id = ?", usage.ID).Updates(map[string]any{
			"status": model.UsageStatusSettled, "charge": charge, "charge_before_cap": uncapped,
			"prompt_tokens":      nonNeg(u.PromptTokens),
			"completion_tokens":  nonNeg(u.CompletionTokens),
			"bill_prompt_tokens": nonNeg(u.PromptTokens), "bill_completion_tokens": nonNeg(u.CompletionTokens),
			"anomaly":     textx.TruncateRunes(anomaly, anomalyMaxLen),
			"error_msg":   textx.TruncateRunes("late_success: 会话已释放后上游迟到成功，按快照补收", 500),
			"finished_at": finishedAt,
			"latency_ms":  spanMs(startedAt, finishedAt),
		}).Error; err != nil {
			return err
		}
		if charge > 0 {
			return tx.Create(&model.Transaction{
				UserID: usage.UserID, Type: model.TxnSettle, Amount: -charge,
				BalanceAfter: newBalance, FrozenAfter: user.Frozen,
				RefType: model.RefTypeUsage, RefID: strconv.FormatUint(usage.ID, 10), Reason: "迟到成功补收",
			}).Error
		}
		return nil
	})
	if err != nil {
		// 唯一流水已存在：另一次补收抢先完成（并发 / 重试 / 上一轮已成功但状态没读到）。
		// 折成哨兵错误而不是原样返回：调用方要能区分「我该重试」与「这件事已经有人做完了」。
		if pgerr.IsUnique(err) {
			return 0, ErrAlreadySettled
		}
		return 0, err
	}
	return charge, nil
}

// anomalyMaxLen 是 usages.anomaly 的列宽。
//
// 255 而不是 128：标记种类已超 20 种，128 会在多标记叠加时截断掉尾部关键项
// （charge_capped 之类的常常排在后面），排障时"有值但看不出是什么"等于没有。
const anomalyMaxLen = 255

// prependFlags 把关键标记放到最前面再合并：截断时先丢的必须是次要标记。
func prependFlags(existing string, flags ...string) string {
	all := make([]string, 0, len(flags)+4)
	all = append(all, flags...)
	if existing != "" {
		all = append(all, strings.Split(existing, ",")...)
	}
	return joinFlagsLocal(all)
}

// appendFlag 追加异常标记（去重，逗号分隔）。
func appendFlag(existing string, flags ...string) string {
	all := []string{}
	if existing != "" {
		all = append(all, strings.Split(existing, ",")...)
	}
	all = append(all, flags...)
	return joinFlagsLocal(all)
}

// joinFlagsLocal 与 relay.joinFlags 同语义（billing 不反向依赖 relay，故本地保留一份）。
func joinFlagsLocal(flags []string) string {
	seen := make(map[string]bool, len(flags))
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return strings.Join(out, ",")
}

// chargeCapFor 返回本次结算的金额上限与原因标签。
//
// 三条约束取「够用且不给谎报开口子」的那个：
//   - 冻结额 > 0：3×冻结额（对上游谎报用量的粗保护）；
//   - 冻结额 = 0：零冻结兜底（短请求估算被舍入成 0 的场景）；
//   - ceiling > 0：配置允许的最坏金额（max_seconds / max_n 显式声明）。
//
// ceiling 只可能**抬高**上限，且它来自运营自己配的上限、不是上游给的数字：
// 「模型声明最多 60 秒 × 0.01 元/秒」就是这一单的天花板。这种场景下再按
// 3×冻结（ASR 甚至为 0）去削它没有任何保护意义，只会系统性少收。
//
// 第三个返回值表示"冻结额×3 溢出"，调用方据此记录 cap_overflow 异常：
// 旧实现 `frozen*3` 在越过 int64 时回绕成负数，被 `triple > 0` 静默吞掉，
// 结果是**对本应收 3 倍的大额巨幅少收**。溢出时退回全局上限是正确结果，
// 但绝不能静默 —— 必须留标记。
func chargeCapFor(frozen, ceiling int64) (cap int64, reason string, overflow bool) {
	limit := maxCharge()
	cap, reason = limit, "global"
	if frozen > 0 {
		triple, err := money.MulDiv(frozen, 3, 1)
		if err != nil {
			overflow = true
		} else if triple < limit {
			cap, reason = triple, "frozen3x"
		}
	} else if z := zeroFrozenCap(); z > 0 && z < limit {
		cap, reason = z, "zero_frozen"
	}
	if ceiling > cap {
		cap, reason = ceiling, "configured_max"
	}
	if cap > limit {
		cap, reason = limit, "global"
	}
	return cap, reason, overflow
}

// Adjust 调账入口：人工增减余额（±，微），写 adjust 流水。
func (s *Service) Adjust(ctx context.Context, userID uint64, amount int64, reason string) error {
	if err := money.Check(amount); err != nil {
		return ErrAmountTooLarge
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var u model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, userID).Error; err != nil {
			return err
		}
		newBalance, err := money.Sum(u.Balance, amount)
		if err != nil {
			return ErrAmountTooLarge
		}
		if newBalance < 0 {
			return ErrNegativeBalance
		}
		// 人工调账同样受余额上限约束（与 CreditTx.Cap 口径一致），
		// 否则管理员可以把余额加到远超 balance_limit。
		if capAmount := conf.BalanceLimitFor(u.BalanceLimit); capAmount > 0 && newBalance > capAmount {
			return ErrBalanceLimitExceeded
		}
		if err := tx.Model(&model.User{}).Where("id = ?", userID).Update("balance", newBalance).Error; err != nil {
			return err
		}
		return tx.Create(&model.Transaction{
			UserID:       userID,
			Type:         model.TxnAdjust,
			Amount:       amount,
			BalanceAfter: newBalance,
			FrozenAfter:  u.Frozen,
			RefType:      "admin_adjust",
			RefID:        strconv.FormatUint(userID, 10),
			Reason:       textx.TruncateRunes(reason, 200),
		}).Error
	})
}

// CreditInput 是余额入账入参：充值 / 兑换 / 退款 / 邀请返利转入。
type CreditInput struct {
	UserID  uint64
	Amount  int64  // 正 = 入账（充值/兑换/邀请），负 = 扣减（退款）
	Type    string // recharge / redeem / refund / invite
	RefType string
	RefID   string
	Reason  string
	// Cap > 0 时校验入账后余额不超过该上限（只计 balance，不含 frozen）。
	Cap int64
	// AllowNegative 允许这一笔把余额扣成负数（= 记欠款）。
	//
	// 只给「渠道已退款、本地补扣额度」（upstream_only）用：那笔钱确实已经原路
	// 退给用户了，本地额度必须扣回，即使用户已经把它消费掉。
	// 普通退款**绝不能**开这个口子 —— 已消费额度不支持退款（见 refund.go 注释）。
	AllowNegative bool
}

// Credit 入账入口（自开事务）：余额增加/扣减 + 写 transactions（含 balance_after 快照）。
// 退款等需与订单状态同事务提交的场景，请改用 CreditTx。
func (s *Service) Credit(ctx context.Context, in CreditInput) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return CreditTx(tx, in) })
}

// CreditTx 在既有事务内执行入账，保证「余额变动 + 业务状态变更」原子提交。
// 余额写入口仍唯一收敛于本包（其他模块不得直写 users.balance）。
func CreditTx(tx *gorm.DB, in CreditInput) error {
	if err := money.Check(in.Amount); err != nil {
		return ErrAmountTooLarge
	}
	var u model.User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, in.UserID).Error; err != nil {
		return err
	}
	newBalance, err := money.Sum(u.Balance, in.Amount)
	if err != nil {
		return ErrAmountTooLarge
	}
	if newBalance < 0 && !in.AllowNegative {
		return ErrNegativeBalance
	}
	if in.Cap > 0 && newBalance > in.Cap {
		return ErrBalanceLimitExceeded
	}
	if err := tx.Model(&model.User{}).Where("id = ?", in.UserID).Update("balance", newBalance).Error; err != nil {
		return err
	}
	return tx.Create(&model.Transaction{
		UserID: in.UserID, Type: in.Type, Amount: in.Amount,
		BalanceAfter: newBalance, FrozenAfter: u.Frozen,
		RefType: in.RefType, RefID: in.RefID, Reason: textx.TruncateRunes(in.Reason, 200),
	}).Error
}
