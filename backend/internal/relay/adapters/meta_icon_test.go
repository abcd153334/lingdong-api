package adapters_test

import (
	"testing"

	"lingdong-api/internal/assets/brandicon"
	"lingdong-api/internal/relay"
	"lingdong-api/internal/relay/adapters"
)

// TestBuiltinMetaCoversRegistry 是 adapters/meta.go 注释里承诺的那条守卫
// （此前只存在于注释，实际没有实现）。
//
// 为什么放在 adapters 的**外部测试**而不是 relay 包里：`relay` 的测试夹具会用
// RegisterAdapter 注册 test-a / test-dup，而注册表是全局的、两者都写进
// builtinNames —— 在 relay 包里断言「每个内置适配器都有 Meta」必然被夹具打红。
// 外部测试包只编译被测包的非测试文件，注册表是干净的生产清单。
//
// 两个方向都要守，缺一个的症状都是「管理台安静地显示错东西」：
//   - 注册了适配器却没有 Meta ⇒ 左栏与下拉显示英文 id；
//   - Meta 写了图库不存在的 key ⇒ iconref 把令牌换算成 ""（它拒绝给出必然 404
//     的地址），前端退首字占位，页面看起来"只是没图标"，没人会去查拼写。
//
// 用 brandicon.Lookup（目录里存在）而不是 ValidBuiltinKey（只校验字符格式：
// `openai-gtp` 能通过格式校验，但目录里没有这张图）。
func TestBuiltinMetaCoversRegistry(t *testing.T) {
	names := make([]string, 0, 8)
	for _, a := range relay.Builtins() {
		names = append(names, a.Name())
	}
	if len(names) == 0 {
		t.Fatal("注册表里没有任何内置适配器：测试本身失去意义")
	}
	for _, name := range names {
		meta, ok := adapters.MetaOf(name)
		if !ok {
			t.Errorf("内置适配器 %q 没有 Meta：管理台会显示英文 id", name)
			continue
		}
		if meta.Label == "" {
			t.Errorf("内置适配器 %q 的 Meta.Label 为空", name)
		}
	}

	for name, meta := range adapters.Metas() {
		if meta.Icon == "" {
			continue // 显式声明「没有合适的图标」，允许
		}
		key, ok := brandicon.BuiltinKey(meta.Icon)
		if !ok {
			t.Errorf("适配器 %q 的 Icon=%q 不是合法的 builtin 令牌", name, meta.Icon)
			continue
		}
		if _, ok := brandicon.Lookup(key); !ok {
			t.Errorf("适配器 %q 的 Icon 指向图库不存在的 key=%q（会被静默换成首字占位）", name, key)
		}
	}
}
