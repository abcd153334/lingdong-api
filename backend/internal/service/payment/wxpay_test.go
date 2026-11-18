package payment

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

/* -------------------- 测试用密钥与报文构造 -------------------- */

// wxTestKey 生成一对 RSA 密钥（2048 位，够快且与生产同算法）。
func wxTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return k
}

func pemPrivate(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, k)}))
}

func pemPublic(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("公钥序列化失败: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func mustPKCS8(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("私钥序列化失败: %v", err)
	}
	return der
}

// wxTestChannel 组装一个「公钥模式」的微信渠道。
func wxTestChannel(t *testing.T, platformPub string) Channel {
	t.Helper()
	merchant := wxTestKey(t)
	return Channel{
		ID: "wxpay", Name: "微信直连", Protocol: string(ProtocolWxpay), Enabled: true,
		Methods: []string{"wechat"},
		Params: ChannelParams{
			AppID:        "wx0000000000000001",
			MchID:        "1600000001",
			CertSerialNo: "5157F09EFDC096DE15EBE81A47057A7232F1B8E1",
			APIv3Key:     "0123456789abcdef0123456789abcdef",
			PrivateKey:   pemPrivate(t, merchant),
			PublicKey:    platformPub,
			PublicKeyID:  "PUB_KEY_ID_0000000000000000000000000000000001",
		},
	}
}

// wxSignHeader 按微信规则给一段 body 签名（签名对象：timestamp\nnonce\nbody\n）。
func wxSignHeader(t *testing.T, signer *rsa.PrivateKey, serial string, body []byte, ts time.Time) http.Header {
	t.Helper()
	nonce := "testnonce12345"
	message := fmt.Sprintf("%d\n%s\n%s\n", ts.Unix(), nonce, string(body))
	sum := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, signer, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	h := http.Header{}
	h.Set("Wechatpay-Serial", serial)
	h.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	h.Set("Wechatpay-Signature-Type", "WECHATPAY2-SHA256-RSA2048")
	h.Set("Wechatpay-Timestamp", fmt.Sprintf("%d", ts.Unix()))
	h.Set("Wechatpay-Nonce", nonce)
	return h
}

