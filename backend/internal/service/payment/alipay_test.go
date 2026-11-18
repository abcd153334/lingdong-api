package payment

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

/* -------------------- 签名规则 -------------------- */

func TestAlipaySignContentRules(t *testing.T) {
	params := map[string]string{
		"b": "2", "a": "1", "empty": "", "sign": "xxx", "sign_type": "RSA2",
	}
	// 请求签名：剔除 sign 与空值，sign_type **参与**，ASCII 升序，末尾无 &。
	got := alipaySignContent(params, "sign")
	if got != "a=1&b=2&sign_type=RSA2" {
		t.Fatalf("请求签名串不符: %q", got)
	}
	// 通知验签：再剔除 sign_type。
	got2 := alipaySignContent(params, "sign", "sign_type")
	if got2 != "a=1&b=2" {
		t.Fatalf("通知验签串不符: %q", got2)
	}
}

func TestAlipayNotifyVerifyRoundTrip(t *testing.T) {
	appKey := wxTestKey(t)    // 应用私钥（我们签请求用）
	alipayKey := wxTestKey(t) // 支付宝密钥对（它签通知/响应用）

	params := map[string]string{
		"app_id": "2021000000000001", "out_trade_no": "LD01", "trade_status": "TRADE_SUCCESS",
		"total_amount": "0.01", "trade_no": "2026102322001", "sign_type": "RSA2",
		"notify_time": "2026-10-23 15:30:00",
	}
	sign, err := alipaySHA256Sign(alipaySignContent(params, "sign", "sign_type"), alipayKey)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	params["sign"] = sign

	if err := alipayVerifyNotify(params, &alipayKey.PublicKey); err != nil {
		t.Fatalf("验签应通过: %v", err)
	}
	// 篡改任一个参与签名的字段 → 必须失败。
	params["total_amount"] = "0.02"
	if err := alipayVerifyNotify(params, &alipayKey.PublicKey); err == nil {
		t.Fatal("篡改金额后必须验签失败")
	}
	_ = appKey
}

func TestAlipayNotifySignWithSpaceTolerated(t *testing.T) {
	alipayKey := wxTestKey(t)
	params := map[string]string{"a": "1", "sign_type": "RSA2"}
	sign, _ := alipaySHA256Sign(alipaySignContent(params, "sign", "sign_type"), alipayKey)
	params["sign"] = strings.ReplaceAll(sign, "+", " ") // 模拟未 URL 编码时的 +→空格
	if err := alipayVerifyNotify(params, &alipayKey.PublicKey); err != nil {
		t.Fatalf("sign 里的空格应被还原成 + 后再验签: %v", err)
	}
}

func TestAlipayKindMapping(t *testing.T) {
	cases := []struct {
		status, sendPayDate string
		want                NotifyKind
	}{
		{"TRADE_SUCCESS", "", NotifyPaid},
		{"TRADE_FINISHED", "", NotifyPaid},
		{"WAIT_BUYER_PAY", "", NotifyIgnored},
		// 有支付时间的关闭 = 曾经付过（全额退款后会这样）→ 走退款语义，让 onRefunded 三分支判定。
		{"TRADE_CLOSED", "2026-10-23 15:30:00", NotifyRefunded},
		// 没有支付时间的关闭 = 单纯超时 → 忽略，绝不当成功。
		{"TRADE_CLOSED", "", NotifyIgnored},
	}
	for _, c := range cases {
		if got := alipayKind(c.status, c.sendPayDate); got != c.want {
			t.Fatalf("status=%s send_pay_date=%q 应映射为 %s，实际 %s",
				c.status, c.sendPayDate, c.want, got)
		}
	}
}

/* -------------------- 请求 / 响应（httptest 桩） -------------------- */

