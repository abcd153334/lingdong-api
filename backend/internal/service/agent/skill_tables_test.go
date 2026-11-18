package agent

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"lingdong-api/internal/service/docdocs"
)

// 本文件是「AI 层」的门禁：知识包只写判据、字典只有一个来源、分块读取不丢字节。
//
// 为什么这些必须是测试而不是约定：约定只写在注释里，下一次改文档的人不会读注释。
// 已经发生过一次 —— 40/60 抄了一份字段表，抄完就漂（`member_notes ≤80 字` 代码里
// 根本没这个校验，而真正的 `field_notes ≤200 字` 反而没写）。

// fieldListRe 匹配"成片字段表"：连续 6 行以上 `- name: 说明` 形态。
//
// 为什么阈值是 6 而不是 2：知识包需要偶尔列举 ≤5 个键（例如"三个必填字段"），
// 那是判据的一部分；真正的问题是"把字典抄一份"。6 行以上必然是在抄。
// 命中了不要加白名单 —— 改写成散文（"哪些字段必须给"），那正是判据化的目标。
var fieldListRe = regexp.MustCompile("(?m)(?:^[-*]\\s+`[a-z][a-z0-9_.]*`[：:].*\\n){6,}")

// fieldTableRe 匹配带"字段/类型"列的 markdown 表头。
var fieldTableRe = regexp.MustCompile(`(?m)^\|\s*(字段|类型)\s*\|`)

// TestSkillNoFieldTables 守"字段字典只有一个来源（schema_get）"。
func TestSkillNoFieldTables(t *testing.T) {
	for _, d := range skillDocs {
		body := readWholeSkill(t, d.ID)
		if m := fieldListRe.FindString(body); m != "" {
			t.Errorf("%s 有成片字段清单（应改成 schema_get 指路；命中片段：%q）", d.ID, clipText(m, 120))
		}
		if m := fieldTableRe.FindString(body); m != "" {
			t.Errorf("%s 有字段表表头（字段/类型列），字段表只能在 schema_get 里", d.ID)
		}
	}
}

// TestSkillIndexHasHeadroom 守"新增一篇不会静默挤掉后面的条目"。
//
// SkillIndex 超限时是 break：该条及之后**全部消失，且没有任何提示**。
// 早前 1600 的预算在 11 篇时只剩 71 字节余量 —— 加一篇就静默少几行。
func TestSkillIndexHasHeadroom(t *testing.T) {
	idx := SkillIndex()
	if n := len(idx); n > skillIndexMaxBytes {
		t.Fatalf("索引超限：%d > %d", n, skillIndexMaxBytes)
	}
	// 余量至少能放下一行普通条目（实测单行 120–175 字节）。
	if n := len(idx) + 250; n > skillIndexMaxBytes {
		t.Fatalf("索引余量不足（%d + 250 > %d）：再加一篇就会静默少条目", len(idx), skillIndexMaxBytes)
	}
	for _, d := range skillDocs {
		if !strings.Contains(idx, d.ID) {
			t.Fatalf("%s 不在索引里：模型看不到就不会读它", d.ID)
		}
	}
}

// TestSkillPiecesFitSingleRead 守"每篇一次能读完"。
//
// 超过 8KB 的正文会被 ReadSkill 分段；而工具结果还有 8KB 的二次裁剪，
// 两者叠加就容易让人以为"读完了"。真正的要求是：单篇一次读完。
func TestSkillPiecesFitSingleRead(t *testing.T) {
	for _, d := range skillDocs {
		piece, next, more, ok := ReadSkill(d.ID, 0, 8<<10)
		if !ok {
			t.Fatalf("读不出 %s", d.ID)
		}
		if more {
			t.Errorf("%s 单次读不完（%d 字节，next_offset=%d）：拆成两篇或删减示例", d.ID, SkillLen(d.ID), next)
		}
		if len(piece) == 0 {
			t.Errorf("%s 正文为空", d.ID)
		}
	}
}

