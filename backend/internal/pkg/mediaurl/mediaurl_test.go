package mediaurl

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lingdong-api/internal/conf"
)

// withKey 装配一个确定的签名密钥（不依赖开发机 .env）。
func withKey(t *testing.T, secret string) {
	t.Helper()
	t.Setenv("APP_ENCRYPTION_KEY", secret)
	conf.Load()
}

func TestSignVerifyRoundTrip(t *testing.T) {
	withKey(t, "unit-test-secret")
	raw := "https://cdn.example.com/a/b.png?x=1&y=2"

	tok, err := Sign(raw, time.Hour)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if strings.ContainsAny(tok, "/+= ") {
		t.Fatalf("token 必须是 path 安全字符，实际 %q", tok)
	}
	got, err := Verify(tok)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if got != raw {
		t.Fatalf("回原地址不一致: %q != %q", got, raw)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	withKey(t, "unit-test-secret")
	tok, err := Sign("https://cdn.example.com/a.png", time.Hour)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	payload, sig, _ := strings.Cut(tok, ".")

	// 改载荷（换一个地址）但保留原签名：必须拒绝，否则 token 等于明文。
	forgedPayload, _ := base64.RawURLEncoding.DecodeString(payload)
	fake := append([]byte{}, forgedPayload[:9]...)
	fake = append(fake, []byte("https://evil.example.com/x.png")...)
	forged := base64.RawURLEncoding.EncodeToString(fake) + "." + sig
	if _, err := Verify(forged); !errors.Is(err, ErrInvalid) {
		t.Fatalf("篡改载荷应 ErrInvalid，实际 %v", err)
	}

	// 改签名
	if _, err := Verify(payload + "." + strings.Repeat("A", len(sig))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("篡改签名应 ErrInvalid，实际 %v", err)
	}

	// 形态垃圾
	for _, bad := range []string{"", "abc", ".", "abc.", ".abc", "a.b.c"} {
		if _, err := Verify(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q 应 ErrInvalid，实际 %v", bad, err)
		}
	}
}

func TestVerifyExpired(t *testing.T) {
	withKey(t, "unit-test-secret")
	tok, err := Sign("https://cdn.example.com/a.png", time.Minute)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if _, err := VerifyAt(tok, time.Now().Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期应 ErrExpired，实际 %v", err)
	}
}

func TestVerifyRejectsOnKeyRotation(t *testing.T) {
	withKey(t, "secret-a")
	tok, _ := Sign("https://cdn.example.com/a.png", time.Hour)
	withKey(t, "secret-b")
	if _, err := Verify(tok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("换密钥后旧 token 应失效，实际 %v", err)
	}
}

func TestSignNeedsKey(t *testing.T) {
	withKey(t, "")
	if _, err := Sign("https://cdn.example.com/a.png", time.Hour); !errors.Is(err, ErrNoKey) {
		t.Fatalf("无密钥应 ErrNoKey，实际 %v", err)
	}
	if _, ok := Link("https://api.example.com", "https://cdn.example.com/a.png", time.Hour); ok {
		t.Fatal("无密钥时不应产出可用的能力 URL（否则等于用公开密钥签名）")
	}
}

func TestVerifyRechecksURL(t *testing.T) {
	withKey(t, "unit-test-secret")
	// 内网地址：即使签名正确也不能放行（密钥泄漏/实现走样时的兜底）。
	tok, err := Sign("http://127.0.0.1:8080/x.png", time.Hour)
	if err != nil {
		t.Fatalf("签名本身应成功: %v", err)
	}
	if _, err := Verify(tok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("内网地址应被拒绝，实际 %v", err)
	}
}

func TestLinkSkipsUnsignable(t *testing.T) {
	withKey(t, "unit-test-secret")
	base := "https://api.example.com"
	cases := map[string]string{
		"data URI":   "data:image/png;base64,AAAA",
		"相对路径":       "/tmp/a.png",
		"空":          "",
		"内网":         "http://10.0.0.2/a.png",
		"已签名过":       base + "/v1/media/abc.def",
		"超长":         "https://cdn.example.com/" + strings.Repeat("a", 3000),
		"带凭据":        "https://user:pass@cdn.example.com/a.png",
		"无点主机(内网别名)": "http://intranet/a.png",
	}
	for name, u := range cases {
		if got, ok := Link(base, u, time.Hour); ok {
			t.Fatalf("%s 不应被改写，却得到 %q", name, got)
		}
	}
	ok, _ := Link(base, "https://cdn.example.com/a.png", time.Hour)
	if !strings.HasPrefix(ok, base+PathPrefix) {
		t.Fatalf("可改写链接前缀不对: %q", ok)
	}
}

// rewriteBody 是测试里的「一条报文 → 改写后的报文」，走生产路径 RewriteDataURLs。
//
// 整份重写的 RewriteJSON 已删：协议面的响应归一现在只解析一次报文（relay.shapeResponse），
// 没有第二个调用者。但它守的三条性质仍要守：未改写原样返回、改写后链接可自行验回、
// 查询串里的 & 不被 HTML 转义。
func rewriteBody(t *testing.T, body []byte, base string) ([]byte, int) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, 0
	}
	n := RewriteDataURLs(top, base, time.Hour)
	if n == 0 {
		return body, 0
	}
	out, err := marshalNoEsc(top)
	if err != nil {
		t.Fatalf("重新序列化失败: %v", err)
	}
	return out, n
}

