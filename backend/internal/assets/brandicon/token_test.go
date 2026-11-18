package brandicon

import (
	"errors"
	"strings"
	"testing"
)

const sampleULID = "01j9x8k2m4n6p8r0t2v4w6y8z0"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"空串是清空", "", "", false},
		{"空白也是清空", "   ", "", false},
		{"内置命中", "builtin:openai-gpt", "builtin:openai-gpt", false},
		{"内置两侧空白被 trim", "  builtin:deepseek \t", "builtin:deepseek", false},
		{"上传命中", "upload:" + sampleULID, "upload:" + sampleULID, false},
		// 大写 ULID 是**合法**输入：Crockford Base32 大小写等价，而库里的 id 恒为小写。
		// 拒绝它就会造出一个"上传成功、立刻 404"的窗口（token 是前端回传的，
		// 谁也无法保证它一路上没被 ToUpper 过）。归一成小写才是唯一无歧义的形态。
		{"上传 id 大写被归一小写", "upload:" + strings.ToUpper(sampleULID), "upload:" + sampleULID, false},

		// 以下是**必须拒绝**的形态，每一条都对应一个真实的攻击面或脏数据形态。
		{"内置 key 不在目录", "builtin:openai", "", true},
		{"内置 key 为空", "builtin:", "", true},
		{"内置 key 带路径", "builtin:../etc/passwd", "", true},
		{"上传 id 太短", "upload:abc", "", true},
		{"上传 id 含斜杠", "upload:../../../etc/passwd", "", true},
		{"上传 id 含非法字符 I/L/O/U", "upload:01J9X8K2M4N6P8R0T2V4W6Y8IL", "", true},
		{"data URI", "data:image/png;base64,iVBORw0KGgo=", "", true},
		{"外链 http", "http://evil.example/a.png", "", true},
		{"外链 https", "https://evil.example/a.png", "", true},
		{"协议相对", "//evil.example/a.png", "", true},
		{"相对路径", "/api/icons/b/openai-gpt", "", true},
		{"javascript 伪协议", "javascript:alert(1)", "", true},
		{"裸 key", "openai-gpt", "", true},
		{"超长", "builtin:" + strings.Repeat("a", MaxLen), "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Normalize(c.in)
			if c.err {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("%q 应当被拒绝，实际 err=%v got=%q", c.in, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q 应当通过，实际 err=%v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("%q 归一结果 %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

func TestEffective(t *testing.T) {
	cases := []struct {
		own, fallback, want string
	}{
		{"", "", ""},
		{"", "builtin:openai-gpt", "builtin:openai-gpt"},
		{"builtin:deepseek", "builtin:openai-gpt", "builtin:deepseek"},
		{"  builtin:suno ", "", "builtin:suno"},
		{"", "  ", ""},
	}
	for _, c := range cases {
		if got := Effective(c.own, c.fallback); got != c.want {
			t.Fatalf("Effective(%q,%q)=%q，期望 %q", c.own, c.fallback, got, c.want)
		}
	}
}

func TestSplitToken(t *testing.T) {
	if k, ok := BuiltinKey("builtin:openai-gpt"); !ok || k != "openai-gpt" {
		t.Fatalf("BuiltinKey 解析错误：%q %v", k, ok)
	}
	if _, ok := BuiltinKey("upload:" + sampleULID); ok {
		t.Fatal("upload token 不应被当成内置")
	}
	if id, ok := UploadID("upload:" + sampleULID); !ok || id != sampleULID {
		t.Fatalf("UploadID 解析错误：%q %v", id, ok)
	}
	if _, ok := UploadID("builtin:openai-gpt"); ok {
		t.Fatal("builtin token 不应被当成上传")
	}
	if ValidUploadID("../x") {
		t.Fatal("非法 id 不应通过")
	}
	// UploadToken 必须产出能通过 Normalize 的 token：两处形态不一致时，
	// "上传成功但保存实体时 400 icon_invalid" 就会成为一个真实缺陷。
	tok := UploadToken(strings.ToUpper(sampleULID))
	if tok != "upload:"+sampleULID {
		t.Fatalf("UploadToken 应归一为小写，实际 %q", tok)
	}
	if _, err := Normalize(tok); err != nil {
		t.Fatalf("UploadToken 的产物必须能通过 Normalize，实际 %v", err)
	}
	if got := UploadToken("not-a-ulid"); got != "" {
		t.Fatalf("非法 id 不应拼出 token，实际 %q", got)
	}
}
