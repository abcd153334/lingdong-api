package payment

// 本文件把「协议能力」收成数据，替代散落在 gateway / refund / worker / order 里的
// `switch proto` 与写死的字符串。
//
// 为什么必须集中：新增一个协议时，最容易漏的不是实现，而是「查单 worker 没带它」
// 与「关单策略没排除它」——两处漏一个，订单就会「永不查单 + 到期即关」，
// 表现为「用户付了钱，订单却已过期」，且没有任何报错。

// Protocols 返回全部协议（稳定顺序，与 channelSchemas 的顺序一致）。
//
// 顺序即管理台「新增支付渠道」里协议 chips 的顺序；不要靠 map 遍历。
func Protocols() []Protocol {
	return []Protocol{ProtocolEpay, ProtocolXunhuPay, ProtocolWxpay, ProtocolAlipay, ProtocolStripe}
}

// ProtocolNames 返回全部协议的字符串形式（用于错误文案，替代写死「只支持 epay / xunhupay」）。
func ProtocolNames() []string {
	all := Protocols()
	out := make([]string, 0, len(all))
	for _, p := range all {
		out = append(out, string(p))
	}
	return out
}

// QuerySupported 判断该协议是否实现了主动查单。
//
// 易支付没有实现（各家 `act=order` 的路径与返回都不统一，未实测前不接入），
// 因此它的订单只能按 expire_at 直接关单。
func QuerySupported(p Protocol) bool {
	switch p {
	case ProtocolXunhuPay, ProtocolWxpay, ProtocolAlipay, ProtocolStripe:
		return true
	}
	return false
}

// RefundQuerySupported 判断该协议是否有独立的「退款单查询」接口。
//
// 虎皮椒没有：它的退款结果只能靠回调或拿支付查单的 CD/UD 作二手证据
// （见 order.RefundQueryOnce 的回落分支）。微信/支付宝都有。
//
// 「是否支持原路退款」不在本文件：那个入口是 `RefundSupported(string)`（refund.go），
// 它接受任意大小写的历史字符串，被 order 包与既有单测共用，不能改签名。
func RefundQuerySupported(p Protocol) bool {
	switch p {
	case ProtocolWxpay, ProtocolAlipay:
		return true
	}
	return false
}

// QueryProtocols 返回「有查单能力」的协议集合。
//
// 两处唯一真相源：
//   - worker/order_query.go 的领取 SQL（`pay_protocol IN (...)`）
//   - order.ExpirePending 的关单策略（有查单能力的协议**不能**到期即关）
//
// 两处必须用同一个集合：只用其一就会出现「不查单却也不关单」（永久 pending）
// 或「查单还没跑完就被关掉」（用户付款后订单已过期）。
func QueryProtocols() []Protocol {
	all := Protocols()
	out := make([]Protocol, 0, len(all))
	for _, p := range all {
		if QuerySupported(p) {
			out = append(out, p)
		}
	}
	return out
}
