package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/metrics"
	"lingdong-api/internal/pkg/pgerr"
	"lingdong-api/internal/pkg/textx"
	"lingdong-api/internal/service/affiliate"
	"lingdong-api/internal/service/billing"
	"lingdong-api/internal/service/payment"
)

// NotifyResult 是回调处理结果。controller 只根据它决定应答体，不做类型猜测。
//
// 分类的依据是「验签结果」而不是「业务是否成功」：
//   - 验签失败 → fail（可能是配置错误，重试给管理员修复时间）
//   - 验签通过但订单不存在 → **success**（本地丢单是我方问题，让网关重试 6 次也变不出订单）
//   - 业务异常（金额不符等）→ success（已转人工，止血）
//   - DB 抖动 → fail（唯一真正该重试的）
type NotifyResult int

const (
	// NotifyDone 已处理/无需处理。
	NotifyDone NotifyResult = iota
	// NotifyOrphan 验签通过但订单不存在：回 success 并落孤儿表 + ERROR。
	NotifyOrphan
	// NotifyNeedManual 已转人工（金额不符、渠道不符、上游已退款等）：回 success 停止重试。
	NotifyNeedManual
	// NotifyReject 验签失败或渠道不存在：回 fail。
	NotifyReject
	// NotifyRetry 内部错误（DB 抖动等）：回 fail，让网关重试。
	NotifyRetry
)

// Ack 返回该结果对应的应答体（易支付 / 虎皮椒 / 支付宝都按「是否 success」判断是否重试）。
//
// 微信 APIv3 不看 body，看 HTTP 状态码，所以它必须走 AckFor。
func (r NotifyResult) Ack() string {
	switch r {
	case NotifyDone, NotifyOrphan, NotifyNeedManual:
		return "success"
	default:
		return "fail"
	}
}

// AckFor 按协议给出回调应答（状态码 + 内容类型 + 包体）。
//
// 这张表是「两端语义不同」的唯一落点：
//
//	结果 \ 协议        epay/xunhupay/未知   wxpay                 alipay
//	Done/Orphan/Manual  200 success          204 空体              200 success
//	Reject              200 fail             401 FAIL json         200 failure
//	Retry（DB 抖动）     200 fail             500 FAIL json         200 failure
//
// 两条不能想当然的地方：
//   - 支付宝失败也必须 **200**（4xx 会被判「地址不可用」并升级告警）；
//   - 微信成功要 **204 无包体**（返 200+success 虽也能过，但文档推荐 204）。
func (r NotifyResult) AckFor(p payment.Protocol) payment.NotifyAck {
	ok := r == NotifyDone || r == NotifyOrphan || r == NotifyNeedManual
	switch p {
	case payment.ProtocolWxpay:
		if ok {
			return payment.NotifyAck{Status: 204}
		}
		status := 401
		if r == NotifyRetry {
			status = 500
		}
		return payment.NotifyAck{
			Status:      status,
			ContentType: "application/json; charset=utf-8",
			Body:        `{"code":"FAIL","message":"处理失败，请稍后重试"}`,
		}
	case payment.ProtocolAlipay:
		if ok {
			return payment.NotifyAck{Status: 200, ContentType: "text/plain; charset=utf-8", Body: "success"}
		}
		return payment.NotifyAck{Status: 200, ContentType: "text/plain; charset=utf-8", Body: "failure"}
	}
	return payment.NotifyAck{Status: 200, ContentType: "text/plain; charset=utf-8", Body: r.Ack()}
}

// HandleNotify 处理带渠道标识的回调（旧入口：只有表单参数）。
//
// 新代码请用 HandleNotifyRequest：微信 APIv3 的验签需要**原始 body**，
// 只传 map 等于把所有微信回调判成验签失败。
func (s *Service) HandleNotify(ctx context.Context, channelID string, params map[string]string) NotifyResult {
	res, _ := s.HandleNotifyRequest(ctx, channelID, payment.NotifyRequest{Params: params})
	return res
}

