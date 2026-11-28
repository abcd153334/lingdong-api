package releasecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"lingdong-api/internal/conf"
	"lingdong-api/internal/pkg/ssrf"
)

// ConfigKey 是清单地址在 system_config 里的键。
//
// 为什么放运营配置而不是环境变量：这是运营要能自己改的东西（换一个发布清单地址
// 不该需要改 .env 并重启）。配置文件里没有它，`deploy/.env.example` 也不必新增变量。
const ConfigKey = "release_manifest_url"

// ManifestURLMaxLen 是清单地址的长度上限（防脏数据把配置撑爆）。
const ManifestURLMaxLen = 512

// 抓取约束。这三个值都是**安全边界**的一部分，不要按"性能"调大：
//   - 体积：清单只该有几百字节，1MB 的清单一定是错的或被投毒的；
//   - 超时：管理台点一次要等，5 秒足够；
//   - 冷却：失败也要计冷却，否则"点一下检查"就能变成对目标站的压力源。
const (
	maxBytes    = 64 << 10
	fetchTimout = 5 * time.Second
	cooldown    = 15 * time.Second
)

// 检查结果的状态码（对外就是一个字符串）。
//
// 关键约定：**空的 ErrorKind 才表示"比较成功"**。任何非空值都意味着
// "不知道有没有新版本"，前端必须显示原因，而不是"已是最新"。
const (
	KindNone              = ""
	KindNotConfigured     = "not_configured"
	KindBlocked           = "blocked" // 非 https / 私网 / DNS 指向内网
	KindTimeout           = "timeout"
	KindNetwork           = "network"
	KindHTTPStatus        = "http_status"
	KindTooLarge          = "too_large"
	KindBadJSON           = "bad_json"
	KindUnsupportedSchema = "unsupported_schema"
	KindInvalidVersion    = "invalid_version"
	KindInvalidField      = "invalid_field"
)

// Release 是清单里的一个版本条目（最小字段集）。
type Release struct {
	Version     string `json:"version"`
	Tag         string `json:"tag"`
	PublishedAt string `json:"published_at"`
	Notes       string `json:"notes"`
	Commit      string `json:"commit"`
	URL         string `json:"url"`
}

// Status 是一次检查的结果。
type Status struct {
	// Configured 为 false 表示根本没配清单地址（不是"检查失败"）。
	Configured bool
	CheckedAt  time.Time
	ErrorKind  string
	// HTTPStatus 只在 ErrorKind == http_status 时有意义。
	HTTPStatus int
	Latest     *Release
}

// OK 报告这次检查是否拿到了可比较的结果。
func (s Status) OK() bool { return s.Configured && s.ErrorKind == KindNone && s.Latest != nil }

// Age 返回这份结果距今多久。
//
// 缓存本身**不会过期**：管理台 GET 必须永远有东西可显示，过期就把界面变成
// "不知道"是更糟的体验。但管理台要能看出"这个结论是 3 分钟前的还是 5 小时前的" ——
// 否则一次没跑成的后台检查会让页面无限期显示旧版本，而管理员无从判断。
func (s Status) Age() time.Duration {
	if s.CheckedAt.IsZero() {
		return 0
	}
	return time.Since(s.CheckedAt)
}

// Service 是版本检查的唯一实例（进程内缓存）。
//
// 为什么缓存只有进程内：清单是"提示性"信息，两个副本在 15 秒窗口里可能看到
// 不同结果 —— 对"要不要更新"这个决策没有任何实际影响，而引入分布式缓存要
// 多加一次 Redis 往返和一个失效通道。这里刻意选择更简单的那一侧。
type Service struct {
	mu          sync.Mutex
	cachedURL   string
	st          Status
	lastAttempt time.Time

	// fetchMu 把并发抓取串行化（同一时刻只有一次出站）。
	fetchMu sync.Mutex

	client *http.Client
	// validate 默认是 ssrf.ValidatePublicURLResolved；测试注入空实现，
	// 否则任何本地 httptest 服务都会被"私网地址"规则拒掉，解析逻辑就没法测。
	validate func(context.Context, string) error
}

