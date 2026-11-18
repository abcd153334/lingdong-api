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
