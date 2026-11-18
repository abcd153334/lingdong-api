package channelhealth

import "testing"

// TestEvaluate 覆盖熔断判定全部分支（阈值/窗口/开关）。
func TestEvaluate(t *testing.T) {
	on := Policy{AutoDisable: true, FailThreshold: 0.5, FailWindow: 10, AutoEnable: true, ProbeIntervalMin: 5}
	cases := []struct {
		name    string
		total   int64
		fails   int64
		pol     Policy
		wantBan bool
	}{
		{"样本不足不熔断", 9, 9, on, false},
		{"样本达标且超阈值熔断", 10, 6, on, true},
		{"样本达标刚好等于阈值不熔断", 10, 5, on, false},
		{"低于阈值不熔断", 20, 3, on, false},
		{"全局关闭不熔断", 20, 20, Policy{AutoDisable: false, FailThreshold: 0.5, FailWindow: 10}, false},
		{"阈值为 0 不熔断", 20, 20, Policy{AutoDisable: true, FailThreshold: 0, FailWindow: 10}, false},
		{"窗口为 0 不熔断", 20, 20, Policy{AutoDisable: true, FailThreshold: 0.5, FailWindow: 0}, false},
	}
	for _, tc := range cases {
		if got := Evaluate(tc.total, tc.fails, tc.pol); got != tc.wantBan {
			t.Errorf("%s: Evaluate(%d,%d)=%v，期望 %v", tc.name, tc.total, tc.fails, got, tc.wantBan)
		}
	}
}

// TestCurrentPolicyDefaults 未配置 monitor.* 时兜底默认值与 seed 一致。
func TestCurrentPolicyDefaults(t *testing.T) {
	p := CurrentPolicy()
	if !p.AutoDisable || !p.AutoEnable {
		t.Fatalf("默认应开启自动禁用与自动恢复: %+v", p)
	}
	if p.FailThreshold != 0.5 || p.FailWindow != 10 || p.ProbeIntervalMin != 5 {
		t.Fatalf("默认参数与 seed 不一致: %+v", p)
	}
}
