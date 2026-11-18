package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/pkg/textx"
)

// requestIDGinKey 是 request_id 在 gin.Context 里的键名（与 Audit 中间件保持一致）。
const requestIDGinKey = "request_id"


// AccessLog 是 /v1 网关的访问日志：注入 request_id 并在请求结束后记录一行结构化日志。
//
// 为什么必须有它：`/api` 组有 Audit()，但 `/v1` 此前完全没有 request_id 与访问日志 ——
// 线上出现「某次调用扣费错误」时既没有 id 可以串日志，也没有 method/status/latency 可看。
//
// 与 Audit() 的差异：Audit 只服务管理台写操作且不记录响应；AccessLog 记录网关每次调用的
// 结果（含 status、latency、模型、错误码），是排障与计费纠纷的**唯一线索**。
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 透传客户端自带的 id（便于用户侧与网关侧对齐），但必须校验形态：
		// 允许任意可打印字符等于允许日志注入（换行/制表符会把一行日志拆成两行）。
		id := strings.TrimSpace(c.GetHeader("X-Request-Id"))
		if !validRequestID(id) {
			id = newRequestID()
		}
		// 回写请求头：relay/task 侧统一用 c.GetHeader("X-Request-Id") 取它落库，
		// 不回写的话「客户端没传」时落库恒为空串（改前就是这个状态）。
		c.Request.Header.Set("X-Request-Id", id)
		c.Set(requestIDGinKey, id)
		c.Header("X-Request-Id", id)
		c.Request = c.Request.WithContext(logger.WithRequestIDValue(c.Request.Context(), id))

		start := time.Now()
		c.Next()
		latency := time.Since(start)

		status := c.Writer.Status()
		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", scrubPath(c.Request.URL)),
			zap.Int("status", status),
			zap.Int64("latency_ms", latency.Milliseconds()),
			zap.Int("bytes", c.Writer.Size()),
		}
		if v, ok := c.Get("key_id"); ok {
			if id, ok := v.(uint64); ok && id != 0 {
				fields = append(fields, zap.Uint64("key_id", id))
			}
		}
		if v, ok := c.Get("user_id"); ok {
			if id, ok := v.(uint64); ok && id != 0 {
				fields = append(fields, zap.Uint64("user_id", id))
			}
		}
		// 以下三项由 relay / response 在关键路径写入；缺失时不补占位值，
		// 让「没记录」与「记录为空」在日志里可区分。
		fields = appendOptionalString(fields, c, "gateway_model", "model")
		fields = appendOptionalString(fields, c, "gateway_modality", "modality")
		fields = appendOptionalString(fields, c, "upstream_status", "upstream_status")
		fields = appendOptionalString(fields, c, "error_code", "error_code")
		if c.Request.URL.Path == "/v1" || strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			fields = append(fields, zap.Bool("gateway", true))
			// 线格式进日志而不是进 usages 列：按 request_id 与调用记录一一对应即可还原
			// 「这笔钱是哪种协议产生的」；加一个只写不读的列属于冗余设计。
			// 将来真要做「按协议的收入报表」，再单独立项加列。
			fields = append(fields, zap.String("protocol", string(gateway.FormatOf(c))))
		}

		l := logger.WithRequestID(id)
		switch {
		case status >= http.StatusInternalServerError:
			l.Error("网关请求", fields...)
		case status >= http.StatusBadRequest:
			l.Warn("网关请求", fields...)
		default:
			l.Info("网关请求", fields...)
		}
	}
}

// appendOptionalString 把 gin.Context 里的可选字符串字段加入日志（空值跳过）。
func appendOptionalString(fields []zap.Field, c *gin.Context, ginKey, logKey string) []zap.Field {
	v, ok := c.Get(ginKey)
	if !ok {
		return fields
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return fields
	}
	return append(fields, zap.String(logKey, s))
}

// newRequestID 生成 16 字节随机 id 的十六进制表示（32 字符）。
//
// 刻意不用 uuid：这里只需要唯一与不可预测，不参与跨系统解析，
// 32 位 hex 比带连字符的 uuid 更适合直接 grep。
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand 失败极罕见（熵源不可用）；退化为时间戳，仍保证唯一性优先于随机性。
		return strconvHex(time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// strconvHex 把 int64 转成十六进制字符串（避免为这一处引入 strconv 之外的依赖）。
func strconvHex(v int64) string {
	var buf [16]byte
	u := uint64(v)
	for i := 15; i >= 0; i-- {
		buf[i] = "0123456789abcdef"[u&0xf]
		u >>= 4
	}
	return string(buf[:])
}

// validRequestID 校验外部传入的 request_id：1..64 字符且仅含可打印 ASCII（不含空格）。
func validRequestID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// scrubPath 返回用于日志的 path：保留结构信息，剔除可能携带密钥的 query。
func scrubPath(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.RawQuery == "" {
		return u.Path
	}
	q := u.Query()
	// 脱敏规则与上游抓包共用 textx（唯一清单）：`?Key=` 这种大写写法能过鉴权
	// （取钥用 EqualFold），判定若区分大小写就会把明文密钥写进访问日志。
	if !textx.RedactValues(q) {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path + "?" + q.Encode()
}
