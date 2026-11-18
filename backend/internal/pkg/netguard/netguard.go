// Package netguard 是出站 HTTP 请求的唯一安全边界：URL 校验、解析后 IP 校验、重定向策略与
// 连接池复用都收敛在这里，避免各处各写一个 `&http.Client{}` 导致策略漂移。
//
// 设计要点：
//   - 默认拒绝私网/环回/链路本地/元数据地址；自建上游（vLLM/Ollama）通过 Policy 显式放行；
//   - 校验落在 DialContext.Control（连接建立前拿到的是已解析 IP），因此对 DNS rebinding 有效；
//   - 默认禁止重定向（ErrUseLastResponse）：上游 302 到内网是最容易被忽视的 SSRF 通道；
//   - Transport 按「超时档 + 策略」缓存并复用，避免每次请求新建连接池（TLS 握手成本）。
package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrBlocked 表示目标地址被安全策略拒绝（私网/环回/链路本地/元数据/非法主机）。
var ErrBlocked = errors.New("netguard: 目标地址不被允许")

// Policy 是出站访问策略。
// 默认零值 = 只允许公网；自建上游场景可用 AllowPrivate 打开（由 LD_UPSTREAM_ALLOW_PRIVATE 控制）。
type Policy struct {
	AllowPrivate  bool
	AllowLoopback bool
}

// PolicyFromEnv 读取 LD_UPSTREAM_ALLOW_PRIVATE（"1"/"true" 时放行私网与回环）。
// 说明：回环与私网用同一开关，避免出现「放了私网但没放回环」的半开状态。
func PolicyFromEnv() Policy {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LD_UPSTREAM_ALLOW_PRIVATE")))
	if v == "1" || v == "true" || v == "yes" {
		return Policy{AllowPrivate: true, AllowLoopback: true}
	}
	return Policy{}
}

// ValidateIP 按策略校验单个 IP。
func (p Policy) ValidateIP(ip net.IP) error {
	if ip == nil {
		return ErrBlocked
	}
	switch {
	// 未指定/组播/接口本地组播无可连接语义，任何策略下都拒绝（含 224.0.0.0/24 链路本地组播，
	// 必须先于 IsLinkLocalMulticast 判断，否则会被 AllowPrivate 误放行）。
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast():
		return ErrBlocked
	case ip.IsLoopback():
		if p.AllowLoopback {
			return nil
		}
		return ErrBlocked
	case ip.IsPrivate(), ip.IsLinkLocalUnicast():
		if p.AllowPrivate {
			return nil
		}
		return ErrBlocked
	}
	return nil
}

// ValidateURL 做字面量校验（不解析 DNS）：协议、主机名形态、字面量 IP。
func ValidateURL(raw string, p Policy) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ErrBlocked
	}
	u, err := url.Parse(s)
	if err != nil {
		return ErrBlocked
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrBlocked
	}
	// URL 内嵌凭据（user:pass@host）会随日志/错误串泄漏，一律拒绝。
	if u.User != nil {
		return ErrBlocked
	}
	host := u.Hostname()
	if host == "" {
		return ErrBlocked
	}
	if ip := net.ParseIP(host); ip != nil {
		return p.ValidateIP(ip)
	}
	lower := strings.ToLower(host)
	// 无点主机名（含 localhost）、.local/.internal 一律拒绝：内网服务最常见的就是这些别名。
	if lower == "localhost" || !strings.Contains(lower, ".") ||
		strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return ErrBlocked
	}
	return nil
}

// ValidateURLResolved 在字面量校验之上追加解析后 IP 校验（防 DNS rebinding 的第一道闸；
// 第二道闸是 Client 的 DialContext.Control）。
func ValidateURLResolved(ctx context.Context, raw string, p Policy) error {
	if err := ValidateURL(raw, p); err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ErrBlocked
	}
	if net.ParseIP(u.Hostname()) != nil {
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(addrs) == 0 {
		return ErrBlocked
	}
	for _, a := range addrs {
		if err := p.ValidateIP(a.IP); err != nil {
			return err
		}
	}
	return nil
}

// transportKey 是连接池的复用键：同策略共享 Transport。
//
// 刻意**不含** timeout：整体超时是 http.Client 的字段，不进 Transport 的任何字段，
// 把它放进 key 只会让同一份策略被切成多个连接池（每个池各自 TLS 握手）。
type transportKey struct {
	policy Policy
}

// controlFor 生成建连期 IP 校验钩子（在 TCP 建连前执行，DNS 已解析完毕）。
func controlFor(p Policy) func(_, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return ErrBlocked
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return ErrBlocked
		}
		return p.ValidateIP(ip)
	}
}

// DialContext 返回一个带策略校验的拨号函数。
//
// 给"必须自定义 Transport"的调用方复用（S3 需要单次读超时、代理、签名等
// http.Transport 级配置，套 Client() 会丢掉它们）。共享同一份 Control，
// 保证「出站一律过 netguard」这条规则没有例外分支。
func DialContext(timeout time.Duration, p Policy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control:   controlFor(p),
	}
	return d.DialContext
}

var transports sync.Map // map[transportKey]*http.Transport

func transportFor(p Policy) *http.Transport {
	key := transportKey{policy: p}
	if v, ok := transports.Load(key); ok {
		return v.(*http.Transport)
	}
	t := &http.Transport{
		DialContext:           DialContext(10*time.Second, p),
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	actual, _ := transports.LoadOrStore(key, t)
	return actual.(*http.Transport)
}

// Transport 返回按策略共享的连接池，供「需要自定义 Transport」的调用方复用。
//
// 典型调用方是对象存储驱动（OSS/COS 的 SDK 会自己包一层签名 RoundTripper，
// 但底层的拨号与连接池应当共享）。返回的指针是**进程级共享对象**：
// 调用方只读，不得修改任何字段（改了会影响所有同策略的使用方）。
func Transport(p Policy) *http.Transport { return transportFor(p) }

// Client 返回带策略的客户端：禁重定向 + 建连期 IP 校验 + 复用连接池。
// timeout<=0 表示不设整体超时（流式场景由调用方自行控制空闲计时）。
func Client(timeout time.Duration, p Policy) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     transportFor(p),
		CheckRedirect: DenyRedirect,
	}
}

// DenyRedirect 是默认重定向策略：不跟随，把 3xx 原样返回给调用方。
func DenyRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// ErrTooManyRedirects 表示重定向跳数超过调用方允许的上限。
var ErrTooManyRedirects = errors.New("netguard: 重定向次数过多")

// ClientWithRedirects 返回**允许有限重定向**的客户端（最多 maxHops 跳），且每跳都重新校验目标。
//
// 为什么需要它：拉取第三方图片（如支付二维码）时，图片地址经 CDN 302 是常态，
// 一律 DenyRedirect 会拿到空体，而报错还是「目标地址不被允许」，极难排查。
// 但放开重定向不能同时放开安全校验——每次跳转后的新地址都必须重新过 URL 校验与解析后 IP 校验，
// 否则 302 到 169.254.169.254 就是教科书式的 SSRF 绕道。
func ClientWithRedirects(timeout time.Duration, p Policy, maxHops int) *http.Client {
	c := Client(timeout, p)
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxHops {
			return ErrTooManyRedirects
		}
		if err := ValidateURL(req.URL.String(), p); err != nil {
			return err
		}
		return ValidateURLResolved(req.Context(), req.URL.String(), p)
	}
	return c
}
