package order

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/service/affiliate"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/payment"
)

// 退款方式（orders.refund_mode）。
//
// 这两者**不能混为一谈**：gateway 是「钱真的原路退回」，balance 只是「把额度扣回来」。
// 混在一起会让对账、客服话术、用户投诉处理全部失去依据。
const (
	// RefundModeGateway 原路退回：先扣额度并置 refunding，再调上游退款接口。
	RefundModeGateway = "gateway"
	// RefundModeBalance 仅回退平台余额：上游未接入退款，或运营已在网关后台手工退款。
	RefundModeBalance = "balance"
)

// 退款状态（orders.refund_status）。
//
// 字面量的唯一声明处是 model（affiliate 包也要按它筛数据，且不能反向 import 本包）。
// 这里只保留本包常用的短名字，避免调用点大面积改写。
const (
	RefundStatePending = model.RefundStatePending // 已受理，结果待确认（RD / 超时 / 响应不可信）
	RefundStateSuccess = model.RefundStateSuccess // 上游确认已退款，或余额模式已完成
	RefundStateFailed  = model.RefundStateFailed  // 上游确认失败，额度已回滚
	// RefundStateUpstreamOnly 表示「上游已退款，但我们从未发起过退款」。
	//
	// 只能出现在订单仍是 paid 的场景（运营在网关后台手工退款）：此时**不能**把
	// status 改成 need_manual —— 那会退出终态集合，网关重放「支付成功」时
	// onPaid 的状态短路失效，于是同一笔钱再入账一次。
	// 额度要不要扣回由人工决定（「仅回退平台余额」），机器不自动扣（额度可能已被消费）。
	RefundStateUpstreamOnly = model.RefundStateUpstreamOnly
	// statusRefunding 直接引用 model 里的定义：这个字符串同时出现在
	// worker（退款巡检的领取条件）、controller（筛选白名单）、systemconfig
	//（删除渠道时的在途检查）与本包的状态机里。各自写字面量时，改一处就等于
	// 让巡检「领不到单」——而它不报错，只是退款的单再也没人推进。
	statusRefunding = model.OrderRefunding
)

// refundCallTimeout 是单次退款调用上限。
//
// 比下单宽松：退款是资金动作，宁可多等 10 秒，也不能因为掐断而进入「不知道退没退」。
const refundCallTimeout = 20 * time.Second

// RefundInput 是人工退款入参。
//
// Mode 为空表示按渠道自动选择：协议支持原路退款且订单是 paid → gateway，否则 balance。
type RefundInput struct {
	OrderID uint64
	Reason  string
	Mode    string
}

// RefundOutcome 是退款结果（对管理台）。
//
// 三种 RefundStatus 对应三种运作结果，管理台必须分别提示：
//   - success → 已退款（原路退回 / 已扣回余额）
//   - pending → 已提交，结果待定（**不是失败**，钱可能已经退出去了）
//   - failed  → 上游拒绝，额度已回滚（订单回到 paid）
type RefundOutcome struct {
	OrderID      uint64 `json:"order_id"`
	Mode         string `json:"refund_mode"`
	RefundStatus string `json:"refund_status"`
	RefundNo     string `json:"refund_no,omitempty"`
	AmountCent   int64  `json:"refund_amount_cent"`
	// BalanceDeducted 是退款时从余额扣回的金额（微）。
	BalanceDeducted int64  `json:"balance_deducted"`
	Message         string `json:"message"`
}