// HandleNotifyRequest 处理带渠道标识的回调（推荐入口：先按渠道验签，再查订单）。
//
// 顺序不能反：先查订单再验签，等于把「订单号是否存在」变成一个不需要密钥就能探测的接口。
//
// 第二个返回值是渠道协议：调用方要用它选应答格式（微信 204 / 支付宝 success），
// 而 payment 包不能反向 import order，所以只能在这一层带出来。
// 渠道不存在时返回空协议，调用方应按「通用 success/fail」应答。
func (s *Service) HandleNotifyRequest(ctx context.Context, channelID string, req payment.NotifyRequest) (NotifyResult, payment.Protocol) {
	// FindChannel 不看 enabled：渠道停用后，在途 pending 订单的回调仍必须能验签入账。
	ch, ok := payment.FindChannel(channelID)
	if !ok {
		logger.L().Error("支付回调的渠道不存在",
			zap.String("channel", channelID), zap.String("payload_keys", keyList(req.Params)))
		return NotifyReject, ""
	}
	n, err := payment.ParseNotifyRequest(ch, req)
	if err != nil {
		// 配置事故与验签失败必须分开记：
		//   - ErrNotConfigured = 这条渠道没有密钥（或密钥不可解析）。它是**内部问题**，
		//     意味着该渠道的回调端点对任何人都是关着的（在途订单只能人工处理），
		//     必须 ERROR + 计数，让运维去补配置；
		//   - 其余（ErrSignInvalid 等）= 公开端点的日常噪音（扫描器乱打），Warn 即可。
		//   合在一起用 ERROR 会把真正的配置事故淹没，合在一起用 Warn 则没人会发现它。
		if errors.Is(err, payment.ErrNotConfigured) {
			metrics.Inc(metrics.NamePayNotifyUnconfigured, "channel", ch.ID)
			logger.L().Error("支付回调命中未配置的渠道，已拒绝",
				zap.String("channel", ch.ID), zap.String("protocol", ch.Protocol), zap.Error(err))
			return NotifyReject, payment.ParseProtocol(ch.Protocol)
		}
		// Warn 而不是 Error：这是**公开端点**，每天都有扫描器乱打。
		// 用 ERROR 会让真正的告警被噪音淹没，而验签失败本身不产生任何副作用。
		logger.L().Warn("支付回调验签失败",
			zap.String("channel", ch.ID), zap.String("protocol", ch.Protocol), zap.Error(err))
		return NotifyReject, payment.ParseProtocol(ch.Protocol)
	}
	logger.L().Info("收到支付回调",
		zap.String("channel", ch.ID), zap.String("out_trade_no", n.OutTradeNo),
		zap.String("kind", string(n.Kind)), zap.Int64("amount_cent", n.AmountCent))
	// 事件级幂等锚（Stripe evt_...）：验签通过即记录，唯一索引冲突 = 重复事件重放。
	// 写失败不阻断入账（payment_events 是审计辅助，不是入账前提），但要留痕。
	if n.EventID != "" {
		if err := s.db.WithContext(ctx).Create(&model.PaymentEvent{
			EventID: n.EventID, Provider: ch.Protocol,
		}).Error; err != nil && !pgerr.IsUnique(err) {
			logger.L().Warn("写支付事件去重记录失败", zap.String("event_id", n.EventID), zap.Error(err))
		}
	}
	if strings.TrimSpace(n.OutTradeNo) == "" {
		// 验签通过但没有订单号：典型是微信的签名探测流量（WECHATPAY/SIGNTEST/）与
		// 某些只带退款单号的通知。没有订单号无从入账，也没有任何副作用，
		// 直接按已完成应答（否则会被上游当成失败重试 15 次）。
		logger.L().Warn("支付回调验签通过但没有订单号，按已完成处理",
			zap.String("channel", ch.ID),
			zap.String("kind", string(n.Kind)),
			zap.String("raw_event", n.Raw["_event_type"]))
		return NotifyDone, payment.ParseProtocol(ch.Protocol)
	}
	return s.settle(ctx, ch, n, "notify"), payment.ParseProtocol(ch.Protocol)
}

// HandleLegacyNotify 是旧端点 /api/pay/notify 的处理：没有渠道参数，只能先查订单再验签。
//
// 它的固有缺陷（已在路由注释里标注）：订单不存在时**无法验签**（不知道用哪个密钥），
// 所以这种情况只回 success 止血、不落孤儿表 —— 否则任何人都能往孤儿表里灌数据。
func (s *Service) HandleLegacyNotify(ctx context.Context, params map[string]string) NotifyResult {
	outTradeNo := strings.TrimSpace(params["out_trade_no"])
	if outTradeNo == "" {
		outTradeNo = strings.TrimSpace(params["trade_order_id"])
	}
	if outTradeNo == "" {
		return NotifyReject
	}
	var o model.Order
	err := s.db.WithContext(ctx).Select("id", "pay_channel_id").
		Where("out_trade_no = ?", outTradeNo).First(&o).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			logger.L().Warn("旧回调端点收到未知订单号（无法验签，仅止血）",
				zap.String("out_trade_no", outTradeNo))
			return NotifyOrphan
		}
		return NotifyRetry
	}
	if strings.TrimSpace(o.PayChannelID) == "" {
		logger.L().Warn("订单缺少支付渠道标识，无法验签（历史数据）", zap.Uint64("order_id", o.ID))
		return NotifyReject
	}
	return s.HandleNotify(ctx, o.PayChannelID, params)
}

