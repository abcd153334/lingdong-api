package order

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/payment"
)

// QueryOnce 对单笔订单主动查单一次，返回「是否已入账」。
//
// 只处理 pending：已终态的订单直接返回，不打上游。
// **查单失败不改变任何状态** —— worker 依赖这个语义：关单是终态动作，
// 不能用「查不到」代替「确认未付」。
func (s *Service) QueryOnce(ctx context.Context, orderID uint64) (bool, error) {
	var o model.Order
	if err := s.db.WithContext(ctx).First(&o, orderID).Error; err != nil {
		return false, err
	}
	return s.queryOrder(ctx, &o)
}

// queryOrder 是查单主体（worker 已经持有订单快照时走这里，省一次查询）。
func (s *Service) queryOrder(ctx context.Context, o *model.Order) (bool, error) {
	if o.Status != "pending" {
		return o.Status == "paid", nil
	}
	ch, ok := payment.FindChannel(o.PayChannelID)
	if !ok {
		return false, errs.PayMethodDisabled
	}
	n, err := payment.Query(ctx, ch, o.OutTradeNo, o.ChannelOrderID)
	if err != nil {
		if errors.Is(err, payment.ErrUnsupported) {
			return false, nil // 该协议没实现查单（易支付）：不是错误，按 TTL 关单
		}
		return false, err
	}
	if n == nil || n.Kind != payment.NotifyPaid {
		return false, nil // 确认未支付
	}
	// 走与回调完全相同的入账路径，绝不另写一套。
	res := s.settle(ctx, ch, n, "query")
	logger.L().Info("主动查单命中已支付",
		zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo),
		zap.String("result", resultName(res)))
	return res == NotifyDone, nil
}

// RefundQueryOnce 对「退款中」订单查一次上游，命中退款终态就推进状态。
//
// 为什么需要：虎皮椒**没有退款查询接口**，退款结果只能靠回调；回调丢失时
// 订单会永远停在 refunding（额度已扣、用户看不到结果）。这里复用支付查单接口的
// CD/UD 状态作二手证据 —— 只对「我们自己发起过退款」的订单生效，
// 所以 CD 不会被误当成「用户取消支付」。
//
// 查不到（上游仍 OD）或查询失败：什么都不做，等下一轮或人工确认。
func (s *Service) RefundQueryOnce(ctx context.Context, orderID uint64) error {
	var o model.Order
	if err := s.db.WithContext(ctx).First(&o, orderID).Error; err != nil {
		return err
	}
	if o.Status != statusRefunding {
		return nil
	}
	ch, ok := payment.FindChannel(o.PayChannelID)
	if !ok {
		return errs.PayMethodDisabled
	}

	// ① 有**独立退款查询接口**的协议（微信/支付宝）走这一条：它给的是权威终态。
	//
	// 必须先试它、再回落支付查单：虎皮椒没有退款查询接口（RefundQuery 返回
	// ErrUnsupported），如果一上来就用 RefundQuery 并且当成唯一路径，
	// 虎皮椒的退款巡检会直接失效（订单永远停在 refunding）。
	if payment.RefundQuerySupported(payment.ParseProtocol(ch.Protocol)) {
		if strings.TrimSpace(o.RefundNo) == "" {
			// 历史在途退款可能没有退款单号（D3 之前发起的）：查不了，等人工确认。
			logger.L().Warn("退款中订单没有退款单号，跳过退款查询",
				zap.Uint64("order_id", o.ID), zap.String("channel", ch.ID))
			return nil
		}
		res, qerr := payment.RefundQuery(ctx, ch, o.OutTradeNo, o.RefundNo, o.AmountCent)
		switch {
		case qerr == nil && res != nil && res.Status == payment.RefundDone:
			if err := s.markRefundSuccess(ctx, o.ID, res); err != nil {
				return err
			}
			logger.L().Info("退款查询命中成功",
				zap.Uint64("order_id", o.ID), zap.String("refund_no", o.RefundNo))
			return nil
		case qerr == nil && res != nil && res.Status == payment.RefundDeclined:
			if err := s.markRefundFailed(ctx, o.ID, firstText(res.Message, "上游退款已关闭")); err != nil {
				return err
			}
			logger.L().Warn("退款查询命中失败（额度已回滚）",
				zap.Uint64("order_id", o.ID), zap.String("refund_no", o.RefundNo))
			return nil
		case qerr == nil:
			return nil // 仍在处理中：什么都不做，等下一轮
		case errors.Is(qerr, payment.ErrUnsupported), errors.Is(qerr, payment.ErrNotConfigured):
			// 协议没实现 / 配置暂时不全：不当作错误，静默等人工或下一轮。
			return nil
		default:
			return qerr
		}
	}

	// ② 回落：虎皮椒没有退款查询接口，拿支付查单的 CD/UD 作二手证据。
	//    只对「我们自己发起过退款」的订单生效，所以 CD 不会被误当成「用户取消支付」。
	n, err := payment.Query(ctx, ch, o.OutTradeNo, o.ChannelOrderID)
	if err != nil {
		if errors.Is(err, payment.ErrUnsupported) {
			return nil
		}
		return err
	}
	if n == nil {
		return nil
	}
	switch n.Kind {
	case payment.NotifyRefunded, payment.NotifyRefundFailed:
		// 复用与回调完全相同的状态流转（settle），不另写一条退款终结路径。
		if res := s.settle(ctx, ch, n, "refund_query"); res == NotifyRetry {
			return errors.New("退款查单落库失败")
		}
		logger.L().Info("退款查单命中终态",
			zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo),
			zap.String("kind", string(n.Kind)))
	case payment.NotifyRefundUnknown:
		// 异常态不是终态：钱可能已退出、停在中间账户，必须保持 refunding 等人工核销。
		// 这里显式列出（而不是让它落到 default 静默忽略），是为了让「故意不处理」可被搜索到。
		logger.L().Warn("退款查单返回异常态，保持 refunding 等待人工处理",
			zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo))
	}
	return nil
}