// Refund 人工退款。
//
// 顺序是**先扣额度、再调上游**，与下单的「先落库、再调上游」同一套思路：
//  1. 扣额度与状态变更同事务提交（额度当月已消费时直接拒绝，绝不发出无法扣回的退款）；
//  2. 事务提交后才发出退款请求，避免长事务持有行锁；
//  3. 上游明确拒绝 → 补偿入账并回到 paid；结果未知 → **保持 refunding 不回滚**。
//
// 为什么未知态不回滚：退款请求可能已在网关处理，回滚额度会让用户既拿到钱又保留额度；
// 而停在 refunding 最坏只是需要人工到网关后台核对一次。
func (s *Service) Refund(ctx context.Context, in RefundInput) (*RefundOutcome, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || in.OrderID == 0 {
		return nil, errs.InvalidParam
	}

	var o model.Order
	var mode string
	var ch payment.Channel
	// deducted 是**实际**从余额扣回的金额（微）。未入账的 need_manual 单为 0，
	// 必须按实际值回给运营看，不能一律回 o.Credited。
	var deducted int64
	// refundedCent 是**实际回到渠道的钱**（分）。balance 模式为 0；upstream_only 例外，
	// 因为那笔钱运营已经在网关后台退过了，本次只是补扣额度。
	var refundedCent int64
	now := time.Now()

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, in.OrderID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.OrderNotFound
			}
			return err
		}
		switch o.Status {
		case "refunded":
			return errs.OrderRefunded
		case statusRefunding:
			// 在途退款不允许再发一次：上游没有退款查询接口，重复提交可能真的退两笔。
			return errs.OrderRefunding
		case "paid", "need_manual":
			// need_manual = 上游已退款/金额不符等人工态：只允许「仅回退余额」，
			// 否则会对一笔可能已经退过款的订单再发退款请求。
			//
			// refund_status=failed 必须在此之前拦下：上一次原路退款失败时 onRefundFailed
			// 已把额度全额回滚、状态退回 paid，订单看起来「又可以退了」。但退款流水已经
			// 存在（uq_transactions_order_refund 一单一条），再退一次要么撞索引回 500，
			// 要么（索引缺失的库）真的再扣一次已经还回去的额度。两种情况都不该发生。
			if o.RefundStatus == RefundStateFailed {
				return errs.InvalidParam.WithMessage(
					"该订单上一次原路退款已失败，额度已全额回滚，不能再次发起退款；如需原路退款请在上游商户后台操作")
			}
		default:
			return errs.OrderNotPaid
		}

		m, c, merr := resolveRefundMode(in.Mode, &o)
		if merr != nil {
			return merr
		}
		mode, ch = m, c

		// 额度扣回与状态变更同事务：余额不足时整笔中止，不发上游请求。
		//
		// `need_manual` 有两种来源，处置完全相反：
		//   - 已入账但金额不符/超上限 → 必须扣回；
		//   - **从未入账**（回调金额不符被直接拒绝、到账会越过余额上限）→ 扣回等于
		//     扣用户从没拿到的钱。判据只能是"订单是否真有 recharge 流水"，
		//     余额为 0 的用户会被 ErrNegativeBalance 挡住纯属运气，余额非 0 就被静默多扣。
		credited := o.Status == "paid"
		if !credited {
			var n int64
			if err := tx.Model(&model.Transaction{}).
				Where("ref_type = ? AND ref_id = ? AND type = ?",
					"order", strconv.FormatUint(o.ID, 10), "recharge").
				Count(&n).Error; err != nil {
				return err
			}
			credited = n > 0
		}
		if credited {
			// upstream_only = 渠道**已经**把钱退给用户了，本地只是补扣额度。
			// 这时用户可能已把额度消费掉，必须允许扣成负（记欠款）：
			// 否则这条唯一的"已退款"路径永远走不通，只能人工改库。
			//
			// 普通退款（模式 balance、非 upstream_only）不开口子：
			// 已消费的额度不支持退款，返回明确的 402 让运营去走渠道退款。
			upstreamOnly := o.RefundStatus == RefundStateUpstreamOnly
			err := billing.CreditTx(tx, billing.CreditInput{
				UserID: o.UserID, Amount: -o.Credited, Type: model.TxnRefund,
				RefType: model.RefTypeOrder, RefID: strconv.FormatUint(o.ID, 10),
				Reason:        "退款扣回：" + reason,
				AllowNegative: upstreamOnly,
			})
			if err != nil {
				if errors.Is(err, billing.ErrNegativeBalance) {
					// 文案必须说清"不是系统坏了"：这是产品规则（已消费不退），
					// 运营看到"余额不足"会以为要充值或改库。
					return errs.InsufficientBalance.WithMessage(
						"用户已消费的额度不支持直接退款（余额不足以扣回），请联系用户走渠道退款")
				}
				return err
			}
			deducted = o.Credited
		}

		// refund_amount_cent 只记「真的回到渠道的钱」：balance 模式一分钱都没回渠道，
		// 写订单金额会让人误以为已经原路退款（对账与客诉都按它判断）。
		// 实际扣回的平台额度另记 refund_credited（微）。
		//
		// 但 upstream_only 是唯一例外：那笔钱**确实已经原路退给用户了**（运营在网关
		// 后台手工退的），本次只是在本地补扣额度。若照样写 0，就把「已退出去多少钱」
		// 这个事实永久抹掉 —— 渠道退款总额会少算一笔，客诉时也拿不出凭证。
		amountCent := o.AmountCent
		if mode == RefundModeBalance && o.RefundStatus != RefundStateUpstreamOnly {
			amountCent = 0
		}
		refundedCent = amountCent
		updates := map[string]any{
			"refund_mode":        mode,
			"refund_status":      RefundStatePending,
			"refund_amount_cent": amountCent,
			"refund_credited":    deducted,
			"refund_reason":      textx.TruncateRunes(reason, 120),
			"refund_error":       "",
			"refund_at":          now,
			// 余额模式不打上游，attempts 也记一次：审计要能看出「发起过退款」。
			"refund_attempts": gorm.Expr("refund_attempts + 1"),
		}
		if mode == RefundModeBalance {
			updates["status"] = "refunded"
			updates["refund_status"] = RefundStateSuccess
			updates["refund_done_at"] = now
		} else {
			updates["status"] = statusRefunding
			// **必须在发出请求之前落库**：微信/支付宝都要求「重试复用原退款单号」，
			// 响应丢失时如果本地没有号，退款会永远停在 refunding 且无从查起。
			// DeriveRefundNo 是纯函数，重试必然得到同一个号。
			updates["refund_no"] = payment.DeriveRefundNo(o.OutTradeNo)
		}
		if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
			return err
		}
		// 只有「钱真的退回去了」才回收返利：
		// 原路退款此刻还在途（可能要确认失败并回滚），所以那种情况放到
		// markRefundSuccess / ConfirmRefund(success) 里做。
		if mode == RefundModeBalance {
			reclaimAffiliate(tx, o.ID)
		}
		return nil
	})
	if err != nil {
		// 唯一索引 uq_transactions_order_refund 是「一单只退一次」的最后一道闸，
		// 这里是它的兜底翻译：走到这一步说明状态闸漏了某个入口（比如新增的退款路径
		// 没判 refund_status），错误原文是 SQLSTATE 23505，直接上抛会变成 500
		// 「服务器内部错误」并让运营反复重试 —— 而重试在数据层面永远不会成功。
		//
		// 事务已整体回滚：余额没有被扣第二次。
		if pgerr.IsUnique(err) {
			return nil, errs.InvalidParam.WithMessage(
				"该订单已有一条退款流水，不能重复退款；如需人工核对请按退款状态处理")
		}
		return nil, err
	}

	out := &RefundOutcome{
		OrderID: o.ID, Mode: mode, AmountCent: refundedCent, BalanceDeducted: deducted,
	}
	if mode == RefundModeBalance {
		out.RefundStatus = RefundStateSuccess
		switch {
		case o.RefundStatus == RefundStateUpstreamOnly:
			// 上游早就退过款了，本次是「补扣额度」：不能再说「未原路退款」，
			// 否则运营会去网关再核对一遍已经核对过的事。
			if deducted > 0 {
				out.Message = "上游已退款，本次已补扣平台额度"
			} else {
				out.Message = "上游已退款，但该订单从未入账，无需补扣额度"
			}
		case deducted > 0:
			out.Message = "已扣回平台额度（未原路退款，请确认上游是否已退款）"
		default:
			out.Message = "该订单从未入账，未扣回任何额度（未原路退款，请确认上游是否已退款）"
		}
		logger.L().Info("订单退款（仅回退平台余额）",
			zap.Uint64("order_id", o.ID), zap.Int64("deducted", deducted),
			zap.String("reason", reason))
		return out, nil
	}

	// 退款请求不能被「管理员关掉浏览器」取消：context.WithoutCancel 保留值与链路，
	// 丢掉取消信号，再叠加独立超时。
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refundCallTimeout)
	defer cancel()

	// 退款结果通知与支付回调共用同一个入口（官方直连会按 event_type 区分）。
	// 地址拿不到不该阻断退款：微信只在有 notify_url 时才推退款回调，
	// 拿不到就退化成「只能靠退款巡检」，比直接拒绝退款好。
	refundNotify, _, nerr := payURLs(ch)
	if nerr != nil {
		refundNotify = ""
		logger.L().Warn("退款通知地址不可用，将依赖退款巡检",
			zap.Uint64("order_id", o.ID), zap.Error(nerr))
	}

	res, rerr := payment.Refund(rctx, ch, payment.RefundRequest{
		OutTradeNo: o.OutTradeNo, ChannelOrderID: o.ChannelOrderID,
		// Stripe 认 payment_intent；其它协议留空（各自用自己的单号）。
		PaymentID: o.ChannelPaymentID, Currency: o.Currency,
		AmountCent: o.AmountCent, Reason: reason,
		RefundNo:  payment.DeriveRefundNo(o.OutTradeNo),
		NotifyURL: refundNotify,
	})

	switch {
	case rerr == nil && res != nil && res.Status == payment.RefundDone:
		if err := s.markRefundSuccess(ctx, o.ID, res); err != nil {
			return nil, err
		}
		out.RefundStatus, out.RefundNo = RefundStateSuccess, res.RefundNo
		out.Message = "已原路退款，额度已扣回"
		logger.L().Info("订单退款成功",
			zap.Uint64("order_id", o.ID), zap.String("refund_no", res.RefundNo),
			zap.Int64("amount_cent", res.RefundedCent))

	case rerr == nil && res != nil && res.Status == payment.RefundDeclined:
		msg := firstText(res.Message, "上游返回退款失败")
		if err := s.markRefundFailed(ctx, o.ID, msg); err != nil {
			return nil, err
		}
		out.RefundStatus, out.RefundNo = RefundStateFailed, res.RefundNo
		out.Message = "上游退款失败，额度已回滚：" + msg
		logger.L().Warn("订单退款被上游判定失败",
			zap.Uint64("order_id", o.ID), zap.String("msg", msg))

	case rerr == nil && res != nil:
		// 已受理（RD / OD / 金额待核）：保持 refunding，等回调、查单或人工确认。
		if err := s.markRefundAccepted(ctx, o.ID, res); err != nil {
			return nil, err
		}
		out.RefundStatus, out.RefundNo = RefundStatePending, res.RefundNo
		out.Message = "退款已提交，等待上游处理结果（通常 10 秒内完成，银行卡可能需 1–3 个工作日）"
		logger.L().Info("订单退款已受理",
			zap.Uint64("order_id", o.ID), zap.String("refund_no", res.RefundNo))

	case errors.Is(rerr, payment.ErrRefundRejected),
		errors.Is(rerr, payment.ErrNotConfigured),
		errors.Is(rerr, payment.ErrInvalidAmount),
		errors.Is(rerr, payment.ErrUnsupported):
		// 这几类都确定「请求没有产生退款」：可以安全回滚额度。
		msg := rerr.Error()
		if err := s.markRefundFailed(ctx, o.ID, msg); err != nil {
			return nil, err
		}
		out.RefundStatus = RefundStateFailed
		out.Message = "退款未提交成功，额度已回滚：" + msg
		logger.L().Warn("订单退款被拒绝（额度已回滚）",
			zap.Uint64("order_id", o.ID), zap.Error(rerr))

	default:
		// 未知：不判定、不回滚，只记录原因，等回调 / 查单 / 人工核对。
		s.markRefundUnknown(ctx, o.ID, rerr)
		out.RefundStatus = RefundStatePending
		out.Message = "退款请求结果未知，请到上游商户后台核对后再确认结果"
		logger.L().Error("订单退款结果未知（保持退款中，不回滚额度）",
			zap.Uint64("order_id", o.ID), zap.Error(rerr))
	}
	return out, nil
}