// settle 是**唯一**的入账路径：回调与主动查单都走这里。
//
// 事务内顺序（顺序本身就是正确性的一部分）：
//  1. 行锁重读订单（调用方传来的订单快照不可信，必须用最新状态判断）
//  2. notify_count 自增 —— **必须在终态判断之前**，否则已支付订单的重复回调永远不计数，
//     而「上游还在重试」正是靠这个计数发现的
//  3. 渠道 / 商户号 / 金额校验（金额严格相等，0 容差）
//  4. 已支付但交易号不同 → 多收钱，必须留痕
//  5. 幂等短路
//  6. 入账（唯一索引冲突视为幂等）
func (s *Service) settle(ctx context.Context, ch payment.Channel, n *payment.Notification, source string) NotifyResult {
	result := NotifyDone
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.Order
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("out_trade_no = ?", n.OutTradeNo).First(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				result = NotifyOrphan
				return nil
			}
			result = NotifyRetry
			return err
		}

		// 计数：放在终态判断之前，且用 SQL 原子自增。
		if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(map[string]any{
			"notify_count":   gorm.Expr("notify_count + 1"),
			"last_notify_at": time.Now(),
		}).Error; err != nil {
			result = NotifyRetry
			return err
		}
		// 本地内存同步一份，后续 needManual 写 notify_raw 时以 DB 为准不再依赖它。
		o.NotifyCount++

		// 渠道一致性：订单有稳定 ID 时必须与回调渠道一致（历史订单为空则跳过）。
		if o.PayChannelID != "" && o.PayChannelID != ch.ID {
			return s.needManual(tx, &o, fmt.Sprintf("回调渠道 %s 与订单渠道 %s 不一致", ch.ID, o.PayChannelID), &result)
		}
		// 退款回调**按单号认领**：只处理本单这个退款单号的通知。
		//
		// refund_no 在发起退款时由 DeriveRefundNo(out_trade_no) 派生并落库，正常链路必然一致。
		// 不一致说明这是同一订单的**另一笔**退款（或旧单重放）：改状态会把结论覆盖到
		// 不属于它的那一笔上（第二笔「成功」会把第一笔的 refunding 直接推成 refunded）。
		// 空值不拦：其它协议不上报退款单号，保持原口径。
		if payment.IsRefundKind(n.Kind) && n.OutRefundNo != "" && n.OutRefundNo != o.RefundNo {
			metrics.Inc(metrics.NamePayNotifyDropped, "reason", "refund_no_mismatch")
			logger.L().Error("退款回调的退款单号与订单不符，已忽略（不改状态、不动额度）",
				zap.Bool("pay_refund_no_mismatch", true), zap.Uint64("order_id", o.ID),
				zap.String("kind", string(n.Kind)),
				zap.String("got", n.OutRefundNo), zap.String("want", o.RefundNo))
			result = NotifyDone
			return updateNotifyRawTx(tx, &o, n)
		}

		// 商户号一致性：只在双方都有值时比对（部分网关不返回该字段）。
		// M6：渠道配了商户号但回调没报（want 非空、n.MerchantID 空）时写告警但不阻断
		// —— 单币种网关确实可能不回传，阻断会造成误伤；可观测性靠这条 Warn 补齐。
		if want := merchantOf(ch); want != "" {
			if n.MerchantID == "" {
				logger.L().Warn("回调未携带商户号（渠道已配置）",
					zap.String("channel", ch.ID), zap.String("out_trade_no", n.OutTradeNo))
			} else if n.MerchantID != want {
				return s.needManual(tx, &o, "回调商户号与渠道配置不一致", &result)
			}
		}
		// 币种一致性（第一顺位，先于金额）：金额数字相同但币种不同，是 ¥72 与 $72 的区别，
		// 直接入账会按错币种记 7.2 倍额度。空值回落渠道默认币种（单币种协议不上报）。
		{
			notifyCur := strings.ToUpper(strings.TrimSpace(n.Currency))
			if notifyCur == "" {
				notifyCur = payment.DefaultCurrencyOf(ch)
			}
			if notifyCur != currencyOf(&o) {
				return s.needManual(tx, &o,
					fmt.Sprintf("回调币种 %s 与订单币种 %s 不一致", notifyCur, currencyOf(&o)), &result)
			}
		}
		// 金额：**严格相等**。原实现留了 ±1 分容差，等于「少付 1 分也算付清」，
		// 那是可被批量利用的定价漏洞。
		//
		// 退款类回调唯一的例外是「未带金额」：上游退款通知可能只给 status 与单号，
		// 此时不校验金额是安全的 —— 退款回调不产生任何入账，只终结一笔早已按
		// 订单金额发起、且退款金额在响应里核对过的退款。
		//
		// 退款类比对的是**本笔退款额**（orders.refund_amount_cent = 发起退款时的快照），
		// 不是订单总额：用总额只对「全额退款」成立，一旦出现部分退款会被误判成金额不符。
		// refund_amount_cent=0（余额模式 / 从未发起退款）时退回与订单额比对，
		// 因为那种情况下的回调只可能是「上游已退款而我们没发起过」，金额就是订单额。
		wantAmount := o.AmountCent
		if payment.IsRefundKind(n.Kind) && o.RefundAmountCent > 0 {
			wantAmount = o.RefundAmountCent
		}
		if n.AmountCent != wantAmount {
			if n.AmountCent == 0 && payment.IsRefundKind(n.Kind) {
				logger.L().Warn("退款回调未带金额，跳过金额核对（不影响资金）",
					zap.Uint64("order_id", o.ID), zap.String("kind", string(n.Kind)))
			} else {
				return s.needManual(tx, &o,
					fmt.Sprintf("回调金额 %d 分与订单 %d 分不一致", n.AmountCent, wantAmount), &result)
			}
		}

		// 已支付但交易号不同：用户可能扫了两个码、或上游重复扣款。
		// 不能静默 return —— 多收的钱必须留下可查的证据。
		if o.Status == model.OrderPaid && o.TradeNo != nil && n.TradeNo != "" && *o.TradeNo != n.TradeNo {
			// 指标与日志是给运营的两个入口：日志能定位到具体两笔，
			// 指标才能回答「这类多收最近是不是在变多」——只写日志等于没人看。
			metrics.Inc(metrics.NamePayDuplicatePaid, "channel", string(n.Kind))
			logger.L().Error("订单已支付但收到不同交易号（疑似重复扣款）",
				zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo),
				zap.String("paid_trade_no", *o.TradeNo),
				zap.String("new_trade_no", n.TradeNo), zap.Int64("amount_cent", n.AmountCent))
			result = NotifyNeedManual
			return updateNotifyRawTx(tx, &o, n)
		}

		// 终态幂等短路：本订单已终结（paid/refunded/refunding）且收到的是**支付成功**重放
		// 时直接返回，不进入 dedup 检查、不覆盖 notify_raw。
		//
		// 顺序必须在本段之前：dedupHit 会把「同一 trade_no 的合法重放」误判成
		// 「上游交易号已用于其它订单」转 need_manual 并覆盖原始证据；真正需要
		// dedup 拦截的只有「同 trade_no 跨不同订单」的重放，那个场景订单必是 pending。
		//
		// 只短路 NotifyPaid：退款回调（NotifyRefunded/NotifyRefundFailed）即使订单已是
		// refunding，也必须继续走到 onRefunded/onRefundFailed 把状态推进为 refunded/failed。
		if n.Kind == payment.NotifyPaid && (o.Status == model.OrderPaid || o.Status == model.OrderRefunded || o.Status == statusRefunding) {
			// 终态重放本身是正常幂等。但**两边都没有交易号**时，这条路径无法区分
			// 「网关重试同一笔」与「用户真的又付了一次」—— 必须留痕。
			// 否则「付两次只到一次额度」在系统里没有任何线索：
			// 没有 notify_raw、没有计数判据、也不会有对账差异。
			if n.TradeNo == "" && (o.TradeNo == nil || *o.TradeNo == "") {
				logger.L().Warn("支付回调无交易号且订单已终态（无法区分重试与重复支付）",
					zap.Bool("pay_notify_unverifiable_replay", true),
					zap.Uint64("order_id", o.ID),
					zap.String("order_status", o.Status),
					zap.String("out_trade_no", o.OutTradeNo))
			}
			return nil
		}

		// R4：payment_dedup 读侧。这个上游交易号若已入账过**另一笔订单**，
		// 说明是「同一笔支付被重放到不同订单」，不能让它再次入账。
		// 唯一约束只兜底「同渠道同交易号」的写入冲突，覆盖不了这个跨订单重放场景。
		if n.TradeNo != "" && n.Kind == payment.NotifyPaid {
			hit, err := dedupHit(tx, n.TradeNo, o.ID)
			if err != nil {
				result = NotifyRetry
				return err
			}
			if hit {
				return s.needManual(tx, &o, "上游交易号已用于其它订单", &result)
			}
		}

		switch n.Kind {
		case payment.NotifyPaid:
			return s.onPaid(ctx, tx, &o, n, &result)
		case payment.NotifyRefunded:
			return s.onRefunded(tx, &o, n, &result)
		case payment.NotifyRefundFailed:
			return s.onRefundFailed(tx, &o, n, &result)
		case payment.NotifyRefundUnknown:
			return s.onRefundUnknown(tx, &o, n, &result)
		case payment.NotifyRefunding, payment.NotifyIgnored:
			// 只留痕，不改状态：RD 只是「正在退」，未知状态更不能当成功。
			//
			// 但 NotifyIgnored + 订单还在等钱，是最危险的一类**静默丢失**：验签通过、
			// 金额与订单严格相等（上面的金额核对已放行才会走到这里），说明用户确实按
			// 订单金额付了钱，而我们的状态映射不认识那个字符串（上游新增终态、渠道改版、
			// 大小写变化）。不入账、不报错，订单永远 pending —— 钱只存在于上游账单里。
			// 所以必须 ERROR + 计数：这是客诉之前唯一能发现它的入口。
			if n.Kind == payment.NotifyIgnored && o.Status == model.OrderPending {
				metrics.Inc(metrics.NamePayNotifyDropped, "channel", ch.ID, "kind", string(n.Kind))
				logger.L().Error("支付回调状态不可识别且订单仍在等钱（可能已付款但未入账）",
					zap.String("channel", ch.ID), zap.String("protocol", ch.Protocol),
					zap.Uint64("order_id", o.ID),
					zap.String("out_trade_no", n.OutTradeNo),
					zap.Int64("amount_cent", n.AmountCent),
					zap.String("event_type", n.Raw["_event_type"]),
					zap.String("trade_status", n.Raw["trade_status"]))
			}
			return updateNotifyRawTx(tx, &o, n)
		}
		return nil
	})
	if err != nil {
		// 「并发已经入过账」不是失败：事务回滚后订单可能仍是 pending（另一路还没提交状态），
		// 但钱只加了一次。这里在事务**外**确认资金事实，再决定是否算幂等成功。
		//
		// 为什么必须在事务外：23505 会让 PostgreSQL 把当前事务置为 aborted，
		// 事务内任何查询都会报 25P02（旧实现就死在这里，每次都回滚重试）。
		if errors.Is(err, errAlreadyCredited) && s.countRecharge(n.OutTradeNo) > 0 {
			logger.L().Info("支付回调命中并发的重复入账，按幂等处理",
				zap.String("channel", ch.ID), zap.String("out_trade_no", n.OutTradeNo),
				zap.String("source", source))
			return NotifyDone
		}
		logger.L().Error("支付回调处理失败",
			zap.String("channel", ch.ID), zap.String("out_trade_no", n.OutTradeNo),
			zap.String("source", source), zap.Error(err))
		return NotifyRetry
	}
	if result == NotifyOrphan {
		s.recordOrphan(ctx, ch.ID, n)
	}
	return result
}

