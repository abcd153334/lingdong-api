package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件覆盖「执行过程落库」的三条硬约束：
// 段数上限、单段上限、末段正文不入库。它们是"刷新后过程还在"与"不把库写爆"的分界线。

func TestClipTraceLimitsItemCount(t *testing.T) {
	items := make([]traceItem, 0, 200)
	for i := 0; i < 200; i++ {
		items = append(items, traceItem{K: traceTool, N: "schema_get", S: i + 1})
	}
	got := clipTrace(items)
	if len(got) > traceMaxItems {
		t.Fatalf("段数应压到 %d 以内，实际 %d", traceMaxItems, len(got))
	}
	// 头尾都要留：开头是"从哪开始查的"，结尾是"最后做了什么"，
	// 只留开头会让最容易出问题的那几步（最后几个工具）消失。
	if got[0].S != 1 {
		t.Fatalf("首段应保留第 1 步，实际 %+v", got[0])
	}
	if got[len(got)-1].S != 200 {
		t.Fatalf("末段应保留最后一步，实际 %+v", got[len(got)-1])
	}
	// 被折叠掉的段数要明示，否则前端会以为"这轮只跑了 51 次工具"。
	found := false
	for _, it := range got {
		if it.K == traceElided {
			found = true
			if it.S != 200-traceHeadKeep-traceTailKeep {
				t.Fatalf("折叠段数不对：%d", it.S)
			}
		}
	}
	if !found {
		t.Fatal("超限时必须留一条 elided 说明，而不是静默丢段")
	}
}

func TestClipTraceClipsSingleSegment(t *testing.T) {
	long := strings.Repeat("想", 5000)
	got := clipTrace([]traceItem{{K: traceReason, T: long}})
	if len(got) != 1 {
		t.Fatalf("不应改变段数：%d", len(got))
	}
	if r := []rune(got[0].T); len(r) != traceReasonMax+1 {
		t.Fatalf("单段应截到 %d 字符（含省略号），实际 %d", traceReasonMax+1, len(r))
	}
}

func TestClipTraceKeepsWithinTotalBudget(t *testing.T) {
	items := make([]traceItem, 0, 12)
	for i := 0; i < 12; i++ {
		items = append(items, traceItem{K: traceText, T: strings.Repeat("段", 1000)})
	}
	got := clipTrace(items)
	if total := traceRunes(got); total > traceTotalMax {
		t.Fatalf("文本总量应 ≤ %d，实际 %d", traceTotalMax, total)
	}
	if len(got) < 3 {
		t.Fatalf("不该把段丢到只剩 %d 段（开头与结尾最有用）", len(got))
	}
}

func TestBuildPartsDropsTrailingAnswer(t *testing.T) {
	env := &toolEnv{}
	env.appendText("我先查一下平台支持的端点。")
	env.appendTool(traceItem{K: traceTool, N: "schema_get"})
	env.appendText("平台支持 chat/completions。")

	parts := buildParts(env, true)
	if parts == nil {
		t.Fatal("有内容时必须落 parts")
	}
	raw := string(parts)
	// 末段正文 = assistant.content，留在轨迹里会让前端把同一段话渲染两遍。
	if strings.Contains(raw, "平台支持 chat/completions") {
		t.Fatalf("末段正文不应入轨迹：%s", raw)
	}
	if !strings.Contains(raw, "我先查一下平台支持的端点") {
		t.Fatalf("中间叙述应保留：%s", raw)
	}
	var parsed struct {
		Items []traceItem `json:"items"`
		Tools []string    `json:"tools"`
	}
	if err := json.Unmarshal(parts, &parsed); err != nil {
		t.Fatalf("parts 不是合法 JSON：%v", err)
	}
	if len(parsed.Items) != 2 || parsed.Items[0].K != traceText || parsed.Items[1].K != traceTool {
		t.Fatalf("段的顺序/类型不对：%+v", parsed.Items)
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0] != "schema_get" {
		t.Fatalf("tools 平铺数组要保留（老前端与事后检索靠它）：%+v", parsed.Tools)
	}
}

func TestBuildPartsNilWhenEmpty(t *testing.T) {
	if parts := buildParts(&toolEnv{}, true); parts != nil {
		t.Fatalf("没有过程时不应写 parts：%s", parts)
	}
	if parts := buildParts(nil, true); parts != nil {
		t.Fatal("nil env 不应 panic / 不应产出 parts")
	}
}

func TestAppendStreamMergesConsecutiveDeltas(t *testing.T) {
	env := &toolEnv{}
	env.appendReason("先")
	env.appendReason("想一下")
	env.appendText("好的")
	// 连续同类型合并：否则 3000 字的回答会变成 3000 段，直接顶爆段数上限。
	if len(env.Trace) != 2 {
		t.Fatalf("连续增量应合并成 2 段，实际 %d：%+v", len(env.Trace), env.Trace)
	}
	if env.Trace[0].T != "先想一下" || env.Trace[1].T != "好的" {
		t.Fatalf("合并结果不对：%+v", env.Trace)
	}
}
