package controller

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lingdong-api/internal/db"
	"lingdong-api/internal/service/order"
	"lingdong-api/internal/service/payment"
)

// maxNotifyBody 是回调请求体上限。
//
// 没有上限时，公开端点可以让服务端分配任意内存（攻击者发 100MB 的 form 就能拖垮进程）。
// 正常回调只有几百字节到几 KB（微信 APIv3 的 JSON 也在 2KB 内）。
const maxNotifyBody int64 = 64 << 10

// PayNotifyChannel 处理 POST /api/pay/:channel/notify（推荐入口）。
//
// 挂载时**不经 api 组**：那里有 RateLimit（60rpm/IP），而支付回调的源 IP 是共享出口，
// 超限时返回的是 JSON 429，而网关只判断 body 是否等于 success → 当失败重试 → 制造重试风暴。
//
// 顺序固定为「先按渠道验签、再查订单」：反过来等于把「订单号是否存在」
// 变成一个不需要密钥就能探测的接口。
func PayNotifyChannel(c *gin.Context) {
	req, ok := readNotify(c)
	if !ok {
		return // 响应已写好
	}
	res, protocol := order.New(db.DB).
		HandleNotifyRequest(c.Request.Context(), strings.TrimSpace(c.Param("channel")), req)
	writeNotifyAck(c, res.AckFor(protocol))
}

// PayNotifyLegacy 处理 POST /api/pay/notify（易支付老地址）。
//
// 该端点没有渠道参数，只能先查订单再验签（订单不存在时无法验签，所以只回 success 止血、
// 不落孤儿表，避免被任意请求灌数据）。它是过渡入口，新配置一律指向 /api/pay/{channel}/notify。
func PayNotifyLegacy(c *gin.Context) {
	req, ok := readNotify(c)
	if !ok {
		return
	}
	res := order.New(db.DB).HandleLegacyNotify(c.Request.Context(), req.Params)
	// 老端点没有渠道，无法知道协议：一律按「通用 success/fail」应答（易支付语义）。
	writeNotifyAck(c, res.AckFor(""))
}

// readNotify 取回调的**完整**原始输入（表单参数 + 原始 body + header）。
//
// 为什么不能只 ParseForm：微信 APIv3 的签名对象是 `timestamp\nnonce\nbody\n` 的
// 逐字节原文，ParseForm 之后 body 已被消费且不再保证字节一致 —— 拿不到原文
// 就等于把所有微信回调判成验签失败。所以顺序必须是「先读原始字节 → 还原 Body →
// 再解析表单」。
//
// 失败时**已经写好响应**并返回 false，调用方只需 return。
// body 超限返回 413 而不是 200+fail：两者都会让网关重试，但 413 能让运维在日志里
// 一眼看出「有人在灌大包」，而不是误判成验签问题。
func readNotify(c *gin.Context) (payment.NotifyRequest, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxNotifyBody)

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.String(http.StatusRequestEntityTooLarge, "fail")
			return payment.NotifyRequest{}, false
		}
		c.String(http.StatusOK, "fail")
		return payment.NotifyRequest{}, false
	}
	// 还原 Body：后面的 ParseForm（以及任何后续中间件/日志）都依赖它。
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	// 仅当 Content-Type 是表单时才会真正解析 body；JSON 回调不会读 body，也不会报错。
	// 解析失败不阻断：JSON 回调本来就没有表单参数，交由各协议自己判定。
	params := map[string]string{}
	if perr := c.Request.ParseForm(); perr == nil {
		for k, v := range c.Request.PostForm {
			if len(v) > 0 && v[0] != "" {
				params[k] = v[0]
			}
		}
	}
	// 有些网关用 GET/query 回调：只在 body 缺该键时补齐，绝不覆盖 body 里的真实值。
	for k, v := range c.Request.URL.Query() {
		if _, ok := params[k]; !ok && len(v) > 0 {
			params[k] = v[0]
		}
	}

	return payment.NotifyRequest{
		Params: params,
		Body:   raw,
		Header: c.Request.Header,
	}, true
}

// writeNotifyAck 按协议写应答。
func writeNotifyAck(c *gin.Context, ack payment.NotifyAck) {
	if ack.Status == http.StatusNoContent {
		// 204 必须没有包体：带上 body 会让 Content-Length 与实际不符。
		c.Status(http.StatusNoContent)
		return
	}
	if ack.ContentType != "" {
		c.Header("Content-Type", ack.ContentType)
	}
	c.String(ack.Status, ack.Body)
}