// RefundConfirmInput 是「人工确认退款结果」入参。
type RefundConfirmInput struct {
	OrderID uint64
	Result  string // success / failed
	Reason  string
	// Evidence 是上游核实凭证（网关后台单号/工单号）。确认 failed 回滚额度时必填 ——
	// 回滚额度等于「把钱还给用户」，没有外部凭据就无法证明钱真的没退出去。
	Evidence string
}

// ConfirmRefund 人工确认退款结果（处理「结果未知」与「上游后台已处理」）。
//
// 为什么必须有它：虎皮椒没有退款查询接口，退款结果只靠回调；
// 回调丢失或响应不可解析时，订单会永远停在 refunding（额度已扣）。
// 与其让运维改库，不如给一个带原因、带审计的正式出口。
func (s *Service) ConfirmRefund(ctx context.Context, in RefundConfirmInput) error {
	result := strings.ToLower(strings.TrimSpace(in.Result))
	if in.OrderID == 0 || (result != RefundStateSuccess && result != RefundStateFailed) {
		return errs.InvalidParam
	}
	reason := strings.TrimSpace(in.Reason)
	evidence := strings.TrimSpace(in.Evidence)
	if result == RefundStateFailed && evidence == "" {
		return errs.RefundEvidenceRequired
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, in.OrderID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errs.OrderNotFound
			}
			return err
		}
		if o.Status != statusRefunding {
			return errs.RefundNotPending
		}
		now := time.Now()
		if result == RefundStateSuccess {
			if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(map[string]any{
				"status": "refunded", "refund_status": RefundStateSuccess,
				"refund_done_at": now, "refund_error": "",
			}).Error; err != nil {
				return err
			}
			reclaimAffiliate(tx, o.ID)
			return nil
		}
		// 确认失败：额度在发起时已扣，这里必须补偿回去，否则用户白丢额度。
		//
		// 这段是**无条件**回滚，所以调用方必须已核实「上游确实没退成功」；
		// evidence 就是那份核实凭据，随退款备注一起落库，事后可回查。
		note := "人工确认退款失败：" + reason + "（凭证：" + evidence + "）"
		if err := refundRevertBalanceTx(tx, &o, note); err != nil {
			return err
		}
		return tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(map[string]any{
			"status": "paid", "refund_status": RefundStateFailed,
			"refund_done_at": now,
			"refund_error":   textx.TruncateRunes(note, 200),
		}).Error
	})
}

