package siteurl

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 本文件守住「对外地址不能落到内网 Host」这条判据。
//
// 线上 nginx 模板统一 `proxy_set_header Host 127.0.0.1`，所以 Request.Host
// 在任何真实部署里都是内网地址。一旦 Base 只用它，图标 icon_url / 产物直链 /
// 文档示例会全部变成 https://127.0.0.1/...：接口返回的是语法合法的 URL，
// 「字段非空」的断言全绿，只有真人打开页面才发现图全裂。

func ctxWith(host, xfHost, xfProto string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("GET", "http://internal.local/api/x", nil)
	req.Host = host
	if xfHost != "" {
		req.Header.Set("X-Forwarded-Host", xfHost)
	}
	if xfProto != "" {
		req.Header.Set("X-Forwarded-Proto", xfProto)
	}
	c.Request = req
	return c
}

func TestFromRequestPrefersForwardedHost(t *testing.T) {
	setSiteBase(t, "")
	c := ctxWith("127.0.0.1", "api.example.com", "https")
	if got := FromRequest(c); got != "https://api.example.com" {
		t.Fatalf("反代头必须压过内网 Host，实际 %q", got)
	}
}

func TestFromRequestFallsBackToHostWithoutProxy(t *testing.T) {
	setSiteBase(t, "")
	c := ctxWith("api.example.com:8080", "", "")
	if got := FromRequest(c); got != "http://api.example.com:8080" {
		t.Fatalf("直连场景用 Request.Host，实际 %q", got)
	}
}

func TestFromRequestTakesFirstForwardedHost(t *testing.T) {
	setSiteBase(t, "")
	c := ctxWith("127.0.0.1", "a.example.com, b.internal", "https")
	if got := FromRequest(c); got != "https://a.example.com" {
		t.Fatalf("多级代理取最左（原始域名），实际 %q", got)
	}
}

// 配置优先：运营填了正式域名时，连反代头都不该参与。
func TestBaseConfiguredBeatsForwardedHost(t *testing.T) {
	setSiteBase(t, "https://cdn.example.com/")
	c := ctxWith("127.0.0.1", "api.example.com", "https")
	if got := Base(c); got != "https://cdn.example.com" {
		t.Fatalf("配置基址必须优先且去尾斜杠，实际 %q", got)
	}
}

// 没有反代头、Host 又是内网地址时只能返回内网基址 —— 这是唯一的真实信号，
// 不能为了"看起来对"把它猜成公网域名。
func TestFromRequestKeepsInternalHostWhenNothingElse(t *testing.T) {
	setSiteBase(t, "")
	c := ctxWith("127.0.0.1:6000", "", "https")
	if got := FromRequest(c); got != "https://127.0.0.1:6000" {
		t.Fatalf("无可用线索时保留真实 Host，实际 %q", got)
	}
}

// 多地址：按请求入口返回对应那条；入口不在配置里才回落第一条。
func TestBaseFollowsEntryLine(t *testing.T) {
	setSite(t, map[string]any{"api_base_urls": []string{"https://a.example.com", "https://b.example.com"}})

	// X-Forwarded-Host 命中第二条
	c := ctxWith("127.0.0.1", "b.example.com", "https")
	if got := Base(c); got != "https://b.example.com" {
		t.Fatalf("入口命中第二条时应返回它，实际 %q", got)
	}

	// 无 XFH（直连/反代覆盖 Host）：Host 命中同样要选对
	c = ctxWith("b.example.com", "", "https")
	if got := Base(c); got != "https://b.example.com" {
		t.Fatalf("Host 命中第二条时应返回它，实际 %q", got)
	}

	// 未知入口：回落第一条（与旧行为一致，绝不回显攻击者域名）
	c = ctxWith("evil.example.net", "", "https")
	if got := Base(c); got != "https://a.example.com" {
		t.Fatalf("未知入口必须回落第一条，实际 %q", got)
	}
}

// 显式端口必须参与匹配：同一域名不同端口是两条不同的入口。
func TestBaseMatchesExplicitPort(t *testing.T) {
	setSite(t, map[string]any{"api_base_urls": []string{
		"https://a.example.com",
		"https://b.example.com:8443",
	}})
	c := ctxWith("127.0.0.1", "b.example.com:8443", "https")
	if got := Base(c); got != "https://b.example.com:8443" {
		t.Fatalf("带端口的入口必须按 host+port 命中，实际 %q", got)
	}
}

// XFH 白名单必须覆盖**全部**已配置域名。
//
// 只认站点地址时，入口域名与之不同就会被拒 → 对外地址回落 127.0.0.1，
// 而接口返回的仍是一个语法合法的 URL，任何非空断言都发现不了。
func TestForwardedHostAcceptsEveryConfiguredHost(t *testing.T) {
	setSite(t, map[string]any{
		"api_base_urls": []string{"https://api.example.com", "https://api2.example.com:8443"},
		"site_url":      "https://www.example.com",
	})
	c := ctxWith("127.0.0.1", "api2.example.com", "https")
	if got := FromRequest(c); got != "https://api2.example.com" {
		t.Fatalf("第二条地址的域名也必须在白名单内，实际 %q", got)
	}

	// 站点地址（门户域名）同样在集合内
	c = ctxWith("127.0.0.1", "www.example.com", "https")
	if got := FromRequest(c); got != "https://www.example.com" {
		t.Fatalf("站点地址的域名必须在白名单内，实际 %q", got)
	}
}

func TestForwardedHostRejectsUnknownHost(t *testing.T) {
	setSite(t, map[string]any{"api_base_urls": []string{"https://api.example.com"}})
	c := ctxWith("127.0.0.1", "evil.example.net", "https")
	if got := FromRequest(c); got != "https://127.0.0.1" {
		t.Fatalf("伪造的 XFH 必须被忽略并回落 Host，实际 %q", got)
	}
}

// DisplayBases 的三段回落：配置 → 站点地址 → 请求 origin，并各自带来源标记。
func TestDisplayBasesFallbackChain(t *testing.T) {
	c := ctxWith("127.0.0.1:6000", "", "https")

	setSite(t, map[string]any{
		"api_base_urls": []string{"https://api.example.com", "https://api2.example.com"},
		"site_url":      "https://www.example.com",
	})
	if got, src := DisplayBases(c); src != SourceConfig || len(got) != 2 || got[0] != "https://api.example.com" {
		t.Fatalf("有配置时必须原样返回全部地址，实际 %v / %s", got, src)
	}

	setSite(t, map[string]any{"site_url": "https://www.example.com"})
	if got, src := DisplayBases(c); src != SourceSiteURL || len(got) != 1 || got[0] != "https://www.example.com" {
		t.Fatalf("未配置 API 地址时回落站点地址，实际 %v / %s", got, src)
	}

	setSite(t, map[string]any{})
	if got, src := DisplayBases(c); src != SourceRequest || len(got) != 1 || got[0] != "https://127.0.0.1:6000" {
		t.Fatalf("都没配时回落请求 origin，实际 %v / %s", got, src)
	}
}