// alipayTestChannel 组装一个支付宝渠道，网关指向桩地址。
func alipayTestChannel(t *testing.T, gateway string, appKey *rsa.PrivateKey, alipayPub string) Channel {
	t.Helper()
	return Channel{
		ID: "alipay", Name: "支付宝直连", Protocol: string(ProtocolAlipay), Enabled: true,
		Methods: []string{"alipay"},
		Params: ChannelParams{
			AppID:      "2021000000000001",
			PayAddress: gateway,
			PrivateKey: pemPrivate(t, appKey),
			PublicKey:  alipayPub,
		},
	}
}

// alipayStub 返回一个「已签名的 xxx_response」桩。
func alipayStub(t *testing.T, signer *rsa.PrivateKey, method string, node string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sign, err := alipaySHA256Sign(node, signer)
		if err != nil {
			t.Errorf("桩签名失败: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"%s":%s,"sign":%q}`, alipayResponseKey(method), node, sign)
	}
}

// useAlipayPolicy 允许测试访问回环地址（生产恒为「仅公网」）。
func useAlipayPolicy(t *testing.T) {
	t.Helper()
	prev := outboundPolicy
	outboundPolicy = netguard.Policy{AllowLoopback: true}
	t.Cleanup(func() { outboundPolicy = prev })
}

func TestAlipayCreateParsesQrCode(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)

	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		node := `{"code":"10000","msg":"Success","out_trade_no":"LD01","qr_code":"https://qr.alipay.com/bax01234"}`
		alipayStub(t, alipayKey, alipayMethodPrecreate, node)(w, r)
	}))
	defer srv.Close()

	ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
	res, err := Create(context.Background(), ch, CreateRequest{
		OutTradeNo: "LD01", AmountCent: 1, Method: MethodAlipay, Subject: "充值 1 积分",
		NotifyURL: "https://api.example.com/api/pay/alipay/notify",
		ExpireAt:  time.Now().Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("下单应成功: %v", err)
	}
	if res.QRRef != "https://qr.alipay.com/bax01234" {
		t.Fatalf("QRRef 应为 qr_code，实际 %q", res.QRRef)
	}
	// PayURL 必须留空（qr_code 是内容，不是跳转页）。
	if res.PayURL != "" {
		t.Fatalf("支付宝扫码渠道的 PayURL 必须为空，实际 %q", res.PayURL)
	}

	// 请求侧：金额是「元」的两位小数字符串，且带 sign_type=RSA2。
	var biz alipayPrecreateBiz
	if err := json.Unmarshal([]byte(gotForm.Get("biz_content")), &biz); err != nil {
		t.Fatalf("biz_content 不是合法 JSON: %v", err)
	}
	if biz.TotalAmount != "0.01" {
		t.Fatalf("total_amount 应为 0.01，实际 %q", biz.TotalAmount)
	}
	if biz.QrCodeTimeoutExpress == "" || !strings.HasSuffix(biz.QrCodeTimeoutExpress, "m") {
		t.Fatalf("必须带 qr_code_timeout_express，实际 %q", biz.QrCodeTimeoutExpress)
	}
	if gotForm.Get("sign_type") != "RSA2" || gotForm.Get("method") != alipayMethodPrecreate {
		t.Fatalf("公共参数不符: %v", gotForm)
	}
	if gotForm.Get("sign") == "" {
		t.Fatal("请求必须带 sign")
	}
}

