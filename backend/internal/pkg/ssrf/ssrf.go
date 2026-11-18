// Package ssrf 提供外部素材 URL 的 SSRF 防护校验（v2.0 §4.7.6 R8）。
//
// 实现已收敛到 internal/pkg/netguard：本包只保留「公网默认策略」的薄包装与对外错误语义，
// 避免同一套 IP 判定在仓库里出现两份。需要放行私网（自建上游）的场景请直接用 netguard.Policy。
package ssrf

import (
	"context"
	"errors"
	"net/http"
	"time"

	"lingdong-api/internal/pkg/netguard"
)

// ErrInvalidURL 表示 URL 不是可用的 http/https 公网地址。
var ErrInvalidURL = errors.New("ssrf: URL 不是可用的公网地址")

// ValidatePublicURL 校验 URL 为 http/https 公网地址：
// 拒绝非 http(s) 协议、私网/环回/链路本地/未指定/组播地址，以及 localhost / 无点主机名 / .local / .internal 域名。
// 说明：本步仅做字面量校验（不触发 DNS 解析）。
func ValidatePublicURL(raw string) error {
	if err := netguard.ValidateURL(raw, netguard.Policy{}); err != nil {
		return ErrInvalidURL
	}
	return nil
}

// ValidatePublicURLResolved 在字面量校验之上追加 DNS 解析后 IP 校验：
// 任一解析结果落在私网/环回/链路本地等非公网地址即拒绝（防 DNS rebinding）。
// 域名解析失败按不合法处理（无法确认目标可达性）。
func ValidatePublicURLResolved(ctx context.Context, raw string) error {
	if err := netguard.ValidateURLResolved(ctx, raw, netguard.Policy{}); err != nil {
		return ErrInvalidURL
	}
	return nil
}

// SafeClient 返回外部资源下载客户端：禁止跟随重定向，且在建连时校验对端 IP（解析后校验，防 rebinding）。
// Transport 按超时档缓存复用（netguard 内部维护），不会每次请求重建连接池。
func SafeClient(timeout time.Duration) *http.Client {
	return netguard.Client(timeout, netguard.Policy{})
}
