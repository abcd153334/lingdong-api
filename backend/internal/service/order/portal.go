package order

import (
	"time"

	"lingdong-api/internal/service/payment"
)

// 本文件是**用户端**订单数据的对外投影（fail-closed 白名单）。
//
// 为什么不直接给 View 加 `json:"-"`：View 是门户与管理台共用的结构，把
// pay_channel / pay_channel_id / pay_protocol 隐掉会让管理台订单页与退款排障
// 失去关键信息。分成两份结构体，门户这份**只加不改** —— 以后 View 新增字段，
// 不显式搬到这里，用户端就看不到。
//
// 用户端不得出现的信息（本文件的存在理由）：
//   - pay_channel（如「虎皮椒-微信支付」）与 pay_channel_id（如 xunhupay）：
//     直接暴露收单供应链，用户据此知道平台走的是哪家聚合支付；
//   - pay_protocol：同上，且它只是内部协议枚举；
//   - pay_error：来自支付网关的原始报错，可能带商户号、签名参数甚至渠道名；
//   - refund_no / refund_error：上游退款单号与上游报错，与用户无关；
//   - user_id / username：本人订单不需要，管理台才需要。

// PortalOrderView 是订单在用户端的白名单投影。
//
// 保留 TradeNo（支付平台的交易号）是刻意的：用户找客服核对支付凭证时只有它能对上，
// 而一个纯数字单号不构成「哪家渠道」的归属暴露。
type PortalOrderView struct {
	ID         uint64 `json:"id"`
	OutTradeNo string `json:"out_trade_no"`
	TradeNo    string `json:"trade_no,omitempty"`
	AmountCent int64  `json:"amount_cent"`
	Currency   string `json:"currency"`
	// Credited 是到账金额（微）= 售价（分→微）+ 赠送。用户端可以直接复算：
	// credited = amount_cent × 1e4 + 赠送，**没有**倍率与刻度参与。
	Credited int64  `json:"credited"`
	Status   string `json:"status"`
	// PayMethod 是支付方式枚举（wechat / alipay / aggregate），
	// PayMethodLabel 是它的中文（微信支付 / 支付宝）—— 展示一律用 label。
	PayMethod      string     `json:"pay_method"`
	PayMethodLabel string     `json:"pay_method_label"`
	Subject        string     `json:"subject,omitempty"`
	PackageName    string     `json:"package_name,omitempty"`
	ExpireAt       *time.Time `json:"expire_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	PaidAt         *time.Time `json:"paid_at"`

	// 退款信息：只给用户该知道的（状态/模式/金额/原因/时间）。
	RefundStatus     string     `json:"refund_status,omitempty"`
	RefundMode       string     `json:"refund_mode,omitempty"`
	RefundAmountCent int64      `json:"refund_amount_cent,omitempty"`
	RefundReason     string     `json:"refund_reason,omitempty"`
	RefundAt         *time.Time `json:"refund_at,omitempty"`
	RefundDoneAt     *time.Time `json:"refund_done_at,omitempty"`
}

// PortalOrderViewOf 把管理台视图投影成用户端视图。
func PortalOrderViewOf(v View) PortalOrderView {
	return PortalOrderView{
		ID: v.ID, OutTradeNo: v.OutTradeNo, TradeNo: v.TradeNo,
		AmountCent: v.AmountCent, Currency: v.Currency,
		Credited:  v.Credited,
		Status:    v.Status,
		PayMethod: v.PayMethod, PayMethodLabel: v.PayMethodLabel,
		Subject: v.Subject, PackageName: v.PackageName,
		ExpireAt: v.ExpireAt, CreatedAt: v.CreatedAt, PaidAt: v.PaidAt,
		RefundStatus: v.RefundStatus, RefundMode: v.RefundMode,
		RefundAmountCent: v.RefundAmountCent, RefundReason: v.RefundReason,
		RefundAt: v.RefundAt, RefundDoneAt: v.RefundDoneAt,
	}
}

// PortalOrderViews 批量投影（保持顺序；nil 入参返回空切片而非 nil，
// 让 JSON 恒为 []，前端不必处理 null）。
func PortalOrderViews(vs []View) []PortalOrderView {
	out := make([]PortalOrderView, 0, len(vs))
	for _, v := range vs {
		out = append(out, PortalOrderViewOf(v))
	}
	return out
}

// PortalCreateResult 是下单/续付/重试支付的用户端返回。
//
// 与原 CreateResult 的差别只有「删掉渠道三件套、把协议换成行为标志」：
//   - AutoRedirect 取代前端对 pay_protocol == "stripe" 的判断 ——
//     「哪种协议要自动跳转」属于后端知识，前端不该维护这张映射表；
//   - PayError 删掉：网关原始报错可能带渠道名与商户号，
//     用户端统一显示「支付方式暂不可用，请重试」。
type PortalCreateResult struct {
	ID         uint64 `json:"id"`
	OutTradeNo string `json:"out_trade_no"`
	AmountCent int64  `json:"amount_cent"`
	Currency   string `json:"currency"`
	Credited   int64  `json:"credited"`
	// Status 是订单当前状态（pending/paid/expired/…）。
	//
	// 门户的「新建订单」永远拿到 pending，所以这里多一个字段看不出价值；
	// 但 `POST /api/user/orders` 会做**幂等重放**，重放时必须能回答
	// 「上次那笔到底怎样了」—— 只回支付链接的话，客户端无法区分
	// 「还没付」与「已经付过了」，只能再查一次单，而它正是在响应丢失时重放的。
	Status         string `json:"status"`
	PayMethod      string `json:"pay_method"`
	PayMethodLabel string `json:"pay_method_label"`
	CashierURL     string `json:"cashier_url"`
	PayURL         string `json:"pay_url"`
	QRRef          string `json:"qr_ref"`
	ExpiresIn      int    `json:"expires_in"`
	// AutoRedirect 表示 PayURL 是 hosted 收银台页（Stripe 一类），
	// 拿到地址即跳转，不必等用户点「打开支付页面」。
	AutoRedirect bool `json:"auto_redirect"`
}

// PortalCreateResultOf 投影下单结果。
func PortalCreateResultOf(r *CreateResult) *PortalCreateResult {
	if r == nil {
		return nil
	}
	return &PortalCreateResult{
		ID: r.ID, OutTradeNo: r.OutTradeNo,
		AmountCent: r.AmountCent, Currency: r.Currency,
		Credited:  r.Credited,
		Status:    r.Status,
		PayMethod: r.PayMethod, PayMethodLabel: r.PayMethodLabel,
		CashierURL: r.CashierURL, PayURL: r.PayURL, QRRef: r.QRRef,
		ExpiresIn:    r.ExpiresIn,
		AutoRedirect: autoRedirect(r.PayProtocol),
	}
}

// autoRedirect 判断某协议拿到的 pay_url 是否为「必须跳转的收银台页」。
//
// 目前只有 Stripe 是 hosted 页；易支付/迅虎返回的是收银台页但也支持二维码，
// 前端保留「打开支付页面」按钮更稳（避免被弹窗拦截时用户无路可走）。
func autoRedirect(protocol string) bool {
	return payment.ParseProtocol(protocol) == payment.ProtocolStripe
}

// PortalVerifyResult 是「我已支付，立即核对」的用户端返回。
type PortalVerifyResult struct {
	Order   PortalOrderView `json:"order"`
	QueryOK bool            `json:"query_ok"`
}

// PortalVerifyResultOf 投影核对结果。
func PortalVerifyResultOf(r *VerifyResult) *PortalVerifyResult {
	if r == nil {
		return nil
	}
	return &PortalVerifyResult{Order: PortalOrderViewOf(r.Order), QueryOK: r.QueryOK}
}

// PortalPayOption 是收银台上的一个支付方式。
//
// **不含 channel_id / protocol / 渠道名**：用户只关心「微信还是支付宝」，
// 走哪家聚合支付是平台的供应链。下单时前端只回传 method，
// 由 resolveChannel 的「按方式路由」分支在服务端决定承接渠道 ——
// 这个映射因此只有一份实现（后端），前端不可能传错、也不会成为第二真相源。
type PortalPayOption struct {
	Method string `json:"method"`
	Label  string `json:"label"`
}

// PortalPayOptions 把内部收银台选项投影成用户端选项（按方式去重，顺序不变）。
func PortalPayOptions() []PortalPayOption {
	opts := payment.Options()
	out := make([]PortalPayOption, 0, len(opts))
	seen := make(map[string]bool, len(opts))
	for _, o := range opts {
		if o.Method == "" || seen[o.Method] {
			continue
		}
		seen[o.Method] = true
		out = append(out, PortalPayOption{Method: o.Method, Label: o.Label})
	}
	return out
}