// resolveRefundMode 决定退款方式。
//
// 自动模式的判据是「协议实现了退款 + 订单仍是 paid」：
// need_manual 说明上游侧状态可疑（已退款/金额不符），只能扣余额；
// 协议未实现退款（易支付）同样只能扣余额。
func resolveRefundMode(want string, o *model.Order) (string, payment.Channel, error) {
	ch, found := payment.FindChannel(o.PayChannelID)
	// upstream_only = 上游钱已经退了（运营在网关后台手工退的），本地只是还没扣额度。
	// 这种单再发一次原路退款就是**退第二次**，所以原路退款必须排除。
	// 旧实现靠「把 status 改成 need_manual」来间接挡住，代价是重放支付成功会二次入账。
	upstreamRefunded := o.RefundStatus == RefundStateUpstreamOnly
	// Stripe 退款只认 payment_intent（pi_...），而 orders.channel_order_id 存的是
	// checkout session（cs_...）。历史订单（本列上线前支付）没有 payment_intent，
	// 发出去必然被 Stripe 拒绝，而退款是「先扣额度再出网」—— 用户额度已消费时
	// 会被 ErrNegativeBalance 卡住，订单永远退不掉。
	missingPayID := payment.ParseProtocol(ch.Protocol) == payment.ProtocolStripe &&
		strings.TrimSpace(o.ChannelPaymentID) == ""
	gatewayReady := found && payment.RefundSupported(ch.Protocol) && o.Status == "paid" &&
		!upstreamRefunded && !missingPayID

	switch strings.ToLower(strings.TrimSpace(want)) {
	case RefundModeBalance:
		return RefundModeBalance, payment.Channel{}, nil
	case "":
		if gatewayReady {
			return RefundModeGateway, ch, nil
		}
		return RefundModeBalance, payment.Channel{}, nil
	case RefundModeGateway:
		if o.Status != "paid" || upstreamRefunded {
			return "", payment.Channel{}, errs.RefundModeInvalid
		}
		if missingPayID {
			return "", payment.Channel{}, errs.RefundNoPaymentID
		}
		if !found || !payment.RefundSupported(ch.Protocol) {
			return "", payment.Channel{}, errs.RefundUnsupported
		}
		return RefundModeGateway, ch, nil
	}
	return "", payment.Channel{}, errs.InvalidParam
}

