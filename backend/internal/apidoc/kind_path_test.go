package apidoc

import (
	"testing"

	"lingdong-api/internal/pkg/wireformat"
)

// TestKindOfPathCoversEveryEndpoint 守住「路径 → Kind」反查与端点表同步。
//
// 反查表由 endpointTable 派生，但**别名**与新增端点最容易漏：漏一条的表现是
// ingress 中间件拿不到格式 → 那条路径的错误体永远是 OpenAI 形状。
func TestKindOfPathCoversEveryEndpoint(t *testing.T) {
	for _, e := range endpointTable {
		if e.t.Path == "" {
			continue
		}
		if _, ok := KindOfPath(e.t.Path); !ok {
			t.Fatalf("路径 %q（%s）反查不到 Kind", e.t.Path, e.kind)
		}
		for _, alias := range e.t.Aliases {
			if _, ok := KindOfPath(alias); !ok {
				t.Fatalf("别名 %q（%s）反查不到 Kind", alias, e.kind)
			}
		}
	}
	// 未登记路径必须返回 false，让调用方按默认（openai）处理。
	if _, ok := KindOfPath("/v1/not-registered"); ok {
		t.Fatal("未登记路径不应命中")
	}
}

// TestProtocolOfKindDefaultsToOpenAI 保证「没声明协议 = openai」。
//
// 这条是零行为变化的底线：所有既有端点都不写 Protocol，
// 一旦 Normalize 把它们判成别的值，全站错误体与流式行为会一起变。
func TestProtocolOfKindDefaultsToOpenAI(t *testing.T) {
	for _, k := range Kinds() {
		want := wireformat.OpenAI
		switch k {
		case KindMessages:
			want = wireformat.Claude
		case KindGeminiModels, KindGeminiGenerate, KindGeminiEmbedContent:
			// Gemini 家族整棵树都是 Google 的形状（错误体、流式帧、字段名）。
			want = wireformat.Gemini
		case KindVideoSubmit, KindVideoTaskGet,
			// /suno/* 是 new-api 的 SUNO 协议面：信封 `{code,message,data}`、
			// 提交回执 data 是任务号字符串、错误码是它的七码。
			KindSunoSubmit, KindSunoFetch, KindSunoFetchBatch:
			want = wireformat.NewAPI
		}
		if got := ProtocolOfKind(k); got != want {
			t.Fatalf("%q 的协议=%q，期望 %q", k, got, want)
		}
	}
	// 未登记 Kind 也回落 openai。
	if got := ProtocolOfKind("not/a-kind"); got != wireformat.OpenAI {
		t.Fatalf("未登记端点应回落 openai，实际 %q", got)
	}
}

// TestKindMessagesIsProtocolVariant 守住「messages 不是新能力」。
//
// EndpointKey 必须为空：一旦它有值，models.endpoints 就会多出一个可勾选项，
// 运营要在两处表达同一件事（能力 + 协议变体），而保存期校验、能力探针、
// 文档判定都要跟着改 —— 多真相源的典型起点。
func TestKindMessagesIsProtocolVariant(t *testing.T) {
	tr, ok := TransportOf(KindMessages)
	if !ok {
		t.Fatal("messages 端点未登记")
	}
	if tr.EndpointKey != "" {
		t.Fatalf("messages 的 EndpointKey 必须为空（它是协议变体，不是新能力），实际 %q", tr.EndpointKey)
	}
	if !tr.Dedicated {
		t.Fatal("messages 必须标 Dedicated：它走适配器专用方法，不是通用转发")
	}
	if tr.Modality != "chat" {
		t.Fatalf("messages 的模态应为 chat，实际 %q", tr.Modality)
	}
	if !tr.Stream {
		t.Fatal("messages 支持流式，Transport.Stream 必须为 true")
	}
}

// TestProtocolVariantsHaveNoEndpointKey 是上面那条的**清单版**。
//
// 协议变体（同一能力的另一种线格式）一律不填 EndpointKey：填了就等于在
// `models.endpoints` 里多出一个可勾选项，运营要在两处表达同一件事。
// 手写清单而不是"遍历所有空 EndpointKey 的端点"：后者会把「查询/取消/元信息」
// 这类本来就不该进能力表的端点也算进来，门禁就变成恒真。
func TestProtocolVariantsHaveNoEndpointKey(t *testing.T) {
	variants := []struct {
		kind Kind
		// dedicated 表示走适配器专用方法（Chat / Embeddings）而不是通用 Forward。
		// compact 是例外：它有真实的上游路径，靠 Forward 直发 —— 但它仍然不是新能力。
		dedicated bool
	}{
		{KindMessages, true},          // chat 的 Anthropic 线格式
		{KindResponsesCompact, false}, // responses 的上下文压缩（同名路径转发）
		{KindGeminiGenerate, true},    // chat 的 Gemini 线格式
		{KindGeminiEmbedContent, true},
	}
	for _, v := range variants {
		tr, ok := TransportOf(v.kind)
		if !ok {
			t.Fatalf("%q 未登记", v.kind)
		}
		if tr.EndpointKey != "" {
			t.Errorf("%q 是协议变体，EndpointKey 必须为空（否则能力表会多一个勾选项），实际 %q",
				v.kind, tr.EndpointKey)
		}
		if tr.Dedicated != v.dedicated {
			t.Errorf("%q 的 Dedicated=%v，期望 %v", v.kind, tr.Dedicated, v.dedicated)
		}
	}
}