// GetForUser 返回本人订单的只读视图。
//
// 属主校验放在查询条件里（`id = ? AND user_id = ?`）：不是本人订单一律 404，
// 不区分「不存在」与「不是你的」。否则它就是一个订单号枚举接口 ——
// 攻击者从 404 与 403 的差异就能确认某个订单号是否存在、属于谁。
// appID 非 0 时额外限定订单归属的应用（托管密钥的隔离维度）。
func (s *Service) GetForUser(ctx context.Context, userID, appID, orderID uint64) (View, error) {
	var o model.Order
	q := s.db.WithContext(ctx).Where("id = ? AND user_id = ?", orderID, userID)
	if appID != 0 {
		q = q.Where("app_id = ?", appID)
	}
	if err := q.First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return View{}, errs.OrderNotFound
		}
		return View{}, err
	}
	return toView(&o), nil
}

// VerifyResult 是「我已支付，立即核对」的返回。
type VerifyResult struct {
	Order   View `json:"order"`
	QueryOK bool `json:"query_ok"` // false = 本次查单失败，不代表未支付
}

// VerifyForUser 对本人订单主动查单一次并返回最新状态。
//
// 属主校验放在查询条件里（`id = ? AND user_id = ?`）：不是本人订单一律 404，
// 不区分「不存在」与「不是你的」，避免变成订单号枚举接口。
func (s *Service) VerifyForUser(ctx context.Context, userID, orderID uint64) (*VerifyResult, error) {
	var o model.Order
	if err := s.db.WithContext(ctx).Where("id = ? AND user_id = ?", orderID, userID).First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.OrderNotFound
		}
		return nil, err
	}

	queryOK := true
	if o.Status == "pending" {
		if _, err := s.queryOrder(ctx, &o); err != nil {
			queryOK = false
			logger.L().Warn("用户主动核对查单失败",
				zap.Uint64("order_id", o.ID), zap.Error(err))
		}
		// 无论查单成功与否，都以库里的最新状态为准（查单可能已入账）。
		if err := s.db.WithContext(ctx).First(&o, orderID).Error; err != nil {
			return nil, err
		}
	}
	return &VerifyResult{Order: toView(&o), QueryOK: queryOK}, nil
}