// markRefundSuccess 落「退款成功」终态（只对 refunding 生效，避免覆盖并发回调结果）。
func (s *Service) markRefundSuccess(ctx context.Context, orderID uint64, res *payment.RefundResult) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, orderID).Error; err != nil {
			return err
		}
		if o.Status != statusRefunding {
			return nil // 已被回调/人工确认终结
		}
		updates := map[string]any{
			"status": "refunded", "refund_status": RefundStateSuccess,
			"refund_done_at": time.Now(), "refund_error": "",
		}
		if res != nil && res.RefundNo != "" {
			updates["refund_no"] = res.RefundNo
		}
		// trade_no 有唯一索引：只在本地为空时补齐，绝不覆盖已有值。
		if res != nil && res.TradeNo != "" && o.TradeNo == nil {
			updates["trade_no"] = res.TradeNo
		}
		if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
			return err
		}
		reclaimAffiliate(tx, o.ID)
		return nil
	})
}

// reclaimAffiliate 作废该订单发出的邀请返利（可重复调用，内部按 voided 标记幂等）。
//
// 只作废「还没转出」的行：转出后钱已归用户，退款不再追回（不制造负余额、不记欠款）。
// **不再看是否到期**：到期但没转出，钱仍在平台手里，必须作废；
// 旧实现要求「未到期」，于是 hold_hours=0（签发即到期）时退款完全不作废。
//
// 失败只记 ERROR 不回滚退款：退款是已经发生的资金事实，不能被返利作废拖住。
func reclaimAffiliate(tx *gorm.DB, orderID uint64) {
	// 必须套一层**嵌套事务（SAVEPOINT）**：PostgreSQL 里一条语句报错就把整个事务
	// 置为 aborted，后续任何语句都报 25P02，最后 COMMIT 变成 rollback ——
	// 表现是「退款明明该成功却整笔回滚」。包一层之后，作废失败只回滚到存档点，
	// 退款本身照常提交。
	var res affiliate.Voided
	err := tx.Transaction(func(ntx *gorm.DB) error {
		var e error
		res, e = affiliate.VoidTx(ntx, orderID)
		return e
	})
	if err != nil {
		logger.L().Error("作废邀请返利失败（退款仍按成功处理）",
			zap.Uint64("order_id", orderID), zap.Error(err))
		return
	}
	// 0 行也要记：这是「退款了但没作废」唯一的现场证据，对账只能事后发现。
	logger.L().Info("退款完成，邀请返利作废处理",
		zap.Uint64("order_id", orderID), zap.Int64("voided_rows", res.Rows))
}

