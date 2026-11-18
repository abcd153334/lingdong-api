package iconref_test

import (
	"strings"
	"testing"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/service/iconref"
)

// uploadID 是 26 位 Crockford Base32（去 I/L/O/U）的小写 ulid 形态。
const uploadID = "01j9z3k4m5n6p7q8r9s0t1v2w3"

func TestPathBuiltinCarriesPerKeyVersion(t *testing.T) {
	got := iconref.Path("builtin:openai-gpt")
	ver := brandicon.AssetVersion("openai-gpt")
	if ver == "" {
		t.Fatal("openai-gpt 不在内置库，测试前提不成立")
	}
	want := "/api/icons/b/openai-gpt?v=" + ver
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
	if len(ver) != 8 {
		t.Fatalf("版本前缀应为 8 hex，得到 %q", ver)
	}
}

func TestPathBuiltinVersionIsPerKey(t *testing.T) {
	// 两个 key 的版本各自独立：全局单值会让任意一张图变动拖垮全部 URL。
	a := brandicon.AssetVersion("openai-gpt")
	b := brandicon.AssetVersion("deepseek")
	if a == "" || b == "" {
		t.Fatal("测试用 key 必须存在")
	}
	if a == b {
		t.Fatalf("不同图标的版本不应相同（%q），否则 URL 无法随内容失效", a)
	}
}

func TestPathUnknownBuiltinIsEmpty(t *testing.T) {
	// 形状合法但不在 catalog：宁可不下发，也不产出必然 404 的地址。
	if got := iconref.Path("builtin:no-such-brand"); got != "" {
		t.Fatalf("Path = %q, want empty", got)
	}
}

func TestPathUploadHasNoVersion(t *testing.T) {
	got := iconref.Path("upload:" + uploadID)
	want := "/api/icons/u/" + uploadID
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
	if strings.Contains(got, "?") {
		t.Fatalf("上传图标按内容寻址，不应带版本串：%q", got)
	}
}

func TestPathCanonicalizesUploadID(t *testing.T) {
	// 库里的 id 恒为小写；大写令牌必须归一到同一形态，否则"上传成功、立刻 404"。
	upper := strings.ToUpper(uploadID)
	if got, want := iconref.Path("upload:"+upper), "/api/icons/u/"+uploadID; got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestPathRejectsMalformed(t *testing.T) {
	cases := []string{
		"",                           // 未设置
		"openai-gpt",                 // 裸 key（旧形态）
		"builtin:",                   // 空 key
		"upload:",                    // 空 id
		"upload:../../etc",           // 路径穿越
		"builtin:../x",               // 路径穿越
		"data:image/png;base64,AAAA", // data URI
		"https://evil/x.png",         // 外链
	}
	for _, c := range cases {
		if got := iconref.Path(c); got != "" {
			t.Errorf("Path(%q) = %q, want empty", c, got)
		}
	}
}

func TestURLJoinsBase(t *testing.T) {
	p := iconref.Path("builtin:deepseek")
	if got, want := iconref.URL("https://api.example.com", "builtin:deepseek"), "https://api.example.com"+p; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	// 尾斜杠不许拼成双斜杠。
	if got, want := iconref.URL("https://api.example.com/", "builtin:deepseek"), "https://api.example.com"+p; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestURLFallsBackToPath(t *testing.T) {
	p := iconref.Path("upload:" + uploadID)
	if p == "" {
		t.Fatal("测试前提不成立")
	}
	for _, base := range []string{
		"",                      // 未配置
		"   ",                   // 空白
		"javascript:alert(1)",   // 注入
		"ftp://api.example.com", // 非 http(s)
		"/relative-only",        // 无 host
		"https://",              // 解析不出 host
	} {
		if got := iconref.URL(base, "upload:"+uploadID); got != p {
			t.Errorf("URL(%q) = %q, want fallback %q", base, got, p)
		}
	}
}

func TestURLEmptyToken(t *testing.T) {
	if got := iconref.URL("https://api.example.com", ""); got != "" {
		t.Fatalf("URL = %q, want empty", got)
	}
}
