package agent

import (
	"context"
	"testing"

	"lingdong-api/internal/model"
)

// TestValidateSettingsRanges 锁死四个旋钮的区间与 0 的语义。
//
// 为什么这条必须有：区间曾经只写在 controller 的六段 if 里，
// 放宽 token 上限时漏改其中一段 —— 表现是"页面允许、保存永远 400"，
// 而错误信息只写"参数不合法"，运营只能挨个字段试。
func TestValidateSettingsRanges(t *testing.T) {
	base := func() *model.AgentSetting {
		return &model.AgentSetting{
			MaxSteps:        model.DefaultAgentMaxSteps,
			MaxTokensPerRun: model.DefaultAgentMaxTokensPerRun,
			// 窗口与输出都留 0 = 用内置默认，此时跨字段校验必须按**生效值**算，
			// 而不是拿 0 去比（否则每次默认配置都被判"窗口装不下输出"）。
			ContextWindow:   0,
			MaxOutputTokens: 0,
		}
	}
	cases := []struct {
		name  string
		mut   func(*model.AgentSetting)
		field string
	}{
		{"默认值合法", func(s *model.AgentSetting) {}, ""},
		{"步数 0", func(s *model.AgentSetting) { s.MaxSteps = 0 }, "max_steps"},
		{"步数超上界", func(s *model.AgentSetting) { s.MaxSteps = 41 }, "max_steps"},
		{"步数下界", func(s *model.AgentSetting) { s.MaxSteps = 1 }, ""},
		{"步数上界", func(s *model.AgentSetting) { s.MaxSteps = 40 }, ""},
		// 0 = 不限，是合法的；1..999 不合法。
		{"token 预算 0 合法", func(s *model.AgentSetting) { s.MaxTokensPerRun = 0 }, ""},
		{"token 预算过小", func(s *model.AgentSetting) { s.MaxTokensPerRun = 999 }, "max_tokens_per_run"},
		{"token 预算上界", func(s *model.AgentSetting) { s.MaxTokensPerRun = model.MaxAgentTokensPerRun }, ""},
		{"token 预算超上界", func(s *model.AgentSetting) { s.MaxTokensPerRun = model.MaxAgentTokensPerRun + 1 }, "max_tokens_per_run"},
		{"窗口过小", func(s *model.AgentSetting) { s.ContextWindow = 1000 }, "context_window"},
		{"窗口下界", func(s *model.AgentSetting) { s.ContextWindow = model.MinAgentContextWindow }, ""},
		{"窗口超上界", func(s *model.AgentSetting) { s.ContextWindow = model.MaxAgentContextWindow + 1 }, "context_window"},
		{"输出过小", func(s *model.AgentSetting) { s.MaxOutputTokens = 1 }, "max_output_tokens"},
		{"输出上界", func(s *model.AgentSetting) { s.MaxOutputTokens = model.MaxAgentOutputTokens }, ""},
		{"输出超上界", func(s *model.AgentSetting) { s.MaxOutputTokens = model.MaxAgentOutputTokens + 1 }, "max_output_tokens"},
		// 窗口全给输出：历史只剩一点点，装配永远只能放当前这句。
		{"窗口装不下输出", func(s *model.AgentSetting) {
			s.ContextWindow = 32_000
			s.MaxOutputTokens = 32_768
		}, "context_window"},
		{"每日预算为负", func(s *model.AgentSetting) { s.DailyAmountBudget = -1 }, "daily_amount_budget"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := base()
			c.mut(s)
			err := ValidateSettings(s)
			if c.field == "" {
				if err != nil {
					t.Fatalf("应合法，实际报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错（字段 %s），实际通过", c.field)
			}
			se, ok := err.(*SettingsError)
			if !ok {
				t.Fatalf("错误类型必须是 *SettingsError（前端靠 Field 定位），实际 %T", err)
			}
			if se.Field != c.field {
				t.Fatalf("错误字段应为 %s，实际 %s（%s）", c.field, se.Field, se.Reason)
			}
		})
	}
}

