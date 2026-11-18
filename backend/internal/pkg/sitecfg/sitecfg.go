// Package sitecfg 是 system_config.site 的**唯一**模型、归一与字段校验点。
//
// 为什么要有这个包：`site` 是一个整棵 JSONB blob，此前有 7 处各自声明
// `struct{ APIBaseURL string \`json:"api_base_url"\` }` 再做 json.Unmarshal。
// 改一个字段（例如把单地址改成数组）漏掉任一处，`Unmarshal` 只会**静默失败**，
// 对外地址随即回落 `Request.Host`（反代下恒为 127.0.0.1）——接口 200、
// URL 语法合法，没有任何断言能发现。归一到一处之后，"漏一处"不再可能。
//
// 与本包的分工（不要互相越界）：
//   - sitecfg：解析、归一（去尾斜杠/剥 /v1/去重/限长）、字段级校验；
//   - pkg/siteurl：对外地址推导（配置 → X-Forwarded-Host → Request.Host）；
//   - service/systemconfig：跨键校验（例如首条必须 https 才能开微信直连）。
package sitecfg

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"lingdong-api/internal/conf"
)

// 上限集中在这里：新增一处硬编码的 "8 条" / "100KB" 就是第二个真相源。
const (
	// MaxBases 是多地址条数上限。多到需要十条的场景不存在，
	// 而每多一条都要进 XFH 白名单与门户展示，放开只会让配置页变噪音。
	MaxBases = 8
	// MaxPrimaryLen 是**首条**地址的长度上限：支付回调 = base + "/api/pay/" + 渠道ID + "/notify"，
	// 而网关侧硬性要求回调 ≤128 字符（见 service/order 的 payURLs）。
	// "/api/pay/"(9) + 渠道ID(≤32) + "/notify"(7) = 48 ⇒ base ≤ 80。
	MaxPrimaryLen = 80
	// MaxFaviconBytes 是站点图标**编码后**的字节上限（dataURI 比原文件大约 1/3）。
	// 它被 /api/portal/config 下发给每一个访客（staleTime 30s），不是本地资源。
	MaxFaviconBytes = 140_000
	// MaxLogoBytes 沿用既有上限（3MB）。
	MaxLogoBytes = 3_000_000
)

// Site 是 system_config.site 的完整形状。
//
// 只放**会落库**的字段。派生字段（currency_symbol）不在其中：它由币种算出，
// 写回库里等于给一个已删除的键续命（切到 USD 后库里还留着 ¥）。
type Site struct {
	SiteName     string   `json:"site_name"`
	LogoImage    string   `json:"logo_image"`
	FaviconImage string   `json:"favicon_image"`
	SiteURL      string   `json:"site_url"`
	APIBaseURLs  []string `json:"api_base_urls"`
	ContactEmail string   `json:"contact_email"`
	FooterText   string   `json:"footer_text"`
	ICPBeian     string   `json:"icp_beian"`
	Currency     string   `json:"currency"`
}

// Load 读取并解析当前站点配置（缺键/脏数据返回零值，不 panic）。
//
// 读边界统一做一次 canonical：库里若留着带尾斜杠/空行的历史值，
// 下游 `base + "/f/..."` 就会拼出 `//f/`，而这类双斜杠在浏览器与网关那里
// 行为并不一致（有的规范化、有的 404）。一处收口胜过每个调用点各自 Trim。
func Load() Site {
	var s Site
	_ = conf.System().Unmarshal("site", &s)
	return canonical(s)
}

