package sitecfg

import (
	"encoding/json"
	"strings"
	"testing"

	"lingdong-api/internal/conf"
)

// 本文件守住「多地址的归一与边界」：每一条都对应一个真实会发生的输入，
// 而不是为了覆盖而覆盖。
func site(bases ...string) Site {
	return Site{
		SiteName:     "灵动 API",
		SiteURL:      "https://www.example.com",
		APIBaseURLs:  bases,
		ContactEmail: "ops@example.com",
	}
}

func TestNormalizeTrimsAndDedupes(t *testing.T) {
	in := site("  https://a.example.com/  ", "https://b.example.com//", "https://A.example.com", "")
	got, err := Normalize(in)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	want := []string{"https://a.example.com", "https://b.example.com"}
	if len(got.APIBaseURLs) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got.APIBaseURLs)
	}
	for i := range want {
		if got.APIBaseURLs[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v", want, got.APIBaseURLs)
		}
	}
	// 去重保留首次出现的**顺序**：第一条是"主"，顺序即语义
	if got.APIBaseURLs[0] != want[0] {
		t.Fatalf("去重不得改变首条，实际 %v", got.APIBaseURLs)
	}
}

// 运营最常见的输入是把 OpenAI 兼容地址整串粘进来（带 /v1）。
// 不剥就会在门户显示 https://api.x.com/v1/v1。
func TestNormalizeStripsV1Suffix(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com/v1":   "https://api.example.com",
		"https://api.example.com/v1/":  "https://api.example.com",
		"https://api.example.com/v1/ ": "https://api.example.com",
		"https://api.example.com":      "https://api.example.com",
	}
	for in, want := range cases {
		got, err := Normalize(site(in))
		if err != nil {
			t.Fatalf("%s 不应报错：%v", in, err)
		}
		if got.APIBaseURLs[0] != want {
			t.Fatalf("%s：期望 %s，实际 %s", in, want, got.APIBaseURLs[0])
		}
	}
}

// 地址必须是 origin：带路径/参数/账号信息的写法会在下游拼出
// `https://x/foo/v1/...`，而错误现场只表现为"接口不通"。
func TestNormalizeRejectsNonOrigin(t *testing.T) {
	for _, raw := range []string{
		"https://api.example.com/prefix",
		"https://api.example.com/prefix/v1",
		"https://api.example.com/?a=1",
		"https://api.example.com/#frag",
		"https://user:pass@api.example.com",
	} {
		if _, err := Normalize(site(raw)); err == nil {
			t.Fatalf("%q 必须被拒绝（只能是 origin）", raw)
		}
	}
}

func TestNormalizeRejectsBadAddresses(t *testing.T) {
	for _, raw := range []string{
		"api.example.com",       // 没 scheme
		"ftp://api.example.com", // scheme 不对
		"https://",              // 没 host
		"javascript:alert(1)",   // 非 http(s)
		"/relative/path",        // 相对地址
	} {
		if _, err := Normalize(site(raw)); err == nil {
			t.Fatalf("%q 必须被拒绝", raw)
		} else if !strings.Contains(err.Error(), "API 调用地址") {
			t.Fatalf("%q 的报错要指明是哪个字段，实际 %v", raw, err)
		}
	}
}

func TestNormalizeLimits(t *testing.T) {
	eight := make([]string, 8)
	for i := range eight {
		eight[i] = "https://h" + string(rune('a'+i)) + ".example.com"
	}
	if _, err := Normalize(site(eight...)); err != nil {
		t.Fatalf("8 条必须允许：%v", err)
	}
	nine := append(eight, "https://h9.example.com")
	if _, err := Normalize(site(nine...)); err == nil {
		t.Fatal("9 条必须被拒绝")
	}

	// 支付回调 = base + "/api/pay/" + 渠道ID + "/notify"，网关要求 ≤128 ⇒ base ≤80
	ok := "https://" + strings.Repeat("a", 68) + ".com" // 8 + 68 + 4 = 80
	if len(ok) != 80 {
		t.Fatalf("用例长度构造错误：%d", len(ok))
	}
	if _, err := Normalize(site(ok)); err != nil {
		t.Fatalf("80 字符必须允许：%v", err)
	}
	if _, err := Normalize(site(ok + "x")); err == nil {
		t.Fatal("81 字符必须被拒绝")
	}
	// 只有**首条**受限：第二条再长也不影响回调
	if _, err := Normalize(site("https://a.example.com", ok+"x")); err != nil {
		t.Fatalf("非首条不受 80 字符限制：%v", err)
	}
}

