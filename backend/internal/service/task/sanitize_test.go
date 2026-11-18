package task

import (
	"strings"
	"testing"
)

// TestSanitizeReasonRedactsBeforeTruncating 校验「先脱敏后截断」的顺序。
//
// 反过来（先截断后脱敏）会在截断点切断 token，正则匹配不到完整形态，
// 于是库里留下半截密钥 —— 正是脱敏要防的事。这里把密钥放在第 495 字符处，
// 只要顺序错了，结果里就会出现 `sk-` 残片。
func TestSanitizeReasonRedactsBeforeTruncating(t *testing.T) {
	prefix := strings.Repeat("错", 495)
	got := sanitizeReason(prefix + "sk-abcdefgh12345")

	if strings.Contains(got, "sk-") {
		t.Fatalf("截断点上的密钥残片未被脱敏: 末尾 %q", tail(got, 20))
	}
	if n := len([]rune(got)); n > 500 {
		t.Fatalf("超长未截断: %d 字符", n)
	}
	// 占位符被截断时只允许整段消失，不允许留 `<reda` 这种半截。
	for _, ph := range redactPlaceholders {
		for n := len(ph) - 1; n > 0; n-- {
			if strings.HasSuffix(got, ph[:n]) {
				t.Fatalf("末尾残留半截占位符 %q", ph[:n])
			}
		}
	}
}

// TestSanitizeReasonKeepsFullPlaceholder 未触及截断时占位符必须完整保留。
func TestSanitizeReasonKeepsFullPlaceholder(t *testing.T) {
	got := sanitizeReason(strings.Repeat("a", 100) + " sk-abcdefgh12345")
	if !strings.Contains(got, "<redacted-key>") {
		t.Fatalf("完整占位符应保留，实际末尾 %q", tail(got, 30))
	}
}

// TestSanitizeReasonKeepsReadableError 正常错误文案必须原样保留（脱敏不该吃掉信息）。
func TestSanitizeReasonKeepsReadableError(t *testing.T) {
	const msg = "上游任务失败：参考图尺寸超出限制"
	if got := sanitizeReason(msg); got != msg {
		t.Fatalf("可读文案被改写: %q", got)
	}
}

// TestSanitizeReasonTrimsPartialHostPlaceholder 守「末尾残片裁剪对主机名占位符同样生效」。
//
// 提交链路会先用 relay.RedactUpstream(msg, ch, key) 把上游主机名抹成
// `<redacted-host>`，再进这里；若裁剪表里写的还是早已不存在的 `<upstream-host>`，
// 截断点落在占位符中间就会留下 `<redacted-hos` 这样的残片。
func TestSanitizeReasonTrimsPartialHostPlaceholder(t *testing.T) {
	prefix := strings.Repeat("错", 495)
	got := sanitizeReason(prefix + "<redacted-host>")

	for _, ph := range redactPlaceholders {
		for n := len(ph) - 1; n > 0; n-- {
			if strings.HasSuffix(got, ph[:n]) {
				t.Fatalf("末尾残留半截占位符 %q（末尾 %q）", ph[:n], tail(got, 20))
			}
		}
	}
	if n := len([]rune(got)); n > 500 {
		t.Fatalf("超长未截断: %d 字符", n)
	}
}

// tail 返回末尾 n 个字符（日志断言用，避免打印整段前缀）。
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