// wxEncryptResource 用 APIv3 密钥加密 resource（AEAD_AES_256_GCM）。
func wxEncryptResource(t *testing.T, apiV3Key, plaintext string) (ciphertext, nonce, ad string) {
	t.Helper()
	block, err := aes.NewCipher([]byte(apiV3Key))
	if err != nil {
		t.Fatalf("AES 失败: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("GCM 失败: %v", err)
	}
	nonce = "abcdefghijkl" // 12 字节
	ad = "transaction"
	sealed := gcm.Seal(nil, []byte(nonce), []byte(plaintext), []byte(ad))
	return base64.StdEncoding.EncodeToString(sealed), nonce, ad
}

// wxBuildNotify 构造一条完整的微信回调请求。
func wxBuildNotify(t *testing.T, ch Channel, signer *rsa.PrivateKey, eventType, plaintext string, ts time.Time) NotifyRequest {
	t.Helper()
	cipherText, nonce, ad := wxEncryptResource(t, ch.Params.APIv3Key, plaintext)
	envelope := map[string]any{
		"id":            "evt-0001",
		"create_time":   ts.Format(time.RFC3339),
		"event_type":    eventType,
		"resource_type": "encrypt-resource",
		"summary":       "支付成功",
		"resource": map[string]string{
			"algorithm":       "AEAD_AES_256_GCM",
			"ciphertext":      cipherText,
			"associated_data": ad,
			"nonce":           nonce,
			"original_type":   "transaction",
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("序列化回调失败: %v", err)
	}
	header := wxSignHeader(t, signer, ch.Params.PublicKeyID, body, ts)
	return NotifyRequest{Body: body, Header: header}
}

/* -------------------- 回调：验签 + 解密 -------------------- */

func TestWxpayNotifyPaidDecryptsAndMaps(t *testing.T) {
	platform := wxTestKey(t)
	ch := wxTestChannel(t, pemPublic(t, platform))

	plaintext := `{"mchid":"1600000001","appid":"wx0000000000000001","out_trade_no":"LD01JYZ0K3M4N5P6Q7R8S9T0V1W",
		"transaction_id":"4200002000202610230000000001","trade_state":"SUCCESS",
		"success_time":"2026-10-23T15:30:00+08:00",
		"amount":{"total":1,"payer_total":1,"currency":"CNY"}}`
	req := wxBuildNotify(t, ch, platform, "TRANSACTION.SUCCESS", plaintext, time.Now())

	n, err := wxpayParseNotify(ch, req)
	if err != nil {
		t.Fatalf("应验签并解密成功: %v", err)
	}
	if n.Kind != NotifyPaid {
		t.Fatalf("event_type=TRANSACTION.SUCCESS 应映射为 paid，实际 %s", n.Kind)
	}
	if n.OutTradeNo != "LD01JYZ0K3M4N5P6Q7R8S9T0V1W" {
		t.Fatalf("订单号不符: %s", n.OutTradeNo)
	}
	// 金额必须取 amount.total（不是 payer_total）：用了代金券时后者更小。
	if n.AmountCent != 1 {
		t.Fatalf("金额应为 1 分，实际 %d", n.AmountCent)
	}
	if n.MerchantID != "1600000001" {
		t.Fatalf("商户号应来自回调里的 mchid，实际 %q", n.MerchantID)
	}
	if n.Raw["_event_type"] != "TRANSACTION.SUCCESS" || n.Raw["_plaintext"] == "" {
		t.Fatalf("原始明文必须落进 Raw 供排查: %+v", n.Raw)
	}
	// EventID 必须是通知 ID：payment_events 靠它做事件级幂等，留空等于微信回调
	// 完全不进事件表（与 Stripe 侧口径不一致，事后只能一条条翻 notify_raw）。
	if n.EventID != "evt-0001" {
		t.Fatalf("EventID 应取信封 id，实际 %q", n.EventID)
	}
}

func TestWxpayNotifyRejectsTamperedBody(t *testing.T) {
	platform := wxTestKey(t)
	ch := wxTestChannel(t, pemPublic(t, platform))
	req := wxBuildNotify(t, ch, platform, "TRANSACTION.SUCCESS",
		`{"mchid":"1600000001","out_trade_no":"X","amount":{"total":1}}`, time.Now())

	// 改一个字节（信封里的 id）：签名对象变了，必须拒绝。
	// 注意不能改 resource 里的明文 —— 那是密文，改信封才是「验签原文变了」。
	if !bytes.Contains(req.Body, []byte("evt-0001")) {
		t.Fatalf("测试前提不成立：信封里没有预期字段，body=%s", req.Body)
	}
	req.Body = bytes.Replace(req.Body, []byte("evt-0001"), []byte("evt-9999"), 1)
	if _, err := wxpayParseNotify(ch, req); err == nil {
		t.Fatal("篡改 body 后必须验签失败")
	}
}

func TestWxpayNotifyRejectsStaleTimestamp(t *testing.T) {
	platform := wxTestKey(t)
	ch := wxTestChannel(t, pemPublic(t, platform))
	// 10 分钟前：SDK 内置 5 分钟时间窗，必须拒绝（这是防重放的核心，手写版最容易漏）。
	req := wxBuildNotify(t, ch, platform, "TRANSACTION.SUCCESS",
		`{"mchid":"1600000001","out_trade_no":"X","amount":{"total":1}}`,
		time.Now().Add(-10*time.Minute))
	if _, err := wxpayParseNotify(ch, req); err == nil {
		t.Fatal("时间戳超窗必须拒绝（否则回调可无限重放）")
	}
}

func TestWxpayNotifyRefundEvents(t *testing.T) {
	platform := wxTestKey(t)
	ch := wxTestChannel(t, pemPublic(t, platform))
	plaintext := `{"mchid":"1600000001","out_trade_no":"LD01","transaction_id":"4200",
		"out_refund_no":"RFLD01","refund_id":"5000","refund_status":"SUCCESS",
		"amount":{"total":1,"refund":1,"payer_total":1,"currency":"CNY"}}`

	// 三种退款事件的处置必须区分：ABNORMAL 不是终态失败（钱已划出、停在微信中间账户），
	// 归成 NotifyRefundFailed 会让上层回滚额度，用户于是既拿额度又拿上游退款。
	cases := map[string]NotifyKind{
		"REFUND.SUCCESS":  NotifyRefunded,
		"REFUND.ABNORMAL": NotifyRefundUnknown,
		"REFUND.CLOSED":   NotifyRefundFailed, // CLOSED 才是终态失败：资金留在商户
	}
	for event, want := range cases {
		req := wxBuildNotify(t, ch, platform, event, plaintext, time.Now())
		n, err := wxpayParseNotify(ch, req)
		if err != nil {
			t.Fatalf("%s 应解析成功: %v", event, err)
		}
		if n.Kind != want {
			t.Fatalf("%s 应映射为 %s，实际 %s", event, want, n.Kind)
		}
		if n.AmountCent != 1 {
			t.Fatalf("退款回调的 amount.total 应作订单总额，实际 %d", n.AmountCent)
		}
	}
}

func TestWxpayNotifyPlatformCertMode(t *testing.T) {
	// 平台证书模式：回调头是十六进制序列号，按 serial 精确匹配。
	platform := wxTestKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(0x5157F09E),
		Subject:      pkix.Name{CommonName: "Tenpay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &platform.PublicKey, platform)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}

	ch := wxTestChannel(t, "")
	ch.Params.PublicKey = ""
	ch.Params.PublicKeyID = ""
	ch.Params.PlatformCert = certPEM

	plaintext := `{"mchid":"1600000001","out_trade_no":"LD01","amount":{"total":1}}`
	cipherText, nonce, ad := wxEncryptResource(t, ch.Params.APIv3Key, plaintext)
	body, _ := json.Marshal(map[string]any{
		"id": "evt-2", "event_type": "TRANSACTION.SUCCESS", "resource_type": "encrypt-resource",
		"resource": map[string]string{
			"algorithm": "AEAD_AES_256_GCM", "ciphertext": cipherText,
			"associated_data": ad, "nonce": nonce, "original_type": "transaction",
		},
	})
	header := wxSignHeader(t, platform, strings.ToUpper(cert.SerialNumber.Text(16)), body, time.Now())

	n, err := wxpayParseNotify(ch, NotifyRequest{Body: body, Header: header})
	if err != nil {
		t.Fatalf("平台证书模式应验签成功: %v", err)
	}
	if n.AmountCent != 1 {
		t.Fatalf("金额应为 1 分，实际 %d", n.AmountCent)
	}
}

func TestWxpayNotifyMissingMaterial(t *testing.T) {
	ch := wxTestChannel(t, "")
	ch.Params.PublicKey = ""
	ch.Params.PublicKeyID = ""
	// 既无公钥也无平台证书：必须明确报「缺材料」，而不是含糊的验签失败。
	_, err := wxpayParseNotify(ch, NotifyRequest{Body: []byte(`{}`), Header: http.Header{}})
	if err == nil || !strings.Contains(err.Error(), "验签材料") {
		t.Fatalf("应提示缺少验签材料，实际: %v", err)
	}
}

/* -------------------- 下单 / 查单（httptest 桩） -------------------- */

// rewriteRT 把发往 api.mch.weixin.qq.com 的请求重定向到本地桩，
// 保留 path/query —— 这样生产代码里的 SDK 调用路径与真实环境完全一致。
type rewriteRT struct {
	base *url.URL
	next http.RoundTripper
}

func (r rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.base.Scheme
	clone.URL.Host = r.base.Host
	clone.Host = ""
	return r.next.RoundTrip(clone)
}

// useWxStub 起一个桩，把 outboundClient / outboundPolicy 指向它，并在用例结束时还原。
func useWxStub(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析桩地址失败: %v", err)
	}
	prevClient, prevPolicy := outboundClient, outboundPolicy
	outboundPolicy = netguard.Policy{AllowLoopback: true}
	outboundClient = func(timeout time.Duration, p netguard.Policy) *http.Client {
		return &http.Client{Transport: rewriteRT{base: base, next: http.DefaultTransport}}
	}
	t.Cleanup(func() {
		srv.Close()
		outboundClient, outboundPolicy = prevClient, prevPolicy
		wxPool.Lock()
		wxPool.m = make(map[string]*wxEntry)
		wxPool.Unlock()
	})
	return srv
}

// wxStubJSON 让桩返回一段已签名的 JSON（SDK 会校验应答签名）。
func wxStubJSON(t *testing.T, signer *rsa.PrivateKey, serial string, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-ID", "req-1")
		for k, v := range wxSignHeader(t, signer, serial, []byte(body), time.Now()) {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestWxpayCreateReturnsCodeURL(t *testing.T) {
	platform := wxTestKey(t)
	ch := wxTestChannel(t, pemPublic(t, platform))

	var got map[string]any
	const respBody = `{"code_url":"weixin://wxpay/bizpayurl?pr=abc"}`
	useWxStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := new(bytes.Buffer)
		_, _ = raw.ReadFrom(r.Body)
		_ = json.Unmarshal(raw.Bytes(), &got)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-ID", "req-create")
		for k, v := range wxSignHeader(t, platform, ch.Params.PublicKeyID, []byte(respBody), time.Now()) {
			w.Header()[k] = v
		}
		_, _ = w.Write([]byte(respBody))
	}))

	expire := time.Now().Add(5 * time.Minute)
	res, err := Create(context.Background(), ch, CreateRequest{
		OutTradeNo: "LD01JYZ0K3M4N5P6Q7R8S9T0V1W", AmountCent: 1, Method: MethodWechat,
		Subject: "充值 1 积分", NotifyURL: "https://api.example.com/api/pay/wxpay/notify",
		Attach: "42", ExpireAt: expire,
	})
	if err != nil {
		t.Fatalf("下单应成功: %v", err)
	}
	if res.QRRef != "weixin://wxpay/bizpayurl?pr=abc" {
		t.Fatalf("QRRef 应为 code_url，实际 %q", res.QRRef)
	}
	// PayURL 必须留空：否则前端会露出「打开支付页面」按钮（weixin:// 点了没反应）。
	if res.PayURL != "" {
		t.Fatalf("微信扫码渠道的 PayURL 必须为空，实际 %q", res.PayURL)
	}
	// 请求体关键字段：金额是**分**（整数），且带 time_expire。
	if v, ok := got["amount"].(map[string]any); !ok || v["total"] != float64(1) {
		t.Fatalf("amount.total 必须是整数分，实际 %v", got["amount"])
	}
	if got["out_trade_no"] != "LD01JYZ0K3M4N5P6Q7R8S9T0V1W" {
		t.Fatalf("out_trade_no 不符: %v", got["out_trade_no"])
	}
	if _, ok := got["time_expire"]; !ok {
		t.Fatal("必须传 time_expire（否则上游按 2 小时有效，与本地 5 分钟 TTL 分叉）")
	}
}

func TestWxpayCreateRejectsHttpNotifyURL(t *testing.T) {
	platform := wxTestKey(t)
	ch := wxTestChannel(t, pemPublic(t, platform))
	_, err := Create(context.Background(), ch, CreateRequest{
		OutTradeNo: "LD01", AmountCent: 1, Method: MethodWechat, Subject: "x",
		NotifyURL: "http://api.example.com/api/pay/wxpay/notify",
	})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http 回调地址必须被拒（微信要求 https），实际: %v", err)
	}
}