// RetryPay 对**同一订单**重新获取支付信息（绝不换单号）。
//
// 为什么不能靠「重新下单」重试：重新下单会生成新的 out_trade_no，
// 而上游可能已有第一笔订单，用户付了第二笔又付第一笔 → 双倍扣款。
//
// 顺序刻意是「先查单、再决定是否 Create」：
// 上游超时 ≠ 上游未建单，直接重复 Create 会在上游留下垃圾单。
func (s *Service) RetryPay(ctx context.Context, userID, orderID uint64) (*CreateResult, error) {
	var o model.Order
	if err := s.db.WithContext(ctx).Where("id = ? AND user_id = ?", orderID, userID).First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.OrderNotFound
		}
		return nil, err
	}
	if o.Status != "pending" {
		return nil, errs.OrderNotPayable
	}
	if o.ExpireAt != nil && !time.Now().Before(*o.ExpireAt) {
		return nil, errs.OrderNotPayable
	}
	// 已经有支付信息：直接返回（幂等，不打上游）。
	if o.PayURL != "" || o.QRRef != "" {
		return resultOf(&o), nil
	}

	release, ok := lockRetry(ctx, orderID)
	if !ok {
		return nil, errs.TopupInProgress
	}
	defer release()

	// 双检：等锁期间可能已被另一个请求填好。
	if err := s.db.WithContext(ctx).First(&o, orderID).Error; err != nil {
		return nil, err
	}
	if o.PayURL != "" || o.QRRef != "" {
		return resultOf(&o), nil
	}
	if o.PayAttempts >= maxPayAttempts {
		// 多次失败后继续 Create 只会在上游堆垃圾单；让用户重新下单（新单号）。
		return nil, errs.PayGatewayError
	}

	// 重试锁死原渠道：换渠道建的单，回调地址与验签密钥都对不上，永远无法入账。
	ch, method, err := resolveRetryChannel(&o)
	if err != nil {
		return nil, err
	}

	// 先查单：上游若已建单，要么直接入账、要么说明「确实未支付」才继续 Create。
	if n, qerr := payment.Query(ctx, ch, o.OutTradeNo, o.ChannelOrderID); qerr == nil && n != nil {
		if n.Kind == payment.NotifyPaid {
			s.settle(ctx, ch, n, "query")
			return nil, errs.OrderNotPayable
		}
	}

	notifyURL, returnURL, err := payURLs(ch)
	if err != nil {
		return nil, err
	}
	res, createErr := payment.Create(ctx, ch, payment.CreateRequest{
		OutTradeNo: o.OutTradeNo, AmountCent: o.AmountCent, Method: method,
		// 重试沿用订单**固化**的币种（currencyOf(&o)），不能读当前站点币种：
		// 运营若在订单存续期间切换结算币种，重试必须仍按原币种收款，否则回调对不上。
		Currency: currencyOf(&o),
		Subject:  o.Subject, NotifyURL: notifyURL, ReturnURL: returnURL, Attach: orderIDText(o.ID),
		ExpireAt: expireAtOf(&o),
	})
	if createErr != nil {
		s.markPayError(ctx, o.ID, createErr)
		logger.L().Warn("重试创建支付失败",
			zap.Uint64("order_id", o.ID), zap.String("channel", ch.ID), zap.Error(createErr))
		return nil, mapGatewayErr(createErr)
	}

	// 幂等守卫必须同时看 pay_url 与 qr_ref：官方直连的 pay_url 恒为空
	// （支付信息只有 qr_ref），只判 pay_url 会让守卫永远成立、并发写互相覆盖。
	if err := s.db.WithContext(ctx).Model(&model.Order{}).
		Where("id = ? AND pay_url = '' AND qr_ref = ''", o.ID).
		Updates(map[string]any{
			"pay_url":          res.PayURL,
			"qr_ref":           res.QRRef,
			"channel_order_id": res.ChannelOrderID,
			"pay_attempts":     o.PayAttempts + 1,
			"pay_error":        "",
		}).Error; err != nil {
		return nil, err
	}
	o.PayURL, o.QRRef, o.ChannelOrderID = res.PayURL, res.QRRef, res.ChannelOrderID
	o.PayAttempts++
	o.PayError = ""
	logger.L().Info("重试创建支付成功",
		zap.Uint64("order_id", o.ID), zap.String("channel", ch.ID))
	return resultOf(&o), nil
}

// QRCodeFor 返回订单二维码的展示数据（图片已由后端代理成 data URL）。
//
// 属主校验同上；同时返回剩余有效期，前端据此切换「二维码已过期」的文案。
// appID 非 0 时额外限定订单归属的应用（托管密钥的隔离维度）。
func (s *Service) QRCodeFor(ctx context.Context, userID, appID, orderID uint64) (*payment.QRPayload, int, error) {
	var o model.Order
	q := s.db.WithContext(ctx).Where("id = ? AND user_id = ?", orderID, userID)
	if appID != 0 {
		q = q.Where("app_id = ?", appID)
	}
	if err := q.First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, 0, errs.OrderNotFound
		}
		return nil, 0, err
	}
	left := expiresIn(&o)
	if o.QRRef == "" {
		return &payment.QRPayload{Mode: payment.QRModeNone}, left, nil
	}
	// 必须按**下单时的协议快照**判定形态：官方直连的 qr_ref 是内容
	// （支付宝的 https://qr.alipay.com/... 会被 scheme 嗅探误判成图片地址）。
	payload, err := payment.ResolveQRFor(ctx, payment.Protocol(o.PayProtocol), o.QRRef)
	if err != nil {
		logger.L().Warn("获取二维码失败", zap.Uint64("order_id", o.ID), zap.Error(err))
		return nil, left, errs.PayQRUnavailable
	}
	return payload, left, nil
}

// orderIDText 把订单 ID 转成字符串（避免在多处重复 strconv）。
func orderIDText(id uint64) string {
	return strconv.FormatUint(id, 10)
}

// resultName 只用于日志，便于按关键字检索。
func resultName(r NotifyResult) string {
	switch r {
	case NotifyDone:
		return "done"
	case NotifyOrphan:
		return "orphan"
	case NotifyNeedManual:
		return "need_manual"
	case NotifyReject:
		return "reject"
	default:
		return "retry"
	}
}
