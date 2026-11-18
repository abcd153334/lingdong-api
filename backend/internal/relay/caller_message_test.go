package relay

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"lingdong-api/internal/model"
)

// 本文件覆盖「调用方可见文案」的唯一出口：CallerMessage / CallerMessageOf。
//
// 背景：适配器会把上游 4xx 响应体片段拼进 CallerParamError.Msg（排障需要），
// 而这段原文常带着 `Authorization: Bearer sk-...` 与上游主机名。同步链路此前
// 直接 `WithMessage(callerErr.Msg)` 下发，等于把用户密钥与平台供应链写给对方看。

func chWithBase(base string) *model.Channel {
	return &model.Channel{BaseURL: base}
}

// TestCallerMessageRedactsHostAndKey 守「响应里看不到密钥与上游主机名」。
func TestCallerMessageRedactsHostAndKey(t *testing.T) {
	ch := chWithBase("https://api.internal-proxy.example.com/v1")
	key := "sk-live-0123456789abcdef"
	raw := "多米API 拒绝了本次参数（HTTP 400）：" + key +
		" from https://api.internal-proxy.example.com/v1/images/generations"

	got := CallerMessage(raw, ch, key)
	for _, leak := range []string{key, "api.internal-proxy.example.com"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q 未被脱敏：%q", leak, got)
		}
	}
	// 保留可读信息：状态码与错误类别必须还在，否则用户无法自查。
	if !strings.Contains(got, "HTTP 400") || !strings.Contains(got, "拒绝了本次参数") {
		t.Fatalf("可读部分被破坏：%q", got)
	}
}

// TestCallerMessageTruncatesByRune 守「按字符截断」，不切断 UTF-8。
func TestCallerMessageTruncatesByRune(t *testing.T) {
	long := strings.Repeat("图", callerMessageMaxRunes+200)
	got := CallerMessage(long, nil, "")
	if n := len([]rune(got)); n != callerMessageMaxRunes {
		t.Fatalf("应截到 %d 个字符，实际 %d", callerMessageMaxRunes, n)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatal("截断不应产生非法 UTF-8 序列")
	}
}

// TestCallerMessageNilChannelStillRedacts nil 渠道/空密钥时仍做通用形态脱敏。
//
// 异步链路拿不到渠道上下文：此时若直接返回原文，等于「上层以为脱敏了、实际没有」。
func TestCallerMessageNilChannelStillRedacts(t *testing.T) {
	raw := "上游返回：Authorization: Bearer sk-abcdefgh12345678"
	got := CallerMessage(raw, nil, "")
	if strings.Contains(got, "sk-abcdefgh12345678") {
		t.Fatalf("无渠道上下文时仍必须脱敏：%q", got)
	}
}

// TestCallerMessageOfUnwrapsAndPrefersMsg 守「取 Msg 而不是 Error()」。
//
// 提交链路会把错误再包一层（`fmt.Errorf("%w：%s", ...)`）；直接 err.Error()
// 会让用户看到「提交失败：参数不合法」这种没有信息量的前缀句。
func TestCallerMessageOfUnwrapsAndPrefersMsg(t *testing.T) {
	ch := chWithBase("https://up.example.com")
	ce := &CallerParamError{Msg: "nano 模型不支持 quality；画质请用 image_size"}
	wrapped := fmt.Errorf("提交失败：%w", ce)

	if got := CallerMessageOf(wrapped, ch, ""); got != ce.Msg {
		t.Fatalf("应取下层 Msg，实际 %q", got)
	}
	// 非 CallerParamError 时退回 Error()，且仍脱敏。
	if got := CallerMessageOf(errors.New("boom sk-abcdefgh12345678"), nil, ""); strings.Contains(got, "sk-abcdefgh12345678") {
		t.Fatalf("普通错误也必须脱敏：%q", got)
	}
	if got := CallerMessageOf(nil, nil, ""); got != "" {
		t.Fatalf("nil error 应返回空串，实际 %q", got)
	}
}
