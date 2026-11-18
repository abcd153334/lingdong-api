package apidoc

import (
	"net/url"
	"regexp"
	"strings"
)

// 对外文本的守卫：禁止出现「内部视角」的词与第三方域名。
//
// 为什么这是一条硬门禁而不是文案建议：文档是对外页面，也是被 AI 编程工具整篇读走的
// 产物。里面出现「渠道」「上游」「适配器」，等于把平台的内部结构暴露给用户，
// 而且用户会照着这些词去理解——他们本来只需要知道「调哪个接口、填哪个字段」。
//
// 两种检查：
//  1. 禁词：写了就说明视角错了，不是措辞问题。
//  2. 域名白名单：示例里的 URL 必须是占位域名（example.com）或平台自己的域名。
//     任何第三方域名都会泄漏「这个模型实际由谁提供」——这正是本轮需求
//     「用户端不暴露渠道」的延续。白名单是 fail-closed 的：新增域名要显式加。

// forbiddenWords 是内部视角用词。注意「聚合」也在内：
// 「聚合平台」这个说法会引出「聚合了谁」的追问，直接说「统一接口」更清楚。
var forbiddenWords = []string{
	"渠道", "上游", "适配器", "换渠道", "聚合", "转发给", "厂商真名",
}

// forwarderWords 是「转发平台」的名字（既不是模型厂商，也不是本平台）。
//
// 与 forbiddenWords 分开的理由：那批是**视角错**（说了一次就说明作者在描述内部结构），
// 这批是**信息泄漏**——写上去等于告诉用户"这个模型其实是经谁转的"。
// 两者修正方式不同（改视角 vs 删掉名字），合成一张表后提示语只能写一种，
// 运营照着改也改不对。
//
// 模型厂商品牌不在这里：OpenAI / Gemini / 通义千问 / 即梦 / 可灵 / Sora / SUNO
// 都是用户可以知道的（他们要按厂商选模型），禁掉反而说不清字段归属。
var forwarderWords = []string{"火山", "方舟", "多米", "秘塔"}

// forwarderASCII 是同一批名字里用英文写的那些，必须按词边界匹配。
//
// 子串匹配会把 Markdown、dark、parking 判成违规 —— 一条天天误报的门禁，
// 一周之内就会被注释掉。
var forwarderASCII = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bark\b`),
	regexp.MustCompile(`(?i)\bmetaso\b`),
	regexp.MustCompile(`(?i)\bautodl\b`),
	regexp.MustCompile(`(?i)\bnew-?api\b`),
}

var urlPattern = regexp.MustCompile(`https?://[^\s"'<>\)\]]+`)

// Violation 是一条守卫命中记录。
type Violation struct {
	// Kind 是命中类型：forbidden_word（内部视角用词）/ forwarder_name（转发平台名）/
	// external_host（第三方域名）。
	Kind string `json:"kind"`
	// Text 是命中的原文片段。
	Text string `json:"text"`
	// Hint 是给人看的修正建议。
	Hint string `json:"hint"`
}

// CheckText 检查一段对外文本。
//
// allowHosts 是允许出现的主机名（含子域，如 "lingdong.api" 会放行 "docs.lingdong.api"）。
// 调用方必须把平台自己的域名与占位域名传进来；不传就等于禁止一切 URL。
func CheckText(s string, allowHosts ...string) []Violation {
	// 必须是非 nil 切片：这个返回值会直接进管理台的 JSON（`guard` 字段）。
	// `var out []Violation` 在没有违规时序列化成 `null`，前端 `data.guard.length`
	// 抛 TypeError，ErrorBoundary 把整个文档页换成 "Something went wrong!" ——
	// 而 HTTP 是 200，接口层完全看不出问题。而「没有违规」恰恰是最常见的状态。
	out := make([]Violation, 0, 4)
	for _, w := range forbiddenWords {
		if strings.Contains(s, w) {
			out = append(out, Violation{
				Kind: "forbidden_word", Text: w,
				Hint: "换个说法：把「" + w + "」改成「平台」或直接描述行为，不要描述内部结构。",
			})
		}
	}
	for _, w := range forwarderWords {
		if strings.Contains(s, w) {
			out = append(out, Violation{
				Kind: "forwarder_name", Text: w,
				Hint: "删掉这个名字：只写模型厂商（如 Seedream / 可灵 / SUNO）或不写来源，读者不需要知道中间经过谁。",
			})
		}
	}
	for _, re := range forwarderASCII {
		if m := re.FindString(s); m != "" {
			out = append(out, Violation{
				Kind: "forwarder_name", Text: m,
				Hint: "删掉这个名字：只写模型厂商或不写来源，读者不需要知道中间经过谁。",
			})
		}
	}
	for _, raw := range urlPattern.FindAllString(s, -1) { // 去掉尾部标点，否则 "https://a.com/。" 这种会把句号算进主机名。
		raw = strings.TrimRight(raw, ".,;:。，；：")
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		if !hostAllowed(u.Hostname(), allowHosts) {
			out = append(out, Violation{
				Kind: "external_host", Text: raw,
				Hint: "示例地址只能用占位域名（如 https://cdn.example.com）或平台自己的域名。",
			})
		}
	}
	return out
}

// hostAllowed 判断主机是否在白名单里（按后缀匹配，避免 evil-example.com 命中 example.com）。
func hostAllowed(host string, allow []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// PlaceholderHosts 是允许在示例里出现的占位域名。
//
// 用 example.com 而不是随便编个 cdn.xxx：example.com / example.org 是 RFC 2606
// 保留给文档用的，永远不会指向真实服务，用户也不会误以为是可用的地址。
var PlaceholderHosts = []string{"example.com", "example.org", "example.net"}