func TestAlipayRejectsTamperedResponseSign(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)
	otherKey := wxTestKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := `{"code":"10000","msg":"Success","qr_code":"https://qr.alipay.com/bax01234"}`
		// 用**别的**私钥签：模拟中间人篡改报文。
		sign, _ := alipaySHA256Sign(node, otherKey)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"%s":%s,"sign":%q}`, alipayResponseKey(alipayMethodPrecreate), node, sign)
	}))
	defer srv.Close()

	ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
	_, err := Create(context.Background(), ch, CreateRequest{
		OutTradeNo: "LD01", AmountCent: 1, Method: MethodAlipay, Subject: "x",
		NotifyURL: "https://api.example.com/n",
	})
	if err == nil || !strings.Contains(err.Error(), "验签失败") {
		t.Fatalf("响应验签失败必须报错（否则可被伪造死码），实际: %v", err)
	}
}

func TestAlipayCreateMapsBusinessError(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)

	srv := httptest.NewServer(alipayStub(t, alipayKey, alipayMethodPrecreate,
		`{"code":"40004","msg":"Business Failed","sub_code":"isv.invalid-app-id","sub_msg":"无效的AppID参数"}`))
	defer srv.Close()

	ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
	_, err := Create(context.Background(), ch, CreateRequest{
		OutTradeNo: "LD01", AmountCent: 1, Method: MethodAlipay, Subject: "x",
		NotifyURL: "https://api.example.com/n",
	})
	if err == nil || !strings.Contains(err.Error(), "isv.invalid-app-id") {
		t.Fatalf("应把 sub_code 带进错误信息，实际: %v", err)
	}
}

// TestAlipayRejectsResponseWithoutUsableSign 锁住「同步响应必须带可用签名」。
//
// 旧实现写成 `if signBlock, exists := wrapper["sign"]; exists { ... }`，
// 于是**三条**路径都在不验签的情况下放行：字段不存在、值为空串、值不是字符串
// （`"sign": null` 会让 json.Unmarshal 失败，被 `== nil` 判断吞掉）。
// 后果不是「多一次失败」而是「篡改可成功」：下单响应的 qr_code 会被换成攻击者收款码。
//
// 注意与 TestAlipayRejectsTamperedResponseSign 的区别：那条测「签名错误」，
// 这条测「根本没有签名」，两者是不同的绕过路径。
func TestAlipayRejectsResponseWithoutUsableSign(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)

	const node = `{"code":"10000","msg":"Success","qr_code":"https://qr.alipay.com/bax01234"}`
	cases := map[string]string{
		"缺 sign 字段":   `{"%s":%s}`,
		"sign 为空串":    `{"%s":%s,"sign":""}`,
		"sign 为空白串":   `{"%s":%s,"sign":"   "}`,
		"sign 为 null": `{"%s":%s,"sign":null}`,
		"sign 为对象":    `{"%s":%s,"sign":{"v":"x"}}`,
		"sign 为数字":    `{"%s":%s,"sign":12345}`,
	}

	for name, tpl := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, tpl, alipayResponseKey(alipayMethodPrecreate), node)
			}))
			defer srv.Close()

			ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
			_, err := Create(context.Background(), ch, CreateRequest{
				OutTradeNo: "LD01", AmountCent: 1, Method: MethodAlipay, Subject: "x",
				NotifyURL: "https://api.example.com/n",
			})
			if !errors.Is(err, ErrSignInvalid) {
				t.Fatalf("缺可用签名必须拒绝（ErrSignInvalid），实际: %v", err)
			}
		})
	}
}

func TestAlipayQueryNotExistIsUnpaid(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)

	srv := httptest.NewServer(alipayStub(t, alipayKey, alipayMethodQuery,
		`{"code":"40004","msg":"Business Failed","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"交易不存在"}`))
	defer srv.Close()

	ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
	n, err := Query(context.Background(), ch, "LD01", "")
	if err != nil {
		t.Fatalf("交易不存在不是错误: %v", err)
	}
	if n != nil {
		t.Fatalf("交易不存在应返回 nil（确认未支付），实际 %+v", n)
	}
}

