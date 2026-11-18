package callio

import (
	"strings"
	"testing"
)

// TestSanitizeMasksSecrets 覆盖各类敏感值的脱敏。
//
// 报文留存的整个价值都建立在这一个函数上：一旦漏掉某类密钥，
// 它就会被原样存进 call_io，并在管理台与门户详情里对所有人可见。
func TestSanitizeMasksSecrets(t *testing.T) {
	raw := `{
		"model":"gpt-4o",
		"messages":[{"role":"user","content":"你好"}],
		"api_key":"sk-live-abcdef123456",
		"Authorization":"Bearer tok_abcdef123456",
		"client_secret":"cs_zzz",
		"nested":{"access_token":"at_123456789","x-api-key":"k-999"},
		"private_key":"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
		"image":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"
	}`
	out := string(Sanitize([]byte(raw)))

	for _, leak := range []string{
		"sk-live-abcdef123456", "tok_abcdef123456", "cs_zzz", "at_123456789",
		"k-999", "MIIEow", "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB",
	} {
		if strings.Contains(out, leak) {
			t.Fatalf("脱敏后仍包含 %q:\n%s", leak, out)
		}
	}
	// 非敏感内容必须原样保留 —— 否则"原始报文"就没有意义了。
	for _, keep := range []string{`"model":"gpt-4o"`, `"role":"user"`, "你好"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("可读内容被误删 %q:\n%s", keep, out)
		}
	}
	if !strings.Contains(out, redacted) {
		t.Fatalf("应写入占位符:\n%s", out)
	}
}

// TestSanitizeFormEncoding 覆盖表单/查询串形态（multipart 摘要、urlencoded）。
func TestSanitizeFormEncoding(t *testing.T) {
	raw := "model=x&api_key=sk-abcdef123456&token=tok_9&signature=abcd1234"
	out := string(Sanitize([]byte(raw)))
	for _, leak := range []string{"sk-abcdef123456", "tok_9", "abcd1234"} {
		if strings.Contains(out, leak) {
			t.Fatalf("表单密钥未脱敏 %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "model=x") {
		t.Fatalf("普通字段被误删: %s", out)
	}
}

// TestSanitizeBeforeTruncate 是「先脱敏后截断」的回归锁。
//
// 反过来（先截断再脱敏）会在上限处把一个 token 切成两半：前半截留在库里、
// 后半截丢掉，而正则匹配的是完整形态 —— 两半都漏网，等于没脱敏。
func TestSanitizeBeforeTruncate(t *testing.T) {
	// 构造一个「先截断就会漏密钥」的用例：
	//   prefix 40 字节 + 密钥 30 字节，截断上限 43 字节。
	//   - 先脱敏后截断：变成 40 个 a + ******（46 字节），截到 43 → 只看到 3 个星号；
	//   - 先截断后脱敏：截到 43 → "aaa…sk-l"，此时正则匹配不到完整密钥 → "sk-l" 原样留下。
	prefix := strings.Repeat("a", 40)
	raw := prefix + "sk-live-1234567890abcdef12345"
	got, truncated := cutString(Sanitize([]byte(raw)), 43)
	if !truncated {
		t.Fatal("应判定为截断")
	}
	if strings.Contains(got, "sk-l") {
		t.Fatalf("截断后残留密钥片段（说明是先截断后脱敏）: %q", got)
	}
	if got != prefix+"***" {
		t.Fatalf("应保留脱敏占位的前 3 个字符，实际 %q", got)
	}
}

// TestCutStringKeepsUTF8 保证不在多字节字符中间切断（否则前端 JSON.parse 直接失败）。
func TestCutStringKeepsUTF8(t *testing.T) {
	// "中文" 每个字 3 字节：limit=4 会落在第二个字的中间。
	got, truncated := cutString([]byte("中文内容"), 4)
	if !truncated {
		t.Fatal("应判定为截断")
	}
	if got != "中" {
		t.Fatalf("应按字符边界回退，得到 %q", got)
	}
	if !strings.HasPrefix("中文内容", got) {
		t.Fatal("截断结果必须是原串前缀")
	}
}

// TestSanitizeEmpty 空输入原样返回（不 panic、不产生占位）。
func TestSanitizeEmpty(t *testing.T) {
	if got := Sanitize(nil); len(got) != 0 {
		t.Fatalf("nil 应返回空: %q", got)
	}
	if got := Sanitize([]byte("")); len(got) != 0 {
		t.Fatalf("空串应返回空: %q", got)
	}
	if got := Sanitize([]byte("纯中文无敏感内容")); string(got) != "纯中文无敏感内容" {
		t.Fatalf("无可脱敏内容不应改写: %q", got)
	}
}