// onPaid 处理「支付成功」：幂等判断 → 入账 → 邀请奖励 → 状态与交易号落库 → 去重表。
func (s *Service) onPaid(ctx context.Context, tx *gorm.DB, o *model.Order, n *payment.Notification, result *NotifyResult) error {
	if o.Status == model.OrderPaid || o.Status == model.OrderRefunded || o.Status == statusRefunding {
		// refunding 也在此幂等短路：网关可能在退款后继续重放「支付成功」，
		// 把它改回 paid 会让已扣掉的额度凭空复活。
		return nil
	}
	// **资金事实优先于状态机**：状态可以被人工改（need_manual / 手工修数据），
	// 而「有没有 recharge 流水」不会被改。以它为唯一判据，
	// 才能在「已入账但状态没推进」这种半成功状态下避免二次到账。
	credited, err := creditedAlready(tx, o.ID)
	if err != nil {
		*result = NotifyRetry
		return err
	}
	if credited {
		// 已经到过账：只补齐可能缺失的凭证字段，不再动钱。
		*result = NotifyDone
		logger.L().Warn("订单已有到账流水，跳过重复入账（只补凭证）",
			zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo),
			zap.String("status", o.Status))
		return s.patchPaidEvidence(tx, o, n)
	}
	if o.Status == model.OrderExpired {
		// 订单已过期但用户确实付了款：按已支付入账（不丢单），并留下告警。
		logger.L().Warn("过期订单收到支付成功回调，按已支付入账",
			zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo))
	}

	if err := billing.CreditTx(tx, billing.CreditInput{
		UserID: o.UserID, Amount: o.Credited, Type: model.TxnRecharge,
		RefType: "order", RefID: strconv.FormatUint(o.ID, 10), Reason: "充值到账",
		Cap: s.balanceLimitByID(ctx, tx, o.UserID),
	}); err != nil {
		if errors.Is(err, billing.ErrBalanceLimitExceeded) {
			return s.needManual(tx, o, "到账将超出余额上限", result)
		}
		if pgerr.IsUnique(err) {
			// 并发（回调 + 查单）已经入账过。
			//
			// **不能在这里查库**：23505 之后 PostgreSQL 已把事务置为 aborted，
			// 任何后续语句都会报 25P02 —— 旧实现正是在这里 `tx.Select("status")`，
			// 于是这个「幂等成功」分支从来没有生效过，每次都走 NotifyRetry 回滚重试。
			// 现在只抛哨兵，由调用方在事务**外**判定。
			return errAlreadyCredited
		}
		*result = NotifyRetry
		return err
	}

	// 邀请返利：本单支付成功即按「前 N 笔」发放（幂等由 (order_id, role) 唯一索引保证）。
	// 计费基数是**实付**（amount_cent），不是 credited —— 套餐赠送不该再产生返利成本。
	//
	// 必须用「尽力发放」：返利是营销副作用，它的失败不能让「钱已收到」变成
	// 「入账失败、订单卡 pending」。失败留 ERROR + 指标，由对账扫描与补发通道补齐。
	affiliate.RewardBestEffort(tx, affiliate.RewardInput{
		InviteeID: o.UserID, OrderID: o.ID, AmountCent: o.AmountCent,
	})

	now := time.Now()
	updates := map[string]any{
		"status": model.OrderPaid, "paid_at": now, "pay_error": "",
	}
	putNotifyRaw(updates, o, n)
	if n.TradeNo != "" {
		updates["trade_no"] = n.TradeNo
	}
	if n.ChannelOrderID != "" {
		updates["channel_order_id"] = n.ChannelOrderID
	}
	// 原路退款的凭据必须和支付事实一起落库：Stripe 的 cs_（channel_order_id）
	// 退不了款，只有 pi_ 能退。缺这一步，Stripe 订单永远只能走「仅回退余额」。
	if n.PaymentID != "" {
		updates["channel_payment_id"] = n.PaymentID
	}
	if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
		*result = NotifyRetry
		return err
	}
	if n.TradeNo != "" {
		err := tx.Create(&model.PaymentDedup{ChannelID: o.PayChannelID, TradeNo: n.TradeNo}).Error
		if err != nil {
			if !pgerr.IsUnique(err) {
				*result = NotifyRetry
				return err
			}
			// 同一个交易号第二次落库 = 这条回调被重放过（网关重试，或用户对同一单
			// 付了两次）。业务幂等已由订单行锁 + recharge 流水的唯一索引兜住，
			// 但「重放过」这件事必须留痕：否则没有 trade_no 的重复支付
			// 永远不会出现在任何日志与指标里（见 needManual / 终态短路）。
			logger.L().Warn("支付回调重复交易号",
				zap.Bool("pay_notify_replay", true),
				zap.Uint64("order_id", o.ID),
				zap.String("trade_no", n.TradeNo),
				zap.String("order_status", o.Status))
		}
	}
	return nil
}

