package router

import "testing"

// TestPayNotifyRoutesAcceptBothMethods 守住支付回调的**方法**注册。
//
// 为什么必须单独守：易支付系（彩虹/聚合等）的异步通知是 GET，参数挂在 query 上。
// 只注册 POST 时 Gin 的 NoRoute 返回 404，网关把它当成「地址不可用」重试至放弃；
// 而本地日志里只有一条 404 访问记录，看不出是「方法没注册」还是「渠道配错」。
//
// 同时这条测试也验证路由树能构建：`/api/pay/notify` 与 `/api/pay/:channel/notify`
// 是「静态段」与「参数段」共存，Gin 在注册冲突时会 panic —— 那类问题只在启动瞬间暴露。
func TestPayNotifyRoutesAcceptBothMethods(t *testing.T) {
	r := New()
	got := map[string]bool{}
	for _, ri := range r.Routes() {
		got[ri.Method+" "+ri.Path] = true
	}

	want := []string{
		"POST /api/pay/:channel/notify", // 微信 / 支付宝 / 虎皮椒
		"GET /api/pay/:channel/notify",  // 易支付系
		"POST /api/pay/notify",          // 易支付老地址（无渠道参数）
		"GET /api/pay/notify",
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("支付回调路由缺失: %s", w)
		}
	}
}