func TestNormalizeImageLimits(t *testing.T) {
	in := site("https://api.example.com")
	in.FaviconImage = strings.Repeat("a", MaxFaviconBytes+1)
	if _, err := Normalize(in); err == nil {
		t.Fatal("超限图标必须被拒绝")
	}
	in.FaviconImage = strings.Repeat("a", MaxFaviconBytes)
	if _, err := Normalize(in); err != nil {
		t.Fatalf("边界值必须允许：%v", err)
	}
}

func TestNormalizeSiteURL(t *testing.T) {
	in := site("https://api.example.com")
	in.SiteURL = "https://www.example.com/"
	got, err := Normalize(in)
	if err != nil || got.SiteURL != "https://www.example.com" {
		t.Fatalf("站点地址必须去尾斜杠，实际 %q / %v", got.SiteURL, err)
	}
	in.SiteURL = "www.example.com"
	if _, err := Normalize(in); err == nil {
		t.Fatal("没有 scheme 的站点地址必须被拒绝（它参与 XFH 白名单判定）")
	}
	// 允许留空：未配置时回落到请求 origin
	in.SiteURL = ""
	if _, err := Normalize(in); err != nil {
		t.Fatalf("站点地址允许留空：%v", err)
	}
}

func TestHostsCoversBasesAndSiteURL(t *testing.T) {
	got := site("https://api.example.com", "https://api2.example.com:8443").Hosts()
	// 端口必须被去掉：XFH 的主机名比较不带端口
	want := map[string]bool{"api.example.com": true, "api2.example.com": true, "www.example.com": true}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	for _, h := range got {
		if !want[h] {
			t.Fatalf("出现了不该有的 host：%s", h)
		}
	}
}

func TestLoadCanonicalizes(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"site_name":     " 灵动 ",
		"api_base_urls": []string{"https://a.example.com/", "  "},
	})
	conf.System().Reload(map[string]json.RawMessage{"site": raw})
	t.Cleanup(func() { conf.System().Reload(map[string]json.RawMessage{}) })

	s := Load()
	if s.SiteName != "灵动" {
		t.Fatalf("站名必须去空白，实际 %q", s.SiteName)
	}
	// 读边界就去掉尾斜杠：否则下游 base+"/f/" 会拼出双斜杠
	if len(s.APIBaseURLs) != 1 || s.APIBaseURLs[0] != "https://a.example.com" {
		t.Fatalf("读边界必须归一，实际 %v", s.APIBaseURLs)
	}
	if s.PrimaryBase() != "https://a.example.com" {
		t.Fatalf("首条为主，实际 %q", s.PrimaryBase())
	}
}

// MatchBase 是「按入口线路选地址」的唯一实现，三条匹配档位必须逐条锁住。
func TestMatchBase(t *testing.T) {
	s := Site{APIBaseURLs: []string{
		"https://a.example.com",
		"https://b.example.com:8443",
	}}
	cases := []struct {
		raw    string
		scheme string
		want   string
	}{
		{"b.example.com:8443", "https", "https://b.example.com:8443"}, // ① host+port
		{"B.EXAMPLE.COM:8443", "https", "https://b.example.com:8443"}, // 大小写归一
		{"a.example.com", "https", "https://a.example.com"},           // 默认端口 443
		{"a.example.com:443", "https", "https://a.example.com"},       // 显式默认端口
		{"a.example.com", "http", "https://a.example.com"},            // ② host+scheme 不中 → ③ 仅 host
		{"evil.example.net", "https", ""},
		{"", "https", ""},
	}
	for _, tc := range cases {
		if got := s.MatchBase(tc.raw, tc.scheme); got != tc.want {
			t.Errorf("MatchBase(%q, %s) = %q, want %q", tc.raw, tc.scheme, got, tc.want)
		}
	}
}
