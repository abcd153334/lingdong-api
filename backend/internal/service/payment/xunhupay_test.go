package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

const xunhuTestSecret = "secret"

// xunhuTestChannel 造一个指向 httptest 的虎皮椒渠道。
func xunhuTestChannel(endpoint string) Channel {
	return Channel{
		ID: "xunhupay", Name: "虎皮椒", Protocol: "xunhupay", Enabled: true,
		Methods: []string{"聚合支付"},
		Params: ChannelParams{
			PayAddress: endpoint, AppID: "2019061201", AppSecret: xunhuTestSecret,
		},
	}
}

// allowLoopback 让单测能访问 httptest 的 127.0.0.1；生产策略恒为「仅公网」。
func allowLoopback(t *testing.T) {
	t.Helper()
	oldPolicy, oldClient := outboundPolicy, outboundClient
	outboundPolicy = netguard.Policy{AllowLoopback: true, AllowPrivate: true}
	outboundClient = func(timeout time.Duration, _ netguard.Policy) *http.Client {
		return &http.Client{Timeout: timeout}
	}
	t.Cleanup(func() { outboundPolicy, outboundClient = oldPolicy, oldClient })
}

// TestXunhuSignMatchesOfficialVector 用官方规则手算的向量逐字节比对。
//
// 该向量同时锁住三件事：hash 被剔除、空值被剔除、参数按 ASCII 升序。
// 期望值 = md5("appid=2019061201&nonce_str=abc&time=1700000000&total_fee=0.01&trade_order_id=LD1" + "secret")
func TestXunhuSignMatchesOfficialVector(t *testing.T) {
	params := map[string]string{
		"appid":          "2019061201",
		"trade_order_id": "LD1",
		"total_fee":      "0.01",
		"nonce_str":      "abc",
		"time":           "1700000000",
		"hash":           "should-be-ignored", // 调用方误传也必须剔除
		"callback_url":   "",                  // 空值不参与签名（官方 demo 在这里就是错的）
	}
	const want = "6021e52bd51b6e1e14a41309bcc3442d"
	if got := xunhuSign(params, xunhuTestSecret); got != want {
		t.Fatalf("签名与官方向量不一致:\n got=%s\nwant=%s", got, want)
	}
	// 与「只保留参与签名字段」的 map 结果必须一致
	filtered := map[string]string{
		"appid": "2019061201", "trade_order_id": "LD1", "total_fee": "0.01",
		"nonce_str": "abc", "time": "1700000000",
	}
	if got := xunhuSign(filtered, xunhuTestSecret); got != want {
		t.Fatalf("hash/空值未被剔除: %s", got)
	}
}

// TestXunhuVerify 验签：大小写不敏感、缺 hash 拒绝、篡改任一字段拒绝。
func TestXunhuVerify(t *testing.T) {
	params := map[string]string{
		"trade_order_id": "LD1", "total_fee": "0.01", "status": "OD",
	}
	params["hash"] = xunhuSign(params, xunhuTestSecret)
	if !xunhuVerify(params, xunhuTestSecret) {
		t.Fatal("合法签名应通过")
	}
	params["hash"] = strings.ToUpper(params["hash"])
	if !xunhuVerify(params, xunhuTestSecret) {
		t.Fatal("签名应大小写不敏感")
	}
	params["total_fee"] = "100.00"
	if xunhuVerify(params, xunhuTestSecret) {
		t.Fatal("篡改金额后必须验签失败")
	}
	delete(params, "hash")
	if xunhuVerify(params, xunhuTestSecret) {
		t.Fatal("缺 hash 必须验签失败")
	}
}