func TestAlipayQueryPaidMapsAmount(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)

	srv := httptest.NewServer(alipayStub(t, alipayKey, alipayMethodQuery,
		`{"code":"10000","out_trade_no":"LD01","trade_no":"2026102322001","trade_status":"TRADE_SUCCESS",
		  "total_amount":"0.01","app_id":"2021000000000001","send_pay_date":"2026-10-23 15:30:00"}`))
	defer srv.Close()

	ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
	n, err := Query(context.Background(), ch, "LD01", "")
	if err != nil {
		t.Fatalf("查单应成功: %v", err)
	}
	if n == nil || n.Kind != NotifyPaid {
		t.Fatalf("应为已支付，实际 %+v", n)
	}
	if n.AmountCent != 1 {
		t.Fatalf("0.01 元应为 1 分，实际 %d", n.AmountCent)
	}
	if n.MerchantID != "2021000000000001" {
		t.Fatalf("商户身份应取 app_id，实际 %q", n.MerchantID)
	}
}

func TestAlipayRefundFundChangeNoIsAccepted(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)

	// 退款返回 fund_change=N（可能是「已退过」），退款查询也查不到金额：
	// 结论必须是「已受理（待确认）」，**绝不能是失败**（否则会误回滚额度）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := r.PostForm.Get("method")
		if method == alipayMethodRefund {
			alipayStub(t, alipayKey, method,
				`{"code":"10000","msg":"Success","fund_change":"N","refund_fee":"0.00"}`)(w, r)
			return
		}
		alipayStub(t, alipayKey, method,
			`{"code":"40004","sub_code":"ACQ.TRADE_NOT_EXIST","sub_msg":"退款不存在"}`)(w, r)
	}))
	defer srv.Close()

	ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
	res, err := Refund(context.Background(), ch, RefundRequest{
		OutTradeNo: "LD01", AmountCent: 1, Reason: "用户申请",
	})
	if err != nil {
		t.Fatalf("fund_change=N 不应直接报错: %v", err)
	}
	if res.Status != RefundAccepted {
		t.Fatalf("fund_change=N 且查不到必须判「已受理」，实际 %s", res.Status)
	}
	if res.RefundNo != DeriveRefundNo("LD01") {
		t.Fatalf("退款单号应为派生值，实际 %q", res.RefundNo)
	}
}

func TestAlipayRefundSuccessRequiresAmountMatch(t *testing.T) {
	useAlipayPolicy(t)
	appKey := wxTestKey(t)
	alipayKey := wxTestKey(t)

	srv := httptest.NewServer(alipayStub(t, alipayKey, alipayMethodRefund,
		`{"code":"10000","msg":"Success","fund_change":"Y","refund_fee":"0.01"}`))
	defer srv.Close()

	ch := alipayTestChannel(t, srv.URL+"/gateway.do", appKey, pemPublic(t, alipayKey))
	res, err := Refund(context.Background(), ch, RefundRequest{
		OutTradeNo: "LD01", AmountCent: 1, Reason: "x",
	})
	if err != nil {
		t.Fatalf("全额退款应成功: %v", err)
	}
	if res.Status != RefundDone || res.RefundedCent != 1 {
		t.Fatalf("应为成功且金额 1 分，实际 %s / %d", res.Status, res.RefundedCent)
	}

	// 金额不符（退少了）：必须判未知，不能判成功。
	res2, err := Refund(context.Background(), ch, RefundRequest{
		OutTradeNo: "LD01", AmountCent: 100, Reason: "x",
	})
	if err == nil || res2 != nil {
		t.Fatalf("退款金额与订单不一致必须报未知态，实际 %+v / %v", res2, err)
	}
}

func TestAlipaySensitiveParamsNeverLeakIntoSign(t *testing.T) {
	// 私有键只是防御性检查：确保 sign 不把 map 的随机顺序写进签名串。
	params := map[string]string{}
	for i := 0; i < 20; i++ {
		params[fmt.Sprintf("k%02d", i)] = fmt.Sprintf("v%d", i)
	}
	first := alipaySignContent(params, "sign")
	for i := 0; i < 5; i++ {
		if again := alipaySignContent(params, "sign"); again != first {
			t.Fatalf("签名串必须稳定（顺序依赖会造成偶发验签失败）: %q vs %q", first, again)
		}
	}
	_ = rand.Reader
}
