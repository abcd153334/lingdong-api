package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 支付宝退款：`alipay.trade.refund` + `alipay.trade.fastpay.refund.query`。
//
// 支付宝**没有退款异步通知**（异步通知只有 trade_status_sync），所以退款终态
// 只能靠「同步响应 + 退款查询巡检」推进。这也是 order.RefundQueryOnce 必须存在的理由。

// alipayRefundBiz 是退款请求的业务参数。
type alipayRefundBiz struct {
	OutTradeNo   string `json:"out_trade_no,omitempty"`
	TradeNo      string `json:"trade_no,omitempty"`
	RefundAmount string `json:"refund_amount"`  // 元，两位小数
	OutRequestNo string `json:"out_request_no"` // 幂等键：重试必须同号
	RefundReason string `json:"refund_reason,omitempty"`
}

// alipayRefundResp 是退款响应。
type alipayRefundResp struct {
	alipayNode
	FundChange string `json:"fund_change"` // Y 表示本次确实发生了资金变动
	RefundFee  string `json:"refund_fee"`
	TradeNo    string `json:"trade_no"`
	OutTradeNo string `json:"out_trade_no"`
}

// alipayRefund 发起全额退款。
func alipayRefund(ctx context.Context, ch Channel, req RefundRequest) (*RefundResult, error) {
	c, err := parseAlipayChannel(ch)
	if err != nil {
		return nil, err
	}
	biz := alipayRefundBiz{
		OutTradeNo:   req.OutTradeNo,
		TradeNo:      req.ChannelOrderID,
		RefundAmount: FormatCentToCNY(req.AmountCent),
		OutRequestNo: req.RefundNo,
		RefundReason: TruncateUTF8Bytes(SanitizeReason(req.Reason), 256),
	}
	node, err := alipayCall(ctx, c, alipayMethodRefund, biz, "", 15*time.Second)
	if err != nil {
		return nil, err
	}
	var resp alipayRefundResp
	if err := json.Unmarshal(node, &resp); err != nil {
		return nil, fmt.Errorf("支付宝退款响应解析失败: %w", err)
	}
	if resp.Code != "10000" {
		return nil, alipayRefundError(resp.Code, resp.SubCode, firstNonEmpty(resp.SubMsg, resp.Msg))
	}

	result := &RefundResult{
		RefundNo: req.RefundNo,
		TradeNo:  resp.TradeNo,
		Message:  firstNonEmpty(resp.SubMsg, resp.Msg),
		Raw: map[string]string{
			"fund_change":  resp.FundChange,
			"refund_fee":   resp.RefundFee,
			"trade_no":     resp.TradeNo,
			"out_trade_no": resp.OutTradeNo,
		},
	}
	if fee, perr := ParseCNYToCent(resp.RefundFee); perr == nil {
		result.RefundedCent = fee
	}

	switch strings.ToUpper(strings.TrimSpace(resp.FundChange)) {
	case "Y":
		// 资金确实变了：必须**读得到金额**且与申请额一致。
		// 读不到金额（refund_fee 缺失/不可解析）同样不能判成功 ——
		// 那等于「上游说退了，但我们无法证明退了多少」，只能转人工。
		if result.RefundedCent <= 0 || refundAmountMismatch(req.AmountCent, result.RefundedCent) {
			return nil, fmt.Errorf("%w: 上游退款金额 %s 与订单 %s 不一致",
				ErrRefundUnknown, FormatCentToCNY(result.RefundedCent), FormatCentToCNY(req.AmountCent))
		}
		result.Status = RefundDone
		return result, nil
	case "N":
		// **不等于失败**：可能是「这笔已经退过了」，支付宝对同 out_request_no 幂等返回 N。
		// 唯一的判断依据是退款查询 —— 查不到也不能回滚额度（钱可能已经退出去了）。
		q, qerr := alipayRefundQuery(ctx, ch, req.OutTradeNo, req.RefundNo, req.AmountCent)
		if qerr == nil && q != nil && q.Status == RefundDone {
			return q, nil
		}
		result.Status = RefundAccepted
		if result.Message == "" {
			result.Message = "上游返回资金未变动，需用退款查询确认是否已退过"
		}
		return result, nil
	}
	// fund_change 缺省/异值：一律按「结果未知」处理，绝不判失败（会误回滚额度）。
	result.Status = RefundAccepted
	return result, nil
}