// onRefunded 处理「上游已退款」（虎皮椒 CD）。
//
// 三种情况必须分开：
//   - refunding：**我们发起的**退款成功了 —— 额度在发起时就已扣，这里只终结状态；
//   - paid：我们从未发起退款，是运营在网关后台手工退的 —— 不能自动扣额度
//     （额度可能已被消费，自动扣会让余额变负），转人工核对；
//   - 其他：上游状态与本地严重不一致，转人工。
func (s *Service) onRefunded(tx *gorm.DB, o *model.Order, n *payment.Notification, result *NotifyResult) error {
	switch o.Status {
	case model.OrderRefunded:
		return nil // 幂等
	case statusRefunding:
		updates := map[string]any{
			"status": model.OrderRefunded, "refund_status": RefundStateSuccess,
			"refund_done_at": time.Now(), "refund_error": "",
		}
		putNotifyRaw(updates, o, n)
		// 退款回调里的 transaction_id 是**支付**交易号，不应覆盖本地已有值（唯一索引）。
		if n.TradeNo != "" && o.TradeNo == nil {
			updates["trade_no"] = n.TradeNo
		}
		if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
			return err
		}
		// 上游确认退款成功 = 钱真的退回去了：返利必须在这里一并回收。
		// 只在 markRefundSuccess 里回收是不够的 —— 回调这条路径**不经过**它。
		reclaimAffiliate(tx, o.ID)
		return nil
	case model.OrderPaid:
		// 我们从未发起退款，是运营在网关后台手工退的。**不能自动扣额度**
		// （额度可能已被消费，自动扣会让余额变负），但必须让这笔「钱退了、额度还在」
		// 的差异在管理台可见。
		//
		// **绝不能把 status 改成 need_manual**：need_manual 不在终态集合里
		// （支付成功的短路只认 paid/refunded/refunding），一旦改过去，网关重放
		// 「支付成功」就会再走一次 onPaid → CreditTx → 同一笔钱入账两次。
		// 可见性改用 refund_status=upstream_only（支付事实不变，退款事实另记一列）。
		msg := "上游已退款但本地未发起退款，需人工核对（确认后用「仅回退平台余额」扣回额度）"
		*result = NotifyNeedManual
		logger.L().Error("上游已退款但本地未发起退款，转人工",
			zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo))
		updates := map[string]any{
			"refund_status": RefundStateUpstreamOnly,
			"refund_error":  textx.TruncateRunes(msg, 200),
		}
		putNotifyRaw(updates, o, n)
		if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
			return err
		}
		// 钱已经在网关后台退回用户了，返利必须一并作废 —— 否则用户既拿回本金
		// 又保留返利，而这条路径**不经过** refund 服务的任何一个回收点。
		// 之前漏了这一步，对账也只认 refund_status='success'，两边都看不见。
		reclaimAffiliate(tx, o.ID)
		return nil
	default:
		// pending/expired 收到已退款：上游状态与本地严重不一致。
		return s.needManual(tx, o, "未入账订单收到已退款回调", result)
	}
}

