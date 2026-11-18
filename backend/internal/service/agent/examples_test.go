package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件守的是 **AI 层资产**：给模型看的示例与知识包必须"真的能用"。
//
// 为什么要单独一组用例：示例曾经有三份副本（单测的 happyPlan、e2e 的 indexttsSpec、
// 知识包里的 markdown 代码块），彼此已经漂移；而知识包里那份"正例"带
// `"spec":{...}` 省略号与 "summary"，模型照抄必然被 validate_draft 拒。
// 这些都不会被任何编译错误或旧门禁发现，只能靠"拿示例跑一遍真实管线"。

// exampleArgBudget 是单次工具参数的软上限（与提示词里的「参数预算」同源）。
//
// 只约束字节与条数，**不约束嵌套深度**：一份合法 spec 天然要
// data(1).spec(2).endpoints(3).<端点>(4).body(5).copy(6).<字段>(7) 这么深，
// 写"≤8 层"会让模型误以为自己的合法规格超标。
const exampleArgBudget = 4 << 10

// TestExamplesPassValidateDraft 是本组最重要的一条：示例必须真的能过。
//
// 走的是模型完全一样的入口（ExecTool → validate_draft → DecodePlan →
// ValidateStructure → trialApply），不是单独调某个 Validate。
func TestExamplesPassValidateDraft(t *testing.T) {
	withAgentDB(t)

	names := ExampleNames()
	if len(names) == 0 {
		t.Fatal("没有任何示例资产：知识包里的 <!--example:…--> 会原样露给模型")
	}
	for _, name := range names {
		raw, err := Example(name)
		if err != nil {
			t.Fatalf("读取示例 %s 失败: %v", name, err)
		}
		// 参数必须是 {"operations":[…]}：validate_draft 是 additionalProperties:false，
		// 多一个 summary 就会报参数错误（旧正例正是这么错的）。
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("示例 %s 不是合法 JSON: %v", name, err)
		}
		if _, ok := probe["operations"]; !ok {
			t.Fatalf("示例 %s 缺少 operations", name)
		}
		if len(probe) != 1 {
			t.Fatalf("示例 %s 含多余顶层键（validate_draft 只收 operations）：%v", name, keysOf(probe))
		}

		res, ok := ExecTool(nil, ToolCall{Name: "validate_draft", Arguments: string(raw)}).(map[string]any)
		if !ok {
			t.Fatalf("示例 %s 的工具结果应为对象", name)
		}
		if res["ok"] != true {
			t.Fatalf("示例 %s 没通过预演（模型照抄就会失败）：%v", name, res)
		}
	}
}

