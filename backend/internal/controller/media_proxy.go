package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/pkg/memlimit"
	"lingdong-api/internal/gateway"
	"lingdong-api/internal/pkg/mediaurl"
	"lingdong-api/internal/pkg/netguard"
	"lingdong-api/internal/service/task"
)

// 本文件是 GET /v1/media/{token}：同步产物的能力 URL 代理。
//
// 为什么必须由平台代理，而不是直接把上游 CDN 直链给用户：
//   - 直链里带着上游厂商域名，等于把渠道拓扑写在每一次响应里；
//   - 上游 CDN 普遍有防盗链（Referer / 时效签名），换台设备、换个网络就 403，
//     用户看到的是「图生成了但打不开」，而计费已经发生；
//   - 浏览器端预览需要同源或允许 CORS 的地址，第三方域名时常被企业网关拦截。
//
// 四条边界（改之前先读）：
//  1. **token 就是凭证**。`<img>` 带不了 Authorization，所以这里不能挂 TokenAuth；
//     安全性由 HMAC 签名保证（internal/pkg/mediaurl）。也正因为如此，
//     token 绝不能进日志与调用记录——它就是一把可以直接打开产物的钥匙。
//  2. **Range 必须透传**。视频拖动、断点续传都依赖 206 + Content-Range；
//     忽略 Range 会让播放器每次都从 0 开始拉整段。
//  3. **失败不能暴露上游**。上游过期/防盗链时只回「产物已不可用」，
//     不带上游地址、不带上游错误体——那正是引入代理要消除的信息。
//  4. **独立限流桶**。这个端点不在 /v1 组里（没有 TokenAuth，也就没有组的 RateLimit），
//     不显式加桶等于给了一个无限流的公开出口。

const (
	// mediaProxyRatePerMin 是按 IP 的每分钟上限。
	// 取 600（默认业务限流 60 的 10 倍）：一屏产物列表会并发拉十几张图，
	// 按 60 算会让「正常翻一页」直接触发限流，而它并不产生上游费用。
	mediaProxyRatePerMin = 600
	// mediaProxyHeaderWait 是「连上但迟迟不给响应头」的容忍时长。
	// 不能靠 http.Client.Timeout 兜：那会把整体超时也算进去，大文件必然被掐断。
	mediaProxyHeaderWait = 20 * time.Second
	// mediaProxyMaxBytes 是允许代理的单件上限，防「上游给了个 2GB 的链接」把带宽打满。
	mediaProxyMaxBytes = 64 << 20
	// mediaProxyMaxRedirects 是跟随跳转的上限（CDN 302 到边缘节点是常态）。
	mediaProxyMaxRedirects = 3
)

// mediaProxyUserAgent 是对上游声明的身份。
//
// 不伪装成浏览器：部分 CDN 会对伪装 UA 做更严格的校验，而带着平台标识出问题
// 至少能被对方运营看懂并在日志里对上号。
const mediaProxyUserAgent = "LingDong-MediaProxy/1.0"

