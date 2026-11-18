package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	specdsl "lingdong-api/internal/relay/spec"
)

// 本文件覆盖"严格解析失败 → 可行动中文"这一层。
//
// 它存在的理由很具体：模型把 slug 写在 data 顶层时，过去只得到
// `json: unknown field "slug"` —— 不知道写哪、也不知道还允许什么，
// 于是整份重写、再撞一次。这些用例锁死"一次报全 + 指路"。

func TestUnknownFieldTopLevel(t *testing.T) {
	var d AdapterSpecData
	err := decodeEntity(json.RawMessage(`{"label":"L","slug":"x","spec":{"slug":"x"}}`), &d, "data", EntityAdapterSpec)
	fe := asFieldSpecError(err)
	if fe == nil {
		t.Fatalf("应为 FieldSpecError，实际 %T(%v)", err, err)
	}
	if fe.Kind != "unknown_field" {
		t.Fatalf("Kind 应为 unknown_field，实际 %s", fe.Kind)
	}
	if fe.Path != "data" {
		t.Fatalf("Path 应为 data，实际 %s", fe.Path)
	}
	if !reflect.DeepEqual(fe.Unknown, []string{"slug"}) {
		t.Fatalf("Unknown 应为 [slug]，实际 %v", fe.Unknown)
	}
	// 允许字段来自结构体反射：必须齐全且有序（否则模型看到的是缺项清单）。
	if !reflect.DeepEqual(fe.Allowed, []string{"label", "spec", "vendor"}) {
		t.Fatalf("Allowed 应为 [label spec vendor]，实际 %v", fe.Allowed)
	}
	msg := fe.Message()
	for _, want := range []string{`不认识的字段 "slug"`, "允许的字段：label、spec、vendor", "operations[].key"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("文案缺少 %q：%s", want, msg)
		}
	}
}

func TestUnknownFieldsReportedAllAtOnce(t *testing.T) {
	var d ChannelData
	err := decodeEntity(json.RawMessage(`{"name":"c","adapter_id":"a","base_url":"u","api_key":"k","token":"t"}`),
		&d, "data", EntityChannel)
	fe := asFieldSpecError(err)
	if fe == nil {
		t.Fatalf("应为 FieldSpecError，实际 %v", err)
	}
	if len(fe.Unknown) != 2 {
		t.Fatalf("应一次报出两个未知键，实际 %v", fe.Unknown)
	}
	// 两条都要有指路，而不是只说"不认识"。
	msg := fe.Message()
	if !strings.Contains(msg, "keys") {
		t.Fatalf("缺少密钥应写 keys 的提示：%s", msg)
	}
}

func TestUnknownFieldNestedBindings(t *testing.T) {
	var d BindingData
	err := decodeEntity(json.RawMessage(`{"bindings":[{"upstream_model":"m","channel_id":1,"weightx":3}]}`),
		&d, "data", EntityModelBindings)
	fe := asFieldSpecError(err)
	if fe == nil {
		t.Fatalf("应为 FieldSpecError，实际 %v", err)
	}
	if fe.Path != "data.bindings[0]" {
		t.Fatalf("嵌套路径应带下标，实际 %s", fe.Path)
	}
	if !reflect.DeepEqual(fe.Unknown, []string{"weightx"}) {
		t.Fatalf("Unknown 应为 [weightx]，实际 %v", fe.Unknown)
	}
}

func TestUnknownFieldInSpecDSL(t *testing.T) {
	raw := json.RawMessage(`{"slug":"a-b","label":"L","endpoints":{"chat/completions":{"method":"POST","path":"/v1/chat","authx":{}}}}`)
	fe := unknownFieldsIn(raw, reflect.TypeOf(specdsl.Spec{}), "data.spec", EntityAdapterSpec)
	if fe == nil {
		t.Fatal("应报出 spec 内的未知键")
	}
	if fe.Path != "data.spec.endpoints.chat/completions" {
		t.Fatalf("二级路径不对：%s", fe.Path)
	}
	if !reflect.DeepEqual(fe.Unknown, []string{"authx"}) {
		t.Fatalf("Unknown 应为 [authx]，实际 %v", fe.Unknown)
	}
}

func TestTypeMismatchIsChinese(t *testing.T) {
	var d ChannelData
	err := decodeEntity(json.RawMessage(`{"name":123,"adapter_id":"a","base_url":"u"}`), &d, "data", EntityChannel)
	fe := asFieldSpecError(err)
	if fe == nil {
		t.Fatalf("应为 FieldSpecError，实际 %v", err)
	}
	if fe.Kind != "type_mismatch" {
		t.Fatalf("Kind 应为 type_mismatch，实际 %s", fe.Kind)
	}
	if !strings.HasPrefix(fe.Path, "data.name") {
		t.Fatalf("Path 应指到具体字段，实际 %s", fe.Path)
	}
	if !strings.Contains(fe.Message(), "需要 string 类型") {
		t.Fatalf("文案应说明需要什么类型：%s", fe.Message())
	}
}

func TestNotObjectPayload(t *testing.T) {
	var d VendorData
	err := decodeEntity(json.RawMessage(`[1,2]`), &d, "data", EntityVendor)
	fe := asFieldSpecError(err)
	if fe == nil || fe.Kind != "not_object" {
		t.Fatalf("数组载荷应报 not_object，实际 %v", err)
	}
}

// TestMisplacedHints 锁死"错位表里的每一条都能命中"——
// 否则加了一条提示却拼错键名，它会永远沉默。
func TestMisplacedHints(t *testing.T) {
	for entity, m := range misplacedHints {
		for key := range m {
			if got := hintFor(entity, []string{key}, "data"); got == "" {
				t.Errorf("%s.%s 的提示查不到", entity, key)
			}
		}
	}
}