// onRefundFailed 处理「上游确认退款没发生」（虎皮椒 UD / 微信 REFUND.CLOSED）。
//
// 只有 refunding 才需要动作：把发起时扣掉的额度补偿回去，订单回到 paid。
// 其余状态只留痕 —— 顺序在这里比什么都重要：先落状态再回滚会导致重复回调重复入账。
//
// **只有「资金仍在商户」的状态才能走到这里**。微信 REFUND.ABNORMAL 走 onRefundUnknown：
// 那是「钱已划出、停在中间账户」，回滚额度会让用户既拿额度又拿上游退款。
func (s *Service) onRefundFailed(tx *gorm.DB, o *model.Order, n *payment.Notification, result *NotifyResult) error {
	if o.Status != statusRefunding {
		return updateNotifyRawTx(tx, o, n)
	}
	// 文案不写死渠道术语（旧实现固定成虎皮椒的「（UD）」，微信订单也会显示这句话）。
	msg := "上游退款失败"
	if n.Raw != nil {
		if v := strings.TrimSpace(n.Raw["errmsg"]); v != "" {
			msg = "上游退款失败：" + v
		}
	}
	if err := refundRevertBalanceTx(tx, o, msg); err != nil {
		*result = NotifyRetry
		return err
	}
	updates := map[string]any{
		"status": model.OrderPaid, "refund_status": RefundStateFailed,
		"refund_done_at": time.Now(),
		"refund_error":   textx.TruncateRunes(msg, 200),
	}
	putNotifyRaw(updates, o, n)
	return tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error
}

