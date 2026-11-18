package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
)

// 微信支付 APIv3 Native（PC 扫码）。官方 Go SDK 负责签名、应答验签与错误解析，
// 本文件只做「协议无关入参 → SDK 请求」「SDK 应答 → Notification」两件事。

// wxNotifyURLPattern 校验回调地址：微信要求 **https** 且**不允许携带查询串**。
//
// 这条校验必须在这里做，不能只靠 payURLs：那里只判 http/https（站点配成 http
// 的内网/本地部署很常见），漏过去的表现是上游返回 PARAM_ERROR、对客只有 502，
// 而运营完全看不出是「站点地址填错了」。
func checkWxNotifyURL(raw string) error {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(s), "https://") {
		return fmt.Errorf("%w: 微信支付的回调地址必须是 https（请检查站点设置里的第一条 API 调用地址）", ErrNotConfigured)
	}
	if strings.Contains(s, "?") {
		return fmt.Errorf("%w: 微信支付的回调地址不允许携带查询串", ErrNotConfigured)
	}
	return nil
}

// wxpayCreate 走 Native 下单，返回 code_url（内容型二维码）。
func wxpayCreate(ctx context.Context, ch Channel, req CreateRequest) (*CreateResult, error) {
	client, creds, err := wxClient(ch)
	if err != nil {
		return nil, err
	}
	if err := checkWxNotifyURL(req.NotifyURL); err != nil {
		return nil, err
	}
	if creds.AppID == "" {
		return nil, fmt.Errorf("%w: 缺少 AppID", ErrNotConfigured)
	}

	expire := effectiveExpireAt(req.ExpireAt)
	// 截到秒：SDK 用 encoding/json 序列化 time.Time，会带上纳秒，
	// 而微信要求 rfc3339 的秒级格式（带小数秒可能被拒）。
	expire = expire.Truncate(time.Second)

	svc := native.NativeApiService{Client: client}
	resp, _, err := svc.Prepay(ctx, native.PrepayRequest{
		Appid:       core.String(creds.AppID),
		Mchid:       core.String(creds.MchID),
		Description: core.String(req.Subject),
		OutTradeNo:  core.String(req.OutTradeNo),
		TimeExpire:  &expire,
		Attach:      core.String(req.Attach),
		NotifyUrl:   core.String(req.NotifyURL),
		Amount: &native.Amount{
			Total:    core.Int64(req.AmountCent),
			Currency: core.String("CNY"),
		},
	})
	if err != nil {
		return nil, wxpayError(err, "下单")
	}
	if resp == nil || resp.CodeUrl == nil || strings.TrimSpace(*resp.CodeUrl) == "" {
		return nil, ErrNoPayURL
	}
	// PayURL 必须留空：前端在 qr_ref 分支取不到时会退回 pay_url 按钮，
	// 而 weixin:// 在手机浏览器没有协议处理器，点了毫无反应。
	return &CreateResult{
		QRRef:     strings.TrimSpace(*resp.CodeUrl),
		ExpiresAt: expire,
	}, nil
}

// wxpayQuery 按商户订单号查单。
//
// 返回 (nil, nil) 表示「上游没有这笔订单（未支付）」；查询失败一律返回错误，
// 调用方据此保持 pending（关单不能靠「查不到」）。
func wxpayQuery(ctx context.Context, ch Channel, outTradeNo string) (*Notification, error) {
	client, creds, err := wxClient(ch)
	if err != nil {
		return nil, err
	}
	svc := native.NativeApiService{Client: client}
	tx, _, err := svc.QueryOrderByOutTradeNo(ctx, native.QueryOrderByOutTradeNoRequest{
		OutTradeNo: core.String(outTradeNo),
		Mchid:      core.String(creds.MchID),
	})
	if err != nil {
		if isWxCode(err, "ORDER_NOT_EXIST") || isWxCode(err, "RESOURCE_NOT_EXISTS") {
			return nil, nil // 上游确实没有这笔单 = 确认未支付
		}
		return nil, wxpayError(err, "查单")
	}
	if tx == nil {
		return nil, nil
	}
	state := deref(tx.TradeState)
	amount := int64(0)
	if tx.Amount != nil && tx.Amount.Total != nil {
		amount = *tx.Amount.Total
	}
	raw := map[string]string{
		"trade_state":  state,
		"out_trade_no": deref(tx.OutTradeNo),
		"mchid":        deref(tx.Mchid),
		"appid":        deref(tx.Appid),
		"source":       "query",
	}
	if tx.TransactionId != nil {
		raw["transaction_id"] = *tx.TransactionId
	}
	if tx.SuccessTime != nil {
		raw["success_time"] = *tx.SuccessTime
	}
	return &Notification{
		OutTradeNo:     deref(tx.OutTradeNo),
		TradeNo:        deref(tx.TransactionId),
		ChannelOrderID: deref(tx.TransactionId),
		AmountCent:     amount,
		MerchantID:     deref(tx.Mchid),
		Kind:           wxTradeStateKind(state),
		Raw:            raw,
	}, nil
}