// TestEffectiveLimitsPriority 锁死「模型声明的窗口更小者胜」。
//
// 反过来的后果很具体：全局调大到 2M 是为了允许长上下文，
// 但 8k/128k 的模型若按 2M 装配，每一轮都由上游 400 —— 而且运营会以为是助手坏了。
func TestEffectiveLimitsPriority(t *testing.T) {
	gdb := withAgentDB(t)
	ctx := context.Background()

	seed := func(id string, win, out int) {
		if err := gdb.Table("models").Create(map[string]any{
			"id": id, "display_name": id, "vendor": "v", "modality": "chat",
			"billing_mode": "tokens", "enabled": true,
			"context_window": win, "max_output_tokens": out,
		}).Error; err != nil {
			t.Fatalf("写模型 %s 失败: %v", id, err)
		}
	}
	setGlobal := func(win, out int) {
		if err := gdb.Exec(
			`UPDATE agent_settings SET context_window = ?, max_output_tokens = ? WHERE id = 1`,
			win, out).Error; err != nil {
			t.Fatalf("改全局设置失败: %v", err)
		}
	}

	seed("m-inherit", 0, 0)
	seed("m-128k", 128_000, 4_096)
	seed("m-bigger", 900_000, 0)

	// ① 全局 0（内置默认 2M）+ 模型 0 → 2M / 8192
	setGlobal(0, 0)
	got, err := EffectiveLimits(ctx, "m-inherit")
	if err != nil {
		t.Fatalf("取生效值失败: %v", err)
	}
	if got.ContextWindow != model.DefaultAgentContextWindow || got.MaxOutputTokens != model.DefaultAgentMaxOutputTokens {
		t.Fatalf("应取内置默认，实际 %+v", got)
	}

	// ② 模型声明更小的窗口 → 以模型为准（否则每轮都会被上游 400）
	got, _ = EffectiveLimits(ctx, "m-128k")
	if got.ContextWindow != 128_000 || got.MaxOutputTokens != 4_096 {
		t.Fatalf("模型级覆盖应生效，实际 %+v", got)
	}

	// ③ 模型声明比全局更大 → 全局是预算上限，不能突破
	setGlobal(500_000, 0)
	got, _ = EffectiveLimits(ctx, "m-bigger")
	if got.ContextWindow != 500_000 {
		t.Fatalf("全局上限不应被模型放大，实际 %+v", got)
	}

	// ④ 全局值在模型未声明时生效
	got, _ = EffectiveLimits(ctx, "m-inherit")
	if got.ContextWindow != 500_000 {
		t.Fatalf("模型未声明时应跟随全局，实际 %+v", got)
	}

	// ⑤ 模型不存在：退回全局，不报错（模型刚被删时本轮仍要能跑，
	//    真正的"模型不可用"由 ResolveChatModel 拦）。
	got, err = EffectiveLimits(ctx, "not-exist")
	if err != nil || got.ContextWindow != 500_000 {
		t.Fatalf("模型不存在时应退回全局，实际 %+v, err=%v", got, err)
	}
}

// TestBudgetGateIncludesOutput 锁死预算闸的三个组成项。
//
// 这条是纯算术，但每一项都对应一次真实错账：
//   - 漏掉已花 → 最后一步必然超支；
//   - 漏掉本次整份输入 → 永远不触发（每步都要重付 prompt）；
//   - 漏掉输出上限 → 输入塞满窗口后上游 400，而预扣还在按"没超"算。
func TestBudgetGateIncludesOutput(t *testing.T) {
	if overBudget(0, 999999, 999999) {
		t.Fatal("预算 0 应表示不限")
	}
	if overBudget(1000, 400, 600) {
		t.Fatal("恰好等于预算不应算超（避免边界上无谓地停一轮）")
	}
	if !overBudget(1000, 400, 601) {
		t.Fatal("超过预算必须停")
	}
	if !overBudget(1000, 900, 200) {
		t.Fatal("已花接近预算时，下一步的输入必须被计入（否则最后一步大幅超支）")
	}
}
