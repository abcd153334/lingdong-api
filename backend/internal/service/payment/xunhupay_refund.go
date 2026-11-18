package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 退款请求超时。
//
// 比下单宽松（15s）：退款是资金动作，宁可让管理员多等，也不能因为 5s 掐断而进入
// 「不知道退没退」的未知态。真正的请求取消由调用方用 context.WithoutCancel 隔离。
const xunhuRefundTimeout = 15 * time.Second

// xunhuRefund 调 refund.html 发起**全额**退款。
//
// 官方参数只有 appid / trade_order_id|open_order_id / reason / time / nonce_str / hash ——
// **没有金额参数**，所以本系统只支持全额退款。这不是偷懒：
// 部分退款既无法表达，也就没有「退多少」的幂等歧义，重试与对账都只有一种答案。
func xunhuRefund(ctx context.Context, ch Channel, req RefundRequest) (*RefundResult, error) {
	endpoint := xunhuRefundEndpoint(ch)
	if endpoint == "" || strings.TrimSpace(ch.Params.AppID) == "" || strings.TrimSpace(ch.Params.AppSecret) == "" {
		return nil, ErrNotConfigured
	}

	fields := map[string]string{
		"appid":     ch.Params.AppID,
		"time":      strconv.FormatInt(time.Now().Unix(), 10),
		"nonce_str": randomNonce(32),
	}
	// 两个单号同时传：trade_order_id 是商户侧唯一键，open_order_id 是上游内部键。
	// 上游要求「二选一」，同时传时以其内部单号优先（更能定位到具体支付记录）。
	if v := strings.TrimSpace(req.ChannelOrderID); v != "" {
		fields["open_order_id"] = v
	} else {
		fields["trade_order_id"] = strings.TrimSpace(req.OutTradeNo)
	}
	if r := SanitizeReason(req.Reason); r != "" {
		fields["reason"] = r
	}
	fields["hash"] = xunhuSign(fields, ch.Params.AppSecret)

	body, status, err := postForm(ctx, endpoint, fields, xunhuRefundTimeout)
	if err != nil {
		// 端点字面量非法（非 https / 内嵌凭据）：请求根本没发出去，可以安全判定为拒绝。
		if errors.Is(err, ErrNotConfigured) {
			return nil, err
		}
		// 连接失败/超时：请求可能已到达上游并已受理，**绝不能**判为失败。
		return nil, fmt.Errorf("%w: %v", ErrRefundUnknown, err)
	}
	if status != http.StatusOK {
		// 4xx/5xx 同样不判失败：WAF/负载均衡也可能在请求转发之后返回 5xx。
		return nil, fmt.Errorf("%w: 退款接口返回 HTTP %d", ErrRefundUnknown, status)
	}

	raw, top, err := decodeJSONObject(body)
	if err != nil {
		// 响应不可解析（截断/HTML 错误页）：不知道上游做了什么，一律未知。
		return nil, fmt.Errorf("%w: 退款响应不是合法 JSON", ErrRefundUnknown)
	}

	// 验签是**决策前置条件**：退款响应会直接决定是否回滚用户额度，
	// 只有密钥持有者能签出 hash。验签不过 → 不做任何判定（未知，等人工/回调）。
	_, nodeStr := pickRefundNode(raw)
	if !xunhuVerify(top, ch.Params.AppSecret) && !xunhuVerify(nodeStr, ch.Params.AppSecret) {
		return nil, fmt.Errorf("%w: 退款响应验签失败", ErrRefundUnknown)
	}

	code := firstNonEmpty(strOfStr(nodeStr, "errcode"), strOfStr(top, "errcode"))
	msg := firstNonEmpty(strOfStr(nodeStr, "errmsg"), strOfStr(top, "errmsg"))
	if err := classifyRefundErr(code, msg); err != nil {
		return nil, err
	}

	res := &RefundResult{
		RefundNo: firstNonEmpty(strOfStr(nodeStr, "out_refund_no"), strOfStr(top, "out_refund_no")),
		TradeNo:  firstNonEmpty(strOfStr(nodeStr, "transaction_id"), strOfStr(top, "transaction_id")),
		Message:  msg,
		Raw:      nodeStr,
	}

	switch strings.ToUpper(strings.TrimSpace(
		firstNonEmpty(strOfStr(nodeStr, "refund_status"), strOfStr(top, "refund_status")))) {
	case "CD":
		res.Status = RefundDone
	case "UD":
		res.Status = RefundDeclined
		if res.Message == "" {
			res.Message = "上游返回退款失败（UD）"
		}
	default:
		// OD（尚未执行）/ RD（退款中）/ 空值：都只能确认「已受理」，不能确认「已退款」。
		res.Status = RefundAccepted
	}

	// 金额核对：refund_fee 必须等于订单金额。不等说明上游退的是别的金额
	// （例如运营在网关后台做过部分退款），此时**不判定成功**，留给人核对。
	if res.Status == RefundDone {
		feeRaw := firstNonEmpty(strOfStr(nodeStr, "refund_fee"), strOfStr(top, "refund_fee"))
		switch {
		case feeRaw == "":
			// 协议里 refund_fee 是必返回项；缺了说明响应不完整，不能据此判定入账口径。
			res.Status = RefundAccepted
			res.Message = "上游未返回退款金额，无法核对"
		default:
			fee, ferr := ParseCNYToCent(feeRaw)
			if ferr != nil {
				return nil, fmt.Errorf("%w: 退款金额无法解析 %q（refund_no=%s）",
					ErrRefundUnknown, feeRaw, res.RefundNo)
			}
			if refundAmountMismatch(req.AmountCent, fee) {
				return nil, fmt.Errorf("%w: 退款金额 %d 分与订单 %d 分不一致（refund_no=%s）",
					ErrRefundUnknown, fee, req.AmountCent, res.RefundNo)
			}
			res.RefundedCent = fee
		}
	}
	return res, nil
}

