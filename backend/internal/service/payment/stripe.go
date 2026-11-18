package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v81"
	checkoutsession "github.com/stripe/stripe-go/v81/checkout/session"
	refundclient "github.com/stripe/stripe-go/v81/refund"
	"github.com/stripe/stripe-go/v81/webhook"
)

// Stripe 官方直连（Checkout Session hosted 页）。支持多币种（CNY/USD），
// 是「切美元结算」时唯一能真正收美元的协议。

// stripeSupportedCurrencies 是 Stripe 收款币种白名单。
//
// 只收**两位小数**的币种：订单金额以最小单位整数存储（AmountCent），
// Stripe 的 unit_amount 也是最小单位。JPY（零小数）、KWD（三位小数）等
// 需要额外的 exponent 换算，本系统不支持，配置时直接拒绝而不是静默算错。
var stripeSupportedCurrencies = map[string]bool{"CNY": true, "USD": true}

// stripeBackend 构造 per-channel 的 Stripe backend。
//
// 为什么不能全局 stripe.Key：多渠道多密钥是常态（不同商户号、不同 key）。
// 全局 Key 会让「渠道 A 下单却用渠道 B 的 key 签名」，账单错乱且难排查。
// HTTP 客户端 15s 超时，与其它网关一致。
func stripeBackend() stripe.Backend {
	httpClient := &http.Client{Timeout: 15 * time.Second}
	return stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		HTTPClient: httpClient,
	})
}

// stripeSessionClient 构造 per-channel 的 Checkout Session client。
func stripeSessionClient(ch Channel) (*checkoutsession.Client, error) {
	key := strings.TrimSpace(ch.Params.SecretKey)
	if key == "" {
		return nil, fmt.Errorf("%w: 缺少 Stripe secret key", ErrNotConfigured)
	}
	return &checkoutsession.Client{B: stripeBackend(), Key: key}, nil
}

// stripeCurrency 取订单币种并校验是否在 Stripe 收款白名单内。
//
// 空值返回 ""（由调用方 stripeCreate 按 ErrNotConfigured 拒绝），**绝不静默回落 USD**：
// 下单侧已透传订单币种，这里拿不到币种只可能是「渠道漏配 settle_currency」的配置错误，
// 静默按美元收会把人民币订单建成美元单（¥72→$72），是 7.2 倍超额扣款。
func stripeCurrency(raw string) string {
	cur := normCurrency(raw)
	if cur == "" {
		return ""
	}
	if !stripeSupportedCurrencies[cur] {
		return ""
	}
	return strings.ToLower(cur)
}

// stripeCreate 创建 Checkout Session，返回 hosted 支付页 URL。
func stripeCreate(ctx context.Context, ch Channel, req CreateRequest) (*CreateResult, error) {
	client, err := stripeSessionClient(ch)
	if err != nil {
		return nil, err
	}
	currency := stripeCurrency(req.Currency)
	if currency == "" {
		return nil, fmt.Errorf("%w: Stripe 只支持 CNY/USD（两位小数币种）", ErrNotConfigured)
	}
	if req.AmountCent <= 0 {
		return nil, ErrInvalidAmount
	}

	expire := effectiveExpireAt(req.ExpireAt)
	successURL := req.ReturnURL
	if strings.TrimSpace(successURL) == "" {
		successURL = req.NotifyURL
	}
	mode := string(stripe.CheckoutSessionModePayment)
	quantity := int64(1)

	params := &stripe.CheckoutSessionParams{
		Mode:               &mode,
		ClientReferenceID:  stripe.String(req.OutTradeNo),
		SuccessURL:         stripe.String(successURL),
		CancelURL:          stripe.String(successURL),
		ExpiresAt:          stripe.Int64(expire.Unix()),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Quantity: &quantity,
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String(currency),
					UnitAmount: stripe.Int64(req.AmountCent),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(req.Subject),
					},
				},
			},
		},
		Metadata: map[string]string{
			"out_trade_no": req.OutTradeNo,
		},
	}
	sess, err := client.New(params)
	if err != nil {
		return nil, stripeError(err, "下单")
	}
	if sess == nil || strings.TrimSpace(sess.URL) == "" {
		return nil, ErrNoPayURL
	}
	// Stripe 没有二维码，只有 hosted 页跳转地址。
	return &CreateResult{
		PayURL:         strings.TrimSpace(sess.URL),
		ChannelOrderID: sess.ID,
		ExpiresAt:      expire,
	}, nil
}