// New 构造服务。client 由 ssrf.SafeClient 提供（禁重定向 + 建连期 IP 校验）。
func New() *Service {
	return &Service{
		client:   ssrf.SafeClient(fetchTimout),
		validate: ssrf.ValidatePublicURLResolved,
	}
}

// DefaultManifestURL 是出厂清单地址（官方发布清单，由 .github/workflows/docker.yml
// 在每次推 v* tag 时更新到 release-manifest 分支）。
//
// 为什么需要默认值：配置项出厂是空串。如果"空 = 不检查"，那么每个克隆部署的实例
// 都必须先去管理台填一个地址才可能收到更新通知 —— 也就是"自动获取更新"出厂是关的，
// 而绝大多数自部署用户不会想到去填。默认指向官方清单；fork 后想跟自己的发布，
// 在管理台覆盖这个值即可，填 off 则彻底关闭。
//
// 必须是不重定向的直出地址：抓取用 netguard.DenyRedirect，而
// github.com/.../releases/latest/download/... 是 302，会被判成 http_status。
const DefaultManifestURL = "https://raw.githubusercontent.com/abcd153334/lingdong-api/release-manifest/release.json"

// OffValue 是"显式关闭检查"的取值。空串表示"用出厂默认"，两者必须能区分 ——
// 否则管理员没有任何办法关掉这个出站请求。
const OffValue = "off"

// Shared 返回进程内唯一实例（管理台读路径与后台定时任务共用同一份）。
//
// 唯一性是**契约**：冷却（15s）与结果缓存都挂在实例上。各调用方自己 new 的话，
// 冷却与缓存同时失效，"点一下检查"就变成"对第三方站点发一次请求"；
// 后台任务抓到的结果也没法被管理台看到（两个缓存互不相干）。
func Shared() *Service { return shared }

var shared = New()

// ManifestURL 返回生效的清单地址（未配置走出厂默认，off 返回空串）。
func ManifestURL() string {
	v := strings.TrimSpace(conf.System().String(ConfigKey, ""))
	switch v {
	case "":
		return DefaultManifestURL
	case OffValue:
		return ""
	}
	return v
}

