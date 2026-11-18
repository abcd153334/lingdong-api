package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 本文件守「参数解析只有一条管线，且失败必须可定位」。
//
// fixture 来自真实故障：`testdata/badargs/validate_draft_extra_brace.json`
// 是线上那份 3513 字符（3883 字节）的 `operations[0]` 多开一层 `{` 的原样导出。
// 用它当基准而不是手写坏 JSON —— 手写的一定"坏得恰好被识别"，真实输入不会。

func readBadArgs(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/badargs/" + name)
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	return string(raw)
}

func TestParseToolArgsLocatesRealFailure(t *testing.T) {
	raw := readBadArgs(t, "validate_draft_extra_brace.json")

	_, e := ParseToolArgs(raw, false)
	if e == nil {
		t.Fatal("这份参数必须在 operations[0] 多开的一层花括号处失败")
	}
	if e.Code != argCodeInvalid {
		t.Fatalf("code 应为 %s，实际 %s", argCodeInvalid, e.Code)
	}
	// 括号统计：整份少一个 }。
	if e.Balance.NetCurly != 1 {
		t.Fatalf("整份应少 1 个 }（实际 net=%d，open=%d close=%d）",
			e.Balance.NetCurly, e.Balance.CurlyOpen, e.Balance.CurlyClose)
	}
	// 双份 offset：字节给程序，字符给人/模型。中文文档里两者差很多，
	// 只给字节会让"第 2868 字节"落到一句中文中间。
	if e.ByteOffset <= 0 || e.RuneOffset <= 0 {
		t.Fatalf("必须给出定位：byte=%d rune=%d", e.ByteOffset, e.RuneOffset)
	}
	if e.ByteOffset <= e.RuneOffset {
		t.Fatalf("这份参数含中文，字节数应大于字符数：byte=%d rune=%d", e.ByteOffset, e.RuneOffset)
	}
	if e.Line <= 0 || e.Column <= 0 {
		t.Fatalf("行列必须 1-based 且为正：line=%d col=%d", e.Line, e.Column)
	}
	// 未闭合栈必须指向出问题的那一层（而不是笼统的"根对象"）。
	if len(e.Unclosed) == 0 {
		t.Fatal("必须给出未闭合栈：模型要据此回到那一层")
	}
	if !strings.HasPrefix(e.Unclosed[0].Path, "$.operations[0]") {
		t.Fatalf("最深未闭合容器应位于 $.operations[0] 内，实际 %q", e.Unclosed[0].Path)
	}
	// hint 必须区分"中间多开一层"与"结尾少一层"——两类改法完全不同。
	if !strings.Contains(e.Hint, "多开了一层") {
		t.Fatalf("hint 必须指出「多开了一层」，实际：%s", e.Hint)
	}
	if strings.Contains(e.Hint, "结尾被截断") {
		t.Fatalf("这份不是截断，hint 不能误导成截断：%s", e.Hint)
	}
	// 片段：要能看见出错位置前后的内容。
	if e.Snippet.Before == "" || e.Snippet.After == "" {
		t.Fatalf("snippet 必须带上下文：%+v", e.Snippet)
	}
}

func TestParseToolArgsRepairsOnlyUnambiguous(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantOK  bool
		wantFix string
	}{
		{"围栏", "```json\n{\"operations\":[]}\n```", true, "stripped_fence"},
		{"BOM", "\ufeff{\"operations\":[]}", true, "stripped_bom"},
		{"前后噪声", "参数如下：{\"operations\":[]} 完毕", true, "trimmed_epilogue"},
		// 下面这些**不许**自动修：改了就是把"结构错"变成"结构对但内容错"。
		{"尾逗号不修", `{"operations":[],}`, false, ""},
		{"缺闭合不修", `{"operations":[{"op":"create"`, false, ""},
		{"单引号不修", `{'operations':[]}`, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, e := ParseToolArgs(c.in, false)
			if c.wantOK {
				if e != nil {
					t.Fatalf("应可解析，实际失败：%+v", e)
				}
				return
			}
			if e == nil {
				t.Fatal("这类输入必须拒绝，不能替模型猜")
			}
			if c.wantFix != "" && !containsStr(e.Repairs, c.wantFix) {
				t.Fatalf("应记录修复 %q，实际 %v", c.wantFix, e.Repairs)
			}
		})
	}
}