// stripeQuery 按 Checkout Session ID 查单（channelOrderID = cs_...）。
func stripeQuery(ctx context.Context, ch Channel, outTradeNo, channelOrderID string) (*Notification, error) {
	client, err := stripeSessionClient(ch)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(channelOrderID)
	if id == "" {
		return nil, fmt.Errorf("%w: 缺少 Checkout Session ID", ErrNotConfigured)
	}
	sess, err := client.Get(id, nil)
	if err != nil {
		return nil, stripeError(err, "查单")
	}
	if sess == nil {
		return nil, nil
	}
	var payID string
	if sess.PaymentIntent != nil {
		payID = sess.PaymentIntent.ID
	}
	return &Notification{
		OutTradeNo:     sess.ClientReferenceID,
		TradeNo:        sess.ID,
		ChannelOrderID: sess.ID,
		PaymentID:      payID,
		AmountCent:     sess.AmountTotal,
		Currency:       strings.ToUpper(string(sess.Currency)),
		Kind:           stripePaymentStatusKind(sess.PaymentStatus),
		Raw: map[string]string{
			"payment_status": string(sess.PaymentStatus),
			"session_id":     sess.ID,
			"source":         "query",
		},
	}, nil
}

// stripePaymentStatusKind 把 Stripe 支付状态映射成归一语义。
func stripePaymentStatusKind(status stripe.CheckoutSessionPaymentStatus) NotifyKind {
	switch status {
	case stripe.CheckoutSessionPaymentStatusPaid:
		return NotifyPaid
	}
	return NotifyIgnored
}

// stripeParseNotify 用 webhook secret 验签并把 checkout.session.completed 映射成 Notification。
//
// Stripe 回调是 JSON + Stripe-Signature 头（`t=... ,v1=...`），签名对象是
// 逐字节原文，必须拿原始 body。webhook_secret 缺失时直接拒绝（配置问题，
// 重试一万次也不会好）。
func stripeParseNotify(ch Channel, req NotifyRequest) (*Notification, error) {
	secret := strings.TrimSpace(ch.Params.WebhookSecret)
	if secret == "" {
		return nil, fmt.Errorf("%w: 缺少 Stripe webhook secret", ErrNotConfigured)
	}
	sig := req.Header.Get("Stripe-Signature")
	if strings.TrimSpace(sig) == "" {
		return nil, ErrSignInvalid
	}
	// L2：严格校验 API 版本（不忽略版本不匹配）。SDK 升级后 webhook 签名的版本字段若
	// 与 secret 不匹配，说明是配置/升级问题，应转 fail 可观测，而不是静默放行。
	event, err := webhook.ConstructEvent(req.Body, sig, secret)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignInvalid, err)
	}

	// 只处理 checkout.session.completed（支付成功）。其它事件（如 charge.refunded
	// 的退款语义）先忽略：退款结果由主动查单确认，不靠事件驱动，避免两套状态机分叉。
	if event.Type != "checkout.session.completed" {
		return &Notification{Kind: NotifyIgnored, Raw: map[string]string{"event_type": string(event.Type)}}, nil
	}

	var sess stripe.CheckoutSession
	if err := unmarshalEvent(event, &sess); err != nil {
		return nil, fmt.Errorf("%w: checkout session 解析失败: %v", ErrNotifyStatus, err)
	}
	currency := strings.ToUpper(string(sess.Currency))
	if currency == "" {
		currency = "USD"
	}
	// payment_intent 是退款唯一认的凭据；老 API 版本/异常会话下可能为空，
	// 此时留空并由退款侧拒绝原路退款（宁可让人工处理，也不发一次必败的请求）。
	var payID string
	if sess.PaymentIntent != nil {
		payID = sess.PaymentIntent.ID
	}
	return &Notification{
		OutTradeNo:     sess.ClientReferenceID,
		TradeNo:        sess.ID,
		ChannelOrderID: sess.ID,
		PaymentID:      payID,
		AmountCent:     sess.AmountTotal,
		Currency:       currency,
		Kind:           stripePaymentStatusKind(sess.PaymentStatus),
		// EventID 是事件级幂等锚（evt_...）：Stripe 会重放同一 event，payment_events 去重靠它。
		EventID: event.ID,
		Raw: map[string]string{
			"event_type":     string(event.Type),
			"payment_status": string(sess.PaymentStatus),
			"session_id":     sess.ID,
			"source":         "webhook",
		},
	}, nil
}

