package model

import "fmt"

// 本文件是 AI 编排运行参数的**唯一数值口径**：默认值、合法区间、生效优先级。
//
// 为什么放在 model 包：这三个值要同时被
//   - service/agent（装配上下文、预算闸、设置校验）
//   - service/modelcfg（模型级覆盖的保存校验）
//   - controller（把区间下发给前端渲染 min/max）
//
// 读取。任何一处自己写字面量，就会出现"前端允许 2M、后端拒绝 2M"这种
// 只能靠用户点保存才发现的错位。
const (
	// DefaultAgentMaxSteps 是一轮的最大步数。
	//
	// 16 而不是 12：引导链（读目录→判层→查字段→写 spec→预演→探测→提交）
	// 实测 13–15 步，12 会让正常链路在最后一步被砍（运营看到"它做一半就停了"）。
	// 已有设置行以库内值为准（当前 20），这里只决定新建/缺行时的默认。
	DefaultAgentMaxSteps = 16
	// DefaultAgentMaxTokensPerRun 是一轮的 token 成本闸默认值。
	//
	// 定得足够大是刻意的：它衡量的是"这一轮最多烧多少 token"，
	// 而每一步都要重付整份 prompt（system + 21 个工具声明 + 历史），
	// 固定开销就有 7–9k。旧默认 32000 的实际效果是"第 3–4 步就被掐断"，
	// 运营看到的是"AI 做一半就停了"，却没有任何地方说是预算。
	DefaultAgentMaxTokensPerRun = 2_000_000
	// DefaultAgentContextWindow 是输入上下文预算默认值（模型未声明时使用）。
	DefaultAgentContextWindow = 2_000_000
	// DefaultAgentMaxOutputTokens 是单次请求的输出上限默认值。
	DefaultAgentMaxOutputTokens = 8_192

	// MinAgentSteps / MaxAgentSteps 是步数区间。
	MinAgentSteps = 1
	MaxAgentSteps = 40

	// MinAgentTokensPerRun / MaxAgentTokensPerRun 是成本闸区间（0 = 不限）。
	MinAgentTokensPerRun = 1_000
	MaxAgentTokensPerRun = 20_000_000

	// MinAgentContextWindow / MaxAgentContextWindow 是上下文预算区间（0 = 用默认值）。
	//
	// 下限 32768 不是拍脑袋：system 固定文案 ≈4KB、21 个工具声明 ≈24KB
	// （合计 ≈7–9k token），再扣掉默认输出 8192，输入只剩 24576 ——
	// 刚好等于 AgentInputReserve。写 32000 会低于这条下界本身，
	// 于是"合法的下限"在保存时被自己的跨字段规则拒掉（这条被单测抓出来了）。
	MinAgentContextWindow = 32_768
	MaxAgentContextWindow = 10_000_000

	// MinAgentOutputTokens / MaxAgentOutputTokens 是输出上限区间。
	//
	// 上限 32768 与 relay 的 maxCompletionCap 对齐：两者一旦分叉，
	// 预扣估算按一个数、实际请求按另一个数，账单就再也对不上。
	MinAgentOutputTokens = 256
	MaxAgentOutputTokens = 32_768

	// AgentInputReserve 是窗口里必须留给"输入"的最小余量。
	//
	// 校验 context_window - max_output_tokens >= AgentInputReserve：
	// 把窗口全给输出，剩下给历史的就只有几百 token，装配永远只能放当前这句。
	AgentInputReserve = 24_000
)

// ResolveAgentWindow 按「模型覆盖 > 全局设置 > 内置默认」求出生效的窗口与输出上限。
//
// 这是唯一实现：装配、预算校验、界面展示都必须调它，不允许三处各写一遍 if。
func ResolveAgentWindow(globalCtx, globalOut, modelCtx, modelOut int) (ctx, out int) {
	ctx = globalCtx
	if ctx <= 0 {
		ctx = DefaultAgentContextWindow
	}
	if modelCtx > 0 && modelCtx < ctx {
		// 模型声明的窗口是**硬事实**（上游会 400），只能更小不能更大：
		// 全局调大是为了"允许长上下文"，不是"假装小模型也能装下"。
		ctx = modelCtx
	}
	out = globalOut
	if out <= 0 {
		out = DefaultAgentMaxOutputTokens
	}
	if modelOut > 0 && modelOut < out {
		out = modelOut
	}
	return ctx, out
}

// AgentLimitProblem 校验「上下文窗口 / 输出上限」这一对值，返回第一个不合法项。
//
// 返回 (field, reason)：field 为空表示合法。之所以返回字段名而不是 error，
// 是因为两个调用方（modelcfg 的字段级校验、agent 的设置校验）各有自己的错误类型，
// 但**规则只该有一份** —— 否则"模型页允许、设置页拒绝"这种错位会一直存在。
// 0 一律表示"未声明/继承"，是合法值。
func AgentLimitProblem(window, output int) (field, reason string) {
	if window != 0 && (window < MinAgentContextWindow || window > MaxAgentContextWindow) {
		return "context_window", fmt.Sprintf(
			"上下文窗口只能是 0（继承全局）或 %d–%d（当前 %d）",
			MinAgentContextWindow, MaxAgentContextWindow, window)
	}
	if output != 0 && (output < MinAgentOutputTokens || output > MaxAgentOutputTokens) {
		return "max_output_tokens", fmt.Sprintf(
			"单次输出上限只能是 0（继承全局）或 %d–%d（当前 %d）",
			MinAgentOutputTokens, MaxAgentOutputTokens, output)
	}
	if window > 0 && output > 0 && window-output < AgentInputReserve {
		return "context_window", fmt.Sprintf(
			"窗口 %d 减去输出上限 %d 只剩 %d，至少要留 %d 给输入（固定提示词 + 工具声明）",
			window, output, window-output, AgentInputReserve)
	}
	return "", ""
}
