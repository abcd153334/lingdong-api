package payment

import (
	"context"
	"fmt"
	"strings"

	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
)

// 微信回调：**不手写验签与解密**。
//
// `core/notify.NewRSANotifyHandler` + `ParseNotifyRequest` 已经完整实现了三件事：
//  1. 头部字段完整性校验（Wechatpay-Serial/Signature/Timestamp/Nonce）；
//  2. 验签，且**内置 5 分钟时间窗**——手写版最容易漏的就是它，漏了就等于回调可无限重放；
//  3. `resource` 的 AEAD_AES_256_GCM 解密与明文反序列化。
//
// 我们只负责：按回调头选验签器、把明文映射成 Notification。

// wxNotifyTx 是解密后的业务明文。
//
// 支付与退款两类事件共用这个结构：字段名不重叠（支付没有 out_refund_no，
// 退款没有 trade_state），所以一个结构体能覆盖两者，不必分支解析。
type wxNotifyTx struct {
	Mchid         string `json:"mchid"`
	Appid         string `json:"appid"`
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	TradeState    string `json:"trade_state"`
	SuccessTime   string `json:"success_time"`
	Amount        struct {
		Total      int64  `json:"total"`
		PayerTotal int64  `json:"payer_total"`
		Refund     int64  `json:"refund"`
		Currency   string `json:"currency"`
	} `json:"amount"`
	OutRefundNo  string `json:"out_refund_no"`
	RefundID     string `json:"refund_id"`
	RefundStatus string `json:"refund_status"`
}

// wxpayParseNotify 验签 + 解密 + 映射。
func wxpayParseNotify(ch Channel, req NotifyRequest) (*Notification, error) {
	creds, err := parseWxpayChannel(ch)
	if err != nil {
		return nil, err
	}
	verifier, err := resolveNotifyVerifier(creds, req.Header.Get("Wechatpay-Serial"))
	if err != nil {
		return nil, err
	}
	handler, err := notify.NewRSANotifyHandler(creds.APIv3Key, verifier)
	if err != nil {
		return nil, fmt.Errorf("%w: APIv3 密钥不可用（必须 32 位）: %v", ErrNotConfigured, err)
	}

	var tx wxNotifyTx
	parsed, perr := handler.ParseNotifyRequest(context.Background(), notifyHTTPRequest(req), &tx)
	if perr != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignInvalid, perr)
	}

	eventType := strings.ToUpper(strings.TrimSpace(parsed.EventType))
	kind := wxEventKind(eventType)

	raw := map[string]string{
		"_channel":     "wxpay",
		"_event_type":  eventType,
		"_kind":        string(kind),
		"mchid":        tx.Mchid,
		"appid":        tx.Appid,
		"out_trade_no": tx.OutTradeNo,
	}
	if tx.TransactionID != "" {
		raw["transaction_id"] = tx.TransactionID
	}
	if tx.TradeState != "" {
		raw["trade_state"] = tx.TradeState
	}
	if tx.OutRefundNo != "" {
		raw["out_refund_no"] = tx.OutRefundNo
	}
	if tx.RefundID != "" {
		raw["refund_id"] = tx.RefundID
	}
	if tx.RefundStatus != "" {
		raw["refund_status"] = tx.RefundStatus
	}
	if tx.SuccessTime != "" {
		raw["success_time"] = tx.SuccessTime
	}
	// 解密后的完整明文留一份（截断在 rawJSON 落库时统一做）：
	// 出问题时这是唯一能还原「上游到底说了什么」的证据。
	if parsed.Resource != nil && parsed.Resource.Plaintext != "" {
		raw["_plaintext"] = parsed.Resource.Plaintext
	}
	if parsed.ID != "" {
		raw["_notify_id"] = parsed.ID
	}

	return &Notification{
		OutTradeNo:     strings.TrimSpace(tx.OutTradeNo),
		TradeNo:        strings.TrimSpace(tx.TransactionID),
		ChannelOrderID: strings.TrimSpace(tx.TransactionID),
		// EventID 用通知 ID（EV-...）：微信重试同一条通知时复用它，不同事件各有各的 ID，
		// 正好是 payment_events 的幂等锚。留空的话微信回调完全不进事件表，
		// 事后只能靠 notify_raw 一条条翻（Stripe 侧一直有，两边口径不一致）。
		EventID: strings.TrimSpace(parsed.ID),
		// 金额口径按事件分：
		//   - 支付：amount.total（订单总额），**不是 payer_total** —— 用户用了代金券时
		//     payer_total < total，拿它比对会把正常订单判成金额不符转人工；
		//   - 退款：amount.refund（**本笔**退款额）。用 total 只对「全额退款」成立，
		//     一旦出现部分退款，就会被判成「金额不符」转人工。
		AmountCent: wxAmountOf(kind, &tx),
		// 退款单号：让订单侧能认领到具体是哪一笔退款（见 order.settle 的单号闸）。
		OutRefundNo: strings.TrimSpace(tx.OutRefundNo),
		MerchantID:  strings.TrimSpace(tx.Mchid),
		Kind:        kind,
		Raw:         raw,
	}, nil
}

// wxAmountOf 按事件类型取用于对账的金额（分）。
//
// 退款回调若没带 amount.refund（0），回落到 amount.total：全额退款场景两者相等，
// 而退回 0 会让「零金额」被当成"未带金额"而跳过一切核对。
func wxAmountOf(kind NotifyKind, tx *wxNotifyTx) int64 {
	if IsRefundKind(kind) && tx.Amount.Refund > 0 {
		return tx.Amount.Refund
	}
	return tx.Amount.Total
}

// wxEventKind 映射事件类型。
//
// 三种退款事件的处置**必须区分**（资金安全核心）：
//
//	REFUND.SUCCESS  → NotifyRefunded       钱已到用户
//	REFUND.CLOSED   → NotifyRefundFailed   退款关闭、资金留在商户 → 可回滚额度
//	REFUND.ABNORMAL → NotifyRefundUnknown  原路退回失败、资金停在微信中间账户 → **绝不回滚**
//
// ABNORMAL 以前和 CLOSED 一起归到「失败」，会让 onRefundFailed 把额度加回用户、
// 订单回 paid；而运营随后还会在商户平台把这笔异常退款人工退给用户 —— 用户既拿额度
// 又拿真钱。wxpay_refund.go（退款申请/查询入口）对同一状态一直是保持 refunding 的，
// 两处口径现在对齐了。
func wxEventKind(eventType string) NotifyKind {
	switch eventType {
	case "TRANSACTION.SUCCESS":
		return NotifyPaid
	case "REFUND.SUCCESS":
		return NotifyRefunded
	case "REFUND.ABNORMAL":
		return NotifyRefundUnknown
	case "REFUND.CLOSED":
		return NotifyRefundFailed
	}
	return NotifyIgnored
}
