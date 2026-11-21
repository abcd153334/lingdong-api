package relay

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// timeoutErr 复刻 net 包的连接级超时（实现 net.Error 且 Timeout()==true）。
type wrapTimeoutErr struct{}

func (wrapTimeoutErr) Error() string   { return "i/o timeout" }
func (wrapTimeoutErr) Timeout() bool   { return true }
func (wrapTimeoutErr) Temporary() bool { return true }

func TestIsTimeoutError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context 截止", context.DeadlineExceeded, true},
		{"net 包截止", os.ErrDeadlineExceeded, true},
		{"传输层超时", wrapTimeoutErr{}, true},
		{"包装后的超时", errors.Join(errors.New("dial"), context.DeadlineExceeded), true},
		{"上游明确报错", errors.New("upstream 400: bad request"), false},
		{"客户端取消", context.Canceled, false},
	}
	for _, tc := range cases {
		if got := IsTimeoutError(tc.err); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWrapImageTimeout 锁住「图片超时不得换渠道重跑」这条资金规则。
func TestWrapImageTimeout(t *testing.T) {
	waited := ImageEndpointTimeout

	// 图片 + 超时 → 包成 slow 类错误，让 dispatch 走 409 + submit_ambiguous 而不是 failover。
	got := wrapImageTimeout(kindImagesGenerations, wrapTimeoutErr{}, false, waited)
	var imgTO *ImageGenTimeoutError
	if !errors.As(got, &imgTO) {
		t.Fatalf("图片超时应包成 ImageGenTimeoutError，实际 %T", got)
	}
	if imgTO.Waited != waited {
		t.Fatalf("Waited=%v，期望 %v", imgTO.Waited, waited)
	}
	// Class()=="slow" 是「不换 key、不熔断渠道」的唯一依据，必须仍然成立。
	if cls := imgTO.Class(); cls != "slow" {
		t.Fatalf("Class()=%q，期望 slow", cls)
	}

	// 已包过的不重复包（否则外层 Waited 会覆盖内层真实值）。
	inner := &ImageGenTimeoutError{Waited: 3 * time.Minute}
	if got := wrapImageTimeout(kindImagesEdits, inner, false, waited); got != inner {
		t.Fatal("已包的 ImageGenTimeoutError 不应被重新包装")
	}

	// chat 超时保持原样：token 计价的端点换渠道是正确行为。
	if got := wrapImageTimeout(kindChat, wrapTimeoutErr{}, false, waited); errors.As(got, &imgTO) {
		t.Fatal("chat 超时不应被包成 ImageGenTimeoutError（会让正常 failover 失效）")
	}

	// 客户端已断开：那是 499 语义，不能伪装成"可能已生成"。
	if got := wrapImageTimeout(kindImagesGenerations, wrapTimeoutErr{}, true, waited); errors.As(got, &imgTO) {
		t.Fatal("客户端断开时不应包成 ImageGenTimeoutError")
	}

	// 非超时错误原样返回。
	plain := errors.New("upstream 500")
	if got := wrapImageTimeout(kindImagesGenerations, plain, false, waited); got != plain {
		t.Fatal("非超时错误不应被包装")
	}
}

func TestRaiseTimeoutFor(t *testing.T) {
	cases := []struct {
		name string
		kind endpointKind
		want time.Duration
	}{
		{"出图", kindImagesGenerations, ImageEndpointTimeout},
		{"改图", kindImagesEdits, ImageEndpointTimeout},
		{"变体", kindImagesVariations, ImageEndpointTimeout},
		{"TTS", kindAudioSpeech, AudioEndpointTimeout},
		{"转写", kindAudioTranscriptions, AudioEndpointTimeout},
		{"翻译", kindAudioTranslations, AudioEndpointTimeout},
		{"chat 保持默认", kindChat, DefaultAdapterTimeout},
		{"embeddings 保持默认", kindEmbeddings, DefaultAdapterTimeout},
	}
	for _, tc := range cases {
		cfg := &ChannelConfig{Timeout: DefaultAdapterTimeout}
		cfg.raiseTimeoutFor(tc.kind)
		if cfg.Timeout != tc.want {
			t.Fatalf("%s: Timeout=%v，期望 %v", tc.name, cfg.Timeout, tc.want)
		}
	}

	// 调用方显式配的更长时间不能被压回去。
	cfg := &ChannelConfig{Timeout: 30 * time.Minute}
	cfg.raiseTimeoutFor(kindImagesGenerations)
	if cfg.Timeout != 30*time.Minute {
		t.Fatalf("更长的显式配置被覆盖成 %v", cfg.Timeout)
	}

	// 超时值必须小于反代读取超时（960s），否则 nginx 先回 504、我们还在后台等。
	if ImageEndpointTimeout >= 960*time.Second {
		t.Fatalf("ImageEndpointTimeout=%v 不得 ≥ 反代 960s", ImageEndpointTimeout)
	}
}