// unmarshalEvent 把 Stripe event.Data.Object 反序列化到目标结构。
//
// Stripe 把对象存在 Data.Raw（json.RawMessage），不同事件类型对应不同结构体。
// 用 encoding/json 而不是类型断言：event.Data.Object 是 interface{}，
// 断言到具体结构体会在 SDK 版本变化时静默失败。
func unmarshalEvent(event stripe.Event, out any) error {
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return errors.New("event.Data.Raw 为空")
	}
	return json.Unmarshal(event.Data.Raw, out)
}

// stripeRefund 发起全额退款。
func stripeRefund(ctx context.Context, ch Channel, req RefundRequest) (*RefundResult, error) {
	if req.AmountCent <= 0 {
		return nil, ErrInvalidAmount
	}
	if strings.TrimSpace(ch.Params.SecretKey) == "" {
		return nil, fmt.Errorf("%w: 缺少 Stripe secret key", ErrNotConfigured)
	}
	// Stripe 退款按 payment_intent（pi_...），**不是** checkout session（cs_...）。
	// 旧实现直接把 channel_order_id（= cs_...）当 pi_ 传过去，Stripe 一律拒绝，
	// 于是原路退款从来没成功过。
	//
	// 凭据由 order 侧从 orders.channel_payment_id 取（回调/查单时落的 session.payment_intent）。
	// 这里再兜一次空值：缺凭据时宁可直接报配置缺失，也不要发一次注定失败、
	// 却已被"先扣额度"的请求。
	pi := strings.TrimSpace(req.PaymentID)
	if pi == "" {
		return nil, fmt.Errorf("%w: 缺少 Stripe payment_intent（订单未记录支付凭据）", ErrNotConfigured)
	}
	// 显式下发金额：不带 Amount 时 Stripe 退「剩余可退额」，账面自洽但台账失真 ——
	// 一旦上游已有部分退款，我们会把「退了一部分」记成「全额退完」。
	// 带上金额后，金额不符会走 refundAmountMismatch → 未知态 → 人工，而不是静默成功。
	rc := &refundclient.Client{B: stripeBackend(), Key: strings.TrimSpace(ch.Params.SecretKey)}
	params := &stripe.RefundParams{
		PaymentIntent: stripe.String(pi),
		Amount:        stripe.Int64(req.AmountCent),
	}
	refund, err := rc.New(params)
	if err != nil {
		return nil, stripeError(err, "退款")
	}
	if refund == nil {
		return nil, ErrRefundUnknown
	}
	status := RefundAccepted
	switch refund.Status {
	case "succeeded":
		status = RefundDone
	case "failed", "canceled":
		status = RefundDeclined
	}
	// TradeNo 回填本次退款实际作用的 payment_intent（与入参 pi 同值，取响应为准）。
	if refund.PaymentIntent != nil && refund.PaymentIntent.ID != "" {
		pi = refund.PaymentIntent.ID
	}
	// 金额核对：显式下发金额后，上游实际退回额必须等于它。
	// 不符（上游按剩余可退额调整/部分退款）→ 未知态，交人工，不判成功。
	if refundAmountMismatch(req.AmountCent, refund.Amount) {
		return nil, fmt.Errorf("%w: 上游退款 %s 与订单 %s 不一致",
			ErrRefundUnknown, FormatCentToCNY(refund.Amount), FormatCentToCNY(req.AmountCent))
	}
	return &RefundResult{
		Status:       status,
		RefundNo:     refund.ID,
		RefundedCent: refund.Amount,
		TradeNo:      pi,
		Raw: map[string]string{
			"status":         string(refund.Status),
			"refund_id":      refund.ID,
			"payment_intent": pi,
		},
	}, nil
}

// stripeError 把 Stripe SDK 错误映射到本包错误集。
func stripeError(err error, action string) error {
	if err == nil {
		return nil
	}
	var serr *stripe.Error
	if !errors.As(err, &serr) {
		return fmt.Errorf("Stripe %s失败: %w", action, err)
	}
	// 4xx = 请求/配置错误（不可重试）；5xx/429 = 繁忙（可重试）。
	if serr.HTTPStatusCode >= 500 || serr.HTTPStatusCode == 429 {
		return fmt.Errorf("%w: Stripe %s繁忙 HTTP %d: %s", ErrGatewayBusy, action, serr.HTTPStatusCode, serr.Msg)
	}
	return fmt.Errorf("%w: Stripe %s被拒: %s", ErrGatewayRejected, action, serr.Msg)
}
