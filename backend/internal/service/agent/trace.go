package agent

import (
	"encoding/json"

	"lingdong-api/internal/model"
)

// 本文件是「一轮的执行过程」的落库形态（写进 agent_messages.parts.items）。
//
// 为什么必须落库：前端的"过程可视化"如果只靠 SSE 事件，刷新一次就只剩结论；
// 而运营复盘"它当时看了什么、调了什么、为什么这么改"全靠这段轨迹。
//
// 四条约束：
//  1. **有序**：段按发生顺序追加，前端据此还原"说一句 → 调工具 → 再说"。
//     拆成 tools[] / proposals[] 两个平铺数组就再也还原不出顺序了。
//  2. **有上限**：单段与总量都截断（见下方常量）。一次接入能刷出几十步，
//     不设上限就是几十 KB 的 jsonb，读列表时会连带把正文一起拖慢。
//  3. **末段正文不入库**：它等于 assistant.content，写进去会让正文渲染两遍。
//  4. **只存摘要**：工具参数 200 字符、结果 300 字符（与 SSE 一致的常量），
//     完整结果本来就在上游与日志里，这里要的是"能看懂发生了什么"。

const (
	// traceMaxItems 是一轮轨迹的最大段数（超出时保留头尾，中间折成一条 elided）。
	traceMaxItems = 60
	// traceHeadKeep / traceTailKeep 是超限时保留的头尾段数。
	traceHeadKeep = 30
	traceTailKeep = 20
	// traceTextMax / traceReasonMax 是单段正文/思维链的**字符**上限。
	traceTextMax   = 2000
	traceReasonMax = 2000
	// traceTotalMax 是所有文本段的字符总量上限。
	traceTotalMax = 8000
)

// 段类型。前端按 k 分派渲染，新增类型必须同步前端（未知类型会被忽略而不是报错）。
const (
	traceReason   = "reason"
	traceText     = "text"
	traceTool     = "tool"
	traceProposal = "proposal"
	traceElided   = "elided"
	// traceError 是一轮失败/截断的原因段。它是错误的**唯一落库形态**：
	// 前端把它还原成告警条（含重试/去设置动作），刷新后依然可见；
	// 不新增表列、不走第二条展示管线 —— 实时轮与落库轮渲染同构。
	traceError = "error"
	// traceAsk 是一次"向运营提问"。它是提问的**唯一落库形态**：
	// 卡片、选项、最终有没有答、答了什么，全在这一段里；
	// 实时轮与落库轮共用一条渲染路径（与 error 段同构）。
	traceAsk = "ask"
)

// traceItem 是一段过程。
//
// 字段名刻意短（这是逐条落库的 JSON，一次几十段）：k 类型 / t 文本 / n 工具名 /
// a 参数摘要 / b 结果摘要 / ok 成败 / s 步号 / ms 耗时 / id 提案号。
type traceItem struct {
	K  string `json:"k"`
	T  string `json:"t,omitempty"`
	N  string `json:"n,omitempty"`
	A  string `json:"a,omitempty"`
	B  string `json:"b,omitempty"`
	OK *bool  `json:"ok,omitempty"`
	S  int    `json:"s,omitempty"`
	MS int64  `json:"ms,omitempty"`
	ID uint64 `json:"id,omitempty"`
	// C / R 仅 traceError 段使用：错误码与"是否可重试"。
	// R 必须落库：否则网络类失败刷新后重试按钮消失（实时轮有、落库轮没有 = 两套语义）。
	C string `json:"c,omitempty"`
	R bool   `json:"r,omitempty"`

	// 以下仅 traceAsk 段使用：问题 / 为什么问 / 选项 / 是否多选 / 是否允许补充文字 /
	// 终态 / 答了什么 / agent_asks 行号。
	//
	// ST 必须**结算回写**（pending → answered/timeout/expired）：否则刷新后永远显示
	// "待回答"，而那一轮的 run 早就结束了 —— 一个点不动的死卡片比没有卡片更糟。
	Q   string      `json:"q,omitempty"`
	W   string      `json:"w,omitempty"`
	O   []AskOption `json:"o,omitempty"`
	MU  bool        `json:"mu,omitempty"`
	FT  bool        `json:"ft,omitempty"`
	ST  string      `json:"st,omitempty"`
	ANS string      `json:"ans,omitempty"`
	AID uint64      `json:"aid,omitempty"`
}

