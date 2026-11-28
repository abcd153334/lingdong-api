package conf

import (
	"testing"
)

// TestGetenvBool 校验布尔开关的解析口径。
//
// 为什么单独测：`LD_METRICS_HTTP` 这类开关一旦解析错，症状是「明明配了却没生效」——
// 而它恰恰是部署时唯一能决定「指标口开不开」的东西，错了很难从现象反推。
// 特别是**非法值必须回落默认值**：把 `treu` 当 false 会静默关掉指标口。
func TestGetenvBool(t *testing.T) {
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"", false, false},
		{"", true, true},
		{"1", false, true},
		{"true", false, true},
		{"TRUE", false, true},
		{" yes ", false, true},
		{"on", false, true},
		{"0", true, false},
		{"false", true, false},
		{"off", true, false},
		{"no", true, false},
		// 非法值：回落 def（既不当 true 也不当 false）
		{"treu", false, false},
		{"treu", true, true},
		{"2", true, true},
	}
	for _, c := range cases {
		t.Setenv("LD_TEST_BOOL", c.val)
		if got := getenvBool("LD_TEST_BOOL", c.def); got != c.want {
			t.Fatalf("getenvBool(%q, %v) = %v，期望 %v", c.val, c.def, got, c.want)
		}
	}
}

// TestEnvDefaultsForB7 校验 B7 三个开关的默认值：全部关闭（安全默认）。
//
// 默认值本身就是产品决策：保留期默认 0 = 不删数据；/metrics 默认关闭 = 不暴露拓扑。
// 任何一次「顺手改成默认开启」都应当让这条用例失败。
func TestEnvDefaultsForB7(t *testing.T) {
	t.Setenv("LD_TASK_RETENTION_DAYS", "")
	t.Setenv("LD_METRICS_HTTP", "")
	t.Setenv("LD_CAPACITY_WARN_PERCENT", "")
	t.Setenv("LD_CAPACITY_MIN_FREE_GB", "")

	env := Load()
	if env.TaskRetentionDays != 0 {
		t.Fatalf("LD_TASK_RETENTION_DAYS 默认应为 0（只报告不删），实际 %d", env.TaskRetentionDays)
	}
	if env.MetricsHTTP {
		t.Fatal("LD_METRICS_HTTP 默认应为关闭")
	}
	if env.CapacityWarnPercent != 85 {
		t.Fatalf("容量告警阈值默认应为 85，实际 %d", env.CapacityWarnPercent)
	}
	if env.CapacityWarnMinFreeGB != 5 {
		t.Fatalf("剩余空间下限默认应为 5GB，实际 %d", env.CapacityWarnMinFreeGB)
	}
}
