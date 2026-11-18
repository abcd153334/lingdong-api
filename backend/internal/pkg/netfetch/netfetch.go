// Package netfetch 是「取回上游产物字节」的唯一实现（同步桥接用）。
//
// 为什么要有这个包：产物下载有三处调用（规格 bridge、内置 duomi 同步生图、
// 以及后续任何同步桥接），每处都自己写一遍「允许有限重定向 + 体积上限 + 状态码判定」，
// 必然漏掉其中一条 —— 漏体积上限就是 OOM，漏重定向策略就是 SSRF 入口。
//
// 两条安全边界写死在这里，不交给调用方：
//  1. 出站一律走 netguard（Dial 期 IP 校验 + 禁止跳到私网）；
//  2. 读取一律 LimitReader 封顶，超限返回 ErrTooLarge 而不是静默截断
//     （截断的音频会被当成完整产物计费，用户拿到半首歌）。
package netfetch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

// ErrTooLarge 表示产物超过调用方给定的字节上限。
var ErrTooLarge = errors.New("产物超过大小上限")

// StatusError 表示上游返回了非 2xx。
//
// 成型而不是直接拼字符串：调用方要把它翻成自己的错误档位
// （产物下载失败给 502，不是渠道配置错误）。
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string { return fmt.Sprintf("上游返回 HTTP %d", e.Code) }

// Options 是一次下载的约束。
type Options struct {
	// Timeout 是整次下载的上限（含重定向）。
	Timeout time.Duration
	// MaxRedirects 是允许的跳转次数：对象存储普遍 302 到 CDN，
	// 但无限跳转会被恶意上游用来把请求引到任意地址。
	MaxRedirects int
	// MaxBytes 是读取上限，<=0 时取 32 MiB。
	MaxBytes int64
}

const defaultMaxBytes = 32 << 20

// Get 取回产物字节与 Content-Type。
//
// 不带任何鉴权头：产物链接通常是 CDN 签名地址，多带一个 Authorization 会被
// 对象存储当成冲突签名直接 403（"看起来是权限问题，其实是多发了一个头"）。
func Get(ctx context.Context, rawURL string, opt Options) ([]byte, string, error) {
	target := strings.TrimSpace(rawURL)
	if target == "" {
		return nil, "", errors.New("产物链接为空")
	}
	limit := opt.MaxBytes
	if limit <= 0 {
		limit = defaultMaxBytes
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	redirects := opt.MaxRedirects
	if redirects < 0 {
		redirects = 0
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", fmt.Errorf("下载产物失败：%w", err)
	}
	client := netguard.ClientWithRedirects(timeout, netguard.PolicyFromEnv(), redirects)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("下载产物失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 错误体很小，读一点便于上游日志对齐，但不透传给调用方（可能含内部地址）。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, "", fmt.Errorf("下载产物失败：%w", &StatusError{Code: resp.StatusCode})
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("下载产物失败：%w", err)
	}
	if int64(len(buf)) > limit {
		return nil, "", ErrTooLarge
	}
	return buf, strings.TrimSpace(resp.Header.Get("Content-Type")), nil
}

// DecodeDataURI 解析 `data:<mime>;base64,<payload>` 形态的内联产物。
//
// 只接受 base64：非 base64 的 data URI 是 URL 编码的**文本**，把它当音频
// 写回客户端会让用户拿到一段看起来像乱码的 JSON。
func DecodeDataURI(raw, fallbackType string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	body := strings.TrimPrefix(strings.TrimSpace(raw), "data:")
	comma := strings.IndexByte(body, ',')
	if comma < 0 {
		return nil, "", errors.New("内联产物的 data URI 格式不对（缺少逗号）")
	}
	meta, payload := body[:comma], body[comma+1:]
	if !strings.Contains(meta, "base64") {
		return nil, "", errors.New("内联产物只支持 base64 编码")
	}
	ctype := strings.TrimSpace(strings.TrimSuffix(meta, ";base64"))
	if ctype == "" {
		ctype = fallbackType
	}
	// 先按 base64 长度粗判上限：解完再判会先申请出超限内存。
	if int64(len(payload))/4*3 > maxBytes+3 {
		return nil, "", ErrTooLarge
	}
	buf, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", fmt.Errorf("内联产物 base64 解码失败：%w", err)
	}
	if int64(len(buf)) > maxBytes {
		return nil, "", ErrTooLarge
	}
	return buf, ctype, nil
}
