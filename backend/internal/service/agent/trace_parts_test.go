package agent

import (
	"encoding/json"
	"testing"
)

// TestPartsErrorKeepsToolsProposals 锁死 parts 的三个键由同一结构体维护。
//
// 曾经的两个写者：buildParts 用 map 写 items/tools/proposals，
// appendErrorPart 只反序列化 items 再整体重写 —— 于是"带错误的一轮"里
// tools 与 proposals 被静默抹掉。前端恰好有这两个键的兜底渲染，
// 所以肉眼完全看不出来，只有老数据翻历史时"调了哪些工具"会消失。
func TestPartsErrorKeepsToolsProposals(t *testing.T) {
	ok := true
	env := &toolEnv{
		Trace:       []traceItem{{K: traceTool, N: "docs_read", OK: &ok}},
		ToolsUsed:   []string{"docs_read"},
		AllProposed: []uint64{12, 13},
	}
	parts := buildParts(env, true)
	if parts == nil {
		t.Fatal("有过程时 parts 不应为空")
	}
	withErr := appendErrorPart(parts, "max_steps", "步数用尽", true)

	var doc partsDoc
	if err := json.Unmarshal(withErr, &doc); err != nil {
		t.Fatalf("parts 不是合法 JSON: %v", err)
	}
	if len(doc.Tools) != 1 || doc.Tools[0] != "docs_read" {
		t.Fatalf("追加错误段后 tools 必须保留，实际 %v", doc.Tools)
	}
	if len(doc.Proposals) != 2 {
		t.Fatalf("追加错误段后 proposals 必须保留，实际 %v", doc.Proposals)
	}
	if len(doc.Items) != 2 || doc.Items[len(doc.Items)-1].K != traceError {
		t.Fatalf("错误段必须是最后一段，实际 %+v", doc.Items)
	}
	// 空 parts 也要能建出错误段（前置失败：无模型/预算超限）。
	var lone partsDoc
	if err := json.Unmarshal(appendErrorPart(nil, "agent_disabled", "已关闭", false), &lone); err != nil {
		t.Fatalf("空 parts 追加错误段失败: %v", err)
	}
	if len(lone.Items) != 1 || lone.Items[0].C != "agent_disabled" {
		t.Fatalf("前置失败必须留下错误段，实际 %+v", lone.Items)
	}
}