// MediaProxy 处理 GET/HEAD /v1/media/{token}。
func MediaProxy(c *gin.Context) {
	if m := c.Request.Method; m != http.MethodGet && m != http.MethodHead {
		c.Header("Allow", "GET, HEAD")
		c.String(http.StatusMethodNotAllowed, "只支持 GET / HEAD")
		return
	}
	if !allowMediaProxy(c.ClientIP()) {
		c.Header("Retry-After", "60")
		c.String(http.StatusTooManyRequests, "请求过于频繁，请稍后重试")
		return
	}

	raw, claim, err := mediaurl.VerifyAny(c.Param("token"))
	switch {
	case errors.Is(err, mediaurl.ErrExpired):
		// 410 而不是 404：客户端据此可以判定「expired」并回源重新获取，
		// 而 404 会让它怀疑地址拼错，反复重试。
		c.String(http.StatusGone, "链接已过期，请重新获取")
		return
	case err != nil:
		// 一律 404：不区分「签名错」与「没配密钥」。区分等于给扫描器反馈。
		c.String(http.StatusNotFound, "链接无效")
		return
	}

	// 产物能力 URL：token 里带的是 (任务号, 序号)，字节从平台自己的库里取。
	//
	// 这条路不发任何出站请求 —— 产物在上游成功时就已经转存，
	// 这里只是把它读出来（Range/白名单 MIME 与 /v1/tasks/*/artifacts/*/content 同一实现）。
	if claim != nil {
		// 这里**故意跨用户**：授权来自 HMAC 签名 claim（24h 能力 URL），
		// 不是用户身份。必须显式写 AllScope()，让「跨用户读」在评审时可见。
		t, terr := task.Get(c.Request.Context(), claim.TaskID, task.AllScope())
		if terr != nil {
			c.String(http.StatusNotFound, "产物不存在")
			return
		}
		serveArtifact(c, t, claim.Index, gateway.FailDef)
		return
	}

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	req, rerr := http.NewRequestWithContext(ctx, c.Request.Method, raw, nil)
	if rerr != nil {
		c.String(http.StatusNotFound, "链接无效")
		return
	}
	req.Header.Set("User-Agent", mediaProxyUserAgent)
	req.Header.Set("Accept", "*/*")
	// 明确不要压缩：Go 的 Transport 默认会加 Accept-Encoding: gzip 并**透明解压**，
	// 解压后 Content-Length 变成 -1，我们既拿不到长度也丢掉了「超上限」的前置判断。
	// 图片/音视频本来也压不动，声明 identity 让长度信息如实回传。
	req.Header.Set("Accept-Encoding", "identity")
	// Range 与条件请求原样带过去：拖动进度条、断点续传、协商缓存都靠它们。
	for _, h := range []string{"Range", "If-None-Match", "If-Modified-Since", "If-Range"} {
		if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
			req.Header.Set(h, v)
		}
	}

	// 头部等待看门狗：拿到响应头就停表，此后按“客户端断开”或“超过上限”结束。
	// 用定时器而不是 Client.Timeout，是因为后者会把体积传输时间一并算进超时。
	watchdog := time.AfterFunc(mediaProxyHeaderWait, cancel)
	defer watchdog.Stop()

	client := netguard.ClientWithRedirects(0, netguard.PolicyFromEnv(), mediaProxyMaxRedirects)
	resp, err := client.Do(req)
	watchdog.Stop()
	if err != nil {
		// 上游不可达/被策略拦下：502 而不是 500——这是「我们连不上它」，不是我们崩了。
		c.String(http.StatusBadGateway, "产物获取失败")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		c.Status(http.StatusNotModified)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 上游过期（403/404/410 是 CDN 链接失效最常见的三种表现）统一折成 404：
		// 用户能做的只有「重新生成」，给出上游状态码只会让排查方向跑偏。
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			c.String(http.StatusNotFound, "产物已不可用")
			return
		}
		c.String(http.StatusBadGateway, "产物获取失败")
		return
	}

	limit := mediaProxyMaxBytes
	if mb := mediaMaxMB(); mb > 0 {
		limit = mb << 20
	}
	if resp.ContentLength > int64(limit) {
		c.String(http.StatusBadGateway, "产物超过代理上限，请通过任务产物接口取回")
		return
	}

	safeType, inline := safeArtifactContentType(resp.Header.Get("Content-Type"))
	c.Header("Content-Type", safeType)
	c.Header("X-Content-Type-Options", "nosniff")
	// 能力 URL 自带时效，可以放心让浏览器缓存一小时：同一张图在会话里被反复渲染时
	// 不必回源。不用 immutable——token 会过期，标成不可变会让过期链接在本地永久可用。
	c.Header("Cache-Control", "public, max-age=3600")
	if inline {
		c.Header("Content-Disposition", "inline")
	} else {
		c.Header("Content-Disposition", "attachment")
	}
	copyHeader(c, resp, "Content-Range", "Accept-Ranges", "ETag", "Last-Modified")
	if resp.ContentLength > 0 {
		c.Header("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	if c.Request.Method == http.MethodHead {
		c.Status(resp.StatusCode)
		return
	}

	c.Status(resp.StatusCode)
	// 超过上限时**主动截断连接**：没有 Content-Length 的响应（chunked）没法回改状态码，
	// 与其给用户一个看起来完整、实际缺了一截的文件，不如让下载明确失败。
	// 读 limit+1 字节才能区分「正好等于上限」与「超过上限」。
	written, cerr := io.CopyBuffer(c.Writer, io.LimitReader(resp.Body, int64(limit)+1), make([]byte, 32*1024))
	if cerr != nil {
		// 客户端断开（用户划走/关页面）是常态，不是错误：连接已废，直接结束。
		c.Abort()
		return
	}
	if written > int64(limit) {
		c.Abort()
		return
	}
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

// copyHeader 按白名单搬运上游响应头（不整体复制：Hop-by-Hop 与 Set-Cookie 必须挡掉）。
func copyHeader(c *gin.Context, resp *http.Response, keys ...string) {
	for _, k := range keys {
		if v := strings.TrimSpace(resp.Header.Get(k)); v != "" {
			c.Header(k, v)
		}
	}
}

// mediaMaxMB 读取代理体积上限（LD_MEDIA_MAX_MB，0 表示用默认值）。
func mediaMaxMB() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LD_MEDIA_MAX_MB")))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// allowMediaProxy 是按 IP 的固定窗口限流。
//
// Redis 不可用时**降额放行**（进程内 limit/6），不是全放行：这个端点是匿名可打的，
// 又真读存储与上游 —— Redis 一挂就无限下载，等于把最脆弱的时候交给公网。
// 取向与 Key 维度两道闸（fail-local）一致。
func allowMediaProxy(ip string) bool {
	if !db.RedisReady() {
		return memlimit.DegradedAllow("media", ip, mediaProxyRatePerMin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := "ld:media:rl:" + ip + ":" + strconv.FormatInt(time.Now().Unix()/60, 10)
	n, err := db.RDB.Incr(ctx, key).Result()
	if err != nil {
		return memlimit.DegradedAllow("media", ip, mediaProxyRatePerMin)
	}
	if n == 1 {
		db.RDB.Expire(ctx, key, time.Minute)
	}
	return n <= mediaProxyRatePerMin
}
