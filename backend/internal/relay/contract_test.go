package relay

// B0 契约对账：relay 侧对 internal/apidoc 的依赖必须是「派生」，不能是「碰巧一致」。
//
// 这一组测试守的不是功能，而是**第二真相源**。B0 之前 relay 里躺着三份
// 与端点表重复的东西：`endpointSpec` 的路径字面量、`endpointModality` 的
// switch、`TaskEndpointPaths` 的数组。它们的问题不是"现在错了"，而是
// "改一处漏一处不会报错"——
//   - 路径漏改 → path_overrides 的键永远命中不了，运营配了覆盖毫无作用；
//   - 模态漏改 → 模型广场显示支持、实际路由 404；
//   - 异步清单漏改 → 声明错路径的模型被跳过能力校验，绑到不支持的渠道也能保存成功。

import (
	"os"
	"strings"
	"testing"

	"lingdong-api/internal/apidoc"
)

// allEndpointKinds 是 endpointKind 的完整枚举，顺序与 relay.go 的 iota 声明一致。
//
// 刻意手写而不是用 range：iota 是整数，range 到不了它；而这份清单本身要被
// TestEveryRelayKindMapsToContract 用来做**双向**覆盖（多一项、少一项都红），
// 所以它必须在测试里显式存在——新增端点时忘了更新它，测试会立刻报数量不符。
func allEndpointKinds() []endpointKind {
	return []endpointKind{
		kindChat,
		kindCompletions,
		kindEmbeddings,
		kindImagesGenerations,
		kindImagesEdits,
		kindImagesVariations,
		kindAudioSpeech,
		kindAudioTranscriptions,
		kindAudioTranslations,
		kindModerations,
		kindRerank,
		kindResponses,
		kindMessages,
		kindResponsesCompact,
		kindGeminiGenerate,
		kindGeminiEmbedContent,
	}
}

// TestEveryRelayKindMapsToContract 守住 docKindOf 与 endpointKind 的双向覆盖。
//
// 单向覆盖不够：只查「每个 kind 都能查到映射」会漏掉 docKindOf 里多出来的
// 脏键（比如删了某个 kind 但忘了删映射），而那种脏键会让 relayPathSet
// 多出一个不该入账的路径。
func TestEveryRelayKindMapsToContract(t *testing.T) {
	kinds := allEndpointKinds()

	if len(kinds) != len(docKindOf) {
		t.Fatalf("文档契约映射项数 %d 与端点枚举项数 %d 不一致：新增端点必须同时登记 apidoc 与 docKindOf",
			len(docKindOf), len(kinds))
	}

	declared := make(map[endpointKind]bool, len(kinds))
	for _, k := range kinds {
		declared[k] = true
	}

	usedDocKind := make(map[apidoc.Kind]endpointKind, len(docKindOf))
	for _, k := range kinds {
		dk, ok := docKindOf[k]
		if !ok {
			t.Fatalf("端点 %d 缺少文档契约映射（docKindOf）", int(k))
		}
		if prev, dup := usedDocKind[dk]; dup {
			t.Fatalf("端点 %d 与 %d 映射到同一个文档契约 %q", int(prev), int(k), dk)
		}
		usedDocKind[dk] = k

		tr, ok := apidoc.TransportOf(dk)
		if !ok {
			t.Fatalf("文档契约 %q 未在 apidoc 端点表登记", dk)
		}
		// 每个 relay 端点都要能打到上游，所以必须有一条真实路径；
		// 空路径会让 BuildURL 拼出上游根地址 —— 请求打到错的地址，且错得很安静。
		if tr.Path == "" {
			t.Fatalf("文档契约 %q 没有上游路径，relay 端点无法构建请求", dk)
		}
		if tr.Method == "" {
			t.Fatalf("文档契约 %q 没有 HTTP 方法", dk)
		}
		// 这条断言是"有人把派生改回手写 switch"的探测器。
		if got, want := endpointModality(k), tr.Modality; got != want {
			t.Fatalf("端点 %d 模态 %q 与契约 %q 不一致", int(k), got, want)
		}
		if got, want := endpointPathOf(k), tr.EndpointKey; got != want {
			t.Fatalf("端点 %d 端点标识 %q 与契约 %q 不一致", int(k), got, want)
		}
	}

	for k := range docKindOf {
		if !declared[k] {
			t.Fatalf("docKindOf 中存在枚举外的键 %d（删端点时忘了删映射）", int(k))
		}
	}
}

