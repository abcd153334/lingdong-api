package order

import (
	"testing"

	"lingdong-api/internal/service/payment"
)

// TestNotifyResultAck 锁住「什么情况回 success」。
//
// 这是支付回调最关键的一个约定：回错方向要么让网关无限重试（把网关拖死、并掩盖真实原因），
// 要么让真正该重试的内部错误被当成已处理（丢单）。所以逐个断言，不靠注释。
func TestNotifyResultAck(t *testing.T) {
	cases := map[NotifyResult]string{
		NotifyDone:       "success", // 已处理/重复回调
		NotifyOrphan:     "success", // 验签通过但订单不存在：让网关重试也变不出订单
		NotifyNeedManual: "success", // 已转人工，止血
		NotifyReject:     "fail",    // 验签失败：可能是配置错误，重试给管理员修复时间
		NotifyRetry:      "fail",    // DB 抖动：唯一真正该重试的
	}
	for r, want := range cases {
		if got := r.Ack(); got != want {
			t.Errorf("NotifyResult(%d).Ack()=%q want %q", r, got, want)
		}
	}
}

// TestMerchantOf 商户号取值：易支付取 pid、虎皮椒取 appid。
// 取错会让「回调商户号校验」静默失效（两边都不为空却比的是不同字段）。
func TestMerchantOf(t *testing.T) {
	xunhu := payment.Channel{Protocol: "xunhupay", Params: payment.ChannelParams{AppID: "app-1", PID: "pid-should-be-ignored"}}
	if got := merchantOf(xunhu); got != "app-1" {
		t.Fatalf("虎皮椒应取 app_id，实际 %q", got)
	}
	epay := payment.Channel{Protocol: "epay", Params: payment.ChannelParams{PID: "1001", AppID: "app-should-be-ignored"}}
	if got := merchantOf(epay); got != "1001" {
		t.Fatalf("易支付应取 pid，实际 %q", got)
	}
	unknown := payment.Channel{Protocol: "", Params: payment.ChannelParams{AppID: "a", PID: "p"}}
	if got := merchantOf(unknown); got != "" {
		t.Fatalf("未知协议不应猜字段，实际 %q", got)
	}
}