// Status 只读缓存，**绝不发起网络请求**。
//
// GET 接口必须走它：管理台每 5 分钟拉一次状态，若 GET 会触发抓取，
// 就等于让"打开页面"变成对第三方站点的周期性请求。
func (s *Service) Status() Status {
	url := ManifestURL()
	if url == "" {
		return Status{Configured: false, ErrorKind: KindNotConfigured}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 地址换了 ⇒ 旧结果对新地址没有意义。返回"已配置但未检查"。
	if s.cachedURL != url {
		return Status{Configured: true, ErrorKind: KindNone}
	}
	st := s.st
	st.Configured = true
	return st
}

// Check 抓一次清单。allowCached=false 时忽略"结果新鲜"这一条（但仍遵守冷却）。
func (s *Service) Check(ctx context.Context, allowCached bool) Status {
	url := ManifestURL()
	if url == "" {
		return Status{Configured: false, ErrorKind: KindNotConfigured}
	}

	s.mu.Lock()
	if s.cachedURL == url && allowCached && time.Since(s.lastAttempt) < cooldown {
		st := s.st
		s.mu.Unlock()
		st.Configured = true
		return st
	}
	s.mu.Unlock()

	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	// 排队期间别人已经抓过：直接复用，避免"连点 5 次 = 5 次出站"。
	s.mu.Lock()
	if s.cachedURL == url && time.Since(s.lastAttempt) < cooldown {
		st := s.st
		s.mu.Unlock()
		st.Configured = true
		return st
	}
	s.mu.Unlock()

	st := s.fetch(ctx, url)

	s.mu.Lock()
	s.cachedURL = url
	s.st = st
	// 失败也刷新 lastAttempt：冷却的目的之一是"别把目标站点打成靶子"。
	s.lastAttempt = time.Now()
	s.mu.Unlock()

	st.Configured = true
	return st
}

// fetch 是唯一出站点。所有失败都收敛成 ErrorKind，不上抛 ——
// 调用方（管理台）需要的是"为什么不知道"，不是一个 500。
func (s *Service) fetch(ctx context.Context, url string) Status {
	st := Status{CheckedAt: time.Now().UTC()}

	// https-only：清单决定"要不要把线上换成另一个版本"，明文链路可被中间人改写。
	// http 一律拒，即使目标在公网。
	if !strings.HasPrefix(url, "https://") {
		st.ErrorKind = KindBlocked
		return st
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimout)
	defer cancel()
	if s.validate != nil {
		if err := s.validate(fctx, url); err != nil {
			st.ErrorKind = KindBlocked
			return st
		}
	}

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, url, nil)
	if err != nil {
		st.ErrorKind = KindBlocked
		return st
	}
	// 固定 UA：便于对方站点在日志里认出这类请求（出问题时能对账）。
	req.Header.Set("User-Agent", "lingdong-api/release-check")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		st.ErrorKind = classifyErr(err)
		return st
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		st.ErrorKind = KindHTTPStatus
		st.HTTPStatus = resp.StatusCode
		return st
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		st.ErrorKind = classifyErr(err)
		return st
	}
	if len(body) > maxBytes {
		st.ErrorKind = KindTooLarge
		return st
	}

	rel, kind := parseManifest(body)
	if kind != KindNone {
		st.ErrorKind = kind
		return st
	}
	st.Latest = rel
	return st
}

// parseManifest 解析并校验清单。返回的 ErrorKind 是**唯一**的拒绝原因。
func parseManifest(body []byte) (*Release, string) {
	var m struct {
		Schema int      `json:"schema"`
		Latest *Release `json:"latest"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&m); err != nil {
		return nil, KindBadJSON
	}
	if m.Schema != 1 {
		return nil, KindUnsupportedSchema
	}
	if m.Latest == nil {
		return nil, KindBadJSON
	}
	rel := *m.Latest
	rel.Version = strings.TrimSpace(rel.Version)
	rel.Tag = strings.TrimSpace(rel.Tag)
	rel.Commit = strings.TrimSpace(rel.Commit)
	rel.URL = strings.TrimSpace(rel.URL)
	rel.Notes = strings.TrimSpace(rel.Notes)
	rel.PublishedAt = strings.TrimSpace(rel.PublishedAt)

	if !ValidVersion(rel.Version) {
		return nil, KindInvalidVersion
	}
	// tag 缺省等于 version（绝大多数发布就是这样）。
	if rel.Tag == "" {
		rel.Tag = rel.Version
	}
	// tag 会被拼进 git 命令，形状必须与版本号一致 —— 这是命令注入的第一道闸。
	if !ValidTag(rel.Tag) {
		return nil, KindInvalidVersion
	}
	if rel.Commit != "" && !commitRe.MatchString(rel.Commit) {
		return nil, KindInvalidField
	}
	// 发布说明里的链接会渲染成可点的 <a>：只允许 https，避免 javascript: 之类。
	if rel.URL != "" && !strings.HasPrefix(rel.URL, "https://") {
		return nil, KindInvalidField
	}
	if len(rel.Notes) > 500 {
		rel.Notes = rel.Notes[:500]
	}
	if len(rel.PublishedAt) > 40 {
		rel.PublishedAt = rel.PublishedAt[:40]
	}
	return &rel, KindNone
}

// classifyErr 把网络错误分成"超时"与"其它"。
//
// 为什么要分：超时是"我等到上限了"（可能对方只是慢），其它多半是 DNS/证书/连不上
// （配置错了更可能）。两者的处置不同，给运维看的文案也不同。
func classifyErr(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return KindTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout
	}
	return KindNetwork
}
