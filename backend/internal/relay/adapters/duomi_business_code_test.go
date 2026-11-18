package adapters

import (
	"errors"
	"testing"

	"lingdong-api/internal/relay"
)

// 本文件守「业务码 → 归因」的映射。
//
// 缺陷现场：多米把鉴权/限流失败放在 HTTP 200 的信封里（`{"code":401,"msg":"invalid key"}`）。
// 改造前 duomiBusinessError 对 401/403/429 回裸 error，relay 拿不到状态码，一律
// 记成「上游故障」：一枚坏 key 能把整条渠道打成禁用，而且该 key 永不冷却。

// TestDuomiBusinessCodeCarriesStatus 401/403/429 必须带状态码，402 必须不带。
func TestDuomiBusinessCodeCarriesStatus(t *testing.T) {
	cases := []struct {
		code     int
		wantHTTP int
		caller   bool
	}{
		{401, 401, false},
		{403, 403, false},
		{429, 429, false},
		// 402 刻意保持裸 error：欠费是渠道级问题，要累计失败把渠道熔断下线，
		// 而不是只冷却某一枚 key。
		{402, 0, false},
		// 其它业务码（可灵 1201/1203）是调用方/业务拒绝，不能记渠道故障。
		{1201, 0, true},
		{1203, 0, true},
	}
	for _, c := range cases {
		err := duomiBusinessError(c.code, "上游原话")
		if got := relay.StatusOf(err); got != c.wantHTTP {
			t.Fatalf("code=%d：StatusOf=%d，期望 %d", c.code, got, c.wantHTTP)
		}
		if got := errors.Is(err, relay.ErrCallerParam); got != c.caller {
			t.Fatalf("code=%d：CallerParam=%v，期望 %v", c.code, got, c.caller)
		}
	}
}

// TestDuomiBusinessCodeKeepsCopy 文案必须逐字保留（管理台渠道测试与用户可见 502 会展示）。
func TestDuomiBusinessCodeKeepsCopy(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want string
	}{
		{401, "invalid key", "适配器 duomiapi：鉴权失败（业务码 401），请检查渠道密钥：invalid key"},
		{402, "no balance", "适配器 duomiapi：上游余额不足（业务码 402），请充值后重试：no balance"},
		{429, "slow down", "适配器 duomiapi：上游限流（业务码 429）：slow down"},
		{1201, "bad param", "多米API 拒绝了本次参数（业务码 1201）：bad param"},
	}
	for _, c := range cases {
		if got := duomiBusinessError(c.code, c.msg).Error(); got != c.want {
			t.Fatalf("code=%d 文案变了：\n got %q\nwant %q", c.code, got, c.want)
		}
	}
}

// TestDuomiAuthFailureCoolsKeyNotChannel 端到端归因：业务 401 必须落到 key 冷却，
// 而不是累计渠道失败。
func TestDuomiAuthFailureCoolsKeyNotChannel(t *testing.T) {
	err := duomiBusinessError(401, "invalid key")
	// relay.classify 的行为由 relay 包自己测；这里只守「状态码能被取到」这一前提，
	// 因为归因规则唯一实现在 relay（adapters 不复制一份）。
	if s := relay.StatusOf(err); s != 401 {
		t.Fatalf("业务 401 必须携带状态码供 relay 归因，实际 %d", s)
	}
}