// markRefundFailed 落「退款失败」并把额度补偿回去（只对 refunding 生效，保证幂等）。
func (s *Service) markRefundFailed(ctx context.Context, orderID uint64, reason string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, orderID).Error; err != nil {
			return err
		}
		if o.Status != statusRefunding {
			return nil
		}
		if err := refundRevertBalanceTx(tx, &o, reason); err != nil {
			return err
		}
		return tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(map[string]any{
			"status": "paid", "refund_status": RefundStateFailed,
			"refund_done_at": time.Now(),
			"refund_error":   textx.TruncateRunes(reason, 200),
		}).Error
	})
}

// markRefundAccepted 记录「已受理但未终结」，不改订单状态。
func (s *Service) markRefundAccepted(ctx context.Context, orderID uint64, res *payment.RefundResult) error {
	updates := map[string]any{"refund_status": RefundStatePending, "refund_error": ""}
	if res != nil && res.RefundNo != "" {
		updates["refund_no"] = res.RefundNo
	}
	if res != nil && res.Message != "" {
		updates["refund_error"] = textx.TruncateRunes("上游："+res.Message, 200)
	}
	return s.db.WithContext(ctx).Model(&model.Order{}).
		Where("id = ? AND status = ?", orderID, statusRefunding).
		Updates(updates).Error
}

// markRefundUnknown 只记原因，不动状态、不动额度。
func (s *Service) markRefundUnknown(ctx context.Context, orderID uint64, cause error) {
	if cause == nil {
		return
	}
	msg := textx.TruncateRunes("结果未知，请到网关后台核对："+cause.Error(), 200)
	if err := s.db.WithContext(ctx).Model(&model.Order{}).
		Where("id = ? AND status = ?", orderID, statusRefunding).
		Update("refund_error", msg).Error; err != nil {
		logger.L().Warn("记录退款未知原因失败", zap.Uint64("order_id", orderID), zap.Error(err))
	}
}

// refundRevertBalanceTx 补偿入账：把退款时扣掉的金额加回去。
//
// 用独立流水类型 refund_revert（不是把 refund 行删掉/改掉）：
// 账务必须留痕 —— 流水表是唯一能解释「余额为什么变了」的证据链。
func refundRevertBalanceTx(tx *gorm.DB, o *model.Order, reason string) error {
	return billing.CreditTx(tx, billing.CreditInput{
		UserID: o.UserID, Amount: o.Credited, Type: model.TxnRefundRevert,
		RefType: "order", RefID: strconv.FormatUint(o.ID, 10),
		Reason: textx.TruncateRunes("退款失败回滚："+reason, 180),
	})
}

// firstText 返回第一个非空串。
func firstText(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
