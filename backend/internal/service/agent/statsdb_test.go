package agent

import (
	"context"
	"testing"

	"lingdong-api/internal/model"
)

// TestStatsHandlesFractionalAverageLatency 守的是一个**只在有真实数据时才炸**的错。
//
// PostgreSQL 的 avg() 返回 numeric，耗时平均值一旦不是整数（100ms 与 201ms → 150.5），
// GORM 扫进 int64 就会报类型转换失败，而 GetAdminAgentStats 把它翻成 500。
// 空库时 avg 是 0（能过），所以「刚做完、还没跑过一轮」的验证、桩件 e2e、
// 以及只测空表的用例**全都会绿**，直到线上跑出第一条奇数耗时才 500。
//
// 这条用例刻意构造 150.5 这个非整数均值。
func TestStatsHandlesFractionalAverageLatency(t *testing.T) {
	gdb := withAgentDB(t)
	ctx := context.Background()

	for _, lat := range []int{100, 201} {
		run := model.AgentRun{
			SessionID: 1, MessageID: 1, ModelID: "relay-chat", Status: "ok",
			Steps: 1, TokensIn: 10, TokensOut: 5, QuotaEst: 7, LatencyMS: lat,
		}
		if err := gdb.Create(&run).Error; err != nil {
			t.Fatalf("插入运行记录失败: %v", err)
		}
	}

	st, err := Stats(ctx, 7)
	if err != nil {
		// 曾经的失败长相：converting driver.Value type ... (numeric) to int64
		t.Fatalf("Stats 在非整数平均耗时时失败: %v", err)
	}
	if st.Runs != 2 {
		t.Fatalf("runs 应为 2，实际 %d", st.Runs)
	}
	// 150.5 四舍五入到 151：展示层不该凭空少 1ms（截断到 150 也算"能跑"，但会持续偏差）。
	if st.AvgLatency != 151 {
		t.Fatalf("平均耗时应为 151（150.5 四舍五入），实际 %d", st.AvgLatency)
	}
	if st.EstAmount != 14 || st.TokensIn != 20 || st.TokensOut != 10 {
		t.Fatalf("聚合值不对: %+v", st)
	}
}