func TestWxpayErrorMapping(t *testing.T) {
	cases := []struct {
		body string
		want error
	}{
		{`{"code":"ORDERPAID","message":"已支付"}`, ErrOrderExists},
		{`{"code":"OUT_TRADE_NO_USED","message":"单号重复"}`, ErrOrderExists},
		{`{"code":"SIGN_ERROR","message":"签名错误"}`, ErrGatewayRejected},
		{`{"code":"PARAM_ERROR","message":"参数错误"}`, ErrGatewayRejected},
		{`{"code":"FREQUENCY_LIMITED","message":"限频"}`, ErrGatewayBusy},
	}
	for i, c := range cases {
		platform := wxTestKey(t)
		ch := wxTestChannel(t, pemPublic(t, platform))
		useWxStub(t, wxStubJSON(t, platform, "-", 400, c.body))

		_, err := Create(context.Background(), ch, CreateRequest{
			OutTradeNo: fmt.Sprintf("LD01ERR%d", i), AmountCent: 1, Method: MethodWechat, Subject: "x",
			NotifyURL: "https://api.example.com/n",
		})
		if !errors.Is(err, c.want) {
			t.Fatalf("code=%s 应映射为 %v，实际 %v", c.body, c.want, err)
		}
	}
}

func TestWxTradeStateKind(t *testing.T) {
	cases := map[string]NotifyKind{
		"SUCCESS": NotifyPaid, "REFUND": NotifyRefunded,
		"NOTPAY": NotifyIgnored, "USERPAYING": NotifyIgnored, "PAYERROR": NotifyIgnored,
		"CLOSED": NotifyIgnored, "REVOKED": NotifyIgnored,
	}
	for state, want := range cases {
		if got := wxTradeStateKind(state); got != want {
			t.Fatalf("trade_state=%s 应映射为 %s，实际 %s", state, want, got)
		}
	}
}
