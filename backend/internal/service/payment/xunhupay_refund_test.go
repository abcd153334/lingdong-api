package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// refundRespBody 按「服务端会怎么签」构造响应体：签名视图与 JSON 字面量必须完全一致，
// 否则测出来的是我们的解析器，而不是上游的签名规则。
func refundRespBody(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	str := make(map[string]string, len(fields))
	for k, v := range fields {
		switch x := v.(type) {
		case string:
			str[k] = x
		case json.Number:
			str[k] = x.String()
		}
	}
	str["hash"] = xunhuSign(str, xunhuTestSecret)
	fields["hash"] = str["hash"]
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("序列化响应失败: %v", err)
	}
	return b
}

// refundChannel 指向 httptest 的虎皮椒渠道（退款地址显式配置，覆盖「运维填了完整端点」的路径）。
func refundChannel(endpoint string) Channel {
	return Channel{
		ID: "xunhupay", Name: "虎皮椒", Protocol: "xunhupay", Enabled: true,
		Params: ChannelParams{
			PayAddress: "https://api.xunhupay.com/payment/do.html",
			// 显式端点：单测的 httptest 路径不含 /payment/，推导会拼出错误的地址。
			RefundAddress: endpoint,
			AppID:         "2019061201", AppSecret: xunhuTestSecret,
		},
	}
}

func refundReq() RefundRequest {
	return RefundRequest{OutTradeNo: "LD1", ChannelOrderID: "OPEN1", AmountCent: 1, Reason: "重复支付"}
}

// TestXunhuRefundSuccess 退款成功：CD + 金额核对通过 → RefundDone。
func TestXunhuRefundSuccess(t *testing.T) {
	allowLoopback(t)
	var gotForm map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = map[string]string{}
		for k := range r.PostForm {
			gotForm[k] = r.PostForm.Get(k)
		}
		_, _ = w.Write(refundRespBody(t, map[string]any{
			"errcode":        json.Number("0"),
			"errmsg":         "success",
			"trade_order_id": "LD1",
			"transaction_id": "TXN1",
			"out_refund_no":  "RF1",
			"refund_fee":     json.Number("0.01"),
			"refund_status":  "CD",
			"refund_time":    "2026-10-22 10:00:00",
		}))
	}))
	defer srv.Close()

	res, err := Refund(context.Background(), refundChannel(srv.URL), refundReq())
	if err != nil {
		t.Fatalf("退款应成功: %v", err)
	}
	if res.Status != RefundDone || res.RefundNo != "RF1" || res.RefundedCent != 1 {
		t.Fatalf("退款结果不符: %+v", res)
	}

	// 请求契约：必须带 appid / 上游单号 / 原因 / 时间 / nonce，且 hash 自洽。
	if !xunhuVerify(gotForm, xunhuTestSecret) {
		t.Fatalf("请求签名不自洽: %v", gotForm)
	}
	if gotForm["appid"] != "2019061201" || gotForm["open_order_id"] != "OPEN1" {
		t.Fatalf("请求缺少必要参数: %v", gotForm)
	}
	if gotForm["reason"] != "重复支付" {
		t.Fatalf("退款原因应原样上报: %q", gotForm["reason"])
	}
	if gotForm["time"] == "" || gotForm["nonce_str"] == "" {
		t.Fatalf("time/nonce_str 缺失: %v", gotForm)
	}
	if len(gotForm["nonce_str"]) != 32 {
		t.Fatalf("nonce_str 长度应为 32: %q", gotForm["nonce_str"])
	}
}

// TestXunhuRefundStatusMapping 三个非成功状态都必须被正确归一（RD/OD → 已受理，UD → 失败）。
func TestXunhuRefundStatusMapping(t *testing.T) {
	allowLoopback(t)
	cases := []struct {
		status string
		want   RefundStatus
	}{
		{"RD", RefundAccepted},
		{"OD", RefundAccepted},
		{"UD", RefundDeclined},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fields := map[string]any{
					"errcode":       json.Number("0"),
					"errmsg":        "success",
					"out_refund_no": "RF1",
					"refund_status": c.status,
				}
				if c.status == "UD" {
					// UD 也可能不带 refund_fee：只在成功态核对金额。
					fields["refund_fee"] = json.Number("0.01")
				}
				_, _ = w.Write(refundRespBody(t, fields))
			}))
			defer srv.Close()

			res, err := Refund(context.Background(), refundChannel(srv.URL), refundReq())
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if res.Status != c.want {
				t.Fatalf("状态映射错误: got=%s want=%s", res.Status, c.want)
			}
		})
	}
}

// TestXunhuRefundSignInvalid 返回体验签失败：**绝不当作失败**，必须是「未知」。
//
// 判成失败会回滚额度；若这笔退款其实已成功，用户就白拿额度。
func TestXunhuRefundSignInvalid(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := refundRespBody(t, map[string]any{
			"errcode":       json.Number("0"),
			"out_refund_no": "RF1",
			"refund_fee":    json.Number("0.01"),
			"refund_status": "CD",
		})
		// 篡改任意字段（模拟中间人/错误实现）
		_, _ = w.Write([]byte(strings.Replace(string(body), `"RF1"`, `"RF2"`, 1)))
	}))
	defer srv.Close()

	_, err := Refund(context.Background(), refundChannel(srv.URL), refundReq())
	if !errors.Is(err, ErrRefundUnknown) {
		t.Fatalf("验签失败应为未知态，实际: %v", err)
	}
	if errors.Is(err, ErrRefundRejected) {
		t.Fatal("验签失败绝不能被判为拒绝（会回滚额度）")
	}
}