// appendText / appendReason 把流式增量并入轨迹。
//
// 连续同类型的增量合并成一段：否则一个 3000 字的回答会变成 3000 条 traceItem，
// 既写不进 60 段上限，也让前端渲染出 3000 个 <p>。
// traceItems 返回本轮轨迹（只读）。
//
// finalize 生成规则摘要时要用它：摘要是"只复述发生过的事"，
// 所以输入必须是**这一轮真实的过程**，不能从 parts 里再反解（那是截断后的展示投影）。
func (e *toolEnv) traceItems() []traceItem {
	if e == nil {
		return nil
	}
	return e.Trace
}

func (e *toolEnv) appendText(s string)   { e.appendStream(traceText, s) }
func (e *toolEnv) appendReason(s string) { e.appendStream(traceReason, s) }

func (e *toolEnv) appendStream(kind, s string) {
	if s == "" {
		return
	}
	if n := len(e.Trace); n > 0 && e.Trace[n-1].K == kind {
		e.Trace[n-1].T += s
		return
	}
	e.Trace = append(e.Trace, traceItem{K: kind, T: s})
}

// appendTool 记一次工具调用（调用与结果在同一条上，不拆两条：拆了前端要再配对一次）。
//
// ToolsUsed 也在这里累加：它就是 items 里 tool 段的名字序列，两处各写一次
// 迟早会出现"轨迹里有、tools 里没有"的分叉。
func (e *toolEnv) appendTool(it traceItem) {
	e.Trace = append(e.Trace, it)
	if it.N != "" {
		e.ToolsUsed = append(e.ToolsUsed, it.N)
	}
}

// appendProposal 记一次提案生成（位置感：它在哪一步之后出现的）。
func (e *toolEnv) appendProposal(id uint64, summary string) {
	e.Trace = append(e.Trace, traceItem{K: traceProposal, ID: id, T: clipRunes(summary, 120)})
}

// appendAsk 记一次提问，返回它的下标（结算时要按下标回写终态）。
//
// 下标而不是 id：同一条 ask 段在 trace 里唯一，且回写发生在同一 goroutine 内，
// 不存在"找不到"的可能；用 id 反查反而要多一次线性扫描。
func (e *toolEnv) appendAsk(it traceItem) int {
	e.Trace = append(e.Trace, it)
	return len(e.Trace) - 1
}

// resolveAsk 把提问段结算成终态（下标越界是空操作：轨迹已被裁剪）。
func (e *toolEnv) resolveAsk(idx int, status, brief string) {
	if e == nil || idx < 0 || idx >= len(e.Trace) {
		return
	}
	e.Trace[idx].ST = status
	e.Trace[idx].ANS = clipRunes(brief, 200)
}

// partsDoc 是 parts 的**完整形状**（唯一编解码结构）。
//
// 刻意用它取代 map[string]any：map 写法下"谁负责写哪些键"没有约束，
// 两个写者各写一部分，删键与覆盖都发生过（tools/proposals 就是这么丢的）。
type partsDoc struct {
	Items []traceItem `json:"items,omitempty"`
	// Tools / Proposals 是"事后检索"键（老前端也认），与 items 同时维护。
	Tools     []string `json:"tools,omitempty"`
	Proposals []uint64 `json:"proposals,omitempty"`
}