// canonical 是「读/写两侧共用」的最小规范化：去空白、去尾斜杠、丢弃空行。
//
// 不做合法性判断（那属于 Normalize + 校验），只保证形状一致。
func canonical(in Site) Site {
	out := in
	out.SiteName = strings.TrimSpace(in.SiteName)
	out.ContactEmail = strings.TrimSpace(in.ContactEmail)
	out.ICPBeian = strings.TrimSpace(in.ICPBeian)
	// 页脚允许换行与首尾空格（它是展示文案，不是标识符），只去整体两端空白
	out.FooterText = strings.TrimSpace(in.FooterText)
	out.SiteURL = strings.TrimRight(strings.TrimSpace(in.SiteURL), "/")

	bases := make([]string, 0, len(in.APIBaseURLs))
	for _, raw := range in.APIBaseURLs {
		if v := strings.TrimRight(strings.TrimSpace(raw), "/"); v != "" {
			bases = append(bases, v)
		}
	}
	out.APIBaseURLs = bases
	return out
}

// PrimaryBase 返回首条对外基址（未配置时为空串）。
//
// "第一条为主"是硬约定：产物直链、支付回调、文档示例、OpenAPI 都取它。
func (s Site) PrimaryBase() string {
	if len(s.APIBaseURLs) == 0 {
		return ""
	}
	return s.APIBaseURLs[0]
}

// Hosts 返回「被认可的本站域名」集合：全部 API 地址的 host ∪ 站点地址的 host。
//
// 用途只有一个：判断 `X-Forwarded-Host` 是否值得采信。只认 site_url 时，
// 「入口域名 ≠ 站点域名」会让 XFH 恒被拒 → 对外地址回落 127.0.0.1。
func (s Site) Hosts() []string {
	out := make([]string, 0, len(s.APIBaseURLs)+1)
	seen := make(map[string]struct{}, len(s.APIBaseURLs)+1)
	add := func(raw string) {
		u, err := parseHTTP(raw)
		if err != nil {
			return
		}
		h := strings.ToLower(u.Hostname())
		if h == "" {
			return
		}
		if _, dup := seen[h]; dup {
			return
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	for _, b := range s.APIBaseURLs {
		add(b)
	}
	add(s.SiteURL)
	return out
}

// MatchBase 按「本次请求的入口」在已配置的 API 地址里选一条。
//
// 顺序：① host+port 精确 → ② host + scheme → ③ 仅 host，都未命中返回空串。
//
// 返回值是**配置里的原串**，不是请求头里的字符串 —— 调用方因此不可能被
// `Host` / `X-Forwarded-Host` 投毒；scheme 与端口也来自配置，不会因反代头缺失而降级成 http。
//
// 为什么需要它：多线路部署时每条线路一个域名，响应里的 icon_url / 产物直链 /
// 文档示例应当指向用户实际访问的那条，而不是永远指向第一条。
func (s Site) MatchBase(raw, scheme string) string {
	host, port := splitEntry(raw, scheme)
	if host == "" {
		return ""
	}
	for _, base := range s.APIBaseURLs {
		_, bh, bp := parseBase(base)
		if bh == host && bp == port {
			return base
		}
	}
	for _, base := range s.APIBaseURLs {
		bs, bh, _ := parseBase(base)
		if bh == host && bs == scheme {
			return base
		}
	}
	for _, base := range s.APIBaseURLs {
		_, bh, _ := parseBase(base)
		if bh == host {
			return base
		}
	}
	return ""
}

// splitEntry 把 host[:port] 归一成 (hostname, port)；没有显式端口时按 scheme 补默认端口。
func splitEntry(raw, scheme string) (host, port string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	host = raw
	if h, p, err := net.SplitHostPort(raw); err == nil {
		host, port = h, p
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if port == "" {
		port = defaultPort(scheme)
	}
	return host, port
}

// parseBase 拆解一条已通过 Normalize 的地址；host 为空表示这条不可用。
func parseBase(base string) (scheme, host, port string) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return "", "", ""
	}
	scheme = strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Hostname())
	port = u.Port()
	if port == "" {
		port = defaultPort(scheme)
	}
	return scheme, host, port
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// Normalize 归一输入并做字段级校验。步骤顺序不能换（先剥 /v1 再判合法性，
// 否则 `https://api.x.com/v1` 会被当成合法地址存下来，门户就显示 `/v1/v1`）。
func Normalize(in Site) (Site, error) {
	out := canonical(in)
	if out.SiteURL != "" {
		if _, err := parseHTTP(out.SiteURL); err != nil {
			return out, fmt.Errorf("站点地址必须是 http(s) 链接")
		}
	}

	bases := make([]string, 0, len(out.APIBaseURLs))
	seen := make(map[string]struct{}, len(out.APIBaseURLs))
	for _, raw := range out.APIBaseURLs {
		v := stripV1Suffix(raw)
		u, err := parseHTTP(v)
		if err != nil {
			return out, fmt.Errorf("API 调用地址「%s」不是合法的 http(s) 链接", raw)
		}
		// 必须是 origin：带路径/参数/锚点/账号信息的地址会在下游拼出
		// `https://x/foo/v1/...` 这种谁都没预期的 URL（多数上游直接 404），
		// 而错误现场看起来只是"接口不通"，很难联想到是地址填多了。
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return out, fmt.Errorf("API 调用地址「%s」只能填到域名（如 https://api.example.com），不能带路径、参数或账号", raw)
		}
		// 去重键是 origin（scheme + host:port）：同一域名大小写不同、尾部斜杠不同
		// 都属于同一条；带不同路径的两条在上一段已经被拒，不存在"看起来不同"的合法重复。
		key := strings.ToLower(u.Scheme + "://" + u.Host)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		bases = append(bases, v)
	}
	if len(bases) > MaxBases {
		return out, fmt.Errorf("API 调用地址最多 %d 条", MaxBases)
	}
	if len(bases) > 0 && len(bases[0]) > MaxPrimaryLen {
		return out, fmt.Errorf("第一条 API 调用地址不能超过 %d 字符（支付回调地址总长上限 128）", MaxPrimaryLen)
	}
	out.APIBaseURLs = bases

	if len(out.FaviconImage) > MaxFaviconBytes {
		return out, fmt.Errorf("站点图标过大（上限 100KB）")
	}
	if len(out.LogoImage) > MaxLogoBytes {
		return out, fmt.Errorf("Logo 图片过大（上限 3MB）")
	}
	return out, nil
}

