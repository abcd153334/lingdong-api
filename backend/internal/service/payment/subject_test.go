package payment

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitizeTitleLength 长度按**字符**算，不按字节（用 len() 会把中文砍到 14 个字）。
func TestSanitizeTitleLength(t *testing.T) {
	twenty42 := strings.Repeat("测", 42)
	if got := SanitizeTitle(twenty42); got != twenty42 {
		t.Fatalf("42 个汉字不应被截断: rune=%d", utf8.RuneCountInString(got))
	}
	if got := SanitizeTitle(twenty42 + "试"); utf8.RuneCountInString(got) != 42 {
		t.Fatalf("43 个汉字应截断到 42，实际 %d", utf8.RuneCountInString(got))
	}
	if got := SanitizeTitle(twenty42); len(got) > maxTitleBytes {
		t.Fatalf("字节数超限: %d > %d", len(got), maxTitleBytes)
	}
}

// TestSanitizeTitleRejectsEmojiAndPercent 官方明确禁 emoji 与 %。
func TestSanitizeTitleRejectsEmojiAndPercent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"充值-体验包🎉", "充值-体验包"},
		{"充值-100%到账", "充值-100到账"},
		{"🎉🎉🎉", "充值"}, // 全是 emoji → 回落
		{"", "充值"},
		{"   ", "充值"},
		{"充值 ¥72.00", "充值 72.00"}, // ¥ 不在白名单被剔除；数字/小数点/空格保留
	}
	for _, c := range cases {
		if got := SanitizeTitle(c.in); got != c.want {
			t.Errorf("SanitizeTitle(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeTitleTrimsTrailingPunct 截断后不留尾随标点。
func TestSanitizeTitleTrimsTrailingPunct(t *testing.T) {
	for _, in := range []string{"充值-", "充值——", "充值、", "充值 "} {
		got := SanitizeTitle(in)
		if strings.HasSuffix(got, "-") || strings.HasSuffix(got, "、") || strings.HasSuffix(got, " ") {
			t.Errorf("SanitizeTitle(%q)=%q 仍有尾随标点", in, got)
		}
		if got == "" {
			t.Errorf("SanitizeTitle(%q) 不应返回空", in)
		}
	}
}

// TestSanitizeTitleKeepsSafeChars 允许的字符不能被误删（含中文标点与下划线）。
func TestSanitizeTitleKeepsSafeChars(t *testing.T) {
	const in = "充值-体验包（月付）_v2"
	if got := SanitizeTitle(in); got != in {
		t.Fatalf("安全字符被误删: %q → %q", in, got)
	}
}