// classifyRefundErr 把退款响应的 errcode/errmsg 分成「可回滚」与「不可判定」两类。
//
// 分类错误的代价不对称：
//   - 把「已退款」误判成拒绝 → 回滚额度，用户白拿钱；
//   - 把「参数错误」误判成未知 → 停在 refunding，人工一步就能收尾。
//
// 所以只有**确定没有产生退款**的错误才归到 ErrRefundRejected。
func classifyRefundErr(code, msg string) error {
	code = strings.TrimSpace(code)
	if code == "0" {
		return nil
	}
	if code == "" {
		return fmt.Errorf("%w: 退款响应缺少 errcode（msg=%s）", ErrRefundUnknown, msg)
	}
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "不存在", "not exist", "no such", "invalid order"):
		// 订单号不对：确定没有退款动作发生。
		return fmt.Errorf("%w: %s（errcode=%s）", ErrRefundRejected, msg, code)
	case containsAny(m, "已退款", "退款成功", "refunded", "already refund", "重复", "dup",
		"处理中", "退款中", "refunding", "processing"):
		// 上游说这笔已经退过/正在退：结果只能靠回调或人工核实，不能回滚额度。
		return fmt.Errorf("%w: %s（errcode=%s）", ErrRefundUnknown, msg, code)
	case containsAny(m, "签名", "sign", "参数", "param", "商户", "appid", "未开通", "权限",
		"余额不足", "insufficient", "金额", "amount"):
		// 配置/参数类错误：请求根本没被受理，确定没有退款动作。
		return fmt.Errorf("%w: %s（errcode=%s）", ErrRefundRejected, msg, code)
	default:
		// **未知 errcode 一律按「未知结果」处理**。
		//
		// 旧实现把 default 判成 ErrRefundRejected → 回滚额度回到 paid，
		// 运营看到「退款失败」就会再点一次；如果第一次其实已经在网关侧退款成功，
		// 第二次就是**真的退两笔**（钱多退一次，而额度只扣了一笔）。
		// 未知态保持 refunding 最坏只是需要人工到网关后台核对一次。
		return fmt.Errorf("%w: %s（errcode=%s）", ErrRefundUnknown, msg, code)
	}
}

// xunhuRefundEndpoint 推导退款地址。
//
// 优先级：渠道显式配置的 refund_address → 从 query_address 换文件名 → 从 pay_address 换文件名。
// 官方三个平台（api.xunhupay.com / api.dpweixin.com / api.diypc.com.cn）路径都是
// `/payment/refund.html`，所以「换文件名」比「拼域名」更稳：换备用平台只需改 pay_address。
//
// 这里**不**像查单那样强求 https：退款响应必须验签（见 Refund 的「验签是决策前置条件」），
// 伪造「退款被拒」需要拿到 AppSecret，明文链路挡不住的那一步已经由签名挡住了；
// 而多一道 https 门只会让自建/沙箱网关（http）整条链路不可用。
func xunhuRefundEndpoint(ch Channel) string {
	if v := strings.TrimSpace(ch.Params.RefundAddress); v != "" {
		return v
	}
	for _, cand := range []string{ch.Params.QueryAddress, ch.Params.PayAddress} {
		if ep := swapPaymentFile(cand, "refund.html"); ep != "" {
			return ep
		}
	}
	return ""
}

// swapPaymentFile 把「.../payment/xxx.html」里的文件名换成 file。
//
// 四种输入都要处理：标准端点（.../payment/do.html）、根地址、任意路径的端点文件、空值。
// 不用字符串拼接处理根地址：`api.xunhupay.com` 里的点会被误判成文件名后缀。
func swapPaymentFile(configured, file string) string {
	base := strings.TrimRight(strings.TrimSpace(configured), "/")
	if base == "" {
		return ""
	}
	low := strings.ToLower(base)
	if i := strings.Index(low, "/payment/"); i >= 0 {
		return base[:i+len("/payment/")] + file
	}
	if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
		if u.Path == "" || u.Path == "/" {
			u.Path = "/payment/" + file
			return u.String()
		}
		if j := strings.LastIndex(u.Path, "/"); j >= 0 {
			u.Path = u.Path[:j+1] + file
			return u.String()
		}
	}
	return base + "/payment/" + file
}

// pickRefundNode 取退款响应的「决策节点」：优先含 refund_status 的层级（顶层或 data），
// 同时返回该层级的字符串视图用于验签。
//
// 退款响应的嵌套形态与查单不同（官方示例是平铺），所以不能复用 pickQueryNode
// —— 那个函数以 status 为判据，而退款响应里根本没有 status 字段。
func pickRefundNode(raw map[string]any) (map[string]any, map[string]string) {
	top := scalarMap(raw)
	if _, ok := top["refund_status"]; ok {
		return raw, top
	}
	if d, ok := raw["data"].(map[string]any); ok {
		ds := scalarMap(d)
		if _, ok := ds["refund_status"]; ok {
			return d, ds
		}
		if _, ok := ds["out_refund_no"]; ok {
			return d, ds
		}
	}
	return raw, top
}

// scalarMap 把 JSON 对象转成「字段→字符串」视图（嵌套值保留紧凑 JSON）。
func scalarMap(node map[string]any) map[string]string {
	out := make(map[string]string, len(node))
	for k, v := range node {
		out[k] = jsonScalar(v)
	}
	return out
}

// strOfStr 读字符串视图里的字段（缺省返回空串）。
func strOfStr(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}
