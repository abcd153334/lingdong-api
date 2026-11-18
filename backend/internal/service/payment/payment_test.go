package payment

import (
	"net/url"
	"strings"
	"testing"
)

// TestSignDeterministic 校验签名口径：参数剔除 sign/sign_type 与空值、按 key 升序、末尾追加密钥。
func TestSignDeterministic(t *testing.T) {
	params := map[string]string{
		"pid": "1001", "out_trade_no": "LD1", "money": "72.00",
		"name": "体验包", "sign_type": "MD5", "empty": "",
	}
	got := Sign(params, "secret")
	if Sign(params, "secret") != got {
		t.Fatalf("签名不稳定: %s", got)
	}
	if Sign(params, "other") == got {
		t.Fatalf("更换密钥后签名不应相同")
	}
	// sign_type / 空值必须被剔除：与只保留参与签名参数的结果一致
	if filtered := Sign(map[string]string{
		"pid": "1001", "out_trade_no": "LD1", "money": "72.00", "name": "体验包",
	}, "secret"); filtered != got {
		t.Fatalf("sign_type/空值未被剔除: %s != %s", got, filtered)
	}
}

// TestVerify 校验验签大小写不敏感与非法签名拒绝。
func TestVerify(t *testing.T) {
	params := map[string]string{"pid": "1001", "out_trade_no": "LD1", "money": "72.00", "trade_status": "SUCCESS"}
	params["sign"] = Sign(params, "secret")
	if !Verify(params, "secret") {
		t.Fatalf("合法签名应验签通过")
	}
	params["sign"] = strings.ToUpper(params["sign"])
	if !Verify(params, "secret") {
		t.Fatalf("签名大小写不敏感")
	}
	params["sign"] = "deadbeef"
	if Verify(params, "secret") {
		t.Fatalf("非法签名不应通过")
	}
	delete(params, "sign")
	if Verify(params, "secret") {
		t.Fatalf("缺签名不应通过")
	}
}

// TestCashierURL 校验收银台地址参数完整、签名可复算且端点归一化。
func TestCashierURL(t *testing.T) {
	ch := Channel{Name: "易支付", Enabled: true, Methods: []string{"wechat"},
		Params: ChannelParams{PayAddress: "https://pay.example.com", PID: "1001", Key: "secret"}}
	// 金额参数是**分**：7200 分 = 72.00 元
	raw, err := CashierURL(ch, MethodWechat, "LD1", 7200, "体验包", "https://api.example.com/api/pay/notify", "https://www.example.com/topup")
	if err != nil {
		t.Fatalf("生成收银台地址失败: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("收银台地址非法: %v", err)
	}
	if u.Path != "/submit.php" {
		t.Fatalf("端点应归一化为 /submit.php，实际 %s", u.Path)
	}
	q := u.Query()
	if q.Get("pid") != "1001" || q.Get("money") != "72.00" || q.Get("out_trade_no") != "LD1" {
		t.Fatalf("核心参数缺失: %v", q)
	}
	// 上游 type 必须是英文码：中文「微信支付」会被易支付判为非法支付方式而拒单。
	if got := q.Get("type"); got != "wxpay" {
		t.Fatalf("type 应为 wxpay，实际 %q", got)
	}
	if q.Get("notify_url") != "https://api.example.com/api/pay/notify" {
		t.Fatalf("notify_url 缺失: %v", q)
	}
	// 用返回地址中的参数复算签名必须一致
	params := map[string]string{}
	for k, v := range q {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}
	if !Verify(params, "secret") {
		t.Fatalf("收银台地址签名不可复算")
	}

	// 已指向 submit 端点时不再追加
	ch.Params.PayAddress = "https://pay.example.com/submit"
	raw2, err := CashierURL(ch, MethodWechat, "LD2", 1, "x", "", "")
	if err != nil {
		t.Fatalf("生成收银台地址失败: %v", err)
	}
	if !strings.HasPrefix(raw2, "https://pay.example.com/submit?") {
		t.Fatalf("端点重复追加: %s", raw2)
	}
}

// TestSubmitEndpoint 校验端点归一化规则。
func TestSubmitEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://a.com", "https://a.com/submit.php"},
		{"https://a.com/", "https://a.com/submit.php"},
		{"https://a.com/submit", "https://a.com/submit"},
		{"https://a.com/submit.php", "https://a.com/submit.php"},
		{"", ""},
	}
	for _, c := range cases {
		if got := submitEndpoint(c.in); got != c.want {
			t.Errorf("submitEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
