package agent

import (
	"strings"
	"testing"

	"lingdong-api/internal/db"
)

// TestSystemPromptFitsAndCovers 守住两件事：
//  1. **体积**：promptMaxBytes 是硬上限，超了会被 safeCut 从中间切一半 ——
//     症状是"某条规则只剩半句"，而模型会照着半句执行，且不报错。
//  2. **判据在场**：追问判据、预演即提交、不可信正文，这三句是这个版本的核心约束，
//     被后来的编辑顺手删掉不会有任何编译错误，只能靠断言守。
func TestSystemPromptFitsAndCovers(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb

	p := BuildSystemPrompt()
	t.Logf("prompt bytes=%d / %d", len(p), promptMaxBytes)
	if len(p) > promptMaxBytes {
		t.Fatalf("system prompt 超限：%d > %d（会被截断成半句）", len(p), promptMaxBytes)
	}
	for _, want := range []string{
		"ask_user",                     // 追问工具必须在场
		"必须问",                          // 追问判据
		"不许问",                          // 反判据（只写"必须问"会被滥用）
		"schema_get(section=\"plan\")", // 字段表入口
		"spec_dsl",                     // 规格字典入口
		"validate_draft",               // 预演
		"预演通过 = 提案能提交",                 // 预演语义（否则模型会反复预演）
		"next_offset",                  // 文档推进协议
		"<<<UNTRUSTED_DOC>>>",          // 注入防护说明
		"未验证项",                         // 输出契约
		"async.mode=bridge",            // 平台表达力：异步可桥接（否则模型会把异步判成 L3）
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt 缺少关键约束：%q", want)
		}
	}
	// 半句话防御：被 safeCut 截断时末尾不会有句末标点。
	if !strings.HasSuffix(strings.TrimSpace(p), "。") {
		t.Fatalf("system prompt 疑似被截断，末尾是：%q", tailOf(p, 40))
	}
}

func tailOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// TestPromptKeepsContract 守「加规则不会把契约挤掉」。
//
// 早前的实现是拼完再 safeCut 截尾，而尾部恰好是输出契约与工作流 5/6/7 步：
// 一旦有人再往提示词里加一段，最关键的三条输出格式要求会**静默消失**，
// 模型从此不按格式答复，且没有任何报错。这条用例就是那个故障的回归网。
func TestPromptKeepsContract(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb

	p := BuildSystemPrompt()
	// 输出契约的三条 + 「JSON 只放在工具参数里」这一条硬约束。
	for _, want := range []string{
		"① 选层与理由",
		"② 提案涉及的实体清单",
		"③ 未验证项",
		"正文只写结论与理由，不要贴成品 JSON",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("输出契约被挤掉：缺少 %q", want)
		}
	}
	// 工作流里三个最容易丢、且丢了就直接做出错误动作的步骤。
	for _, want := range []string{
		"5. 预演",
		"6. 探测",
		"7. 提交",
		"参数预算",           // 体积感（否则模型会一次性写 3.6KB）
		"mode=\"shape\"", // 分块校验的入口
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("关键步骤缺失：%q", want)
		}
	}
}

// TestSystemPromptDegradesInOrder 验证超限时**先丢索引、不丢契约**。
//
// 用 assemblePrompt 直接构造超限场景，避免真把生产提示词写大。
func TestSystemPromptDegradesInOrder(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb

	secs := promptSections()
	full := assemblePrompt(secs, promptMaxBytes)
	if len(full) > promptMaxBytes {
		t.Fatalf("正常内容就超限了：%d", len(full))
	}

	// 把预算压到刚好吃不下索引：应当只丢 priority=3 的段。
	tight := len(full) - 120
	out := assemblePrompt(secs, tight)
	if len(out) > tight {
		t.Fatalf("降级后仍超限：%d > %d", len(out), tight)
	}
	if !strings.Contains(out, "③ 未验证项") {
		t.Fatal("降级把输出契约丢了：这是最不能丢的一段")
	}
	if !strings.Contains(out, "已省略") {
		t.Fatal("降级必须告知省略了什么，否则模型不知道要自己去查")
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "。") {
		t.Fatalf("降级后末尾不是句末标点（说明又被切句了）：%q", tailOf(out, 40))
	}

	// 极端：只留得下 P1。契约仍在，且绝不出现半句话。
	out = assemblePrompt(secs, 3000)
	if len(out) > 3000 {
		t.Fatalf("极端降级仍超限：%d", len(out))
	}
	if !strings.Contains(out, "① 选层与理由") {
		t.Fatal("极端降级把输出契约丢了")
	}
}

// TestPromptCoversTaskReadFace 守「助手知道异步任务怎么查」。
//
// 不知道读面的后果很具体：它会给用户设计一套"提交后自己轮询上游"的方案，
// 或者干脆把可异步的接口判成 L3（不支持）。这两句必须同时在
// system prompt（每次都在）与知识包正文（按需读取）里。
func TestPromptCoversTaskReadFace(t *testing.T) {
	gdb := withAgentDB(t)
	db.DB = gdb

	// system prompt 只放**最贵的一句**：读面路径。端点词表放知识包正文（按需读取），
	// 全塞进 prompt 会把别的段落挤掉（promptMaxBytes 硬上限，超了按优先级丢段）。
	p := BuildSystemPrompt()
	if !strings.Contains(p, "/v1/tasks/{id}") {
		t.Fatal("system prompt 缺少读面路径（助手会以为平台只能提交、不能查询）")
	}

	raw, err := skillFS.ReadFile("skill/24-async-task.md")
	if err != nil {
		t.Fatalf("读知识包失败：%v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"/v1/tasks/{id}", "/contents/{index}", "DELETE",
		"audio/music/generations", "audio/lyrics", "video/regenerations",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("24-async-task 缺少 %q", want)
		}
	}
}
