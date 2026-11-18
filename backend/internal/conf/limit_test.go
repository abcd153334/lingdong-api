package conf

import (
	"encoding/json"
	"testing"
)

// TestBalanceLimitFor 锁定余额上限的回落口径：用户级 > 0 优先，否则全局。
//
// 这条规则是上限体系的唯一真相源（下单、兑换、人工调账、对上游回显全调它）。
// 历史上每个调用点各写一份回落，且 users.balance_limit 的库默认值曾是全局默认值 ——
// 「用户级恒有值」让运营调小全局上限对新用户完全无效，而前端按全局值预览、
// 后端按用户存量值放行，两端显示的「上限」根本不是同一个数。
func TestBalanceLimitFor(t *testing.T) {
	prev := System().All()
	t.Cleanup(func() { System().Reload(prev) })

	setGlobal := func(v int64) {
		raw, _ := json.Marshal(v)
		System().Set("balance_limit", raw)
	}

	cases := []struct {
		name    string
		global  int64
		userCap int64
		want    int64
	}{
		{"用户级覆盖全局", 500_000_000, 1_000, 1_000},
		{"用户级更宽也以用户级为准", 1_000, 500_000_000, 500_000_000},
		{"用户级未设置回落全局", 50_000_000, 0, 50_000_000},
		{"用户级为负视为未设置", 50_000_000, -1, 50_000_000},
		{"两层都没配返回 0（不限制）", 0, 0, 0},
	}
	for _, c := range cases {
		setGlobal(c.global)
		if got := BalanceLimitFor(c.userCap); got != c.want {
			t.Fatalf("%s：BalanceLimitFor(%d)（全局 %d）= %d，期望 %d",
				c.name, c.userCap, c.global, got, c.want)
		}
	}
}
