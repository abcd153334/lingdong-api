package apidoc

import (
	"encoding/json"
	"testing"
)

func TestEndpointTableIntegrity(t *testing.T) {
	seenKind := make(map[Kind]bool, len(endpointTable))
	seenRoute := make(map[string]Kind, len(endpointTable))
	seenKey := make(map[string]Kind, len(endpointTable))

	for _, e := range endpointTable {
		if seenKind[e.kind] {
			t.Fatalf("Kind 重复：%s", e.kind)
		}
		seenKind[e.kind] = true

		if e.t.Path == "" {
			t.Fatalf("%s 缺 Path", e.kind)
		}
		if e.t.Method == "" {
			t.Fatalf("%s 缺 Method", e.kind)
		}
		switch e.t.Method {
		case "GET", "POST", "DELETE":
		default:
			t.Fatalf("%s 的 Method %q 不在允许集合内", e.kind, e.t.Method)
		}
		// 同一 (method, path) 只能对应一个 Kind：Path 相同而语义不同的是两个操作
		// （如 POST/GET /v1/video/generations），Kind 必须分开，否则文档会把两件事混写。
		route := e.t.Method + " " + e.t.Path
		if prev, dup := seenRoute[route]; dup {
			t.Fatalf("路由重复：%s 同时属于 %s 与 %s", route, prev, e.kind)
		}
		seenRoute[route] = e.kind

		if e.t.EndpointKey != "" {
			if key := NormalizeEndpointKey(e.t.EndpointKey); key != e.t.EndpointKey {
				t.Fatalf("%s 的 EndpointKey %q 未归一（应为 %q）", e.kind, e.t.EndpointKey, key)
			}
			if prev, dup := seenKey[e.t.EndpointKey]; dup {
				t.Fatalf("EndpointKey 重复：%q 同时属于 %s 与 %s", e.t.EndpointKey, prev, e.kind)
			}
			seenKey[e.t.EndpointKey] = e.kind
		}
		for _, a := range e.t.Aliases {
			if a == "" || a == e.t.Path {
				t.Fatalf("%s 的别名 %q 非法", e.kind, a)
			}
		}
	}
}

func TestTaskEndpointKeysMatchesCapacityChecks(t *testing.T) {
	// 顺序 = 端点表声明顺序（分组顺序），语义上无序：消费方只做等值匹配。
	got := TaskEndpointKeys()
	want := map[string]bool{
		"video/generations":   true,
		"video/regenerations": true,
		// 图像是「同路径可同步可异步」：它必须进这个集合，否则 SubmitTaskOf 的闸门会把
		// async:true 判成「端点不支持任务」，而保存期又因为同步能力而放过 —— 保存成功、
		// 首次异步调用报配置错。
		"images/generations":      true,
		"audio/music/generations": true,
		"audio/lyrics":            true,
	}
	if len(got) != len(want) {
		t.Fatalf("任务端点数量 %d，期望 %d：%v", len(got), len(want), got)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("多出任务端点 %q：%v", k, got)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("缺少任务端点：%v", want)
	}
}

func TestNormalizeEndpointKey(t *testing.T) {
	cases := map[string]string{
		"video/generations":     "video/generations",
		"/video/generations":    "video/generations",
		"v1/video/generations":  "video/generations",
		"/v1/video/generations": "video/generations",
		"  /v1/images/edits  ":  "images/edits",
		"":                      "",
	}
	for in, want := range cases {
		if got := NormalizeEndpointKey(in); got != want {
			t.Errorf("NormalizeEndpointKey(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNormalizeModality(t *testing.T) {
	cases := map[string]string{
		"":        "chat",
		"  ":      "chat",
		"chat":    "chat",
		" video ": "video",
		"music":   "music",
	}
	for in, want := range cases {
		if got := NormalizeModality(in); got != want {
			t.Errorf("NormalizeModality(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 端点只认**显式声明**：没有任何兜底。
//
// 兜底（image → generations+edits、audio → speech+transcriptions）会让
// 没填端点的模型凭空"支持"两个接口：whisper 出现在语音合成页、
// gpt-image-1 出现在"上传图片来编辑"页 —— 用户照着调必然失败，
// 而文档页面看起来完全正常。
func TestDeclaredEndpoints(t *testing.T) {
	t.Run("显式声明归一去重保序", func(t *testing.T) {
		raw := json.RawMessage(`["/v1/images/generations","images/generations","images/edits"]`)
		got := DeclaredEndpoints(raw)
		want := []string{"images/generations", "images/edits"}
		if len(got) != len(want) {
			t.Fatalf("得到 %v，期望 %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("得到 %v，期望 %v", got, want)
			}
		}
	})

	t.Run("未声明就是空", func(t *testing.T) {
		for _, modality := range []string{"video", "image", "", "nope"} {
			if got := DeclaredEndpoints(nil); len(got) != 0 {
				t.Fatalf("%q 未声明端点时应为空（不允许按模态兜底），得到 %v", modality, got)
			}
		}
	})

	t.Run("脏值跳过", func(t *testing.T) {
		got := DeclaredEndpoints(json.RawMessage(`["", "  ", "/v1/audio/speech"]`))
		if len(got) != 1 || got[0] != "audio/speech" {
			t.Fatalf("空写法应被跳过，得到 %v", got)
		}
	})
}

// TestDefaultEndpointsDoesNotNormalizeEmpty 守一个刻意的行为边界：
// DefaultEndpoints 不做「空串 → chat」的归一，因为网关目录用它派生端点，
// 一旦把空模态当 chat，/v1/models 会给没配模态的模型凭空多出两个端点。
func TestDefaultEndpointsDoesNotNormalizeEmpty(t *testing.T) {
	if got := DefaultEndpoints(""); got != nil {
		t.Fatalf("空模态应返回 nil（归一由调用方决定），得到 %v", got)
	}
	if got := DefaultEndpoints("  "); got != nil {
		t.Fatalf("空白模态应返回 nil，得到 %v", got)
	}
}

func TestKindOfEndpointKey(t *testing.T) {
	for _, raw := range []string{"video/generations", "/v1/video/generations"} {
		k, ok := KindOfEndpointKey(raw)
		if !ok || k != KindVideoSubmit {
			t.Fatalf("KindOfEndpointKey(%q) = %v,%v", raw, k, ok)
		}
	}
	// 查询类端点不进 models.endpoints，反查不到是正确语义。
	if _, ok := KindOfEndpointKey("tasks/list"); ok {
		t.Fatal("tasks/list 不应可反查")
	}
	if _, ok := KindOfEndpointKey(""); ok {
		t.Fatal("空串不应可反查")
	}
}
