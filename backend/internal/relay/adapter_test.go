package relay

import (
	"context"
	"errors"
	"testing"
)

// fakeAdapter 是测试用适配器。
type fakeAdapter struct{ name string }

func (f fakeAdapter) Name() string                               { return f.name }
func (f fakeAdapter) Caps() []string                             { return []string{"text"} }
func (f fakeAdapter) Test(context.Context, *ChannelConfig) error { return nil }
func (f fakeAdapter) Chat(context.Context, *ChannelConfig, *RelayRequest) (*RelayResponse, error) {
	return nil, nil
}
func (f fakeAdapter) Embeddings(context.Context, *ChannelConfig, *RelayRequest) (*RelayResponse, error) {
	return nil, nil
}
func (f fakeAdapter) Completions(context.Context, *ChannelConfig, *RelayRequest) (*RelayResponse, error) {
	return nil, nil
}
func (f fakeAdapter) StreamChat(context.Context, *ChannelConfig, *RelayRequest, func(SSEFrame) error) (*StreamResult, error) {
	return nil, nil
}
func (f fakeAdapter) Forward(context.Context, *ChannelConfig, *RelayRequest, string) (*RawResponse, error) {
	return nil, nil
}

// TestRegisterAndGet 校验注册后可取用。
func TestRegisterAndGet(t *testing.T) {
	if err := RegisterAdapter(fakeAdapter{name: "test-a"}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	a, ok := GetAdapter("test-a")
	if !ok {
		t.Fatal("应能取到已注册适配器")
	}
	if a.Name() != "test-a" {
		t.Fatalf("取回的适配器名称不符: %s", a.Name())
	}
}

// TestRegisterDuplicate 校验同名重复注册被拒绝（不静默覆盖）。
func TestRegisterDuplicate(t *testing.T) {
	if err := RegisterAdapter(fakeAdapter{name: "test-dup"}); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}
	err := RegisterAdapter(fakeAdapter{name: "test-dup"})
	if !errors.Is(err, ErrAdapterRegistered) {
		t.Fatalf("重复注册应返回 ErrAdapterRegistered，实际: %v", err)
	}
}

// TestRegisterInvalid 校验空适配器/空名称被拒绝。
func TestRegisterInvalid(t *testing.T) {
	if err := RegisterAdapter(nil); err == nil {
		t.Fatal("nil 适配器应报错")
	}
	if err := RegisterAdapter(fakeAdapter{name: ""}); err == nil {
		t.Fatal("空名称适配器应报错")
	}
}

// TestGetMissing 校验未注册名称返回 false。
func TestGetMissing(t *testing.T) {
	if _, ok := GetAdapter("not-exist-xyz"); ok {
		t.Fatal("未注册适配器不应命中")
	}
}
