package metrics

import (
	"strings"
	"testing"
)

// TestRenderCounterGaugeHistogram 校验三种类型的 exposition 形状。
//
// 断言的是**字节级格式**：Prometheus 的解析器对格式极挑（缺 +Inf 桶、重复序列、
// TYPE 与样本类型不符都会让整个抓取失败），而这些错误在本地肉眼看输出时并不明显。
func TestRenderCounterGaugeHistogram(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Inc(NameGatewayRequests, "endpoint", "/v1/chat/completions", "status", "200")
	Inc(NameGatewayRequests, "endpoint", "/v1/chat/completions", "status", "200")
	Set(NameStorageBytes, 2048)
	Observe(NameGatewayLatency, 0.03, "endpoint", "/v1/chat/completions")
	Observe(NameGatewayLatency, 90, "endpoint", "/v1/chat/completions")

	out := Render()
	for _, want := range []string{
		"# TYPE " + NameGatewayRequests + " counter",
		`gateway_requests_total{endpoint="/v1/chat/completions",status="200"} 2`,
		"# TYPE " + NameStorageBytes + " gauge",
		"storage_bytes 2048",
		"# TYPE " + NameGatewayLatency + " histogram",
		`gateway_latency_seconds_bucket{endpoint="/v1/chat/completions",le="0.05"} 1`,
		`gateway_latency_seconds_bucket{endpoint="/v1/chat/completions",le="+Inf"} 2`,
		`gateway_latency_seconds_sum{endpoint="/v1/chat/completions"} 90.03`,
		`gateway_latency_seconds_count{endpoint="/v1/chat/completions"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q\n--- 实际输出 ---\n%s", want, out)
		}
	}
	// 90s 不落入任何有限桶：le="30" 与 le="60" 的累计值都应仍是 1（只有 0.03 那个样本），
	// 只有 +Inf 是 2。若把 90 也算进 60 桶，这里会立刻变成 2 而被抓出来。
	if !strings.Contains(out, `le="60"} 1`) {
		t.Fatalf("90s 不应落入 60s 桶\n%s", out)
	}
}

// TestRenderEscapesLabels 标签值必须转义反斜杠/引号/换行。
//
// 不转义的后果不是「显示难看」：一个带换行的标签值会凭空多出一行样本，
// 抓取端直接解析失败，而值来源往往是上游错误文案（完全不可控）。
func TestRenderEscapesLabels(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Set(NameStorageBytes, 1, "reason", "a\"b\\c\nd")
	out := Render()
	if !strings.Contains(out, `reason="a\"b\\c\nd"`) {
		t.Fatalf("标签值未正确转义:\n%s", out)
	}
	if strings.Count(out, "\n") < 2 {
		t.Fatalf("转义后仍产生了额外换行:\n%q", out)
	}
}

// TestRenderSortedAndDeterministic 输出必须确定（两次抓取字节相同）。
func TestRenderSortedAndDeterministic(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Inc(NameUsageAnomaly, "kind", "b")
	Inc(NameUsageAnomaly, "kind", "a")
	Set(NameStorageBytes, 5)
	first := Render()
	second := Render()
	if first != second {
		t.Fatalf("两次渲染不一致:\n%s\n---\n%s", first, second)
	}
	if strings.Index(first, `kind="a"`) > strings.Index(first, `kind="b"`) {
		t.Fatalf("标签序列未排序:\n%s", first)
	}
}

// TestGaugeFuncGathersAndReplaces 采集函数在抓取时求值，重复注册覆盖旧函数。
//
// 「覆盖」是必须的：router.New() 在测试里会被多次调用，追加语义会输出重复序列。
func TestGaugeFuncGathersAndReplaces(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	RegisterGaugeFunc(NameTaskInflight, func() float64 { return 3 })
	RegisterGaugeFunc(NameTaskInflight, func() float64 { return 7 })
	out := Render()
	if !strings.Contains(out, "task_inflight 7") {
		t.Fatalf("采集函数未覆盖/未求值:\n%s", out)
	}
	// 只数「样本行」：HELP 行里也含指标名（后接空格 + 中文描述），直接数子串会误判。
	samples := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "task_inflight ") {
			samples++
		}
	}
	if samples != 1 {
		t.Fatalf("重复注册产生了 %d 条序列（应为 1）:\n%s", samples, out)
	}
}

// TestGaugeFuncPanicIsContained 采集函数 panic 不能拖垮整个抓取。
func TestGaugeFuncPanicIsContained(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	RegisterGaugeFunc(NameTaskInflight, func() float64 { panic("boom") })
	Set(NameStorageBytes, 9)
	out := Render()
	if !strings.Contains(out, "storage_bytes 9") {
		t.Fatalf("一个采集器 panic 让其它指标消失:\n%s", out)
	}
	if !strings.Contains(out, NameTaskInflight) {
		t.Fatalf("panic 的采集器仍应输出（NaN）而不是整条消失:\n%s", out)
	}
}

// TestUnknownMetricHasNoDuplicateSeries 未登记的指标名也不能输出重复序列。
//
// 反向保证：Render 的 default 分支曾把 counter 与 gauge 各写一遍，
// 同名同标签的两行会让 Prometheus 报 duplicate metrics。
func TestUnknownMetricHasNoDuplicateSeries(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Inc("custom_total", "k", "v")
	Set("custom_total", 3, "k", "v")
	out := Render()
	if strings.Count(out, `custom_total{k="v"}`) != 1 {
		t.Fatalf("未登记指标输出了重复序列:\n%s", out)
	}
}

// TestAllRegisteredMetricsHaveMeta 契约：每个导出的指标名常量都必须在 defs 里登记。
//
// 漏登记不会报错，只会让输出缺少 HELP/TYPE —— 抓取端仍能解析，但看板上没有描述，
// 半年后没人说得清这个指标是干什么的。用测试把它钉死。
func TestAllRegisteredMetricsHaveMeta(t *testing.T) {
	for _, name := range []string{
		NameGatewayRequests, NameGatewayLatency, NameTaskInflight, NameTaskSettleFailures,
		NameTaskStatusUnmatched,
		NameChannelFailures, NameUsageAnomaly, NameTaskPurgedTotal, NameTaskRetentionReport,
		NameStorageBytes, NameStorageDiskFreeRate,
	} {
		if _, ok := defs[name]; !ok {
			t.Fatalf("指标 %q 未在 defs 登记", name)
		}
	}
}