// TestExamplesInsideArgBudget 保证示例本身就在「参数预算」之内。
//
// 示例比预算还大，等于教模型"就这么写"，预算规则立刻失去说服力。
func TestExamplesInsideArgBudget(t *testing.T) {
	for _, name := range ExampleNames() {
		raw, err := Example(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		if len(raw) > exampleArgBudget {
			t.Fatalf("示例 %s 有 %d 字节，超过参数预算 %d", name, len(raw), exampleArgBudget)
		}
		// 占位符是"看起来能跑、照抄必失败"的典型来源（旧正例的 "<运营提供>"）。
		if strings.Contains(string(raw), `"<`) || strings.Contains(string(raw), "…") {
			t.Fatalf("示例 %s 含占位符，模型会原样照抄", name)
		}
		var doc struct {
			Operations []json.RawMessage `json:"operations"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("示例 %s 解析失败: %v", name, err)
		}
		if len(doc.Operations) > maxDraftOps {
			t.Fatalf("示例 %s 有 %d 个操作，超过单次上限 %d", name, len(doc.Operations), maxDraftOps)
		}
	}
}

// TestSkillRendersExamples 保证知识包里的占位符都能展开。
//
// 少一个名字，模型看到的就是一句注释而不是示例——它不会报错，只是"没学会"。
func TestSkillRendersExamples(t *testing.T) {
	known := map[string]bool{}
	for _, n := range ExampleNames() {
		known[n] = true
	}
	rendered := 0
	for _, d := range skillDocs {
		body := d.Body
		for {
			i := strings.Index(body, "<!--example:")
			if i < 0 {
				break
			}
			j := strings.Index(body[i:], "-->")
			if j < 0 {
				t.Fatalf("%s 有未闭合的示例占位符", d.ID)
			}
			name := strings.TrimSpace(body[i+len("<!--example:") : i+j])
			if !known[name] {
				t.Fatalf("%s 引用了不存在的示例 %q（可选：%v）", d.ID, name, ExampleNames())
			}
			body = body[i+j+len("-->"):]
			rendered++
		}
		// 渲染结果里不许再有占位符残留。
		out, _, _, ok := ReadSkill(d.ID, 0, 1<<20)
		if !ok {
			t.Fatalf("%s 读不出来", d.ID)
		}
		if strings.Contains(out, "<!--example:") {
			t.Fatalf("%s 渲染后仍残留占位符", d.ID)
		}
	}
	if rendered == 0 {
		t.Fatal("没有任何知识包引用示例：模型看不到可运行的正例")
	}
}

// TestSkillFactsMatchCode 守"知识包不许教已经不存在的字段"。
//
// 这一条直接来自真实故障：extract.download 已从 DSL 删除，但知识包与
// spec_support_check 的理由表仍在教它，模型照写必被拒且拒因不指向这里。
func TestSkillFactsMatchCode(t *testing.T) {
	// 已删除/已改名的事实：出现即失败。
	retired := []string{
		"extract.download",
		"/tts/submit",
		"fail-closed",
		"平台四态",
	}
	// 现役事实：这组关键词至少在一篇知识包里出现过（拆分成 30/32 后仍要覆盖）。
	// 断言"某篇必须有"会把知识包结构锁死，改成"整体必须有"。
	mustHave := []string{"extract.result_urls", "duration_unit", "request_fields", "where"}
	for _, d := range skillDocs {
		out, _, _, ok := ReadSkill(d.ID, 0, 1<<20)
		if !ok {
			t.Fatalf("%s 读不出来", d.ID)
		}
		for _, bad := range retired {
			if strings.Contains(out, bad) {
				t.Fatalf("%s 仍在教已废弃的 %q（模型照写必被拒）", d.ID, bad)
			}
		}
	}
	var all strings.Builder
	for _, d := range skillDocs {
		out, _, _, ok := ReadSkill(d.ID, 0, 1<<20)
		if !ok {
			t.Fatalf("%s 读不出来", d.ID)
		}
		all.WriteString(out)
		all.WriteString("\n")
	}
	for _, kw := range mustHave {
		if !strings.Contains(all.String(), kw) {
			t.Fatalf("所有知识包都没提到 %q：模型不知道有这个能力", kw)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestToolDescriptionsCoverFourQuestions 守"写入类工具描述必须回答四问"。
//
// 工具描述是模型在**决定调用的那一刻**必然读到的文本，比 system prompt 更靠近
// 调用点，也不受提示词预算降级影响。四问缺一条的表现是模型"会调但不会用"：
// 不知道什么时候调、写多大、写不下怎么办、失败了怎么改。
func TestToolDescriptionsCoverFourQuestions(t *testing.T) {
	// 四个写入类工具 + 它们各自的必答要点。
	want := map[string][]string{
		"schema_get":     {"何时用", "section="},
		"validate_draft": {"何时用", "一次多大", "分块", "失败怎么改", `mode="shape"`},
		"dry_run_draft":  {"何时用", "失败怎么改", "内联"},
		"propose_change": {"何时用", "一次多大", "失败怎么改"},
	}
	specs := map[string]ToolFunction{}
	for _, s := range ToolSpecs() {
		specs[s.Function.Name] = s.Function
	}
	for name, keys := range want {
		f, ok := specs[name]
		if !ok {
			t.Fatalf("工具 %s 不存在", name)
		}
		if n := len([]rune(f.Description)); n > 240 {
			t.Fatalf("%s 的描述有 %d 字，超过 240：模型只会记住前几句", name, n)
		}
		for _, k := range keys {
			if !strings.Contains(f.Description, k) {
				t.Fatalf("%s 的描述缺少 %q：%s", name, k, f.Description)
			}
		}
	}
	// propose_change 的 operations 必须与 validate_draft 同上限（否则能绕过预演提交任意大）。
	var propose, draft map[string]any
	for _, s := range ToolSpecs() {
		var m map[string]any
		if err := json.Unmarshal(s.Function.Parameters, &m); err != nil {
			continue
		}
		switch s.Function.Name {
		case "propose_change":
			propose = m
		case "validate_draft":
			draft = m
		}
	}
	opsOf := func(m map[string]any) map[string]any {
		props, _ := m["properties"].(map[string]any)
		ops, _ := props["operations"].(map[string]any)
		return ops
	}
	if opsOf(propose)["maxItems"] != opsOf(draft)["maxItems"] {
		t.Fatalf("propose_change 与 validate_draft 的 operations 上限不一致：%v vs %v",
			opsOf(propose)["maxItems"], opsOf(draft)["maxItems"])
	}
}