// buildParts 生成落库的 parts。
//
// dropTrailingText 必须为 true：最后一段正文就是 assistant.content，
// 留在轨迹里会让前端把同一段话渲染两遍（刷新后尤其明显）。
func buildParts(env *toolEnv, dropTrailingText bool) model.JSON {
	if env == nil {
		return nil
	}
	items := make([]traceItem, 0, len(env.Trace)+1)
	items = append(items, env.Trace...)
	if dropTrailingText {
		for len(items) > 0 && items[len(items)-1].K == traceText {
			items = items[:len(items)-1]
		}
	}
	items = clipTrace(items)
	if len(items) == 0 && len(env.AllProposed) == 0 && len(env.ToolsUsed) == 0 {
		return nil
	}
	// 三个键一起由**同一个结构体**编解码：过去 buildParts 用 map 写三个键、
	// appendErrorPart 只认 items 再整体重写，于是"有错误的一轮"会把 tools/proposals
	// 静默抹掉（前端恰好有这两个键的兜底，所以肉眼看不出来）。
	doc := partsDoc{Items: items, Tools: env.ToolsUsed, Proposals: env.AllProposed}
	buf, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	return model.JSON(buf)
}

// clipTrace 依次施加三条上限：单段 → 段数 → 文本总量。
func clipTrace(items []traceItem) []traceItem {
	for i := range items {
		switch items[i].K {
		case traceText:
			items[i].T = clipRunes(items[i].T, traceTextMax)
		case traceReason:
			items[i].T = clipRunes(items[i].T, traceReasonMax)
		}
	}
	// 段数超限：保留开头（需求与最初的判断）与结尾（结论与提案），中间折成一条。
	if len(items) > traceMaxItems {
		head := items[:traceHeadKeep]
		tail := items[len(items)-traceTailKeep:]
		merged := make([]traceItem, 0, traceHeadKeep+traceTailKeep+1)
		merged = append(merged, head...)
		merged = append(merged, traceItem{K: traceElided, S: len(items) - traceHeadKeep - traceTailKeep})
		merged = append(merged, tail...)
		items = merged
	}
	// 文本总量超限：从**中间**丢（开头是需求、结尾是结论，都更有价值）。
	for traceRunes(items) > traceTotalMax && len(items) > 3 {
		mid := len(items) / 2
		items = append(items[:mid], items[mid+1:]...)
	}
	return items
}

func traceRunes(items []traceItem) int {
	total := 0
	for _, it := range items {
		total += len([]rune(it.T)) + len([]rune(it.A)) + len([]rune(it.B))
	}
	return total
}

// clipRunes 按**字符**截断（clipText 按字节，中文会被砍得过短）。
func clipRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// appendErrorPart 把失败/截断原因追加为 parts 的最后一个段。
//
// parts 为 nil（本轮没有任何过程）时新建：前置失败（无模型/预算超限）也要在
// 会话里留下"为什么没动静"的凭证，否则运营只能看到一条悬空的用户消息。
//
// 必须整份 partsDoc 收发改写：只重写 items 会丢掉 tools / proposals 两个键，
// 而那正是前端在老数据上渲染"调了哪些工具"的兜底来源。
func appendErrorPart(parts model.JSON, code, msg string, retryable bool) model.JSON {
	item := traceItem{K: traceError, T: clipRunes(msg, traceTextMax), C: code, R: retryable}
	var doc partsDoc
	if len(parts) > 0 {
		if err := json.Unmarshal(parts, &doc); err != nil {
			// 解析失败说明 parts 不是本版本写的（或已损坏）：宁可只保留错误段，
			// 也不要假装保住了原有内容。
			doc = partsDoc{}
		}
	}
	doc.Items = append(doc.Items, item)
	buf, err := json.Marshal(doc)
	if err != nil {
		// 序列化失败不该吞掉错误段：退化为最小结构（k/t/c 都是基础类型，实际到不了这里）。
		buf = []byte(`{"items":[{"k":"error","t":"internal"}]}`)
	}
	return model.JSON(buf)
}
