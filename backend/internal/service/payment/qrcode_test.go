package payment

import (
	"context"
	"net/http"
	"testing"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

// TestResolveQRForOfficialProtocolsNeverFetch 官方直连的二维码必须是「零出站」。
//
// 这是支付宝上线即故障的回归点：`qr_code` 是 `https://qr.alipay.com/...`，
// 按 scheme 嗅探会被当成图片地址去拉，拿回 HTML 后被拒 → 前端显示「二维码已失效」。
// 这里把 qrClient 换成会 panic 的实现：只要真的发起了请求，用例立刻炸。
func TestResolveQRForOfficialProtocolsNeverFetch(t *testing.T) {
	prev := qrClient
	qrClient = func(timeout time.Duration, p netguard.Policy, hops int) *http.Client {
		t.Fatal("官方直连的二维码不应发起任何网络请求")
		return nil
	}
	defer func() { qrClient = prev }()

	cases := []struct {
		protocol Protocol
		ref      string
	}{
		{ProtocolAlipay, "https://qr.alipay.com/bax0123456789"},
		{ProtocolWxpay, "weixin://wxpay/bizpayurl?pr=abc"},
	}
	for _, c := range cases {
		payload, err := ResolveQRFor(context.Background(), c.protocol, c.ref)
		if err != nil {
			t.Fatalf("%s 应直接返回内容型: %v", c.protocol, err)
		}
		if payload.Mode != QRModeContent {
			t.Fatalf("%s 的二维码模式应为 content，实际 %s", c.protocol, payload.Mode)
		}
		if payload.Content != c.ref {
			t.Fatalf("%s 的内容应原样返回，实际 %q", c.protocol, payload.Content)
		}
		if payload.DataURL != "" {
			t.Fatalf("%s 不应产生 data_url（说明拉了图）", c.protocol)
		}
	}
}

// TestResolveQRForEmptyAndLegacyEmpty 空值返回 none；未知协议保持旧的嗅探行为。
func TestResolveQRForEmptyAndLegacyEmpty(t *testing.T) {
	p, err := ResolveQRFor(context.Background(), ProtocolAlipay, "   ")
	if err != nil || p.Mode != QRModeNone {
		t.Fatalf("空 qr_ref 应返回 none，实际 %+v / %v", p, err)
	}
	// 未知协议 + 非 http 前缀：仍是内容型（等价改造前的行为）。
	p2, err := ResolveQRFor(context.Background(), "", "weixin://wxpay/bizpayurl?pr=x")
	if err != nil || p2.Mode != QRModeContent {
		t.Fatalf("非 http 前缀应返回 content，实际 %+v / %v", p2, err)
	}
}