// TestXunhuRefundAmountMismatch 上游退款金额与订单金额不一致 → 未知（不能判定成功）。
func TestXunhuRefundAmountMismatch(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(refundRespBody(t, map[string]any{
			"errcode":       json.Number("0"),
			"out_refund_no": "RF1",
			"refund_fee":    json.Number("0.02"),
			"refund_status": "CD",
		}))
	}))
	defer srv.Close()

	_, err := Refund(context.Background(), refundChannel(srv.URL), refundReq())
	if !errors.Is(err, ErrRefundUnknown) {
		t.Fatalf("金额不符应为未知态，实际: %v", err)
	}
}

// TestXunhuRefundErrcodeClassification 错误码分类：只有「确定没退款」才允许回滚。
func TestXunhuRefundErrcodeClassification(t *testing.T) {
	allowLoopback(t)
	cases := []struct {
		name string
		msg  string
		want error
	}{
		{"参数错误", "invalid sign!", ErrRefundRejected},
		{"订单不存在", "order not exist", ErrRefundRejected},
		{"已退款", "该订单已退款", ErrRefundUnknown},
		{"处理中", "refunding, please wait", ErrRefundUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(refundRespBody(t, map[string]any{
					"errcode": json.Number("500"),
					"errmsg":  c.msg,
				}))
			}))
			defer srv.Close()

			_, err := Refund(context.Background(), refundChannel(srv.URL), refundReq())
			if !errors.Is(err, c.want) {
				t.Fatalf("分类错误: got=%v want=%v", err, c.want)
			}
		})
	}
}

// TestXunhuRefundNestedData 兼容 data 嵌套形态（官方示例是平铺，但备用平台有嵌套返回）。
func TestXunhuRefundNestedData(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner := map[string]any{
			"out_refund_no": "RF9",
			"refund_fee":    json.Number("0.01"),
			"refund_status": "CD",
		}
		str := map[string]string{"out_refund_no": "RF9", "refund_fee": "0.01", "refund_status": "CD"}
		str["hash"] = xunhuSign(str, xunhuTestSecret)
		inner["hash"] = str["hash"]
		body, _ := json.Marshal(map[string]any{
			"errcode": json.Number("0"),
			"errmsg":  "success",
			"data":    inner,
		})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	res, err := Refund(context.Background(), refundChannel(srv.URL), refundReq())
	if err != nil {
		t.Fatalf("嵌套响应应被解析: %v", err)
	}
	if res.Status != RefundDone || res.RefundNo != "RF9" {
		t.Fatalf("嵌套响应解析错误: %+v", res)
	}
}

// TestRefundUnsupportedProtocol 易支付未接入退款：必须明确报「不支持」，而不是静默成功。
func TestRefundUnsupportedProtocol(t *testing.T) {
	ch := Channel{ID: "epay", Protocol: "epay", Params: ChannelParams{PID: "1", Key: "k"}}
	if _, err := Refund(context.Background(), ch, refundReq()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("易支付退款应返回 ErrUnsupported，实际: %v", err)
	}
	if RefundSupported("epay") {
		t.Fatal("易支付不应被判定为支持原路退款")
	}
	if !RefundSupported("XunhuPay") {
		t.Fatal("协议名大小写不应影响支持判定")
	}
}

// TestSwapPaymentFile 端点推导：三种配置形态都要指向同一目录下的目标文件。
func TestSwapPaymentFile(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.xunhupay.com/payment/do.html", "https://api.xunhupay.com/payment/refund.html"},
		{"https://api.dpweixin.com/payment/query.html", "https://api.dpweixin.com/payment/refund.html"},
		{"https://api.xunhupay.com", "https://api.xunhupay.com/payment/refund.html"},
		{"https://pay.example.com/gw/pay.php", "https://pay.example.com/gw/refund.html"},
		{"", ""},
	}
	for _, c := range cases {
		if got := swapPaymentFile(c.in, "refund.html"); got != c.want {
			t.Fatalf("swapPaymentFile(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}

	// 未配置 refund_address 时，从 pay_address 推导。
	ch := Channel{Params: ChannelParams{PayAddress: "https://api.xunhupay.com/payment/do.html"}}
	if got := xunhuRefundEndpoint(ch); got != "https://api.xunhupay.com/payment/refund.html" {
		t.Fatalf("退款地址推导错误: %s", got)
	}
	// 显式配置优先。
	ch.Params.RefundAddress = "https://api.diypc.com.cn/payment/refund.html"
	if got := xunhuRefundEndpoint(ch); got != ch.Params.RefundAddress {
		t.Fatalf("显式退款地址应优先: %s", got)
	}
}

// TestSanitizeReason 退款原因清洗：& = # 会破坏 k=v 拼接，控制字符/emoji 会被上游拒收。
func TestSanitizeReason(t *testing.T) {
	if got := SanitizeReason("  重复支付 & 用户退款=申请 #1  "); got != "重复支付  用户退款申请 1" {
		t.Fatalf("清洗结果不符: %q", got)
	}
	if got := SanitizeReason("退款🎉\x00说明"); got != "退款说明" {
		t.Fatalf("emoji/控制字符应被剔除: %q", got)
	}
	long := strings.Repeat("退", 100)
	if got := SanitizeReason(long); len([]rune(got)) != 80 {
		t.Fatalf("应按 rune 截断到 80，实际 %d", len([]rune(got)))
	}
}
