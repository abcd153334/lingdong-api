package agent

import (
	"context"
	"fmt"

	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 本文件是「AI 编排运行参数」的唯一规则处：区间校验、生效值解析、区间下发。
//
// 三条约束：
//  1. **只有这里能判区间**。controller 不能再自己写一遍 `if x < 1000`：
//     两处一旦分叉，症状是"页面允许保存、后端拒绝"或反过来（前者更常见），
//     而运营看到的只有一句没有数字的"参数不合法"。
//  2. **只有这里能算生效值**。装配、预算、界面展示都调 EffectiveLimits，
//     谁都不许直接读 agent_settings.context_window —— 那会绕过模型级覆盖，
//     于是给某个模型填的窗口永远不生效。
//  3. 默认值来自 model 包的常量表（与 modelcfg 的模型级校验同源）。

// Limits 是一轮实际生效的上下文与输出预算（模型覆盖后的结果）。
type Limits struct {
	// ContextWindow 是输入上下文预算（token）。
	ContextWindow int
	// MaxOutputTokens 是单次请求下发给上游的 max_tokens。
	MaxOutputTokens int
}

// LimitRanges 是管理台渲染 min/max 用的区间（唯一来源）。
//
// 为什么下发给前端而不是各写一份：前端硬编码 [1000, 200000] 与后端放宽后的
// [1000, 20000000] 同时存在时，页面会把合法值标红、而后端其实接受 ——
// 这种"看着是校验问题、其实是两份常量"的故障排查成本极高。
type LimitRanges struct {
	MaxSteps        [2]int `json:"max_steps"`
	MaxTokensPerRun [2]int `json:"max_tokens_per_run"`
	ContextWindow   [2]int `json:"context_window"`
	MaxOutputTokens [2]int `json:"max_output_tokens"`
}

// Ranges 返回全部区间（前端 min/max 的唯一来源）。
func Ranges() LimitRanges {
	return LimitRanges{
		MaxSteps:        [2]int{model.MinAgentSteps, model.MaxAgentSteps},
		MaxTokensPerRun: [2]int{model.MinAgentTokensPerRun, model.MaxAgentTokensPerRun},
		ContextWindow:   [2]int{model.MinAgentContextWindow, model.MaxAgentContextWindow},
		MaxOutputTokens: [2]int{model.MinAgentOutputTokens, model.MaxAgentOutputTokens},
	}
}

// SettingsError 是设置校验失败的结构化原因（字段 + 可直接展示的中文）。
//
// 带 Field 是给前端定位用的：一个弹窗里有 6 个字段，
// 只回一句"参数不合法"等于让人挨个试。
type SettingsError struct {
	Field  string
	Reason string
}

func (e *SettingsError) Error() string { return e.Reason }

// ValidateSettings 校验单行设置（controller 与 SaveSettings 共用）。
func ValidateSettings(s *model.AgentSetting) error {
	if s == nil {
		return &SettingsError{Field: "settings", Reason: "设置不存在"}
	}
	if s.MaxSteps < model.MinAgentSteps || s.MaxSteps > model.MaxAgentSteps {
		return &SettingsError{Field: "max_steps", Reason: fmt.Sprintf(
			"单轮步数上限只能是 %d–%d（当前 %d）", model.MinAgentSteps, model.MaxAgentSteps, s.MaxSteps)}
	}
	// 0 = 不限；非 0 才判区间。
	if s.MaxTokensPerRun != 0 &&
		(s.MaxTokensPerRun < model.MinAgentTokensPerRun || s.MaxTokensPerRun > model.MaxAgentTokensPerRun) {
		return &SettingsError{Field: "max_tokens_per_run", Reason: fmt.Sprintf(
			"单轮 token 预算只能是 0（不限）或 %d–%d（当前 %d）",
			model.MinAgentTokensPerRun, model.MaxAgentTokensPerRun, s.MaxTokensPerRun)}
	}
	// 窗口与输出上限共用 model 包的一条规则（与模型级校验同源）。
	if field, reason := model.AgentLimitProblem(s.ContextWindow, s.MaxOutputTokens); field != "" {
		return &SettingsError{Field: field, Reason: reason}
	}
	// 全局值的跨字段校验要按"生效值"算：这里两个都可能为 0（表示用内置默认），
	// 直接把 0 拿去比较会得出"窗口 0 装不下输出 8192"的假错误。
	win, out := model.ResolveAgentWindow(s.ContextWindow, s.MaxOutputTokens, 0, 0)
	if win-out < model.AgentInputReserve {
		return &SettingsError{Field: "context_window", Reason: fmt.Sprintf(
			"生效窗口 %d 减去输出上限 %d 只剩 %d，至少要留 %d 给输入（固定提示词 + 工具声明）",
			win, out, win-out, model.AgentInputReserve)}
	}
	if s.DailyAmountBudget < 0 {
		return &SettingsError{Field: "daily_amount_budget", Reason: "每日成本上限不能为负数（0 = 不限）"}
	}
	return nil
}

// EffectiveLimits 求出一轮实际生效的上下文窗口与输出上限。
//
// 优先级唯一：模型声明值（更小者胜）> agent_settings > 内置默认。
// 模型未填（0）时跟随全局；全局未填（0）时用内置默认。
func EffectiveLimits(ctx context.Context, modelID string) (Limits, error) {
	set, err := Settings(ctx)
	if err != nil {
		return Limits{}, err
	}
	var overrideCtx, overrideOut int
	if modelID != "" {
		// 只取两列：这条查询在每一轮开头都会跑，`Select("*")` 会把 capabilities /
		// endpoints / spec_prices 这些 jsonb 一起拖出来。
		var row struct {
			ContextWindow   int
			MaxOutputTokens int
		}
		err := db.DB.WithContext(ctx).Model(&model.Model{}).
			Select("context_window, max_output_tokens").
			Where("id = ?", modelID).Limit(1).Scan(&row).Error
		if err != nil {
			// 模型行读不到不是致命错（可能刚被删）：退回全局值，
			// 让本轮先跑起来 —— 真正的"模型不可用"由 ResolveChatModel 拦。
			overrideCtx, overrideOut = 0, 0
		} else {
			overrideCtx, overrideOut = row.ContextWindow, row.MaxOutputTokens
		}
	}
	win, out := model.ResolveAgentWindow(set.ContextWindow, set.MaxOutputTokens, overrideCtx, overrideOut)
	return Limits{ContextWindow: win, MaxOutputTokens: out}, nil
}
