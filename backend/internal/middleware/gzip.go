package middleware

import (
	"compress/gzip"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// 压缩的默认最小字节数：低于该阈值压缩只会让响应变大（gzip 头 18 字节 + 压缩块开销）。
const gzipMinBytesDef = 1024

// compressibleTypes 是允许压缩的响应 Content-Type 白名单（比较前已归一为小写且去掉参数）。
//
// 刻意用**白名单而不是黑名单**：黑名单漏一个（例如未来的 application/wasm、字体变体）
// 就会让二进制被压缩坏；白名单漏一个只是少压一次，代价不对称。
var compressibleTypes = map[string]bool{
	"text/html":                         true,
	"text/plain":                        true,
	"text/css":                          true,
	"text/xml":                          true,
	"text/javascript":                   true,
	"text/markdown":                     true,
	"application/json":                  true,
	"application/ld+json":               true,
	"application/javascript":            true,
	"application/ecmascript":            true,
	"application/xml":                   true,
	"application/xhtml+xml":             true,
	"application/x-ndjson":              true,
	"application/geo+json":              true,
	"application/problem+json":          true,
	"application/vnd.api+json":          true,
	"image/svg+xml":                     true,
	"application/x-www-form-urlencoded": true,
}

// Gzip 是压缩中间件的统一入口：默认走惰性智能压缩，LD_GZIP_SMART=0 时回落旧实现。
//
// 旧实现（GzipLegacy）对**所有**响应无条件设 Content-Encoding: gzip 并包装 Writer，
// 而产物下载走 c.DataFromReader 写死原始 Content-Length —— 二者叠加会让客户端
// 按未压缩长度读取压缩体，视频/图片/音频/文件直链全部截断。
func Gzip() gin.HandlerFunc {
	if !envBool("LD_GZIP_SMART", true) {
		return gzipLegacy()
	}
	return GzipSmart()
}

// gzipLegacy 是改造前的实现，仅为灰度对比保留（行为已知有缺陷，不要在新环境启用）。
func gzipLegacy() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		gz := gzip.NewWriter(c.Writer)
		defer gz.Close()

		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")
		c.Writer = &gzipResponseWriter{ResponseWriter: c.Writer, w: gz}
		c.Next()
	}
}

type gzipResponseWriter struct {
	gin.ResponseWriter
	w *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.w.Write(b)
}

// GzipSmart 是惰性智能压缩：包装 Writer 时**不做任何决定**，延迟到首次写响应头时按
// 实际 Content-Type / Content-Length 判定，不满足条件就原样透传（不包装、不改头）。
//
// 判定条件（全部满足才压缩）：
//  1. 请求 Accept-Encoding 含 gzip
//  2. Content-Type 属于文本类白名单（text/*、application/json 等）
//  3. 响应未显式声明 Content-Encoding（上游可能已经压过）
//  4. 不是 SSE（text/event-stream 不在白名单里，天然排除）且未设置 X-No-Compress
//  5. 长度未知，或已知长度 > LD_GZIP_MIN_BYTES（默认 1024）
//
// opt-out 约定：handler 在写任何字节**之前** `c.Header("X-No-Compress", "1")`；
// 中间件读取后会删掉该头，不对外暴露内部约定。
func GzipSmart() gin.HandlerFunc {
	minBytes := envInt("LD_GZIP_MIN_BYTES", gzipMinBytesDef)
	if minBytes < 0 {
		minBytes = 0
	}
	return func(c *gin.Context) {
		if !acceptsGzip(c.GetHeader("Accept-Encoding")) {
			c.Next()
			return
		}
		sw := &smartGzipWriter{ResponseWriter: c.Writer, minBytes: minBytes}
		orig := c.Writer
		c.Writer = sw
		defer func() {
			sw.close()
			c.Writer = orig
		}()
		c.Next()
	}
}

// smartGzipWriter 延迟决策的响应写入器。
//
// decided 一旦置位表示「已决定不压缩」或「已创建 gz」；gz == nil && decided 即透传。
type smartGzipWriter struct {
	gin.ResponseWriter
	minBytes int
	decided  bool
	gz       *gzip.Writer
}

func (w *smartGzipWriter) WriteHeaderNow() {
	w.decide(0)
	w.ResponseWriter.WriteHeaderNow()
}

func (w *smartGzipWriter) WriteHeader(code int) {
	// gin 的 WriteHeader 只记状态码不写网络，这里不做决策（此时 Content-Type 可能还没设）；
	// 但要把状态码透传给底层，保持既有语义。
	w.ResponseWriter.WriteHeader(code)
}

