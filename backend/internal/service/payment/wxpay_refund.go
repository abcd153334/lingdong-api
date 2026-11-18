package payment

import (
	"context"
	"fmt"
	"strings"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/services/refunddomestic"
)

// 微信退款：`refunddomestic.RefundsApiService.Create` / `QueryByOutRefundNo`。
//
// 两端都要求「同一笔退款重试必须复用原退款单号」，所以 out_refund_no 由调用方
// 预先生成并落库（order.refund 的事务里就写好），这里只在空值时兜底派生。

// wxpayRefund 发起全额退款。
func wxpayRefund(ctx context.Context, ch Channel, req RefundRequest) (*RefundResult, error) {
	client, _, err := wxClient(ch)
	if err != nil {
		return nil, err
	}
	refundNo := strings.TrimSpace(req.RefundNo)
	if refundNo == "" {
		refundNo = DeriveRefundNo(req.OutTradeNo)
	}
	// reason 上限是 80 **字节**（不是字符）：80 个汉字 = 240 字节，上游直接报参数错误。
	reason := TruncateUTF8Bytes(SanitizeReason(req.Reason), 80)

	svc := refunddomestic.RefundsApiService{Client: client}
	body := refunddomestic.CreateRequest{
		OutTradeNo:  core.String(req.OutTradeNo),
		OutRefundNo: core.String(refundNo),
		Amount: &refunddomestic.AmountReq{
			// Total 是**原订单总额**（我们只做全额退款，所以等于退款额）。
			Refund:   core.Int64(req.AmountCent),
			Total:    core.Int64(req.AmountCent),
			Currency: core.String("CNY"),
		},
	}
	if reason != "" {
		body.Reason = core.String(reason)
	}
	if strings.TrimSpace(req.NotifyURL) != "" {
		body.NotifyUrl = core.String(req.NotifyURL)
	}
	if strings.TrimSpace(req.ChannelOrderID) != "" {
		body.TransactionId = core.String(req.ChannelOrderID)
	}

	resp, _, err := svc.Create(ctx, body)
	if err != nil {
		return nil, wxpayRefundError(err, "发起退款")
	}
	return wxpayRefundResult(resp, refundNo, req.AmountCent)
}

// wxpayRefundQuery 按退款单号查询（退款巡检唯一可靠的终态来源）。
func wxpayRefundQuery(ctx context.Context, ch Channel, refundNo string, amountCent int64) (*RefundResult, error) {
	client, _, err := wxClient(ch)
	if err != nil {
		return nil, err
	}
	refundNo = strings.TrimSpace(refundNo)
	if refundNo == "" {
		return nil, fmt.Errorf("%w: 没有退款单号，无法查询", ErrNotConfigured)
	}
	svc := refunddomestic.RefundsApiService{Client: client}
	resp, _, err := svc.QueryByOutRefundNo(ctx, refunddomestic.QueryByOutRefundNoRequest{
		OutRefundNo: core.String(refundNo),
	})
	if err != nil {
		if isWxCode(err, "RESOURCE_NOT_EXISTS") {
			// 上游查不到这笔退款：可能还没落库，也可能根本没发起 —— 属未知态，不能判失败。
			return &RefundResult{Status: RefundAccepted, RefundNo: refundNo,
				Message: "上游暂无该退款单"}, nil
		}
		return nil, wxpayRefundError(err, "查询退款")
	}
	return wxpayRefundResult(resp, refundNo, amountCent)
}

// wxpayRefundResult 把 SDK 的 Refund 映射成归一结果。
//
// 四种状态的处置差异是**资金安全的核心**：
//
//	SUCCESS     → Done（且必须核对金额）
//	CLOSED      → Declined（唯一可以回滚额度的状态）
//	PROCESSING  → Accepted
//	ABNORMAL    → Accepted（**绝不可当失败**：钱可能已经退出去了，回滚就是双向损失）
func wxpayRefundResult(resp *refunddomestic.Refund, refundNo string, amountCent int64) (*RefundResult, error) {
	if resp == nil {
		// 上游没给 body：结果未知，保持 refunding。
		return &RefundResult{Status: RefundAccepted, RefundNo: refundNo, Message: "上游未返回退款详情"}, nil
	}
	status := ""
	if resp.Status != nil {
		status = strings.ToUpper(strings.TrimSpace(string(*resp.Status)))
	}
	out := &RefundResult{
		RefundNo: refundNo,
		Message:  deref(resp.UserReceivedAccount),
		Raw: map[string]string{
			"status":        status,
			"refund_id":     deref(resp.RefundId),
			"out_refund_no": deref(resp.OutRefundNo),
		},
	}
	if resp.Amount != nil {
		out.RefundedCent = derefInt(resp.Amount.Refund)
		out.Raw["amount_total"] = fmt.Sprintf("%d", derefInt(resp.Amount.Total))
		out.Raw["amount_refund"] = fmt.Sprintf("%d", derefInt(resp.Amount.Refund))
	}

	switch status {
	case "SUCCESS":
		if refundAmountMismatch(amountCent, out.RefundedCent) {
			// 金额对不上：既不能判成功（会漏掉退少）也不能判失败（会误回滚），转未知。
			return nil, fmt.Errorf("%w: 上游退款 %s 与订单 %s 不一致",
				ErrRefundUnknown, FormatCentToCNY(out.RefundedCent), FormatCentToCNY(amountCent))
		}
		out.Status = RefundDone
	case "CLOSED":
		out.Status = RefundDeclined
		if out.Message == "" {
			out.Message = "微信退款已关闭"
		}
	case "ABNORMAL":
		// 退款异常（用户卡注销等）：钱没到用户手里，但**不是我们主动失败**。
		// 保持 refunding 让人工到商户平台处理，绝不自动回滚额度。
		out.Status = RefundAccepted
		if out.Message == "" {
			out.Message = "微信退款异常，需到商户平台人工处理"
		}
	default: // PROCESSING / 空值
		out.Status = RefundAccepted
		if out.Message == "" {
			out.Message = "微信退款处理中"
		}
	}
	return out, nil
}

// wxpayRefundError 把退款错误映射到错误集。
//
// 只有「明确没产生退款」的码才映射 ErrRefundRejected（可安全回滚额度）。
func wxpayRefundError(err error, action string) error {
	mapped := wxpayError(err, action)
	switch {
	case isWxCode(err, "NOT_ENOUGH"),
		isWxCode(err, "USER_ACCOUNT_ABNORMAL"),
		isWxCode(err, "RESOURCE_NOT_EXISTS"),
		isWxCode(err, "PARAM_ERROR"),
		isWxCode(err, "INVALID_REQUEST"),
		isWxCode(err, "NO_AUTH"):
		return fmt.Errorf("%w: %s", ErrRefundRejected, mapped.Error())
	}
	return mapped
}
