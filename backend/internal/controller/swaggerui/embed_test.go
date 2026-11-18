package swaggerui

// 本文件守的是**内嵌资源的完整性与来源可追溯性**。
//
// 为什么需要两个断言合起来看：
//
//	go:embed 的失败方式是**静默**的。少提交一个文件，编译照样过，
//	运行期 `Asset()` 返回 false → 页面白屏或样式全丢，而日志里只有一条 404。
//	所以「4 个必需文件都在且非空」必须在测试里显式列出来。
//
//	更隐蔽的是「提交了一份来源不明的 bundle」：文件在、能跑，但没人能说出
//	它是哪个版本、有没有被改过。Version 常量 + 断言 bundle 里含该版本号，
//	把"说过的话"变成"每次 CI 都重新验证的话"。改 assets 忘改 Version，
//	这里立刻红。

import (
	"strings"
	"testing"
)

// requiredAssets 是运行期真正会被请求到的文件。
//
// index.html 是自写的（原版依赖 standalone-preset 且带 Explore 输入框），
// 其余三个是 swagger-ui-dist 的原样拷贝。LICENSE/NOTICE 不是运行期需要，
// 但 Apache-2.0 要求在分发物里保留，因此一并纳入必需清单。
var requiredAssets = []string{
	"index.html",
	"swagger-ui.css",
	"swagger-ui-bundle.js",
	"LICENSE",
	"NOTICE",
}

// TestRequiredAssetsPresent 必需文件都在、都非空。
func TestRequiredAssetsPresent(t *testing.T) {
	for _, name := range requiredAssets {
		raw, mime, ok := Asset(name)
		if !ok {
			t.Errorf("%s 缺失（go:embed 少提交文件时编译仍会通过，必须在这里拦住）", name)
			continue
		}
		if len(raw) == 0 {
			t.Errorf("%s 是空文件", name)
		}
		if mime == "" {
			t.Errorf("%s 没有 MIME", name)
		}
	}
}

// TestBundleMatchesVersion bundle 里必须能查到 Version 声明的版本号。
func TestBundleMatchesVersion(t *testing.T) {
	raw, ok := read(t, "swagger-ui-bundle.js")
	if !ok {
		return
	}
	// Version 形如 `swagger-ui-dist@5.34.0`，bundle 里是 `PACKAGE_VERSION:"5.34.0"`。
	ver := strings.TrimPrefix(Version, "swagger-ui-dist@")
	if ver == Version || ver == "" {
		t.Fatalf("Version 常量形态变了（当前 %q）：本测试靠 `swagger-ui-dist@X.Y.Z` 解析版本号", Version)
	}
	marker := `PACKAGE_VERSION:"` + ver + `"`
	if !strings.Contains(string(raw), marker) {
		t.Fatalf("bundle 里找不到 %s：assets 与 Version 常量不同步（改了 bundle 忘改常量，或反之）", marker)
	}
}

// TestIndexHTMLHasSecuritySettings 三处安全设置一条都不能被删掉。
//
// 这三条不是"加固"，是**功能正确性**：
//   - validatorUrl 非 null → 打开页面就把管理台 spec 发到外网；
//   - persistAuthorization 为 true → 粘贴的密钥写进 localStorage；
//   - standalone preset → 多 264KB 与一次 AdminAuth，且它带 Explore 输入框的入口。
func TestIndexHTMLHasSecuritySettings(t *testing.T) {
	raw, ok := read(t, "index.html")
	if !ok {
		return
	}
	html := string(raw)
	musts := []struct {
		needle string
		why    string
	}{
		{"validatorUrl: null", "缺了它会把 spec 发到 validator.swagger.io"},
		{"persistAuthorization: false", "缺了它会把粘贴的密钥留在 localStorage"},
		{"tryItOutEnabled: false", "默认展开试调用：接入方会在没读参数的情况下直接发请求"},
	}
	for _, m := range musts {
		if !strings.Contains(html, m.needle) {
			t.Errorf("index.html 缺少 %q —— %s", m.needle, m.why)
		}
	}
	// Explore 输入框是 `?url=` 的入口，必须去掉。
	if strings.Contains(html, `urls:`) || strings.Contains(html, "Topbar") {
		t.Error("index.html 不应启用 urls/Topbar（Explore 输入框接受外部 spec 地址）")
	}
	// 占位符必须存在，否则 IndexHTML 的替换是空操作，页面会去拉字面量 "%%SPEC_URL%%"。
	if !strings.Contains(html, specPlaceholder) {
		t.Errorf("index.html 缺少占位符 %q：IndexHTML 的注入会变成空操作", specPlaceholder)
	}
}