// TestSkillReadClampIsReported 守"夹取要明说，且续读不跳段"。
func TestSkillReadClampIsReported(t *testing.T) {
	res, err := toolSkillRead(nil, json.RawMessage(`{"id":"75-recovery","limit":65536}`))
	if err != nil {
		t.Fatalf("skill_read 失败: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok || m["ok"] != true {
		t.Fatalf("应返回成功回执，实际 %#v", res)
	}
	if m["limit_clamped"] != true {
		t.Fatal("超上限被夹取时必须下发 limit_clamped（否则模型以为读到了 64KB）")
	}
	// offset 是 clipResult 的兜底依据：缺了它，正文被裁时 next_offset 会停在
	// 截断前的值，而那段从没被读过。
	if _, has := m["offset"]; !has {
		t.Fatal("回执必须回显 offset（否则二次裁剪会静默跳段）")
	}

	// 不变式：next_offset 要么等于"起点 + 实际返回长度"，要么被明确标记要重读。
	off, _ := m["offset"].(int)
	text, _ := m["text"].(string)
	next, _ := m["next_offset"].(int)
	if next != off+len(text) {
		if m["retry_smaller"] != true {
			t.Fatalf("next_offset 与正文长度不一致（会跳段）：offset=%d len=%d next=%d", off, len(text), next)
		}
	}
}

// TestDocsTextSectionMatchesProblemCatalog 守"体检码只有一处来源"。
//
// 旧版本这里比的是 `apidoc.DocCaps` 的字数上限；v7 之后字段形状由工具 schema 定义，
// 会漂的东西变成了「发布闸门到底拦哪些码」——AI 看到一份过期的码表，
// 就会照着改一个不存在的问题，而真正的阻断项一次都没被提及。
func TestDocsTextSectionMatchesProblemCatalog(t *testing.T) {
	res, err := toolSchemaGet(nil, json.RawMessage(`{"section":"docs_text"}`))
	if err != nil {
		t.Fatalf("schema_get(docs_text) 失败: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok || m["ok"] != true {
		t.Fatalf("docs_text 段应返回成功，实际 %#v", res)
	}
	dict, ok := m["docs_text"].(map[string]any)
	if !ok {
		t.Fatalf("回执缺 docs_text: %#v", m)
	}
	codes, ok := dict["blocking_codes"].([]docdocs.ProblemDef)
	if !ok || len(codes) == 0 {
		t.Fatalf("docs_text 必须带 blocking_codes（来自 docdocs.ProblemCatalog），实际 %#v", dict["blocking_codes"])
	}
	// 与目录逐条比对：条数与 code 集合都必须一致（多一条是幻觉，少一条是漏拦）。
	want := docdocs.ProblemCatalog()
	if len(codes) != len(want) {
		t.Fatalf("blocking_codes 应有 %d 条，实际 %d 条", len(want), len(codes))
	}
	for i := range want {
		if codes[i].Code != want[i].Code || codes[i].Fix == "" {
			t.Fatalf("第 %d 条不一致：得到 %+v，应为 %+v", i, codes[i], want[i])
		}
	}
	// 每条都要有「怎么改」：只给 code 的话模型只能猜。
	for _, c := range codes {
		if c.Meaning == "" || c.Fix == "" {
			t.Fatalf("%s 缺 meaning/fix（AI 只能看到一个干码）", c.Code)
		}
	}
	if len(dict["forbidden_words"].([]string)) == 0 {
		t.Fatal("docs_text 必须带 forbidden_words")
	}
	if flow, _ := dict["flow"].([]string); len(flow) < 4 {
		t.Fatalf("docs_text 必须给流程（否则模型会直接整篇重写），实际 %v", dict["flow"])
	}
}

// TestSpecRulesAlwaysVisible 守"跨字段约束不因问哪一层而消失"。
func TestSpecRulesAlwaysVisible(t *testing.T) {
	for _, part := range specParts {
		dict, ok := SpecDictionary(part)
		if !ok {
			t.Fatalf("part=%s 应合法", part)
		}
		rules, has := dict["rules"].([]string)
		if !has {
			t.Fatalf("part=%s 拿不到 rules：模型会在看不到跨字段约束的情况下写规格", part)
		}
		if len(rules) != len(specRules) {
			t.Fatalf("part=%s 的 rules 少了：%d != %d", part, len(rules), len(specRules))
		}
	}
}

// TestPlanSchemaEntitiesAreDescribed 守"data 的五个分支能被对上"。
func TestPlanSchemaEntitiesAreDescribed(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(planOperationsSchema(), &schema); err != nil {
		t.Fatalf("operations schema 解析失败: %v", err)
	}
	props, _ := schema["properties"].(map[string]any)
	ent, _ := props["entity"].(map[string]any)
	if s, _ := ent["description"].(string); len(s) < 20 {
		t.Fatalf("entity 必须有说明（否则只能靠英文名猜 data 的形状），实际 %q", s)
	}
	data, _ := props["data"].(map[string]any)
	branches, _ := data["oneOf"].([]any)
	if len(branches) != 5 {
		t.Fatalf("data 应有 5 个分支，实际 %d", len(branches))
	}
	for i, b := range branches {
		bm, _ := b.(map[string]any)
		if s, _ := bm["title"].(string); s == "" {
			t.Fatalf("第 %d 个分支缺 title：模型无法把它与 entity 对上", i)
		}
	}
}