// alipayRefundQuery 查询退款结果。
//
// 判据是 `refund_amount` 是否等于期望金额：`refund_status` 字段已废弃，不可依赖；
// 而「查得到且金额相等」足以证明钱退出去了（我们只做全额退款）。
func alipayRefundQuery(ctx context.Context, ch Channel, outTradeNo, refundNo string, amountCent int64) (*RefundResult, error) {
	c, err := parseAlipayChannel(ch)
	if err != nil {
		return nil, err
	}
	biz := map[string]string{
		"out_trade_no":   outTradeNo,
		"out_request_no": refundNo,
	}
	node, err := alipayCall(ctx, c, alipayMethodRefundQuery, biz, "", 5*time.Second)
	if err != nil {
		return nil, err
	}
	var resp struct {
		alipayNode
		OutTradeNo   string `json:"out_trade_no"`
		TradeNo      string `json:"trade_no"`
		OutRequestNo string `json:"out_request_no"`
		RefundAmount string `json:"refund_amount"`
		RefundStatus string `json:"refund_status"`
		RefundFee    string `json:"refund_fee"`
	}
	if err := json.Unmarshal(node, &resp); err != nil {
		return nil, fmt.Errorf("支付宝退款查询响应解析失败: %w", err)
	}
	if resp.Code != "10000" {
		return nil, alipayRefundError(resp.Code, resp.SubCode, firstNonEmpty(resp.SubMsg, resp.Msg))
	}

	fee, perr := ParseCNYToCent(firstNonEmpty(resp.RefundAmount, resp.RefundFee))
	if perr != nil || fee <= 0 {
		// 查不到退款金额：说明这笔退款在上游还不存在（或仍在处理）。
		return &RefundResult{Status: RefundAccepted, RefundNo: refundNo,
			Message: "退款查询暂无结果"}, nil
	}
	res := &RefundResult{
		RefundNo:     refundNo,
		TradeNo:      resp.TradeNo,
		RefundedCent: fee,
		Raw: map[string]string{
			"refund_amount":  resp.RefundAmount,
			"refund_status":  resp.RefundStatus,
			"out_request_no": resp.OutRequestNo,
		},
	}
	if refundAmountMismatch(amountCent, fee) {
		// 金额对不上：不能判成功也不能判失败，交给人工。
		res.Status = RefundAccepted
		res.Message = fmt.Sprintf("上游退款金额 %s 与订单 %s 不一致",
			FormatCentToCNY(fee), FormatCentToCNY(amountCent))
		return res, nil
	}
	res.Status = RefundDone
	return res, nil
}

// alipayRefundError 把退款相关错误码映射到错误集。
//
// 只有「明确没产生退款」的码才映射成 ErrRefundRejected（可安全回滚额度）；
// 其余一律让调用方走未知态。
//
// `acq.trade_has_refund` / `acq.refund_amt_not_equal_total` **不能**当「没产生退款」：
//   - trade_has_refund：该交易在支付宝侧已存在退款 —— 很可能就是上一笔成功的退款
//     （本地没收到响应、重试命中）。归 Rejected 会把额度再回滚一次，用户既拿钱又拿额度；
//   - refund_amt_not_equal_total：请求金额与订单总额不等。它既可能是「请求被拒、一分未退」，
//     也可能是「已有一笔部分退款」—— 错误码本身无法区分，只能转人工。
//
// 两者都归 Unknown：保持 refunding、不回滚额度，由退款巡检查到终态。
func alipayRefundError(code, subCode, detail string) error {
	sub := strings.ToLower(strings.TrimSpace(subCode))
	switch {
	case sub == "acq.seller_balance_not_enough",
		sub == "acq.trade_not_exist",
		sub == "acq.trade_status_error",
		strings.HasPrefix(sub, "isv.invalid-signature"),
		strings.HasPrefix(sub, "isv.invalid-app-id"):
		return fmt.Errorf("%w: %s", ErrRefundRejected, detail)
	case sub == "acq.trade_has_refund",
		sub == "acq.refund_amt_not_equal_total":
		return fmt.Errorf("%w: %s", ErrRefundUnknown, detail)
	}
	return alipayClassify(code, subCode, detail, alipayMethodRefund)
}
