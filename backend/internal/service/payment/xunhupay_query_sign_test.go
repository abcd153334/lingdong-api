package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

/*
本文件守住 A5：查单是个能改订单状态的入口（响应说"已支付"就直接入账）。

两种结局必须分开：
  - 响应带 hash 且字段全是字符串 → 验签不过 = 不是网关发的，必须失败（订单保持 pending）；
  - 响应没带 hash / 含数值字段 → 无法可靠重算签名，按未验签采信，但记指标。
    后者是刻意的取舍：强制验签会让「上游不返回 hash」的渠道彻底不到账，
    那比伪造到账更常见，也更像我们的错。
*/

// querySignedBody 构造一份「全部是字符串」的查单响应（含正确签名）。
func querySignedBody(t *testing.T, fields map[string]string) []byte {
	t.Helper()
	cp := make(map[string]string, len(fields)+1)
	for k, v := range fields {
		cp[k] = v
	}
	cp["hash"] = xunhuSign(cp, xunhuTestSecret)
	b, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("序列化响应失败: %v", err)
	}
	return b
}

func querySrv(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
}

// 签名正确 → 正常返回已支付。
func TestXunhuQueryAcceptsSigned(t *testing.T) {
	allowLoopback(t)
	body := querySignedBody(t, map[string]string{
		"status": "OD", "total_amount": "0.01", "trade_order_id": "LD1", "transaction_id": "TXN1",
	})
	srv := querySrv(t, body)
	defer srv.Close()

	n, err := Query(context.Background(), xunhuTestChannel(srv.URL+"/payment/do.html"), "LD1", "")
	if err != nil {
		t.Fatalf("验签通过的查单不应失败: %v", err)
	}
	if n == nil || n.Kind != NotifyPaid || n.AmountCent != 1 {
		t.Fatalf("应解析为已支付 1 分，实际 %+v", n)
	}
}

// 带 hash 但对不上 → 必须失败（伪造的"已支付"进不来）。
func TestXunhuQueryRejectsBadSignature(t *testing.T) {
	allowLoopback(t)
	raw := map[string]string{
		"status": "OD", "total_amount": "0.01", "trade_order_id": "LD1", "hash": "deadbeef",
	}
	body, _ := json.Marshal(raw)
	srv := querySrv(t, body)
	defer srv.Close()

	_, err := Query(context.Background(), xunhuTestChannel(srv.URL+"/payment/do.html"), "LD1", "")
	if !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("签名不符必须按验签失败处理，实际 %v", err)
	}
}

// 不带 hash → 采信（记指标），不能因为"没有签名"就永远不到账。
func TestXunhuQueryAcceptsUnsigned(t *testing.T) {
	allowLoopback(t)
	body, _ := json.Marshal(map[string]string{
		"status": "OD", "total_amount": "0.01", "trade_order_id": "LD1",
	})
	srv := querySrv(t, body)
	defer srv.Close()

	n, err := Query(context.Background(), xunhuTestChannel(srv.URL+"/payment/do.html"), "LD1", "")
	if err != nil {
		t.Fatalf("无签名的查单仍要采信，实际 %v", err)
	}
	if n == nil || n.Kind != NotifyPaid {
		t.Fatalf("应解析为已支付，实际 %+v", n)
	}
}

// 含数值字段 + hash（无法可靠重算）→ 采信，不能判失败。
func TestXunhuQueryAcceptsNumericUnverifiable(t *testing.T) {
	allowLoopback(t)
	body, _ := json.Marshal(map[string]any{
		"status": "OD", "total_amount": json.Number("0.01"), "trade_order_id": "LD1", "hash": "whatever",
	})
	srv := querySrv(t, body)
	defer srv.Close()

	n, err := Query(context.Background(), xunhuTestChannel(srv.URL+"/payment/do.html"), "LD1", "")
	if err != nil {
		t.Fatalf("含数值字段时无法验签，应按采信处理，实际 %v", err)
	}
	if n == nil || n.Kind != NotifyPaid {
		t.Fatalf("应解析为已支付，实际 %+v", n)
	}
}

// 配置里残留的 http query_address 必须被忽略，回落到 pay_address 推导（恒为 https）。
func TestXunhuQueryEndpointIgnoresPlainHTTP(t *testing.T) {
	ch := xunhuTestChannel("https://api.xunhupay.com/payment/do.html")
	ch.Params.QueryAddress = "http://evil.example.com/payment/query.html"
	got := xunhuQueryEndpoint(ch)
	if got != "https://api.xunhupay.com/payment/query.html" {
		t.Fatalf("查单地址必须强制 https 并回落 pay_address，实际 %q", got)
	}
}