// onRefundUnknown 处理「退款已受理、但当前状态既非成功也非终态失败」（微信 REFUND.ABNORMAL）。
//
// 与 onRefundFailed 的差别是**资金事实**：ABNORMAL 时钱已经离开商户账户，停在微信的
// 退款中间账户，等商户在商户平台二选一处理（退用户银行卡 / 退回结算账户），成功后流转
// 成 SUCCESS。所以这里只更新证据与文案：
//
//   - 不改 status：必须保持 refunding，退款巡检只领 refunding 的单，改了就不再查；
//   - 不动余额：回滚会让用户在收到上游退款的同时保留平台额度（双向损失）；
//   - 不写 refund_done_at：它不是终态，写上去会让管理台误以为是已结案。
//
// refund_status 保持发起时的 pending（refund.go 里已置），运营看到「待确认 + 异常原因」
// 后去商户平台核销，再调 /api/admin/order/refund/confirm 收尾。
func (s *Service) onRefundUnknown(tx *gorm.DB, o *model.Order, n *payment.Notification, result *NotifyResult) error {
	if o.Status != statusRefunding {
		// 已经不在退款中（人工已确认 / 巡检已收尾 / 上游随后推了 SUCCESS）：只补证据。
		return updateNotifyRawTx(tx, o, n)
	}
	msg := "退款异常：资金可能已退出，需到商户平台处理（勿回滚额度）"
	if n.Raw != nil {
		if v := strings.TrimSpace(n.Raw["_event_type"]); v != "" {
			msg = "退款异常（" + v + "）：资金可能已退出，需到商户平台处理（勿回滚额度）"
		}
	}
	updates := map[string]any{"refund_error": textx.TruncateRunes(msg, 200)}
	putNotifyRaw(updates, o, n)
	if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
		*result = NotifyRetry
		return err
	}
	logger.L().Error("退款异常：资金可能已退出，已转人工处理（绝不回滚额度）",
		zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo),
		zap.String("refund_error", msg))
	return nil
}

// errAlreadyCredited 表示「这一单已经入过账」（并发/重放），由 settle 在事务外判定。
//
// 必须是哨兵而不是普通错误：它要能穿过 `Transaction()` 的回滚边界，且**不能**被
// 当成 NotifyRetry（那会让网关无限重试）。为什么不能在事务内直接返回 NotifyDone：
// 23505 已经把事务置为 aborted，此时读不到任何东西。
var errAlreadyCredited = errors.New("order: 该订单已入账")

