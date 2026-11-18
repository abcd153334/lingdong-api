package agent

import (
	"sort"
	"strings"
)

// 本文件是"工具失败"的唯一计数与判定处。
//
// 为什么必须分开两类失败：参数写坏（JSX/括号/少字段）只说明模型这一段没写对，
// 不代表这个工具或目标不可用；把它和"目标不存在/上游拒绝"混在一个计数器里，
// 结果就是**写坏三次 JSON 就整轮中止** —— 而它明明只需要把参数改对。
//
// 线上实测：那次故障里 validate_draft 连错 3 次（每次都只差一层括号），
// 第 3 次撞上 agent_loop_guard，运营看到的是"这条路走不通"，于是换问法重来，
// 又写出一份同样大小的坏 JSON。

// argAdvice 是第 2 次参数类失败时注入给模型的指令。
//
// 为什么不改 system prompt：提示词是"每轮都在"的通用约束，而这句话只在
// "已经写坏两次"这个具体时刻才有意义；放进回执它就在模型最需要的时刻出现。
const argAdvice = "参数仍不合法。别整份重写：先 validate_draft(mode=\"shape\") 只送 1–2 个 operations 确认语法，" +
	"凑齐后再跑 mode=\"full\"；按回执的 line/column/snippet 只改那一处。"

// isArgFaultCode 判断这个错误码是不是"模型可以自己改"的参数类失败。
//
// 与 context_builder 的 isArgFaultContent 共用同一份码：那边判"要不要回灌原文"，
// 这边判"计入哪个计数器"，两个问题不同但依据必须一致。
func isArgFaultCode(code string) bool {
	switch code {
	case argCodeInvalid, argCodeMissing, argCodeTruncate:
		return true
	}
	return false
}

// resultCode 取工具回执里的 code（不是 map 或没有 code 时返回空串）。
func resultCode(result any) string {
	if m, ok := result.(map[string]any); ok {
		if c, ok := m["code"].(string); ok {
			return c
		}
	}
	return ""
}

// noteToolOutcome 是失败计数的**唯一**加减处。
//
// 三类结果的归属：
//   - 成功：两个计数器都清零（成功一次就证明这条路是通的）；
//   - 防重复（repeated_call）：**不计数**。它说明模型在重复自己，但工具本身
//     没被证明不可用；计入失败会让"重复两次 + 被拒一次"直接凑满 3 次中止；
//   - 参数类失败：只加 ArgStreak；
//   - 其余失败：只加 FailStreak。
func noteToolOutcome(env *toolEnv, name string, result any, repeated bool) {
	if env == nil {
		return
	}
	if env.FailStreak == nil {
		env.FailStreak = map[string]int{}
	}
	if env.ArgStreak == nil {
		env.ArgStreak = map[string]int{}
	}
	m, ok := result.(map[string]any)
	if !ok {
		return
	}
	if okFlag, has := m["ok"].(bool); has && okFlag {
		env.FailStreak[name] = 0
		env.ArgStreak[name] = 0
		env.Repeat[name] = 0
		return
	}
	if repeated {
		// 被防重复拦下的调用：不改任何状态，也不计入失败 —— 但它说明模型卡住了，
		// 单独计数由 guardTrip 用更高的阈值兜住（防止空转到步数耗尽）。
		if env.Repeat == nil {
			env.Repeat = map[string]int{}
		}
		env.Repeat[name]++
		return
	}
	if isArgFaultCode(resultCode(result)) {
		env.ArgStreak[name]++
		return
	}
	env.FailStreak[name]++
}

// injectArgAdvice 在"同一工具第 2 次参数类失败"时把下一步做法写进回执。
//
// 改的是 result 而不是 toolMsg：toolMsg / traceItem.B / SSE Brief 三处
// 共用同一份 json.Marshal(result)，只改一处会让"模型看到的"与"运营看到的"分叉。
func injectArgAdvice(env *toolEnv, name string, result any, repeated bool) {
	if env == nil || repeated {
		return
	}
	if !isArgFaultCode(resultCode(result)) {
		return
	}
	if env.ArgStreak[name] < 2 {
		return
	}
	if m, ok := result.(map[string]any); ok {
		m["advice"] = argAdvice
	}
}

// guardTrip 判断这一轮要不要中止；返回空 code 表示继续。
//
// 阈值刻意不同：
//   - 参数类 4 次：模型的自我修正通常在第 2–3 次奏效（回执已经给了行列与片段），
//     4 次还没改对说明它在结构上写不出来，该停下来让人接手；
//   - 业务类 3 次：目标不存在/上游拒绝这类失败换措辞也不会变，越试越贵。
func guardTrip(env *toolEnv) (string, string) {
	if env == nil {
		return "", ""
	}
	for _, name := range sortedStreakKeys(env.ArgStreak) {
		if env.ArgStreak[name] >= 4 {
			return "agent_tool_args_invalid",
				"工具参数连续写错 4 次，本轮中止；可换问法，或让它分块提交（先 mode=\"shape\"）"
		}
	}
	for _, name := range sortedStreakKeys(env.Repeat) {
		if env.Repeat[name] >= 3 {
			// 卡住与失败不同：给它一次机会换措辞，但连续三次一样的调用就该停。
			return "agent_loop_guard",
				"工具 " + name + " 重复相同调用 3 次（结果没变化），本轮中止；换问法或直接回答"
		}
	}
	for _, name := range sortedStreakKeys(env.FailStreak) {
		if env.FailStreak[name] >= 3 {
			// 点名工具：运营据此换问法或先手工确认目标，而不是笼统地"再试一次"。
			return "agent_loop_guard",
				"工具 " + name + " 连续失败 3 次，本轮中止；可换问法，或先手工确认目标是否存在"
		}
	}
	return "", ""
}

// sortedStreakKeys 让判定顺序稳定（map 遍历随机会让同一场景报出不同工具名）。
//
// 不复用 tools_docs.go 的 sortedKeys：那个收 map[string]bool，语义不同。
func sortedStreakKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// argAdviceHint 供测试断言用（避免测试里抄一份文案，改文案就红）。
func argAdviceHint() string { return strings.TrimSpace(argAdvice) }
