package main

import (
	"testing"
	"time"
)

// TestHTTPReadTimeout 守住读超时的解析口径。
//
// 这条配置的默认值不是"随便填的 120s"：它是**慢 body 攻击的唯一上限**。
// 上下文：此前 `http.Server` 只设了 ReadHeaderTimeout，请求体永远读不完也不放手，
// 一个未认证连接就能声明 Content-Length: 4096 然后只发 10 字节，
// 实测被保持 25s 以上且无任何响应（见 .audit/perf/faults.py 的 F7）。
// 而它又必须可关（LD_HTTP_READ_TIMEOUT=0），否则跨境慢链路上传会被误杀。
func TestHTTPReadTimeout(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want time.Duration
	}{
		{"默认 120s", "", false, 120 * time.Second},
		{"显式 3s", "3", true, 3 * time.Second},
		{"0 表示不设", "0", true, 0},
		// 写错不该静默变成"不设"：那会让慢 body 防护悄悄消失。
		{"非法值回落默认", "abc", true, 120 * time.Second},
		{"负数回落默认", "-5", true, 120 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("LD_HTTP_READ_TIMEOUT", c.env)
			} else {
				t.Setenv("LD_HTTP_READ_TIMEOUT", "")
				// t.Setenv 设成空串等价于"存在但为空"，函数里按空串处理，
				// 语义与"未设置"一致 —— 两种情况都必须走默认值。
			}
			if got := httpReadTimeout(); got != c.want {
				t.Fatalf("httpReadTimeout() = %v，应为 %v", got, c.want)
			}
		})
	}
}
