package textx

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunes(t *testing.T) {
	cjk := strings.Repeat("上游错误", 200) // 800 字符
	cases := []struct {
		name string
		in   string
		n    int
		want int
	}{
		{"中文500", cjk, 500, 500},
		{"英文边界", "abcdef", 3, 3},
		{"长度不足", "abc", 10, 3},
		{"n为0", "abc", 0, 0},
		{"n为负", "abc", -1, 0},
		{"emoji", strings.Repeat("🙂", 10), 5, 5},
		{"空串", "", 5, 0},
	}
	for _, c := range cases {
		got := TruncateRunes(c.in, c.n)
		if !utf8.ValidString(got) {
			t.Fatalf("%s: 输出非法 UTF-8", c.name)
		}
		if n := utf8.RuneCountInString(got); n != c.want {
			t.Fatalf("%s: 长度=%d，期望 %d", c.name, n, c.want)
		}
	}
}

func TestTruncateRunesEllipsis(t *testing.T) {
	got := TruncateRunesEllipsis(strings.Repeat("字", 10), 5)
	if !utf8.ValidString(got) {
		t.Fatal("输出非法 UTF-8")
	}
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("长度=%d，期望 5", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("缺少省略号: %q", got)
	}
}

func TestSnippet(t *testing.T) {
	body := []byte("{\"error\":\n\"上游返回错误\"}\r\n\t第二行")
	got := Snippet(body, 500)
	if !utf8.ValidString(got) {
		t.Fatal("输出非法 UTF-8")
	}
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("未压平空白: %q", got)
	}
	if len(Snippet([]byte(strings.Repeat("错", 1000)), 500)) != 500*3 {
		t.Fatalf("500 汉字字节长度应为 1500，实际 %d", len(Snippet([]byte(strings.Repeat("错", 1000)), 500)))
	}
}

// TestTruncateRunesRepairsInvalidUTF8 锁住「非法字节不落库」。
//
// 历史事故：spec 适配器按字节切片产出非法 UTF-8，写进 usages.error_msg 时
// PG 报 22021，连带结算/释放事务回滚 —— 冻结永久滞留。
// 修复不能只改调用点：本包「未超限直接返回 s」的分支会把非法串原样放行，
// 所以截断函数自己必须先修非法字节。
func TestTruncateRunesRepairsInvalidUTF8(t *testing.T) {
	// 把一段合法中文按字节切裂，再拼回字符串（模拟上游报文被 s[:n] 切过）。
	full := "ab" + strings.Repeat("中文", 80)
	broken := full[:300] // 300 字节落在多字节字符中间
	if utf8.ValidString(broken) {
		t.Fatal("构造的样本仍是合法 UTF-8，用例无效（需调整切点）")
	}

	for name, fn := range map[string]func(string, int) string{
		"TruncateRunes":         TruncateRunes,
		"TruncateRunesEllipsis": TruncateRunesEllipsis,
	} {
		// 关键：长度**未超限**时也必须修（这是原实现的漏网分支）。
		got := fn(broken, 5000)
		if !utf8.ValidString(got) {
			t.Fatalf("%s 未超限分支放行了非法 UTF-8: %q", name, got)
		}
		// 超限分支同样要合法。
		got = fn(broken, 10)
		if !utf8.ValidString(got) {
			t.Fatalf("%s 超限分支产出非法 UTF-8: %q", name, got)
		}
		if n := utf8.RuneCountInString(got); n > 10 {
			t.Fatalf("%s 截断后 %d 字符，超过 10", name, n)
		}
	}
}
