package relay

import (
	"context"
	"errors"
	"testing"
)

// stubAdapter 是最小 Adapter 实现（只用于解析器用例）。
type stubAdapter struct{ name string }

func (s stubAdapter) Name() string                             { return s.name }
func (stubAdapter) Caps() []string                             { return []string{"text"} }
func (stubAdapter) Test(context.Context, *ChannelConfig) error { return nil }
func (stubAdapter) Chat(context.Context, *ChannelConfig, *RelayRequest) (*RelayResponse, error) {
	return nil, errors.New("stub")
}
func (stubAdapter) Embeddings(context.Context, *ChannelConfig, *RelayRequest) (*RelayResponse, error) {
	return nil, errors.New("stub")
}
func (stubAdapter) Completions(context.Context, *ChannelConfig, *RelayRequest) (*RelayResponse, error) {
	return nil, errors.New("stub")
}
func (stubAdapter) StreamChat(context.Context, *ChannelConfig, *RelayRequest, func(SSEFrame) error) (*StreamResult, error) {
	return nil, errors.New("stub")
}
func (stubAdapter) Forward(context.Context, *ChannelConfig, *RelayRequest, string) (*RawResponse, error) {
	return nil, errors.New("stub")
}

// TestGetAdapterLazyResolve 验证惰性解析：注册表未命中时问一次注入的解析器，成功后零 IO。
func TestGetAdapterLazyResolve(t *testing.T) {
	var calls int
	SetAdapterResolver(func(name string) (Adapter, error) {
		calls++
		if name == "spec:lazy" {
			return stubAdapter{name: name}, nil
		}
		return nil, &AdapterResolveError{Kind: ResolveNotFound, Name: name}
	})
	t.Cleanup(func() {
		SetAdapterResolver(nil)
		ForgetAdapter("spec:lazy")
		ForgetAdapter("spec:missing")
	})

	a, ok := GetAdapter("spec:lazy")
	if !ok || a == nil || a.Name() != "spec:lazy" {
		t.Fatalf("应惰性解析出适配器，实际 ok=%v a=%v", ok, a)
	}
	if calls != 1 {
		t.Fatalf("解析器应被调用 1 次，实际 %d", calls)
	}
	// 解析成功即入注册表：后续请求不得再打解析器（否则每个请求都要查库 + 反序列化规格 JSON）。
	for i := 0; i < 3; i++ {
		if _, ok := GetAdapter("spec:lazy"); !ok {
			t.Fatal("已解析的适配器应持续可用")
		}
	}
	if calls != 1 {
		t.Fatalf("解析成功后再取应零 IO，实际解析器被调用 %d 次", calls)
	}

	// 未注册的名字：解析失败后进入 5s 负缓存，不应反复打库
	if _, ok := GetAdapter("spec:missing"); ok {
		t.Fatal("不存在的规格不应解析成功")
	}
	before := calls
	for i := 0; i < 5; i++ {
		if _, ok := GetAdapter("spec:missing"); ok {
			t.Fatal("负缓存期内也不应解析成功")
		}
	}
	if calls != before {
		t.Fatalf("负缓存期内不应再调用解析器，实际新增 %d 次", calls-before)
	}
}

// TestForgetAdapterClearsNegativeCache 守住「新建规格后保存渠道必定 400」这个坑。
//
// 场景：getadapter(spec:new) 发生在规格落库之前（例如渠道配置页先探测能力），
// 失败被记进 5s 负缓存；随后规格建成，若不显式 ForgetAdapter，
// 保存渠道时 adapterRegistered 仍判 false → 运营看到「适配器不存在或未注册」并反复重试。
func TestForgetAdapterClearsNegativeCache(t *testing.T) {
	ready := false
	var calls int
	SetAdapterResolver(func(name string) (Adapter, error) {
		calls++
		if name == "spec:fresh" && ready {
			return stubAdapter{name: name}, nil
		}
		return nil, &AdapterResolveError{Kind: ResolveNotFound, Name: name}
	})
	t.Cleanup(func() {
		SetAdapterResolver(nil)
		ForgetAdapter("spec:fresh")
	})

	if _, ok := GetAdapter("spec:fresh"); ok {
		t.Fatal("规格未就绪时不应解析成功")
	}
	// 负缓存生效：即使 ready 变 true，也还不该成功（这就是那个坑）
	ready = true
	if _, ok := GetAdapter("spec:fresh"); ok {
		t.Fatal("负缓存期内不应重新解析（否则本用例测不到 ForgetAdapter 的必要性）")
	}

	ForgetAdapter("spec:fresh")
	before := calls
	a, ok := GetAdapter("spec:fresh")
	if !ok || a == nil {
		t.Fatal("ForgetAdapter 之后必须能立即解析（不允许等满 5s）")
	}
	if calls != before+1 {
		t.Fatalf("ForgetAdapter 后应恰好重新解析 1 次，实际 %d 次", calls-before)
	}
}

// TestForgetAdapterDropsRegistered 验证 ForgetAdapter 同时清掉已注册实例（规格被改/停用后必须换新）。
func TestForgetAdapterDropsRegistered(t *testing.T) {
	name := "spec:dropme"
	if err := RegisterAdapter(stubAdapter{name: name}); err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	if _, ok := GetAdapter(name); !ok {
		t.Fatal("刚注册的适配器应可取到")
	}
	ForgetAdapter(name)
	if _, ok := GetAdapter(name); ok {
		t.Fatal("ForgetAdapter 之后不应再取到旧实例")
	}
}

// TestGetAdapterWithoutResolver 验证未注入解析器时行为与旧版一致（只查注册表）。
func TestGetAdapterWithoutResolver(t *testing.T) {
	SetAdapterResolver(nil)
	if _, ok := GetAdapter("spec:whatever"); ok {
		t.Fatal("没有解析器时不应凭空拿到适配器")
	}
}

// TestGetAdapterEmptyName 验证空名直接失败（不触发解析、不打库）。
func TestGetAdapterEmptyName(t *testing.T) {
	var calls int
	SetAdapterResolver(func(name string) (Adapter, error) {
		calls++
		return nil, &AdapterResolveError{Kind: ResolveNotFound, Name: name}
	})
	t.Cleanup(func() { SetAdapterResolver(nil) })
	if _, ok := GetAdapter(""); ok {
		t.Fatal("空名字不应解析成功")
	}
	if calls != 0 {
		t.Fatalf("空名字不应触发解析，实际调用 %d 次", calls)
	}
}