// Validate 是字段级校验（跨键校验不在这里，它需要读别的配置键）。
func Validate(s Site) error {
	if s.SiteName == "" {
		return fmt.Errorf("站点名称不能为空")
	}
	if !emailPattern.MatchString(s.ContactEmail) {
		return fmt.Errorf("联系邮箱格式不合法")
	}
	return nil
}

// emailPattern 是联系邮箱的形态校验（不做 MX 查询：那会把保存变成一次网络操作）。
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// stripV1Suffix 剥掉「明确写在路径上的 /v1」。
//
// 运营把 OpenAI 兼容地址整串粘进来是最常见的输入（`https://api.x.com/v1`），
// 不剥就会在门户显示 `https://api.x.com/v1/v1`。
// 判据用 URL 的 Path 而不是字符串后缀：否则主机名恰为 `v1` 的地址（`https://v1`）
// 会被错误截成 `https:/`。
func stripV1Suffix(raw string) string {
	v := strings.TrimRight(raw, "/")
	u, err := url.Parse(v)
	if err != nil {
		return v
	}
	if strings.TrimRight(u.Path, "/") != "/v1" {
		return v
	}
	u.Path, u.RawPath = "", ""
	return strings.TrimRight(u.String(), "/")
}

// parseHTTP 只接受 http(s) 且必须带 host。
//
// 不用 `strings.HasPrefix("http://")`：那会把 `http://` 这种没有主机的残缺值
// 当成合法地址存下来，之后 `url.Parse` 在别处返回空 Host，拼出 `https:///f/...`。
func parseHTTP(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("host")
	}
	return u, nil
}