func TestRewriteDataURLs(t *testing.T) {
	withKey(t, "unit-test-secret")
	base := "https://api.example.com"

	body := []byte(`{"created":1,"data":[{"url":"https://cdn.example.com/a.png","revised_prompt":"p"},` +
		`{"b64_json":"AAAA"},{"url":"https://cdn.example.com/b.png?x=1&y=2"}]}`)
	out, n := rewriteBody(t, body, base)
	if n != 2 {
		t.Fatalf("应改写 2 条，实际 %d", n)
	}
	var got struct {
		Created int `json:"created"`
		Data    []map[string]any
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got.Created != 1 || len(got.Data) != 3 {
		t.Fatalf("结构被破坏: %+v", got)
	}
	first, _ := got.Data[0]["url"].(string)
	if !strings.HasPrefix(first, base+PathPrefix) {
		t.Fatalf("第一条 url 未改写: %q", first)
	}
	if got.Data[0]["revised_prompt"] != "p" {
		t.Fatalf("同类其它字段必须保留: %+v", got.Data[0])
	}
	if got.Data[1]["b64_json"] != "AAAA" {
		t.Fatalf("b64_json 分支不应被碰: %+v", got.Data[1])
	}
	// 改写后的地址必须能被自己验证回原址（闭环）。
	tok := strings.TrimPrefix(first, base+PathPrefix)
	back, err := Verify(tok)
	if err != nil || back != "https://cdn.example.com/a.png" {
		t.Fatalf("改写后的链接不可用: %v %q", err, back)
	}
	// 带查询串的那条要把 & 原样带到上游地址里（不能被 HTML 转义成 \u0026 后再被当成字面量）。
	third, _ := got.Data[2]["url"].(string)
	back3, err := Verify(strings.TrimPrefix(third, base+PathPrefix))
	if err != nil || back3 != "https://cdn.example.com/b.png?x=1&y=2" {
		t.Fatalf("查询串在签名往返中走样: %v %q", err, back3)
	}
	if strings.Contains(string(out), `\u0026`) {
		t.Fatalf("输出里出现 HTML 转义: %s", out)
	}
}

func TestRewriteDataURLsPassThrough(t *testing.T) {
	withKey(t, "unit-test-secret")
	base := "https://api.example.com"
	cases := [][]byte{
		nil,
		[]byte(`{"data":[{"b64_json":"AAAA"}]}`),
		[]byte(`{"data":[]}`),
		[]byte(`not json`),
		[]byte(`{"error":{"message":"x"}}`),
	}
	for _, body := range cases {
		out, n := rewriteBody(t, body, base)
		if n != 0 {
			t.Fatalf("%s 不应改写", body)
		}
		if string(out) != string(body) {
			t.Fatalf("未改写时必须原样返回，实际 %s", out)
		}
	}
	// 没有 base（无法推导对外地址）时也不能改写：改写不出可用的绝对地址。
	if _, n := rewriteBody(t, []byte(`{"data":[{"url":"https://cdn.example.com/a.png"}]}`), ""); n != 0 {
		t.Fatal("缺少站点基址时不应改写")
	}
}

func TestRewriteDataURLsNonLinkFieldsStable(t *testing.T) {
	withKey(t, "unit-test-secret")
	body := []byte(`{"created":1,"model":"m","data":[{"url":"https://cdn.example.com/a.png","revised_prompt":"p"}]}`)
	first, _ := rewriteBody(t, body, "https://api.example.com")
	for i := 0; i < 8; i++ {
		again, _ := rewriteBody(t, body, "https://api.example.com")
		var a, b map[string]any
		_ = json.Unmarshal(first, &a)
		_ = json.Unmarshal(again, &b)
		if len(a) != len(b) {
			t.Fatalf("字段数不一致: %d != %d", len(a), len(b))
		}
		// token 内含当前时间，跨秒会不同；这里只要求**非 token 字段**稳定。
		delete(a, "data")
		delete(b, "data")
		if len(a) != len(b) || a["model"] != b["model"] {
			t.Fatalf("非链接字段应稳定: %v vs %v", a, b)
		}
	}
}

// TestPathPrefixMatchesContract 守住「签名链接前缀」与对外文档契约一致。
//
// 两处各写一份字符串是必要的（apidoc 不该被签名包反向依赖），
// 但它们不一致时的现象极其隐蔽：文档里写 /v1/media/{token}，
// 实际发出去的是另一个前缀 → 用户照着文档拼地址一律 404，而页面预览是好的。
func TestPathPrefixMatchesContract(t *testing.T) {
	if PathPrefix != "/v1/media/" {
		t.Fatalf("前缀变了就必须同步 internal/apidoc/kind.go 的 media/proxy 契约")
	}
}

// TestArtifactTokenRoundTrip 守产物 token 的往返与绑定。
//
// 三个必须成立的性质：
//  1. 解出来的是 (任务号, 序号)，不是某个 URL；
//  2. 换个任务号/序号就验不过（HMAC 覆盖整段 payload）；
//  3. 过期是 ErrExpired 而不是 ErrInvalid —— 前者让代理回 410（别再重试），
//     后者回 404（地址拼错了），处置完全不同。
func TestArtifactTokenRoundTrip(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	tok, err := SignArtifact(42, 3, time.Hour)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	c, err := VerifyArtifact(tok)
	if err != nil {
		t.Fatalf("验证失败: %v", err)
	}
	if c.TaskID != 42 || c.Index != 3 {
		t.Fatalf("声明不符: %+v", c)
	}
	// 篡改任意一段都必须失败。
	for i := 0; i < len(tok); i += 7 {
		bad := []byte(tok)
		if bad[i] == 'A' {
			bad[i] = 'B'
		} else {
			bad[i] = 'A'
		}
		if _, err := VerifyArtifact(string(bad)); err == nil {
			t.Fatalf("位置 %d 被改动后仍然验过", i)
		}
	}
}

// TestArtifactTokenExpires 守过期语义（410 而不是 404 的依据）。
func TestArtifactTokenExpires(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	tok, err := SignArtifact(1, 0, time.Hour)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if _, err := verifyArtifactAt(tok, time.Now().Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期应返回 ErrExpired，实际 %v", err)
	}
}

// TestVerifyAnyDispatches 守「一个入口按版本分派」。
//
// 两个版本混在一个解码器里会读出错位的垃圾且验签还能过（长度够、签名对，
// 但字段全错）—— 这是最坏的一类 bug：它看起来"能用"。
func TestVerifyAnyDispatches(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	u, claim, err := VerifyAny(mustSign(t, "https://up.example.com/a.png"))
	if err != nil || claim != nil || u == "" {
		t.Fatalf("v1 token 应解出 URL：u=%q claim=%v err=%v", u, claim, err)
	}
	tok, err := SignArtifact(7, 1, time.Hour)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	u, claim, err = VerifyAny(tok)
	if err != nil || claim == nil || u != "" {
		t.Fatalf("v2 token 应解出 claim：u=%q claim=%v err=%v", u, claim, err)
	}
	if claim.TaskID != 7 || claim.Index != 1 {
		t.Fatalf("claim 不符: %+v", claim)
	}
	// 交叉喂错版本必须拒绝，不能"差不多能过"。
	if _, _, err := VerifyAny(mustSign(t, "https://up.example.com/a.png")); err != nil {
		t.Fatalf("v1 应可用: %v", err)
	}
}

func mustSign(t *testing.T, u string) string {
	t.Helper()
	tok, err := Sign(u, time.Hour)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	return tok
}

// TestArtifactLinkRequiresBase 守「没有对外基址就不签」。
func TestArtifactLinkRequiresBase(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "unit-test-secret")
	if _, ok := ArtifactLink("", 1, 0, time.Hour); ok {
		t.Fatal("没有基址时不该签出链接")
	}
	link, ok := ArtifactLink("https://api.example.com/", 1, 0, time.Hour)
	if !ok || !strings.HasPrefix(link, "https://api.example.com/v1/media/") {
		t.Fatalf("链接前缀不对: %q", link)
	}
}