// TestXunhuCreateSuccess 校验下单请求参数、返回解析与「服务端可复算签名」。
func TestXunhuCreateSuccess(t *testing.T) {
	allowLoopback(t)
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析表单失败: %v", err)
		}
		gotForm = r.PostForm
		resp := map[string]string{
			"openid":     "2019081202",
			"url":        "https://api.xunhupay.com/wxpay/pay/index.html?id=1",
			"url_qrcode": "https://api.xunhupay.com/qr/1.png",
			"errcode":    "0",
			"errmsg":     "success!",
		}
		resp["hash"] = xunhuSign(resp, xunhuTestSecret)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ch := xunhuTestChannel(srv.URL + "/payment/do.html")
	res, err := Create(context.Background(), ch, CreateRequest{
		OutTradeNo: "LD1", AmountCent: 1, Method: MethodAggregate,
		Subject: "充值-体验包", NotifyURL: "https://api.example.com/api/pay/xunhupay/notify",
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if res.PayURL == "" || res.QRRef == "" {
		t.Fatalf("应同时返回跳转地址与二维码: %+v", res)
	}
	if res.ChannelOrderID != "2019081202" {
		t.Fatalf("上游单号应取 openid（历史命名，值是 orderid）: %q", res.ChannelOrderID)
	}
	if res.ExpiresAt.Before(time.Now()) {
		t.Fatal("过期时间必须晚于现在")
	}

	// 请求参数契约
	if got := gotForm.Get("version"); got != "1.1" {
		t.Fatalf("version 应为 1.1，实际 %q", got)
	}
	if got := gotForm.Get("total_fee"); got != "0.01" {
		t.Fatalf("total_fee 必须是元字符串 0.01，实际 %q", got)
	}
	if got := gotForm.Get("trade_order_id"); got != "LD1" {
		t.Fatalf("trade_order_id 错误: %q", got)
	}
	if got := gotForm.Get("plugins"); got != "lingdong-api" {
		t.Fatalf("plugins 标识错误: %q", got)
	}
	if n := len(gotForm.Get("nonce_str")); n != 32 {
		t.Fatalf("nonce_str 应为 32 位，实际 %d", n)
	}
	// 最关键：服务端按收到的表单复算签名，必须与提交的 hash 一致
	formMap := map[string]string{}
	for k := range gotForm {
		formMap[k] = gotForm.Get(k)
	}
	if !xunhuVerify(formMap, xunhuTestSecret) {
		t.Fatalf("服务端无法复算签名: %v", formMap)
	}
}

// TestXunhuCreateGatewayErrors 覆盖 errcode 三分类。
func TestXunhuCreateGatewayErrors(t *testing.T) {
	allowLoopback(t)
	cases := []struct {
		name string
		resp map[string]string
		want error
	}{
		{"签名错误", map[string]string{"errcode": "500", "errmsg": "invalid sign!"}, ErrGatewayRejected},
		{"订单已存在", map[string]string{"errcode": "500", "errmsg": "order already exist"}, ErrOrderExists},
		{"网关繁忙", map[string]string{"errcode": "429", "errmsg": "too frequent"}, ErrGatewayBusy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(c.resp)
			}))
			defer srv.Close()
			_, err := Create(context.Background(), xunhuTestChannel(srv.URL+"/payment/do.html"), CreateRequest{
				OutTradeNo: "LD1", AmountCent: 1, Method: MethodAggregate, Subject: "x",
				NotifyURL: "https://api.example.com/n",
			})
			if !errors.Is(err, c.want) {
				t.Fatalf("应返回 %v，实际 %v", c.want, err)
			}
		})
	}
}

// TestXunhuCreateRejectsUnsignedSuccess 成功响应没有 hash 时必须拒绝。
//
// 这个响应的 url 会直接展示给用户，不验签等于「谁能改响应谁就能把用户导到钓鱼页」。
func TestXunhuCreateRejectsUnsignedSuccess(t *testing.T) {
	allowLoopback(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"errcode": "0", "errmsg": "success!", "url": "https://evil.example.com",
		})
	}))
	defer srv.Close()
	_, err := Create(context.Background(), xunhuTestChannel(srv.URL+"/payment/do.html"), CreateRequest{
		OutTradeNo: "LD1", AmountCent: 1, Method: MethodAggregate, Subject: "x", NotifyURL: "https://api.example.com/n",
	})
	if !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("缺 hash 的成功响应必须拒绝，实际 %v", err)
	}
}

// TestXunhuParseNotify 回调解析：字段映射正确、篡改金额被拒。
func TestXunhuParseNotify(t *testing.T) {
	ch := xunhuTestChannel("https://api.xunhupay.com/payment/do.html")
	params := map[string]string{
		"trade_order_id": "LD1", "total_fee": "0.01", "transaction_id": "4200001234",
		"open_order_id": "2019081202", "order_title": "充值-体验包", "status": "OD",
		"appid": "2019061201", "time": "1700000100", "nonce_str": "xyz",
	}
	// 用与生产同一算法算出的固定向量（见 xunhupay.go 注释里的拼接串）
	params["hash"] = xunhuSign(params, xunhuTestSecret)

	n, err := ParseNotify(ch, params)
	if err != nil {
		t.Fatalf("回调解析失败: %v", err)
	}
	if n.Kind != NotifyPaid {
		t.Fatalf("OD 应映射为 paid，实际 %q", n.Kind)
	}
	if n.AmountCent != 1 {
		t.Fatalf("金额应为 1 分，实际 %d", n.AmountCent)
	}
	if n.TradeNo != "4200001234" {
		t.Fatalf("交易号应取 transaction_id，实际 %q", n.TradeNo)
	}
	if n.ChannelOrderID != "2019081202" {
		t.Fatalf("上游单号应取 open_order_id，实际 %q", n.ChannelOrderID)
	}
	if n.MerchantID != "2019061201" {
		t.Fatalf("商户号应取 appid，实际 %q", n.MerchantID)
	}

	// 篡改金额 → 验签失败
	tampered := map[string]string{}
	for k, v := range params {
		tampered[k] = v
	}
	tampered["total_fee"] = "100.00"
	if _, err := ParseNotify(ch, tampered); !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("篡改金额后应验签失败，实际 %v", err)
	}
}