// TestDedicatedKindsUseAdapterPath 守住「专用方法 vs 通用转发」的分界。
//
// 这是全量 500 那一类事故的唯一防线：`invokeEndpoint` 用「path 是否为空」
// 决定走适配器专用方法还是 Forward。chat 在 apidoc 里必须记真实路径（文档要展示
// POST /v1/chat/completions），一旦 Dedicated 忘了标，specOf 就会把真实路径交给
// Forward，适配器收到它不认识的路径 → 所有对话请求 500。
// 反过来，同步转发端点若被误标 Dedicated，specOf 返回空路径 → Forward 无路可走。
func TestDedicatedKindsUseAdapterPath(t *testing.T) {
	for _, k := range allEndpointKinds() {
		dk := docKindOf[k]
		tr, ok := apidoc.TransportOf(dk)
		if !ok {
			t.Fatalf("文档契约 %q 未登记", dk)
		}
		spec := specOf(k)

		if tr.Dedicated {
			if spec.path != "" {
				t.Fatalf("专用端点 %q 的 spec.path 必须为空（否则会被当成通用转发），实际 %q", dk, spec.path)
			}
			continue
		}
		if spec.path != tr.Path {
			t.Fatalf("转发端点 %q 的 spec.path 应为 %q，实际 %q", dk, tr.Path, spec.path)
		}
		if spec.multipart != tr.Multipart {
			t.Fatalf("端点 %q 的 multipart 标记与契约不一致", dk)
		}
		if spec.binary != tr.Binary {
			t.Fatalf("端点 %q 的 binary 标记与契约不一致", dk)
		}
		if spec.modality != tr.Modality {
			t.Fatalf("端点 %q 的 modality 与契约不一致", dk)
		}
	}
}

// TestRelayPathSetCoversSyncKinds 守住「预检被拒的请求必须入账」。
//
// relayPathSet 的成员决定了哪些 401/403 会落一条 rejected 记录。
// 漏一个端点 = 该端点的鉴权失败在调用记录里查不到，用户报障时无从对账。
func TestRelayPathSetCoversSyncKinds(t *testing.T) {
	for _, k := range allEndpointKinds() {
		dk := docKindOf[k]
		tr, ok := apidoc.TransportOf(dk)
		if !ok {
			t.Fatalf("文档契约 %q 未登记", dk)
		}
		if !isRelayPath(tr.Path) {
			t.Fatalf("端点 %q 的路径 %q 不在 isRelayPath 集合里，它的预检失败会静默不入账", dk, tr.Path)
		}
	}
	// 元数据端点不该入账（404 是正常行为，记进去只会污染失败率）。
	if isRelayPath(apidoc.MustPath(apidoc.KindModelsList)) {
		t.Fatal("/v1/models 不是网关转发端点，不应计入 rejected")
	}
}

// TestNoPathLiteralInRelay 守住「路径字面量只存在于 apidoc」。
//
// 扫描范围是 internal/relay 顶层的非测试源码。adapters 子包豁免：
// 那里有厂商私有路径（如多米上游的 /v1/tasks），不属于平台对外端点契约。
//
// 为什么用源码扫描这种"笨办法"：路径字面量一旦重新长出来，没有任何运行时
// 行为会变化 —— 错误要等到运营改了 path_overrides 才发现「配了没生效」。
// 只在编译/测试期拦截，才有意义。
//
// 整行 `//` 注释跳过：注释里本来就需要举出真实路径来解释语义
// （如 endpointPathOf 说明「库里可能写成 /v1/video/generations」）。
// 只跳过**整行**注释而不是行尾 `//` 之后的部分，是因为行尾截断会把
// `"http://host/v1/x"` 这类字符串里的 `//` 当注释起点，反而漏掉真字面量。
func TestNoPathLiteralInRelay(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取 relay 目录失败: %v", err)
	}

	// 双引号与反引号两种形态都要拦。用 rune 拼而不是直接写字面量，
	// 否则本文件自己就成了「源码里出现 /v1/ 字面量」的反例。
	quote, backtick := string(rune(34)), string(rune(96))
	needles := []string{quote + "/v1/", backtick + "/v1/"}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, n := range needles {
				if strings.Contains(line, n) {
					t.Fatalf("%s:%d 出现端点路径字面量 %s：路径的唯一定义处是 internal/apidoc，relay 必须派生",
						name, i+1, n)
				}
			}
		}
	}
}

// TestTaskEndpointPathsDerivedFromContract 守住异步端点清单的唯一真相源。
//
// modality_task_paths_test.go 里的期望值是**另一条**独立的规格断言（防的是
// apidoc 自己写错），这里守的是"relay 没有自己另立一份"。
func TestTaskEndpointPathsDerivedFromContract(t *testing.T) {
	want := apidoc.TaskEndpointKeys()
	if len(want) == 0 {
		t.Fatal("apidoc 未派生任何能力校验端点，TaskEndpointPaths 会退化成空集合（能力校验静默失效）")
	}
	if len(TaskEndpointPaths) != len(want) {
		t.Fatalf("项数不一致：relay=%d apidoc=%d", len(TaskEndpointPaths), len(want))
	}
	set := make(map[string]bool, len(want))
	for _, p := range want {
		set[p] = true
	}
	for _, p := range TaskEndpointPaths {
		if !set[p] {
			t.Fatalf("relay 的异步端点 %q 不在 apidoc 契约里", p)
		}
	}
	for _, p := range want {
		if !ProbeEndpointPath(p).Known {
			t.Fatalf("契约里的异步端点 %q 无法被探针识别", p)
		}
	}
}