// creditedAlready 判断订单是否已有到账流水 —— **入账判重的唯一判据**。
//
// 为什么不用订单状态：状态会被人工改（need_manual、手工修库），而且
// 「入账成功但状态更新失败」这种半成功状态真实存在（对账的 pending_but_credited）。
// 流水是资金事实，改不了也对不了假。
func creditedAlready(tx *gorm.DB, orderID uint64) (bool, error) {
	var n int64
	if err := tx.Model(&model.Transaction{}).
		Where("ref_type = ? AND ref_id = ? AND type = ?",
			"order", strconv.FormatUint(orderID, 10), "recharge").
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// countRecharge 按订单号数一遍到账流水（事务外幂等判定用）。
func (s *Service) countRecharge(outTradeNo string) int64 {
	var n int64
	err := s.db.Model(&model.Transaction{}).
		Joins("JOIN orders o ON o.id::text = transactions.ref_id").
		Where("o.out_trade_no = ? AND transactions.ref_type = ? AND transactions.type = ?",
			outTradeNo, "order", "recharge").
		Count(&n).Error
	if err != nil {
		logger.L().Warn("幂等判定查询失败，按失败处理（让网关重试）",
			zap.String("out_trade_no", outTradeNo), zap.Error(err))
		return 0
	}
	return n
}

// patchPaidEvidence 在「钱已入账」的前提下补齐凭证字段，绝不改金额与状态机。
//
// 典型场景：入账成功但随后更新订单状态时进程崩了 → 重放回调走到这里。
// 只补空值，不覆盖已有值（尤其是 trade_no：它有唯一索引，覆盖成别的单号会直接报错）。
func (s *Service) patchPaidEvidence(tx *gorm.DB, o *model.Order, n *payment.Notification) error {
	updates := map[string]any{}
	if o.TradeNo == nil && n.TradeNo != "" {
		updates["trade_no"] = n.TradeNo
	}
	if o.ChannelOrderID == "" && n.ChannelOrderID != "" {
		updates["channel_order_id"] = n.ChannelOrderID
	}
	if o.ChannelPaymentID == "" && n.PaymentID != "" {
		updates["channel_payment_id"] = n.PaymentID
	}
	if o.Status != model.OrderPaid && o.Status != model.OrderRefunded && o.Status != statusRefunding {
		// 钱到了就必须是 paid：need_manual/expired/pending 都是「没推进到终态」的错位。
		updates["status"] = model.OrderPaid
		updates["paid_at"] = time.Now()
	}
	if len(updates) == 0 {
		return nil
	}
	return tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error
}

// isRefundKind 判断是否为退款类回调（这些都不产生入账，金额可缺省）。
//
// 必须与 `settle` 的 switch 覆盖同一组 Kind：漏一个，那条回调的「未带金额」就会被
// 上面的金额严格相等校验判成不符 → 转人工（而它本来只是退款状态变更）。

// dedupHit 判断一个上游交易号是否已入账到**别的**订单（R4）。
//
// 旧实现查的是 `payment_dedup.trade_no`（全局唯一，不含订单维度），于是
// 「同一 trade_no 回到同一订单的合法重放」也会被判成「已用于其它订单」→ 转人工，
// 钱收了额度不到账。唯一索引本身已兜住「同渠道同交易号重复写入」，
// 这里要回答的是另一个问题：**这笔钱是不是已经记到别的单上了**。
// 直接查 orders（trade_no 有唯一索引），天然带订单维度，且不需要迁移。
func dedupHit(tx *gorm.DB, tradeNo string, orderID uint64) (bool, error) {
	var n int64
	if err := tx.Model(&model.Order{}).
		Where("trade_no = ? AND id <> ? AND deleted_at IS NULL", tradeNo, orderID).
		Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// needManual 把订单转人工，并记录原因。
//
// 不改已 paid/refunded 的订单状态：钱已经到账，把它改成 need_manual 只会让用户看不到已支付。
// **也不覆盖 notify_raw**：那是原始回调证据，转人工时最需要它，覆盖成一句中文原因
// 等于把唯一的物证毁掉（原因写 pay_error，两者在同一行、互不挤占）。
func (s *Service) needManual(tx *gorm.DB, o *model.Order, reason string, result *NotifyResult) error {
	*result = NotifyNeedManual
	logger.L().Error("支付回调转人工",
		zap.Uint64("order_id", o.ID), zap.String("out_trade_no", o.OutTradeNo), zap.String("reason", reason))
	updates := map[string]any{"pay_error": textx.TruncateRunes(reason, 200)}
	// **refunding 也必须保护**：漏掉它会造成同一笔退款扣两遍 ——
	// 退款后网关重放（本函数上游的字段校验在终态短路之前）→ status 被打成
	// need_manual → ConfirmRefund（要求 refunding）永久拒绝，
	// 而 Refund 接受 need_manual 且判得到 recharge 流水 → CreditTx 再扣一次。
	switch o.Status {
	case model.OrderPaid, model.OrderRefunded, statusRefunding:
		// 钱已经到账或正在退：只记原因，绝不改状态。
	default:
		updates["status"] = model.OrderNeedManual
	}
	return tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error
}

// recordOrphan 记录「验签通过但订单不存在」的回调。
//
// 应答是 success（否则网关重试 6 次后放弃，而本地根本没有订单可变出来），
// 但必须落库 + ERROR：这是「我方丢单」唯一可观测的证据。
func (s *Service) recordOrphan(ctx context.Context, channelID string, n *payment.Notification) {
	logger.L().Error("支付回调验签通过但订单不存在（疑似我方丢单）",
		zap.String("channel", channelID), zap.String("out_trade_no", n.OutTradeNo),
		zap.String("trade_no", n.TradeNo), zap.Int64("amount_cent", n.AmountCent))
	row := model.PayOrphanNotify{
		ChannelID: channelID, OutTradeNo: n.OutTradeNo, TradeNo: n.TradeNo,
		AmountCent: n.AmountCent, Raw: textx.TruncateRunes(rawJSON(n), 4000),
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		logger.L().Warn("写孤儿回调记录失败", zap.Error(err))
	}
}

// merchantOf 取渠道配置里的商户号 / 商户身份，用于回调的商户一致性校验。
//
// 四套配置键各自对应一个「上游认的身份」：
//   - epay   → pid（易支付的商户号）
//   - xunhupay → app_id（虎皮椒的 APPID，微信/支付宝两套）
//   - wxpay  → mch_id（微信支付商户号，回调 JSON 里的 mchid）
//   - alipay → app_id（支付宝应用的 APPID）
func merchantOf(ch payment.Channel) string {
	switch payment.ParseProtocol(ch.Protocol) {
	case payment.ProtocolXunhuPay:
		return strings.TrimSpace(ch.Params.AppID)
	case payment.ProtocolEpay:
		return strings.TrimSpace(ch.Params.PID)
	case payment.ProtocolWxpay:
		return strings.TrimSpace(ch.Params.MchID)
	case payment.ProtocolAlipay:
		return strings.TrimSpace(ch.Params.AppID)
	}
	return ""
}

// rawJSON 把回调原始参数序列化后落库（截断，避免超长触发 22001）。
func rawJSON(n *payment.Notification) string {
	if n == nil || n.Raw == nil {
		return ""
	}
	b, err := json.Marshal(n.Raw)
	if err != nil {
		return ""
	}
	return textx.TruncateRunes(string(b), 4000)
}

// putNotifyRaw 把回调原文塞进 updates —— **只在该订单还没有原文时**。
//
// notify_raw 是「这笔钱是怎么进来的」的原始物证：渠道、上游交易号、金额、付款方。
// 一份订单会收到多次回调（支付成功 → 退款成功/失败/异常 → 网关重放），若每次都覆盖，
// 后到的退款通知会把支付原文顶掉 —— 客诉或对账时想还原「当时上游到底说了什么」
// 就再也没有了。所以只保留**首次**，后续事件写进 refund_error / notify_count /
// payment_events 各自的列或表。
//
// 顺序上安全：所有调用点都在 settle 的行锁事务内，o.NotifyRaw 是最新快照。
func putNotifyRaw(updates map[string]any, o *model.Order, n *payment.Notification) {
	if o == nil || strings.TrimSpace(o.NotifyRaw) != "" {
		return
	}
	if raw := rawJSON(n); raw != "" {
		updates["notify_raw"] = raw
	}
}

// updateNotifyRawTx 是「只补原文、不动别的列」的落库封装（已有原文时什么都不做）。
func updateNotifyRawTx(tx *gorm.DB, o *model.Order, n *payment.Notification) error {
	updates := map[string]any{}
	putNotifyRaw(updates, o, n)
	if len(updates) == 0 {
		return nil
	}
	return tx.Model(&model.Order{}).Where("id = ?", o.ID).Updates(updates).Error
}

// keyList 只回显回调的键名，不回显值（值里可能有商户号/金额）。
func keyList(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	return strings.Join(keys, ",")
}