// TestIndexHTMLInjectsSpec 注入后占位符消失、地址就位。
func TestIndexHTMLInjectsSpec(t *testing.T) {
	html, ok := IndexHTML("../openapi.json")
	if !ok {
		t.Fatal("IndexHTML 拒绝了法定的相对地址 ../openapi.json")
	}
	s := string(html)
	if strings.Contains(s, specPlaceholder) {
		t.Error("占位符未被替换")
	}
	if !strings.Contains(s, `url: "../openapi.json"`) {
		t.Errorf("spec 地址未注入，实际片段：%s", excerpt(s, "url:", 60))
	}
}

// TestValidSpecPath 白名单形态：接受站内绝对路径与 ../ 相对路径，拒绝一切能指向外站的形式。
func TestValidSpecPath(t *testing.T) {
	ok := []string{
		"../openapi.json",
		"/api/admin/api-docs/openapi.json",
		"/a/b/c.json",
	}
	for _, p := range ok {
		if !ValidSpecPath(p) {
			t.Errorf("%q 应被接受", p)
		}
	}
	bad := []string{
		"",                               // 空
		"//evil.com/spec.json",           // 协议相对 URL
		"http://evil.com/spec.json",      // 绝对 URL
		"https://evil.com/spec.json",     // 绝对 URL
		"javascript:alert(1)",            // 伪协议
		"openapi.json",                   // 既非绝对也非 ../
		"./openapi.json",                 // 白名单只收 `/` 与 `../`：多一种前缀就多一条要审的分支
		`../a"b`,                         // 引号：能打断 HTML 属性
		"../a<b",                         // 尖括号
		"../a b",                         // 空格
		"../a?b=1",                       // query：把变量又带回来了
		"../a#frag",                      // 片段
		strings.Repeat("/a", 200) + ".x", // 超长
	}
	for _, p := range bad {
		if ValidSpecPath(p) {
			t.Errorf("%q 必须被拒绝", p)
		}
	}
}

// TestAssetRejectsTraversal Asset 不接受任何带路径分隔符或 .. 的名字。
func TestAssetRejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		"..",
		"../embed.go",       // 想读源码
		"../../etc/passwd",  // 想读系统文件
		"assets/index.html", // 带前缀
		"/index.html",       // 带前导斜杠
		`assets\index.html`, // Windows 分隔符
		"sub/dir/file.css",  // 子目录
	}
	for _, n := range bad {
		if _, _, ok := Asset(n); ok {
			t.Errorf("Asset(%q) 必须拒绝", n)
		}
	}
	// 合法的 4 个名字必须还能取到（防止把守卫写成了"一律拒绝"）。
	if _, _, ok := Asset("index.html"); !ok {
		t.Error("Asset 误杀了合法文件 index.html")
	}
}

// TestAssetMIME MIME 由扩展名决定。
func TestAssetMIME(t *testing.T) {
	cases := []struct{ name, want string }{
		{"swagger-ui.css", "text/css"},
		{"swagger-ui-bundle.js", "application/javascript"},
		{"index.html", "text/html"},
		{"LICENSE", "application/octet-stream"},
	}
	for _, tc := range cases {
		_, mime, ok := Asset(tc.name)
		if !ok {
			t.Errorf("%s 取不到", tc.name)
			continue
		}
		if !strings.HasPrefix(mime, tc.want) {
			t.Errorf("%s 的 MIME 应为 %s*，实际 %q", tc.name, tc.want, mime)
		}
	}
}

// TestNoStandalonePreset 不打包 standalone-preset（264KB，BaseLayout 用不到）。
func TestNoStandalonePreset(t *testing.T) {
	if _, _, ok := Asset("swagger-ui-standalone-preset.js"); ok {
		t.Fatal("不应内嵌 swagger-ui-standalone-preset.js：BaseLayout 不需要，多 264KB + 多一次鉴权")
	}
}

func read(t *testing.T, name string) ([]byte, bool) {
	t.Helper()
	raw, _, ok := Asset(name)
	if !ok {
		t.Errorf("%s 取不到", name)
	}
	return raw, ok
}

// excerpt 截取 needle 附近的一段，用于失败信息里直接看到实际内容。
func excerpt(s, needle string, n int) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "（未找到 " + needle + "）"
	}
	end := i + n
	if end > len(s) {
		end = len(s)
	}
	return s[i:end]
}
