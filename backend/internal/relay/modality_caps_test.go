package relay

import "testing"

// TestModalityCapsKeysAreDeclaredCaps 防止 modalityCaps 里写出适配器永远不会声明的能力键。
//
// 这类漂移的后果是「校验静默失效」：键名拼错 → Caps 永远不命中 → 悄悄退回端点探针，
// 而端点探针正是本次要绕开的那个缺陷（异步模态没有同步端点）。
//
// 注意：本文件只放**纯数据**断言。凡是要真的查适配器的用例都不能放在 relay 包里 ——
// adapters 包 import relay（注册发生在它的 init），relay 反向 import 会构成循环，
// 因此 relay 的测试二进制里**一个适配器都没注册**，AdapterCaps 一律返回 nil。
// 行为用例见 adapters/modality_caps_test.go。
func TestModalityCapsKeysAreDeclaredCaps(t *testing.T) {
	// 平台 Caps 枚举（plugin/manifest.go 的 8 键 + 决策 H 新增的 music）。
	known := map[string]bool{
		"text": true, "image": true, "edit_image": true, "video": true,
		"tts": true, "asr": true, "embedding": true, "passthrough": true,
		"music": true,
	}
	for modality, caps := range modalityCaps {
		if len(caps) == 0 {
			t.Fatalf("模态 %s 没有登记任何能力键", modality)
		}
		for _, c := range caps {
			if !known[c] {
				t.Fatalf("模态 %s 登记了未知能力键 %q（拼错会让校验静默退回端点探针）", modality, c)
			}
		}
	}
}

// TestAdapterCapsUnknownAdapter 未注册适配器返回 nil，不 panic。
func TestAdapterCapsUnknownAdapter(t *testing.T) {
	if got := AdapterCaps("definitely-not-registered"); got != nil {
		t.Fatalf("未注册适配器应返回 nil，实际 %v", got)
	}
}
