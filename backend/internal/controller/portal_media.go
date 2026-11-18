package controller

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/pkg/errs"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/pkg/response"
	"lingdong-api/internal/pkg/siteurl"
	"lingdong-api/internal/service/logs"
)

const (
	// usageMediaMaxBytes 是单次代理的体积上限（64 MiB）。
	//
	// 直链是上游 CDN，长视频/4K 图完全可能超过这个量级，但平台不是 CDN：
	// 超限宁可回 502 也不边读边丢（截断的图片在浏览器里是"坏图"，比明确报错更难排查）。
	usageMediaMaxBytes = 64 << 20
	// usageMediaTimeout 是整体超时（含 DNS/连接/重定向/读体）。
	usageMediaTimeout = 60 * time.Second
	// usageMediaRedirects 是允许的重定向跳数：上游产物链接经 CDN 302 是常态，
	// 一律禁跳会拿到空体；但每跳都要重新过 IP 校验（见 netguard.ClientWithRedirects）。
	usageMediaRedirects = 3
)

// PortalUsageMedia 处理 GET /api/portal/usage/:id/media/:index：
// 把同步调用的产物（上游直链）经由本域转出。
//
// 为什么必须代理而不是直接下发 URL：
//   - 直链域名本身就在说明"背后是谁"（`*.duomi.*` / `*.volces.com`），一个 img 标签就够了；
//   - 上游链接普遍带签名与有效期，下线后门户就只剩一张坏图；
//   - 反过来，代理之后前端只需要「记录 id + 序号」，与异步任务的产物端点同形。
//
// 安全约束：
//   - 越权统一 404（回 403 等于确认这条记录存在）；
//   - 只读 `call_io.media[index].url`（调用当时落库的上游产物地址），不接受调用方传 URL；
//   - 出站走 netguard（解析后 IP 校验 + 限跳重定向），体积上限 64 MiB，不落盘不转存；
//   - Content-Type 过 safeArtifactContentType 白名单，非白名单降级为附件下载 + nosniff。
//
// 限流：不再单独开桶。/api 组已有全局限流（rate_limit_defaults，默认 60/min/IP），
// 媒体请求同样计入；再挂一个 600/min 的独立桶在全局桶之下永远不会触发，只是死代码。
func PortalUsageMedia(c *gin.Context) {
	userID := c.GetUint64("user_id")
	if userID == 0 {
		response.FailDef(c, errs.Unauthorized)
		return
	}
	id, ok := uintParam(c, "id")
	if !ok {
		response.FailDef(c, errs.NotFound)
		return
	}
	// 序号允许 0（uintParam 会拒 0）。
	idx, err := strconv.Atoi(strings.TrimSpace(c.Param("index")))
	if err != nil || idx < 0 {
		response.FailDef(c, errs.InvalidParam)
		return
	}

	// 作用域写进 SQL（DetailByIDForUser），不是查完再判 user_id。
	// 本处理函数只读 call_io.media，任务对象用不到；contentBase 仍按本入口传，
	// 免得将来有人复用这行代码时拿到空链接。
	d, derr := logs.DetailByIDForUser(c.Request.Context(), id, userID, siteurl.Base(c), taskContentBase(c))
	if derr != nil {
		response.FailDef(c, errs.Internal)
		return
	}
	if d == nil {
		response.FailDef(c, errs.NotFound)
		return
	}
	if d.IO == nil || idx >= len(d.IO.Media) {
		response.FailDef(c, errs.FileNotFound)
		return
	}
	raw := strings.TrimSpace(d.IO.Media[idx].URL)
	if raw == "" {
		// 内联数据未留存 / 产物已被上游清理：对前端语义等同于"已过期"。
		response.FailDef(c, errs.FileExpired)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), usageMediaTimeout)
	defer cancel()
	policy := netguard.PolicyFromEnv()
	if err := netguard.ValidateURLResolved(ctx, raw, policy); err != nil {
		response.FailDef(c, errs.FileExpired)
		return
	}

	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if rerr != nil {
		response.FailDef(c, errs.FileExpired)
		return
	}
	// Range 透传给上游：视频拖动与大图逐块加载都依赖它。
	rangeHeader := strings.TrimSpace(c.GetHeader("Range"))
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, ferr := netguard.ClientWithRedirects(usageMediaTimeout, policy, usageMediaRedirects).Do(req)
	if ferr != nil {
		response.FailDef(c, errs.ServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// 上游已清掉产物：如实告诉用户"过期"，而不是让他以为是平台故障。
		response.FailDef(c, errs.FileExpired)
		return
	default:
		response.FailDef(c, errs.ServiceUnavailable)
		return
	}

	size := resp.ContentLength
	if size > usageMediaMaxBytes {
		response.FailDef(c, errs.ServiceUnavailable)
		return
	}
	var body io.Reader = resp.Body
	if size <= 0 {
		// 长度未知（chunked）：用 LimitReader 兜底，宁可变短也不给无限读。
		body = io.LimitReader(resp.Body, usageMediaMaxBytes)
		size = -1
	}

	// 上游普遍不给类型、或只给 application/octet-stream：调用当时落库的 mime
	// 是更可靠的线索（它由 MIME / 扩展名判定过）。两者都认不出才回落附件下载。
	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if ct == "" || strings.EqualFold(ct, "application/octet-stream") {
		if m := strings.TrimSpace(d.IO.Media[idx].MIME); m != "" {
			ct = m
		}
	}
	safeType, inline := safeArtifactContentType(ct)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("Accept-Ranges", "bytes")
	if inline {
		c.Header("Content-Disposition", "inline")
	} else {
		c.Header("Content-Disposition", "attachment")
	}

	// 只有上游真的按 Range 切片了才回 206：上游忽略 Range 时按 200 全量下发，
	// 自己切片会把「上游不支持范围请求」这件事变成平台的额外内存开销。
	if resp.StatusCode == http.StatusPartialContent && rangeHeader != "" {
		if cr := strings.TrimSpace(resp.Header.Get("Content-Range")); cr != "" {
			c.Header("Content-Range", cr)
		}
		c.DataFromReader(http.StatusPartialContent, size, safeType, body, nil)
		return
	}
	c.DataFromReader(http.StatusOK, size, safeType, body, nil)
}