// TestXunhuNotifyKindMapping 覆盖四种状态与未知状态。
func TestXunhuNotifyKindMapping(t *testing.T) {
	cases := map[string]NotifyKind{
		"OD": NotifyPaid, "CD": NotifyRefunded, "RD": NotifyRefunding, "UD": NotifyRefundFailed,
		"od": NotifyPaid, "": NotifyIgnored, "XX": NotifyIgnored,
	}
	for in, want := range cases {
		got, ok := xunhuKind(in)
		if want == NotifyIgnored {
			if ok {
				t.Errorf("状态 %q 不应被识别", in)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("状态 %q → %q(ok=%v)，期望 %q", in, got, ok, want)
		}
	}
}

// TestXunhuQueryStructures 查单要同时兼容「字段在顶层」与「字段在 data 里」。
func TestXunhuQueryStructures(t *testing.T) {
	allowLoopback(t)
	paid := map[string]any{
		"status": "OD", "total_amount": "0.01", "transaction_id": "4200001234",
		"open_order_id": "2019081202", "out_trade_order": "LD1",
	}
	cases := []struct {
		name string
		body map[string]any
		nilN bool // 期望「确认未支付」
	}{
		{"顶层结构", paid, false},
		{"data 嵌套", map[string]any{"ret": 0, "msg": "ok", "data": paid}, false},
		{"未支付", map[string]any{"status": "WAIT"}, true},
		{"paid 别名", map[string]any{"status": "paid", "total_amount": "0.01"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(c.body)
			}))
			defer srv.Close()

			n, err := Query(context.Background(), xunhuTestChannel(srv.URL+"/payment/do.html"), "LD1", "")
			if err != nil {
				t.Fatalf("查单失败: %v", err)
			}
			if c.nilN {
				if n != nil {
					t.Fatalf("未支付应返回 (nil,nil)，实际 %+v", n)
				}
				return
			}
			if n == nil || n.Kind != NotifyPaid || n.AmountCent != 1 {
				t.Fatalf("已支付解析错误: %+v", n)
			}
		})
	}
}

// TestRandomNonceUnique 连续生成不得重复（官方 demo 直接复用时间戳，同秒重复）。
func TestRandomNonceUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		s := randomNonce(32)
		if len(s) != 32 {
			t.Fatalf("长度应为 32，实际 %d", len(s))
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("第 %d 次出现重复 nonce: %s", i, s)
		}
		seen[s] = struct{}{}
	}
}

// TestXunhuClassify errcode 分类。
func TestXunhuClassify(t *testing.T) {
	cases := []struct {
		code, msg string
		want      error
	}{
		{"0", "success!", nil},
		{"500", "invalid sign!", ErrGatewayRejected},
		{"500", "order already exist", ErrOrderExists},
		{"429", "too frequent", ErrGatewayBusy},
		{"", "", ErrGatewayRejected},
	}
	for _, c := range cases {
		err := xunhuClassify(c.code, c.msg)
		if c.want == nil {
			if err != nil {
				t.Fatalf("errcode=%s 应无错误，实际 %v", c.code, err)
			}
			continue
		}
		if !errors.Is(err, c.want) {
			t.Fatalf("errcode=%s msg=%s 应返回 %v，实际 %v", c.code, c.msg, c.want, err)
		}
	}
}

// TestXunhuEndpointNormalize 端点归一化与查单地址推导。
func TestXunhuEndpointNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.xunhupay.com/payment/do.html", "https://api.xunhupay.com/payment/do.html"},
		{"https://api.xunhupay.com", "https://api.xunhupay.com/payment/do.html"},
		{"https://api.dpweixin.com/", "https://api.dpweixin.com/payment/do.html"},
		{"", ""},
	}
	for _, c := range cases {
		if got := xunhuPayEndpoint(c.in); got != c.want {
			t.Errorf("xunhuPayEndpoint(%q)=%q want %q", c.in, got, c.want)
		}
	}
	ch := Channel{Params: ChannelParams{PayAddress: "https://api.xunhupay.com/payment/do.html"}}
	if got := xunhuQueryEndpoint(ch); got != "https://api.xunhupay.com/payment/query.html" {
		t.Errorf("查单地址推导错误: %q", got)
	}
	ch.Params.QueryAddress = "https://backup.example.com/payment/query.html"
	if got := xunhuQueryEndpoint(ch); got != "https://backup.example.com/payment/query.html" {
		t.Errorf("显式 query_address 应优先: %q", got)
	}
}
