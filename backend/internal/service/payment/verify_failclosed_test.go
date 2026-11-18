package payment

import (
	"errors"
	"testing"
)

// TestVerifyRejectsEmptyKey 锁死「空密钥 = 无签名」这条铁律。
//
// 为什么必须单独守：`epaySign(params, "")` / `xunhuSign(params, "")` 并不是「弱签名」，
// 而是「参数串联后的裸 MD5」—— 任何看过源码的人都能自己算出来。一旦空密钥被当作
// 合法配置放行，公开的回调端点就变成「免费充值接口」（伪造 out_trade_no + 金额 + 自算签名）。
func TestVerifyRejectsEmptyKey(t *testing.T) {
	params := map[string]string{
		"out_trade_no": "LD01ABCDEF",
		"money":        "10.00",
		"trade_status": "SUCCESS",
	}
	// 攻击者能算出的那个签名：用空密钥正常签名。
	params["sign"] = epaySign(params, "")

	if epayVerify(params, "") {
		t.Fatal("空密钥时 epayVerify 必须返回 false（否则伪造回调可入账）")
	}
	if epayVerify(params, "   ") {
		t.Fatal("空白密钥同样必须拒绝（TrimSpace 后为空）")
	}

	xunhu := map[string]string{
		"trade_order_id": "LD01ABCDEF",
		"total_fee":      "10.00",
		"status":         "OD",
	}
	xunhu["hash"] = xunhuSign(xunhu, "")
	if xunhuVerify(xunhu, "") {
		t.Fatal("空密钥时 xunhuVerify 必须返回 false")
	}

	// 对照组：有密钥时同一组参数必须验签通过，确认上面的失败是「因为空密钥」而不是拼串写错。
	if !epayVerify(map[string]string{
		"out_trade_no": "LD01ABCDEF", "money": "10.00", "trade_status": "SUCCESS",
		"sign": epaySign(map[string]string{
			"out_trade_no": "LD01ABCDEF", "money": "10.00", "trade_status": "SUCCESS",
		}, "real-key"),
	}, "real-key") {
		t.Fatal("有密钥时验签必须通过")
	}
}

// TestParseNotifyEmptyKeyIsConfigError 锁死「配置问题」与「验签失败」必须可区分。
//
// 上层据此选择 ERROR（要运维处理）还是 Warn（公开端点的日常噪音）。
// 两者混在一起，要么真正的配置事故被噪音淹没，要么每条扫描请求都触发告警。
func TestParseNotifyEmptyKeyIsConfigError(t *testing.T) {
	epayCh := Channel{
		ID: "epay1", Name: "易支付", Protocol: "epay", Enabled: true,
		Params: ChannelParams{PayAddress: "https://pay.example.com/", PID: "1001"},
	}
	if _, err := epayParseNotify(epayCh, map[string]string{"out_trade_no": "X", "money": "1.00"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("易支付空密钥应返回 ErrNotConfigured，实得 %v", err)
	}

	xunhuCh := Channel{
		ID: "xunhupay1", Name: "虎皮椒", Protocol: "xunhupay", Enabled: true,
		Params: ChannelParams{PayAddress: "https://pay.example.com/", AppID: "2019061201"},
	}
	if _, err := xunhuParseNotify(xunhuCh, map[string]string{"trade_order_id": "X", "status": "OD"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("虎皮椒空密钥应返回 ErrNotConfigured，实得 %v", err)
	}

	// 有密钥但签名不对：必须是 ErrSignInvalid（噪音），不能升级成配置事故。
	epayCh.Params.Key = "real-key"
	if _, err := epayParseNotify(epayCh, map[string]string{
		"out_trade_no": "X", "money": "1.00", "sign": "deadbeef",
	}); !errors.Is(err, ErrSignInvalid) {
		t.Fatalf("签名错误应为 ErrSignInvalid，实得 %v", err)
	}
}

// TestChannelUsableRejectsIncomplete 锁死「必填不全的渠道不能出现在收银台」。
//
// 反例（本测试防的就是它）：运营新加一条易支付渠道、忘了填密钥就启用 →
// 收银台照样显示「微信支付」→ 用户付了钱 → 回调因为无法验签被 fail-closed 拒绝 →
// 订单永远 pending，只能人工退款。
func TestChannelUsableRejectsIncomplete(t *testing.T) {
	ok := Channel{
		ID: "epay1", Name: "易支付", Protocol: "epay", Enabled: true,
		Params: ChannelParams{PayAddress: "https://pay.example.com/", PID: "1001", Key: "k"},
	}
	if !ChannelUsable(ok) {
		t.Fatal("配置齐全的启用渠道必须可用")
	}

	noKey := ok
	noKey.Params.Key = ""
	if ChannelUsable(noKey) {
		t.Fatal("缺密钥的启用渠道必须不可用（否则用户付钱后回调必被拒）")
	}

	disabled := ok
	disabled.Enabled = false
	if ChannelUsable(disabled) {
		t.Fatal("停用渠道必须不可用")
	}

	noProto := ok
	noProto.Protocol = ""
	if ChannelUsable(noProto) {
		t.Fatal("空协议（未实现占位）必须不可用")
	}
}