// wxpayClose 关闭订单（一期不进主链路，留给后续「管理台强制关单」）。
func wxpayClose(ctx context.Context, ch Channel, outTradeNo string) error {
	client, creds, err := wxClient(ch)
	if err != nil {
		return err
	}
	svc := native.NativeApiService{Client: client}
	_, err = svc.CloseOrder(ctx, native.CloseOrderRequest{
		OutTradeNo: core.String(outTradeNo),
		Mchid:      core.String(creds.MchID),
	})
	if err != nil && !isWxCode(err, "ORDER_NOT_EXIST") {
		return wxpayError(err, "关单")
	}
	return nil
}

// wxTradeStateKind 把 trade_state 映射成归一语义。
//
// CLOSED/REVOKED 一律 ignored：本地关单由 ExpirePending 按 expire_at 决定，
// 让上游状态去改本地订单只会在「用户已付款但上游先关单」的竞态里制造 need_manual 噪音。
func wxTradeStateKind(state string) NotifyKind {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUCCESS":
		return NotifyPaid
	case "REFUND":
		return NotifyRefunded
	}
	return NotifyIgnored
}

// wxpayError 把 SDK 错误映射到本包错误集。
//
// 分类不是为了好看：签名/参数错要立刻告警给管理员（配置问题，重试一万次也不会好），
// 而限频/系统错误只该让用户稍后重试。
func wxpayError(err error, action string) error {
	if err == nil {
		return nil
	}
	var apiErr *core.APIError
	if !errors.As(err, &apiErr) {
		// 网络错误 / 应答验签失败：原样上抛，交给 mapGatewayErr 判 502。
		return fmt.Errorf("微信支付%s失败: %w", action, err)
	}
	code := strings.ToUpper(strings.TrimSpace(apiErr.Code))
	detail := fmt.Sprintf("%s %s", code, apiErr.Message)
	switch code {
	case "ORDERPAID", "OUT_TRADE_NO_USED":
		return fmt.Errorf("%w: %s", ErrOrderExists, detail)
	case "SIGN_ERROR", "APPID_MCHID_NOT_MATCH", "MCH_NOT_EXISTS", "PARAM_ERROR",
		"INVALID_REQUEST", "NO_AUTH", "APPID_NOT_EXIST":
		return fmt.Errorf("%w: 微信支付%s被拒: %s", ErrGatewayRejected, action, detail)
	case "FREQUENCY_LIMITED", "SYSTEM_ERROR", "BANK_ERROR":
		return fmt.Errorf("%w: 微信支付%s繁忙: %s", ErrGatewayBusy, action, detail)
	}
	if apiErr.StatusCode >= 500 {
		return fmt.Errorf("%w: 微信支付%s异常 HTTP %d: %s", ErrGatewayBusy, action, apiErr.StatusCode, detail)
	}
	return fmt.Errorf("%w: 微信支付%s失败: %s", ErrGatewayRejected, action, detail)
}

// isWxCode 判断错误是否为特定微信错误码。
func isWxCode(err error, code string) bool {
	var apiErr *core.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(apiErr.Code), code)
}

// deref 安全解引用字符串指针。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// derefInt 安全解引用 int64 指针。
func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