func (w *smartGzipWriter) Write(b []byte) (int, error) {
	// 首次写入且响应头尚未落到网络：小于阈值时直接透传，避免「小 JSON 压缩后更大」。
	if !w.decided && !w.ResponseWriter.Written() && len(b) < w.minBytes {
		w.decided = true
	}
	w.decide(len(b))
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *smartGzipWriter) WriteString(s string) (int, error) {
	// 必须自己走 Write：gin 的默认实现是 io.WriteString(底层 Writer)，会绕过压缩。
	return w.Write([]byte(s))
}

func (w *smartGzipWriter) Flush() {
	// SSE 与流式 JSON 依赖逐帧下发：压缩分支必须先冲 gzip 缓冲再冲底层。
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	w.ResponseWriter.Flush()
}

func (w *smartGzipWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
	}
}

// decide 在首次调用时确定是否压缩；firstLen 为 0 表示「长度未知或不是首次写入」。
func (w *smartGzipWriter) decide(firstLen int) {
	if w.decided {
		return
	}
	w.decided = true

	h := w.ResponseWriter.Header()
	// opt-out 是一次性约定，读完即删，避免泄漏给客户端。
	if v := h.Get("X-No-Compress"); v != "" {
		h.Del("X-No-Compress")
		return
	}
	if h.Get("Content-Encoding") != "" {
		return
	}
	if !compressible(h.Get("Content-Type")) {
		return
	}
	h.Del("X-No-Compress")

	// 长度闸门：已知长度时按 Content-Length 判；未知时按首次写入长度判；
	// 两者都未知（chunked 且首次写入就很大以外的情况）按压缩处理。
	if n, ok := contentLength(h); ok {
		if n < int64(w.minBytes) {
			return
		}
	} else if firstLen > 0 && firstLen < w.minBytes {
		return
	}

	h.Set("Content-Encoding", "gzip")
	// 压缩后长度不再等于原值：必须删掉，交给 Go 用 chunked 输出。
	h.Del("Content-Length")
	addVary(h, "Accept-Encoding")
	w.gz = gzip.NewWriter(w.ResponseWriter)
}

// compressible 判断 Content-Type 是否属于可压缩白名单（大小写不敏感、忽略参数）。
func compressible(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		// 没声明类型时保守处理：可能被 Go 嗅探成二进制。
		return false
	}
	if compressibleTypes[ct] {
		return true
	}
	// text/* 除 text/event-stream（SSE 必须逐帧明文）外一律可压。
	if strings.HasPrefix(ct, "text/") && ct != "text/event-stream" {
		return true
	}
	return false
}

// contentLength 解析响应头里的 Content-Length；缺失或非法返回 ok=false。
func contentLength(h interface{ Get(string) string }) (int64, bool) {
	raw := strings.TrimSpace(h.Get("Content-Length"))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// addVary 追加 Vary 值并去重（已有 Accept-Encoding 时不重复添加）。
func addVary(h interface {
	Get(string) string
	Set(string, string)
}, value string) {
	cur := h.Get("Vary")
	if cur == "" {
		h.Set("Vary", value)
		return
	}
	for _, part := range strings.Split(cur, ",") {
		if strings.EqualFold(strings.TrimSpace(part), value) {
			return
		}
	}
	h.Set("Vary", cur+", "+value)
}

// acceptsGzip 判断客户端是否接受 gzip（支持 q=0 显式拒绝）。
//
// 逐段解析而非子串匹配：`Accept-Encoding: gzip;q=0, deflate` 必须视为不接受，
// 用 strings.Contains 会把 q=0 当接受，进而给明确拒绝压缩的客户端发压缩体。
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		tokens := strings.Split(part, ";")
		name := strings.TrimSpace(tokens[0])
		if name == "" || (!strings.EqualFold(name, "gzip") && name != "*") {
			continue
		}
		if q, ok := qValue(tokens[1:]); ok && q == 0 {
			continue // 显式拒绝 gzip
		}
		return true
	}
	return false
}

// qValue 从 Accept-Encoding 的参数段里取 q 值；未声明时 ok=false（按 HTTP 语义等同 q=1）。
func qValue(params []string) (float64, bool) {
	for _, p := range params {
		p = strings.TrimSpace(p)
		if len(p) < 2 || (p[0] != 'q' && p[0] != 'Q') || p[1] != '=' {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(p[2:]), 64)
		if err != nil {
			continue
		}
		return q, true
	}
	return 0, false
}

// envBool 读取布尔环境变量（未设置或无法解析时用默认值）。
func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// envInt 读取整型环境变量（未设置或非法时用默认值）。
func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}
