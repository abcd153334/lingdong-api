package netguard

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"lingdong-api/internal/pkg/textx"
)

// BodyTap 接收「真正写到连接上的请求」。
//
// 只做留痕：不参与请求构造、不改 header、不改 body。任何实现都必须保证
// 幂等且不阻塞（它在 RoundTrip 的同步路径上）。
type BodyTap interface {
	// NoteUpstreamRequest 在请求发出**之前**调用。
	// body 为 nil 且 truncated=false 表示「取不到请求体」（GetBody 为空，多为流式上传）。
	NoteUpstreamRequest(ctx context.Context, method, url, contentType string, body []byte, truncated bool)
	// NoteUpstreamResponse 在收到响应头之后调用；err != nil 时 status 为 0。
	NoteUpstreamResponse(ctx context.Context, status int)
}

type bodyTapKey struct{}

// WithBodyTap 把 tap 挂到 ctx 上。适配器的 client 会从 req.Context() 取它。
//
// 用 ctx 而不是 ClientConfig 字段：适配器的 `client(cfg)` 是包内共享函数，
// 拿不到 request 级状态；而探活/轮询/支付/存储的 ctx 没有 tap，天然零开销。
func WithBodyTap(ctx context.Context, t BodyTap) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, bodyTapKey{}, t)
}

// BodyTapOf 取 ctx 上的 tap（没有则返回 nil）。
func BodyTapOf(ctx context.Context) BodyTap {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(bodyTapKey{}).(BodyTap)
	return t
}

// tapMaxBytes 是单次留痕的请求体上限。
//
// 超过则只记前缀并标 truncated：图像编辑的 multipart / base64 内联可能很大，
// 而排障要看的是「字段结构」，不是几十 MB 的像素。
const tapMaxBytes = 8 << 20


// WrapTap 用 tap 包装 transport。
//
// 必须在 **Client 层**包装，不能在 transportFor 里：后者是进程级共享对象，
// 支付（xunhupay）、媒体代理、对象存储都用同一份，包在那里会把它们一起套上抓包层。
func WrapTap(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &tapTransport{base: base}
}

type tapTransport struct{ base http.RoundTripper }

func (t *tapTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tap := BodyTapOf(req.Context())
	// 没有请求体的请求（轮询 GET / 探活）不参与留痕：它不是「出站报文」。
	//
	// 不跳过的话，同步图像桥接在同一个 ctx 上提交完立刻轮询，那次 GET 会把
	// 刚记下的 POST 正文覆盖成空（而后端只能把「有尝试但没正文」标成 tap-skipped），
	// 于是 `upstream_raw` 永久为空、`upstream_url` 记成轮询地址、`attempts`
	// 变成轮询次数 —— 排障时看到的是「上游什么都没收到」，与事实相反。
	if tap == nil || req.Body == nil || req.Body == http.NoBody {
		return t.base.RoundTrip(req)
	}
	t.noteRequest(req, tap)
	resp, err := t.base.RoundTrip(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	tap.NoteUpstreamResponse(req.Context(), status)
	return resp, err
}

func (t *tapTransport) noteRequest(req *http.Request, tap BodyTap) {
	ct := req.Header.Get("Content-Type")
	u := sanitizeTapURL(req.URL)
	// GetBody 为空 = 流式体（io.Pipe / LimitReader）。绝不能读 req.Body：
	// 读掉之后上游收到的是空体，请求会「莫名 400」，而钱已经冻结了。
	if req.GetBody == nil {
		tap.NoteUpstreamRequest(req.Context(), req.Method, u, ct, nil, false)
		return
	}
	rc, err := req.GetBody()
	if err != nil {
		tap.NoteUpstreamRequest(req.Context(), req.Method, u, ct, nil, false)
		return
	}
	defer rc.Close()
	buf, err := io.ReadAll(io.LimitReader(rc, tapMaxBytes+1))
	if err != nil {
		tap.NoteUpstreamRequest(req.Context(), req.Method, u, ct, nil, false)
		return
	}
	truncated := len(buf) > tapMaxBytes
	if truncated {
		buf = buf[:tapMaxBytes]
	}
	// 二进制请求体（图片编辑/音频上传的 multipart）不落库：留存表是 text 列，
	// 写进无效 UTF-8 会让**整条调用记录**插入失败（PostgreSQL: invalid byte sequence），
	// 排障时反而什么都看不到。结构类信息仍可从 URL/状态码/Content-Type 得到。
	if !utf8.Valid(buf) {
		tap.NoteUpstreamRequest(req.Context(), req.Method, u, ct, nil, false)
		return
	}
	// 拷贝：*bytes.Buffer 的 GetBody 返回底层数组切片，调用方若复用 buffer 会改动它。
	tap.NoteUpstreamRequest(req.Context(), req.Method, u, ct, append([]byte(nil), buf...), truncated)
}

// sanitizeTapURL 去掉 query 里的凭据，只保留排障需要的路径与普通参数。
func sanitizeTapURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	cp := *u
	cp.User = nil
	if cp.RawQuery != "" {
		q := cp.Query()
		// 与访问日志共用 textx 的唯一清单（含大小写不敏感匹配）：两份清单各写一遍时，
		// 「哪个泄漏」只取决于走的是访问日志还是抓包。
		if textx.RedactValues(q) {
			cp.RawQuery = q.Encode()
		}
	}
	// 去掉 fragment：它不参与请求，留着只会让两次记录看起来不同。
	cp.Fragment = ""
	return strings.TrimSpace(cp.String())
}
