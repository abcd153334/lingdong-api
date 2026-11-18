package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/service/session"
)

// 本文件是 CSRF 的**行为契约**：凭证从 Bearer 改成 Cookie 之后，
// 「不带令牌也能改数据」的每一条路径都是一个可被第三方页面触发的资金操作。
// 这些用例不需要 DB / Redis。

func newCSRFRouter(withCookie bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CSRF())
	r.POST("/api/portal/keys", func(c *gin.Context) {
		if withCookie {
			// 模拟会话中签发的双提交令牌
			c.SetSameSite(http.SameSiteLaxMode)
			c.SetCookie(session.CSRFCookieName, "csrf-abc", 3600, "/", "", false, false)
		}
		c.Status(http.StatusOK)
	})
	return r
}

func run(r *gin.Engine, method, target string, headers map[string]string, cookies []*http.Cookie) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	r.ServeHTTP(w, req)
	return w.Code
}

// TestCSRFRejectsMissingOrigin 断言「非浏览器请求」在写方法上一律被拒。
//
// fail-closed 是刻意的：curl 不带 Origin 也拿不到 Cookie 里的令牌，
// 所以这条规则不会误伤真实调用方（脚本用 API KEY 走 /v1，那里不挂 CSRF）。
func TestCSRFRejectsMissingOrigin(t *testing.T) {
	r := newCSRFRouter(true)
	ck := []*http.Cookie{{Name: session.CSRFCookieName, Value: "csrf-abc"}}
	code := run(r, http.MethodPost, "/api/portal/keys",
		map[string]string{session.CSRFHeaderName: "csrf-abc"}, ck)
	if code != http.StatusForbidden {
		t.Fatalf("缺少 Origin/Referer 必须 403，实际 %d", code)
	}
}

func TestCSRFRejectsCrossOrigin(t *testing.T) {
	r := newCSRFRouter(true)
	ck := []*http.Cookie{{Name: session.CSRFCookieName, Value: "csrf-abc"}}
	code := run(r, http.MethodPost, "/api/portal/keys",
		map[string]string{
			"Origin":               "https://evil.example",
			session.CSRFHeaderName: "csrf-abc",
		}, ck)
	if code != http.StatusForbidden {
		t.Fatalf("跨站来源必须 403，实际 %d", code)
	}
}

// TestCSRFRequiresMatchingToken 断言双提交：同源但令牌不匹配也要拒。
//
// 这条防的是「同站子域」：攻击者控制的 a.example.com 可以向 api.example.com
// 发同站请求（SameSite 不拦），Origin 看起来也合法，唯一挡住它的是读不到令牌。
func TestCSRFRequiresMatchingToken(t *testing.T) {
	r := newCSRFRouter(true)
	origin := map[string]string{"Origin": "https://example.com"}

	if code := run(r, http.MethodPost, "https://example.com/api/portal/keys", origin, nil); code != http.StatusForbidden {
		t.Fatalf("没有 CSRF Cookie 必须 403，实际 %d", code)
	}

	ck := []*http.Cookie{{Name: session.CSRFCookieName, Value: "csrf-abc"}}
	withBad := map[string]string{"Origin": "https://example.com", session.CSRFHeaderName: "wrong"}
	if code := run(r, http.MethodPost, "https://example.com/api/portal/keys", withBad, ck); code != http.StatusForbidden {
		t.Fatalf("令牌不匹配必须 403，实际 %d", code)
	}

	ok := map[string]string{"Origin": "https://example.com", session.CSRFHeaderName: "csrf-abc"}
	if code := run(r, http.MethodPost, "https://example.com/api/portal/keys", ok, ck); code != http.StatusOK {
		t.Fatalf("同源 + 令牌匹配应放行，实际 %d", code)
	}
}

// TestCSRFSkipsSafeMethods 断言只读方法不受影响（否则所有页面都打不开）。
func TestCSRFSkipsSafeMethods(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CSRF())
	r.GET("/api/portal/keys", func(c *gin.Context) { c.Status(http.StatusOK) })
	if code := run(r, http.MethodGet, "/api/portal/keys", nil, nil); code != http.StatusOK {
		t.Fatalf("GET 不应被 CSRF 拦截，实际 %d", code)
	}
}