// TestSharedPathKeepsPrimaryEndpoint 守住「同路径不同协议靠方法区分」时的归属。
//
// `/v1/models/{model}` 同时是「模型详情」(GET) 的主路径与「Gemini 生成」(POST)
// 的别名。索引只按路径建，所以归属只能靠规则决定 —— 规则必须是
// **主路径优先**，不能是"谁先声明谁赢"：后者会把 GET /v1/models/<id> 判成
// Gemini 协议，于是该端点的错误体变成 `{code,status}` 形状，OpenAI SDK 读不懂。
//
// 这条门禁存在的意义不是"验证某个 Kind"，而是**锁住索引的裁决规则**：
// 一旦有人把两遍扫描改回一遍，这里立刻红。
func TestSharedPathKeepsPrimaryEndpoint(t *testing.T) {
	if k, ok := KindOfPath("/v1/models/gpt-4o"); !ok || k != KindModelsGet {
		t.Fatalf("/v1/models/<id> 应归「模型详情」，实际 %q(ok=%v)", k, ok)
	}
	// 别名本身仍可反查（文档对账要用），但不允许抢主路径。
	if _, ok := KindOfPath("/v1/models/{model}"); !ok {
		t.Fatal("别名路径应可反查")
	}
	// 同一路径的 Gemini 侧仍必须能在 /v1beta 下被认出。
	if k, ok := KindOfPath("/v1beta/models/gemini-x:generateContent"); !ok || k != KindGeminiGenerate {
		t.Fatalf("/v1beta 下的 Gemini 生成应归 KindGeminiGenerate，实际 %q(ok=%v)", k, ok)
	}
}

// TestProtocolOfRequestPathFallsBackToPrefix 覆盖 NoRoute 场景。
//
// `/v1/messages/xxx` 不是已注册端点，但它显然属于 Claude 协议；
// 返回 OpenAI 形状的错误体会让客户端只显示"未知错误"。
func TestProtocolOfRequestPathFallsBackToPrefix(t *testing.T) {
	cases := map[string]wireformat.Format{
		"/v1/messages":              wireformat.Claude,
		"/v1/messages/xxx":          wireformat.Claude,
		"/v1/messages/count_tokens": wireformat.Claude,
		"/v1/chat/completions":      wireformat.OpenAI,
		"/v1/chat/completions/xx":   wireformat.OpenAI,
		"/v1/unknown":               wireformat.OpenAI,
		// /suno 是顶层路径的 SUNO 协议面：拼错的路径也必须拿到同一种信封，
		// 否则客户端解析失败时会说"成功但没有数据"。
		"/suno/submit/music":      wireformat.NewAPI,
		"/suno/submit/not-an-act": wireformat.NewAPI,
		"/suno/fetch":             wireformat.NewAPI,
		"/suno/definitely-nope":   wireformat.NewAPI,
	}
	for path, want := range cases {
		if got := ProtocolOfRequestPath(path); got != want {
			t.Fatalf("%s 的协议=%q，期望 %q", path, got, want)
		}
	}
}

// TestProtocolOfBetaTreeIsGemini 守住「整棵 /v1beta 树都是 Gemini 形状」。
//
// 三个面必须一致：已登记端点、未登记路径、以及唯一例外 /v1beta/openai/*。
// 只要有一面回落 OpenAI，客户端就会在"路径拼错"时拿到一个它读不懂的错误体，
// 而那时候恰恰最需要看懂错误。
func TestProtocolOfBetaTreeIsGemini(t *testing.T) {
	cases := []struct {
		path string
		want wireformat.Format
	}{
		{"/v1beta/models", wireformat.Gemini},
		{"/v1beta/models/gemini-2.0-flash:generateContent", wireformat.Gemini},
		{"/v1beta/models/gemini-2.0-flash:embedContent", wireformat.Gemini},
		{"/v1beta/definitely-not-a-route", wireformat.Gemini}, // 未登记路径
		{"/v1beta/openai/models", wireformat.OpenAI},          // 兼容层例外
		{"/v1beta/openai/nope", wireformat.OpenAI},            // 例外的未登记路径
		{"/v1/chat/completions", wireformat.OpenAI},
	}
	for _, c := range cases {
		if got := ProtocolOfRequestPath(c.path); got != c.want {
			t.Errorf("ProtocolOfRequestPath(%q)=%q，期望 %q", c.path, got, c.want)
		}
	}
}
