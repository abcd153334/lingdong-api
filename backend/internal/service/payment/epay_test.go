package payment

import (
	"errors"
	"testing"
)

// TestEPaySignMatchesOfficialVector 用**硬编码期望值**锁住易支付的签名串规则。
//
// 为什么必须硬编码而不是「Sign 出来再 Verify」：签名与验签共用 epaySign，
// 自签自验在实现写错时永远通过。这个函数曾经就是多写了一个尾随 `&`
// （`a=1&b=2&` + key），所有 round-trip 测试全绿，但上游一律判签名错误。
//
// 向量来源：参数按 ASCII 升序拼成 `a=1&b=2`（末尾无 &）后**直接**拼密钥再 MD5。
// 复算命令：printf '%s%s' "<拼接串>" "testkey" | md5sum
func TestEPaySignMatchesOfficialVector(t *testing.T) {
	params := map[string]string{
		"money":        "100.00",
		"name":         "充值-100元",
		"notify_url":   "https://api.example.com/api/pay/epay1/notify",
		"out_trade_no": "LD01HXYZABCDEFGHJKMNPQRS",
		"pid":          "1001",
		"return_url":   "https://example.com/pay/return",
		"sitename":     "灵动机场",
		"type":         "wxpay",
		"sign_type":    "MD5", // 必须被剔除，不参与签名
	}

	// 官方规则：..."&type=wxpay" + "testkey"
	const want = "930de945af3a52ae818e6036c7bf8890"
	// 历史错值：..."&type=wxpay&" + "testkey"（多一个尾随 &）。留作反向断言防退化。
	const withTrailingAmp = "cc97dfc5a78cc1e468cad6cf56f61755"

	got := Sign(params, "testkey")
	if got != want {
		t.Fatalf("易支付签名与官方规则不一致\n得到: %s\n期望: %s\n"+
			"提示：签名串末尾不能带 &（应为 a=1&b=2 直接接 key）", got, want)
	}
	if got == withTrailingAmp {
		t.Fatalf("签名串仍带尾随 &：这是上游必判签名错误的写法")
	}
}

// TestEPayParseNotifyTradeStatus 锁住回调状态映射。
//
// 易支付系（彩虹/聚合等）的成功值是 TRADE_SUCCESS；只认 SUCCESS 会让
// 「钱收了、额度不到账」且应答仍是 success（上游停止重试），是最难查的一类丢账。
func TestEPayParseNotifyTradeStatus(t *testing.T) {
	ch := Channel{
		ID: "epay1", Name: "易支付", Protocol: string(ProtocolEpay), Enabled: true,
		Params: ChannelParams{PayAddress: "https://pay.example.com", PID: "1001", Key: "testkey"},
	}

	cases := []struct {
		status string
		want   NotifyKind
	}{
		{"TRADE_SUCCESS", NotifyPaid},
		{"SUCCESS", NotifyPaid},       // 兼容少数自研网关
		{"trade_success", NotifyPaid}, // 大小写不敏感
		{"WAIT_BUYER_PAY", NotifyIgnored},
		{"TRADE_CLOSED", NotifyIgnored},
		{"", NotifyIgnored},
	}

	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			params := map[string]string{
				"out_trade_no": "LD1", "money": "1.00", "pid": "1001", "trade_no": "T1",
			}
			if c.status != "" {
				params["trade_status"] = c.status
			}
			params["sign"] = Sign(params, "testkey")

			n, err := epayParseNotify(ch, params)
			if err != nil {
				t.Fatalf("解析回调失败: %v", err)
			}
			if n.Kind != c.want {
				t.Fatalf("trade_status=%q → Kind=%v，期望 %v", c.status, n.Kind, c.want)
			}
		})
	}
}

// TestEPayParseNotifyMissingOutTradeNo 区分「字段缺失」与「签名被伪造」。
//
// 缺 out_trade_no 时签名已经通过，说明是上游报文问题；若复用 ErrSignInvalid，
// 上层会按「验签失败」记 Warn，排障方向直接跑偏。
func TestEPayParseNotifyMissingOutTradeNo(t *testing.T) {
	ch := Channel{
		ID: "epay1", Name: "易支付", Protocol: string(ProtocolEpay), Enabled: true,
		Params: ChannelParams{PayAddress: "https://pay.example.com", PID: "1001", Key: "testkey"},
	}
	params := map[string]string{"money": "1.00", "trade_status": "TRADE_SUCCESS"}
	params["sign"] = Sign(params, "testkey")

	_, err := epayParseNotify(ch, params)
	if !errors.Is(err, ErrGatewayRejected) {
		t.Fatalf("缺 out_trade_no 应为 ErrGatewayRejected，实得 %v", err)
	}
	if errors.Is(err, ErrSignInvalid) {
		t.Fatalf("字段缺失不能报成验签失败")
	}
}

// TestEPayParseNotifyInvalidMoney 金额格式不合法时要带上原始值，便于对账排查。
func TestEPayParseNotifyInvalidMoney(t *testing.T) {
	ch := Channel{
		ID: "epay1", Name: "易支付", Protocol: string(ProtocolEpay), Enabled: true,
		Params: ChannelParams{PayAddress: "https://pay.example.com", PID: "1001", Key: "testkey"},
	}
	params := map[string]string{"out_trade_no": "LD1", "money": "1.234", "trade_status": "TRADE_SUCCESS"}
	params["sign"] = Sign(params, "testkey")

	_, err := epayParseNotify(ch, params)
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("金额非法应为 ErrInvalidAmount，实得 %v", err)
	}
}

// TestSubmitEndpointOnlyLooksAtPath 网关地址归一化只看路径，不看域名。
//
// 「整个 URL 里找 submit」的写法会把 https://submit.xxx.com/ 当成已指向提交端点，
// 于是下单打到站点根路径（404 或一段首页 HTML），而报错只说「响应无法解析」——
// 排查方向会跑到签名上去。
func TestSubmitEndpointOnlyLooksAtPath(t *testing.T) {
	cases := map[string]string{
		"https://pay.example.com":            "https://pay.example.com/submit.php",
		"https://pay.example.com/":           "https://pay.example.com/submit.php",
		"https://pay.example.com/pay":        "https://pay.example.com/pay/submit.php",
		"https://pay.example.com/submit.php": "https://pay.example.com/submit.php",
		"https://pay.example.com/Submit.PHP": "https://pay.example.com/Submit.PHP",
		"https://pay.example.com/submit":     "https://pay.example.com/submit",
		"https://pay.example.com/x/submit":   "https://pay.example.com/x/submit",
		// 域名含 submit，路径为空 → 仍要补端点。
		"https://submit.example.com":      "https://submit.example.com/submit.php",
		"https://submit.epay.xyz/gateway": "https://submit.epay.xyz/gateway/submit.php",
		"":                                "",
	}
	for in, want := range cases {
		if got := submitEndpoint(in); got != want {
			t.Fatalf("submitEndpoint(%q) = %q，期望 %q", in, got, want)
		}
	}
}
