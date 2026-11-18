package payment

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

// 二维码拉图的限额。
const (
	// maxQRBytes 是单张二维码图片的上限：正常二维码 5–20KB，512KB 足够且能防超大响应。
	maxQRBytes = 512 << 10
	// qrFetchTimeout 是拉图超时。
	qrFetchTimeout = 5 * time.Second
	// qrMaxRedirects 是允许的重定向跳数（CDN 常见一跳，留一跳余量）。
	qrMaxRedirects = 2
)

// QRMode 表示 qr_ref 的形态，前端据此选择渲染方式。
type QRMode string

const (
	// QRModeImage：qr_ref 是图片地址，后端已代理成 data URL。
	QRModeImage QRMode = "image"
	// QRModeContent：qr_ref 本身就是二维码内容（如 weixin://xxx）。
	// 后端没有二维码生成库（且离线环境无法引入），由前端降级为「复制链接」。
	QRModeContent QRMode = "content"
	// QRModeNone：没有二维码（易支付这类跳转式收银台）。
	QRModeNone QRMode = "none"
)

// QRPayload 是二维码展示数据。
type QRPayload struct {
	Mode    QRMode `json:"mode"`
	DataURL string `json:"data_url,omitempty"`
	Content string `json:"content,omitempty"`
}

// qrClient 是拉图客户端的**测试注入点**，生产恒为「公网 + 限 2 跳」。
var qrClient = func(timeout time.Duration, p netguard.Policy, hops int) *http.Client {
	return netguard.ClientWithRedirects(timeout, p, hops)
}

// ResolveQRFor 按协议决定 qr_ref 的形态。
//
// 为什么必须按协议而不是嗅探 scheme：支付宝当面付返回的 `qr_code` 是
// `https://qr.alipay.com/xxx` —— 它长得像图片地址，实际是**二维码内容**。
// 按 scheme 判断会去拉它，拿回一段 HTML，被 allowedImageType 拒绝后
// 返回 ErrNoPayURL，前端显示「二维码已失效」，支付宝 100% 不可用。
//
// 微信 Native 的 `code_url`（`weixin://wxpay/bizpayurl?pr=...`）没有这个问题，
// 但它同样必须走内容型（后端没有二维码生成库，也不该为此出网）。
func ResolveQRFor(ctx context.Context, protocol Protocol, qrRef string) (*QRPayload, error) {
	qrRef = strings.TrimSpace(qrRef)
	if qrRef == "" {
		return &QRPayload{Mode: QRModeNone}, nil
	}
	// 官方直连的 qr_ref 一律是**内容**：零网络请求是硬要求，
	// 否则既多一个失败模式，又会把用户可见的支付链接发到第三方域名上去。
	switch protocol {
	case ProtocolWxpay, ProtocolAlipay:
		return &QRPayload{Mode: QRModeContent, Content: qrRef}, nil
	}
	if !strings.HasPrefix(strings.ToLower(qrRef), "http://") &&
		!strings.HasPrefix(strings.ToLower(qrRef), "https://") {
		// 内容型（weixin:// 等）：交给前端渲染/降级。
		return &QRPayload{Mode: QRModeContent, Content: qrRef}, nil
	}
	data, ctype, err := fetchQRImage(ctx, qrRef)
	if err != nil {
		return nil, err
	}
	return &QRPayload{
		Mode:    QRModeImage,
		DataURL: "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(data),
	}, nil
}

// fetchQRImage 拉取图片并校验类型与大小。
//
// 为什么不直接把上游地址给前端当 <img src>：
//  1. 上游可能做 Referer 防盗链，浏览器直接加载会 403 裂图；
//  2. 站点是 HTTPS 而上游图片是 HTTP 时，浏览器按混合内容直接拦截；
//  3. <img> 无法携带 Authorization 头（本项目是 Bearer 鉴权，不是 Cookie）。
//
// 用 base64 data URL 而不是二进制代理，是因为 data URL 对前端零成本（img src 直接用），
// 而二维码只有 10–30KB，收银台弹窗一次性使用。这条路径只服务「图片型」qr_ref
// （虎皮椒的 url_qrcode），官方直连永远走内容型，见 ResolveQRFor。
func fetchQRImage(ctx context.Context, raw string) ([]byte, string, error) {
	if err := netguard.ValidateURL(raw, netguard.Policy{}); err != nil {
		return nil, "", ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", ErrNotConfigured
	}
	req.Header.Set("User-Agent", "lingdong-api/payment")

	resp, err := qrClient(qrFetchTimeout, netguard.Policy{}, qrMaxRedirects).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", ErrNoPayURL
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQRBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxQRBytes {
		return nil, "", errors.New("二维码图片超过 512KB")
	}
	ctype := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if !allowedImageType(ctype) {
		// 上游返回的不是图片（可能是 HTML 错误页），不能当图片下发。
		return nil, "", ErrNoPayURL
	}
	return body, ctype, nil
}

// allowedImageType 是允许的图片类型白名单。
//
// **排除 svg**：svg 可以内嵌 <script>，作为 data URL 内联到页面里等于给上游一个 XSS 入口。
func allowedImageType(ctype string) bool {
	switch ctype {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp":
		return true
	}
	return false
}