// TestCSRFPublicOnlyChecksOrigin 断言公开写端点（登录/注册/找回）在校验来源后放行，
// 且**不**要求令牌（这些端点还没有会话）。
func TestCSRFPublicOnlyChecksOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CSRFPublic())
	r.POST("/api/portal/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	if code := run(r, http.MethodPost, "https://example.com/api/portal/auth/login", nil, nil); code != http.StatusForbidden {
		t.Fatalf("公开写端点也必须校验来源，实际 %d", code)
	}
	if code := run(r, http.MethodPost, "https://example.com/api/portal/auth/login",
		map[string]string{"Origin": "https://evil.example"}, nil); code != http.StatusForbidden {
		t.Fatalf("跨站来源必须 403，实际 %d", code)
	}
	if code := run(r, http.MethodPost, "https://example.com/api/portal/auth/login",
		map[string]string{"Origin": "https://example.com"}, nil); code != http.StatusOK {
		t.Fatalf("同源应放行（无需令牌），实际 %d", code)
	}
	// Referer 兜底：老浏览器/某些客户端只带 Referer
	if code := run(r, http.MethodPost, "https://example.com/api/portal/auth/login",
		map[string]string{"Referer": "https://example.com/login"}, nil); code != http.StatusOK {
		t.Fatalf("Referer 同源应放行，实际 %d", code)
	}
	// 纯 http 部署（无 TLS）只要代理如实回传 X-Forwarded-Proto 就仍按同源放行：
	// 不认这个头时请求协议被当成 https，`http://` 来源会与 443 对不上而 403。
	if code := run(r, http.MethodPost, "http://example.com/api/portal/auth/login",
		map[string]string{"Origin": "http://example.com", "X-Forwarded-Proto": "http"}, nil); code != http.StatusOK {
		t.Fatalf("http 部署带 XFP 时应放行，实际 %d", code)
	}
}

// TestOriginFromReferer 断言 Referer 解析只取 scheme+host（路径不能当来源）。
func TestOriginFromReferer(t *testing.T) {
	cases := map[string]string{
		"http://a.example/login?x=1": "http://a.example",
		"https://a.example/x/y":      "https://a.example",
		"":                           "",
		"not a url":                  "",
		"/relative/path":             "",
	}
	for in, want := range cases {
		if got := originFromReferer(in); got != want {
			t.Errorf("originFromReferer(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestCSRFRejectsSameHostDifferentPort 是本轮收紧的**核心契约**。
//
// 历史上这里只比主机名（理由是「SameSite 的站不含端口」），于是
// `http://app.example:8080` 上任何一个服务都能带着 `app.example` 的会话
// 去 POST `http://app.example`：Cookie 不区分端口，Lax 也拦不住跨端口。
// 现在按完整 origin（scheme + host + port）比对，跨端口一律拒。
func TestCSRFRejectsSameHostDifferentPort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CSRFPublic())
	r.POST("/api/portal/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		name   string
		origin string
		want   int
	}{
		// 反代在 TLS 之后（XFP=https），浏览器侧拿到的 Host 不带端口
		{"同源隐式端口", "https://app.example", http.StatusOK},
		{"同源显式端口", "https://app.example:443", http.StatusOK},
		{"同主机换端口", "https://app.example:8443", http.StatusForbidden},
		{"同主机降级到 http", "http://app.example", http.StatusForbidden},
		{"子域不是同源", "https://evil.app.example", http.StatusForbidden},
		{"端口不能救跨站", "https://evil.example:443", http.StatusForbidden},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://app.example/api/portal/auth/login", nil)
		req.Host = "app.example"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", tc.origin)
		r.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s：Origin=%s 期望 %d，实际 %d", tc.name, tc.origin, tc.want, w.Code)
		}
	}
}

// TestCSRFAllowsLoopbackCrossPortInDev 是本轮收紧的**兼容契约**。
//
// 本地是「页面 :5178 / 接口 :8080」两个 origin，完整比对必然判跨站；
// 开发态靠 devLikelyOrigin 放宽到回环/私有地址，`pnpm dev` 才能直接用。
// 生产不走这条：生产要么同源反代，要么在 LD_ALLOWED_ORIGINS 里登记。
func TestCSRFAllowsLoopbackCrossPortInDev(t *testing.T) {
	if !devLike() {
		t.Skip("仅开发态放宽到回环地址")
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CSRFPublic())
	r.POST("/api/portal/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/portal/auth/login", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://127.0.0.1:5178")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("开发态回环跨端口应放行，实际 %d", w.Code)
	}
}

// TestCSRFTrustsSecFetchSite 断言浏览器自填的 Sec-Fetch-Site 优先于 Origin。
//
// 现实场景：反向代理剥掉了 Origin（部分网关默认不开头透传），
// 但 Sec-Fetch-Site 是浏览器加的、脚本无法伪造，用它判定既安全又兼容。
func TestCSRFTrustsSecFetchSite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CSRFPublic())
	r.POST("/api/portal/auth/login", func(c *gin.Context) { c.Status(http.StatusOK) })

	post := func(h map[string]string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://app.example/api/portal/auth/login", nil)
		req.Host = "app.example"
		for k, v := range h {
			req.Header.Set(k, v)
		}
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := post(map[string]string{"Sec-Fetch-Site": "same-origin"}); code != http.StatusOK {
		t.Errorf("same-origin 应放行（即使没有 Origin），实际 %d", code)
	}
	if code := post(map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("cross-site 必须拒绝，实际 %d", code)
	}
	// same-site 不能单独作数：兄弟子域属 same-site 但仍是跨站，
	// 公开写端点没有双提交令牌兜底，必须继续走 Origin 比对。
	if code := post(map[string]string{
		"Sec-Fetch-Site": "same-site",
		"Origin":         "https://evil.app.example",
	}); code != http.StatusForbidden {
		t.Errorf("same-site + 跨子域 Origin 必须拒绝，实际 %d", code)
	}
}
