package payment

import (
	"fmt"
	"strings"
)

// alipayParseNotify 处理支付宝异步通知（form-urlencoded）。
//
// 验签规则与请求签名**不同**（剔除 sign 与 sign_type），必须走 alipayVerifyNotify，
// 不能复用 alipaySignContent 的默认剔除集。
func alipayParseNotify(ch Channel, req NotifyRequest) (*Notification, error) {
	c, err := parseAlipayChannel(ch)
	if err != nil {
		return nil, err
	}
	if len(req.Params) == 0 {
		return nil, fmt.Errorf("%w: 通知没有任何参数", ErrSignInvalid)
	}
	if err := alipayVerifyNotify(req.Params, c.PublicKey); err != nil {
		return nil, err
	}

	outTradeNo := strings.TrimSpace(req.Params["out_trade_no"])
	if outTradeNo == "" {
		return nil, fmt.Errorf("%w: 通知缺少 out_trade_no", ErrSignInvalid)
	}
	amount, err := ParseCNYToCent(req.Params["total_amount"])
	if err != nil {
		// 退款/关闭类通知可能只带 refund_fee；只有「已支付」必须能对上金额。
		amount = 0
	}
	kind := alipayKind(strings.TrimSpace(req.Params["trade_status"]), req.Params["send_pay_date"])

	raw := make(map[string]string, len(req.Params)+1)
	for k, v := range req.Params {
		// sign 不落库：它是签名本身，对排查无意义，却会把 notify_raw 撑长。
		if k == "sign" {
			continue
		}
		raw[k] = v
	}
	raw["_channel"] = "alipay"
	raw["_kind"] = string(kind)

	return &Notification{
		OutTradeNo: outTradeNo,
		TradeNo:    strings.TrimSpace(req.Params["trade_no"]),
		AmountCent: amount,
		MerchantID: strings.TrimSpace(req.Params["app_id"]),
		Kind:       kind,
		Raw:        raw,
	}, nil
}