func TestParseToolArgsTruncationIsNotSyntaxError(t *testing.T) {
	// 半截 JSON + finish_reason=length：必须回 output_truncated，
	// 否则模型会以为是自己数错括号，反复重写同样大小的参数。
	_, e := ParseToolArgs(`{"operations":[{"op":"create","entity":"chann`, true)
	if e == nil {
		t.Fatal("截断必须被识别")
	}
	if e.Code != argCodeTruncate {
		t.Fatalf("code 应为 %s，实际 %s", argCodeTruncate, e.Code)
	}
	if !strings.Contains(e.Hint, "不要原样重试") {
		t.Fatalf("截断的 hint 必须阻止原样重试，实际：%s", e.Hint)
	}
	if e.Tail == "" {
		t.Fatal("截断回执要给 tail：运营/模型据它判断截在哪")
	}
}

func TestParseToolArgsFullwidthHint(t *testing.T) {
	_, e := ParseToolArgs(`｛"operations":[]｝`, false)
	if e == nil {
		t.Fatal("全角括号必须失败")
	}
	if !strings.Contains(e.Hint, "全角") {
		t.Fatalf("hint 应指出全角标点，实际：%s", e.Hint)
	}
}

func TestToolCallTruncatedFromFinishReason(t *testing.T) {
	agg := newToolAggregator()
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"validate_draft","arguments":"{\"operations\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"validate_draft","arguments":"[]}"}}]},"finish_reason":"length"}]}`,
	}
	for _, f := range frames {
		if _, _, err := agg.feed([]byte(f)); err != nil {
			t.Fatalf("喂帧失败: %v", err)
		}
	}
	out := agg.result()
	if len(out) != 1 {
		t.Fatalf("应有 1 个调用，实际 %d", len(out))
	}
	// 首帧定名：每帧都带 name 也不能拼成 "validate_draftvalidate_draft"。
	if out[0].Name != "validate_draft" {
		t.Fatalf("工具名被重复拼接：%q", out[0].Name)
	}
	if out[0].Arguments != `{"operations":[]}` {
		t.Fatalf("参数拼接错误：%q", out[0].Arguments)
	}
	if !out[0].Truncated {
		t.Fatal("finish_reason=length 必须标记 Truncated")
	}
}

func TestRedactJSONLikeCoversBrokenJSON(t *testing.T) {
	// 这段正是 dry_run_draft 的真实参数形状（且故意是坏 JSON：少一个 }）。
	raw := `{"adapter_id":"spec:x","api_key":"sk-live-123456","base_url":"https://a.example","spec":{"endpoints":{}}`
	out := redactJSONLike(raw)
	if strings.Contains(out, "sk-live-123456") {
		t.Fatalf("密钥没被脱敏：%s", out)
	}
	if !strings.Contains(out, `"api_key":"***"`) {
		t.Fatalf("脱敏后应保留键名并替换值：%s", out)
	}
	// 非敏感键必须原样保留（否则摘要失去排障价值）。
	if !strings.Contains(out, "https://a.example") {
		t.Fatalf("非敏感值被误删：%s", out)
	}
	// keys 数组也要盖住。
	arr := redactJSONLike(`{"keys":["sk-1","sk-2"],"name":"c"}`)
	if strings.Contains(arr, "sk-1") {
		t.Fatalf("keys 数组没被脱敏：%s", arr)
	}
}

func TestSafeArgsBriefRedactsBeforeClipping(t *testing.T) {
	raw := `{"api_key":"sk-live-abcdefghijklmnop","base_url":"https://x.example"}`
	brief := SafeArgsBrief(raw, 20)
	if strings.Contains(brief, "sk-live") {
		t.Fatalf("先截断后脱敏会漏出密钥前缀：%s", brief)
	}
	if len(brief) > 20 {
		t.Fatalf("摘要超长：%d", len(brief))
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

var _ = json.Marshal
