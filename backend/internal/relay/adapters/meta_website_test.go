package adapters

import (
	"net/url"
	"testing"
)

// TestBuiltinWebsiteMeta 守「每个内置适配器的默认官网」。
//
// 规则：
//   - 除 openai 外，每个内置适配器都必须有默认官网（内建渠道的跳转按钮不能是空的）；
//   - openai 必须为空：它是协议适配器，上游可能是 OpenAI / DeepSeek / 任意兼容服务，
//     填一个固定官网会把别的上游跳到错误站点。具体化由渠道级 website 或 host 规则负责。
func TestBuiltinWebsiteMeta(t *testing.T) {
	for id, m := range Metas() {
		if id == "openai" {
			if m.Website != "" {
				t.Fatalf("openai 的默认官网必须为空（协议适配器），实际 %q", m.Website)
			}
			continue
		}
		if m.Website == "" {
			t.Fatalf("%s 缺少默认官网", id)
		}
		u, err := url.Parse(m.Website)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			t.Fatalf("%s 的官网不是合法 http(s) 地址：%q", id, m.Website)
		}
		if got := WebsiteOf(id); got != m.Website {
			t.Fatalf("WebsiteOf(%q) = %q，与 Meta.Website 不一致", id, got)
		}
	}
}
