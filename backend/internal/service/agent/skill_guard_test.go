package agent

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件守的是"知识包与代码不漂移"。
//
// 为什么必须有用例：md 是自然语言，写错一个 section 名不会有任何编译错误，
// 症状是模型按说明去调一个不存在的分支 → invalid_arguments → 它开始猜字段。
// 这正是本轮故障（schema_get 缺 plan 分支）的同一个坑，只是换了一层。

var (
	// schema_get(section="x") / section='x' 两种引号都认。
	skillSectionRe = regexp.MustCompile(`section\s*=\s*["']([a-z_]+)["']`)
	// spec_dsl 的 part 取值：part="x"。
	skillPartRe = regexp.MustCompile(`part\s*=\s*["']([a-z]+)["']`)
)

// TestSkillSectionsExist 断言知识包里提到的每个 section / part 都真实存在。
func TestSkillSectionsExist(t *testing.T) {
	known := map[string]bool{}
	for _, s := range schemaSectionNames {
		known[s] = true
	}
	parts := map[string]bool{}
	for _, p := range specParts {
		parts[p] = true
	}
	for _, id := range SkillIDs() {
		body, _, _, ok := ReadSkill(id, 0, 0)
		if !ok {
			t.Fatalf("读不出知识包 %s", id)
		}
		for _, m := range skillSectionRe.FindAllStringSubmatch(body, -1) {
			if !known[m[1]] {
				t.Errorf("%s 引用了不存在的 section：%q（合法值：%s）",
					id, m[1], strings.Join(schemaSectionNames, "/"))
			}
		}
		for _, m := range skillPartRe.FindAllStringSubmatch(body, -1) {
			if !parts[m[1]] {
				t.Errorf("%s 引用了不存在的 spec_dsl part：%q（合法值：%s）",
					id, m[1], strings.Join(specParts, "/"))
			}
		}
	}
}

// readWholeSkill 读出知识包的**全部**内容（ReadSkill 单次最多 8KB）。
//
// 用首块做"全文断言"是错的：知识包一长，结论句落到第二块以后，
// 守卫就会误报"知识包不再提 validate_draft"——那是测试的错，不是文档的错。
func readWholeSkill(t *testing.T, id string) string {
	t.Helper()
	var b strings.Builder
	offset := 0
	for {
		piece, next, more, ok := ReadSkill(id, offset, 8<<10)
		if !ok {
			t.Fatalf("读不出知识包 %s", id)
		}
		b.WriteString(piece)
		if !more || next <= offset {
			break
		}
		offset = next
	}
	return b.String()
}

// TestSkillHasNoRetiredShapes 断言知识包里不残留已废弃的写法。
//
// 旧写法留在 md 里比"没写"更糟：模型会照着它生成被拒绝的提案，
// 而运营看到的是"AI 明明读过说明还是写错"。
func TestSkillHasNoRetiredShapes(t *testing.T) {
	retired := []struct {
		pat string
		why string
	}{
		{`"entity":"spec"`, "validate_draft 已收 operations 数组，旧的 entity/data 形状不再接受"},
		{`"entity": "spec"`, "同上"},
		{"四态必须齐全", "status_map 已放宽为「succeeded/failed 必填」，旧文案会让模型去凑 queued/running 的假值"},
		{"只能按次", "音频已支持 per_second 按秒计价，旧文案会让模型放弃按秒"},
		{"音频只能 `call`", "同上"},
		// 计费整改后这几个词已经不存在于系统里，留在知识包会让模型去"查一个不存在的字段"。
		{"额度刻度", "金额单位已统一为「微」，没有刻度这层换算"},
		{"充值倍率", "充值倍率已删除：金额直接是微"},
		{"分组倍率", "已改为「分组折扣」（微比例，1e6 = 不打折）"},
		{"qpc", "qpc 是旧刻度字段，已不存在"},
		{"quota_type", "计费方式是 billing_mode（tokens/call/spec）"},
	}
	for _, id := range SkillIDs() {
		body := readWholeSkill(t, id)
		for _, r := range retired {
			if strings.Contains(body, r.pat) {
				t.Errorf("%s 含有已废弃的写法 %s：%s", id, r.pat, r.why)
			}
		}
	}
	// 反向保险：知识包里必须还能找到 validate_draft 的正确用法说明。
	all := ""
	for _, id := range SkillIDs() {
		all += readWholeSkill(t, id)
	}
	if !strings.Contains(all, "validate_draft") {
		t.Fatal("知识包整体不再提 validate_draft：模型会跳过预演直接提交")
	}
	// 桥接是这一轮新增的表达力，文档缺失会让模型重新把「异步」判成 L3。
	if !strings.Contains(all, "mode=bridge") && !strings.Contains(all, `"mode": "bridge"`) {
		t.Fatal("知识包没有讲异步桥接（mode=bridge）：模型会把可接入的异步接口判成 L3")
	}
	// 运行参数与上下文是这一轮新增的表达力：知识包缺失时，模型会给出"调大倍率"这类
	// 已经不存在于系统的建议，或者根本不知道上下文会被摘要。
	for _, want := range []string{"context_window", "max_output_tokens", "分组折扣", "per_second"} {
		if !strings.Contains(all, want) {
			t.Fatalf("知识包缺少 %q：模型无法解释运行参数与计费口径", want)
		}
	}
}

// TestAskGuidancePresent 断言"何时该问"这篇存在且被判据覆盖。
//
// 单独一条的原因：这篇是行为约束（不是字段表），被删掉不会影响任何编译或契约，
// 只会让助手重新开始"盲目设计与交付"。
func TestAskGuidancePresent(t *testing.T) {
	body, _, _, ok := ReadSkill("05-when-to-ask", 0, 0)
	if !ok {
		t.Fatal("缺少 05-when-to-ask：助手会退化成盲目假设")
	}
	for _, want := range []string{"必须问", "不许问", "ask_user", "未验证项"} {
		if !strings.Contains(body, want) {
			t.Errorf("05-when-to-ask 缺少要点：%q", want)
		}
	}
	// 它必须在默认注入的索引里（否则模型看不到就不会读）。
	if !strings.Contains(SkillIndex(), "05-when-to-ask") {
		t.Fatal("05-when-to-ask 不在索引里：模型不会读到它")
	}
}
