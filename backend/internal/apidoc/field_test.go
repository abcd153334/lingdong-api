package apidoc

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// 契约自洽性门禁。这一组测试守的是「契约本身没写错」，与 relay 侧无关。
//
// 为什么必须这么细：契约是投影与运行表单的唯一输入。契约里一个字段名写错，
// 表现不是报错，而是——运行面板少一个控件、Markdown 表格少一行、
// 运营在 model_docs 里写的补充说明永远命不中（V4 直接拒绝保存）。
// 三者都不会崩，但用户看到的是残缺文档。

var fieldNamePattern = regexp.MustCompile(`^[a-z_A-Z][a-zA-Z0-9_]*(\[\])?(\.[a-zA-Z_][a-zA-Z0-9_]*(\[\])?)*$`)

// TestEveryKindHasFieldDoc 守住「契约不缺项」。
//
// 字段契约有两个来源，分工是固定的：
//   - `endpointDocs`：平台自有端点（协议篇 / 通用接口 / 任务读面）；
//   - 各族的 `family_*.go`：厂商协议端点（同一路由在各族是不同协议，必须各写一份）。
//
// 所以判据是「总能找到一份」，而不是「一定在 endpointDocs 里」。
func TestEveryKindHasFieldDoc(t *testing.T) {
	declared := make(map[Kind]bool, len(endpointTable))
	for _, e := range endpointTable {
		declared[e.kind] = true
	}
	have := make(map[Kind]bool, len(endpointDocs))
	for k := range endpointDocs {
		have[k] = true
	}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			have[e.Doc.Kind] = true
		}
	}

	pending := map[Kind]bool{}
	for _, f := range AllFamilyDocs() {
		for _, k := range f.PendingKinds {
			pending[k] = true
		}
	}
	for _, k := range Kinds() {
		if !have[k] && !pending[k] {
			t.Fatalf("端点 %q 在 kind.go 里登记了，但没有任何字段契约（endpointDocs 与 family_*.go 都没有）", k)
		}
	}
	for k := range endpointDocs {
		if !declared[k] {
			t.Fatalf("endpoints.go 里的 %q 在 kind.go 端点表里不存在（删端点时忘了删契约）", k)
		}
	}
}

// TestEndpointDocIntegrity 逐项校验契约写法。
func TestEndpointDocIntegrity(t *testing.T) {
	for _, ent := range AllEndpointDocs() {
		k, doc := ent.Doc.Kind, ent.Spec
		tr, _ := TransportOf(k)

		if strings.TrimSpace(doc.Name) == "" {
			t.Fatalf("%q 缺 Name", k)
		}
		if strings.TrimSpace(doc.Summary) == "" {
			t.Fatalf("%q 缺 Summary", k)
		}
		if doc.Kind != k {
			t.Fatalf("%q 的 Kind 字段写成了 %q", k, doc.Kind)
		}

		seen := make(map[string]bool, len(doc.Fields))
		hasFile := false
		for _, f := range doc.Fields {
			if !fieldNamePattern.MatchString(f.Name) {
				t.Fatalf("%q 的字段 %q 路径写法不合法（只允许 a.b[].c 形式）", k, f.Name)
			}
			if seen[f.Name] {
				t.Fatalf("%q 的字段 %q 重复声明", k, f.Name)
			}
			seen[f.Name] = true

			if strings.TrimSpace(f.Desc) == "" {
				t.Fatalf("%q 的字段 %q 缺说明", k, f.Name)
			}
			if f.Tier == "" {
				t.Fatalf("%q 的字段 %q 缺 Tier", k, f.Name)
			}
			switch f.In {
			case InBody, InQuery, InPath, InForm:
			default:
				t.Fatalf("%q 的字段 %q 的 In 非法：%q", k, f.Name, f.In)
			}
			if f.Required != ReqRequired && f.Required != ReqOptional && f.Required != ReqConditional {
				t.Fatalf("%q 的字段 %q 的 Required 非法：%q", k, f.Name, f.Required)
			}
			// 条件必填必须写清条件：运行面板要在控件旁直接显示这句话，
			// 否则用户看到「条件必填」却不知道条件是什么。
			if f.Required == ReqConditional && strings.TrimSpace(f.RequiredNote) == "" {
				t.Fatalf("%q 的字段 %q 标了条件必填却没写条件", k, f.Name)
			}
			// 静态枚举与模型驱动枚举互斥：两者同时有，运行面板不知道该信谁。
			if f.EnumSource == EnumStatic && len(f.Enum) == 0 {
				t.Fatalf("%q 的字段 %q 标了静态枚举却没给取值", k, f.Name)
			}
			if f.EnumSource.IsModelDriven() && len(f.Enum) > 0 {
				t.Fatalf("%q 的字段 %q 的枚举来自模型事实，不应写死取值", k, f.Name)
			}
			if f.Type == TypeFile {
				hasFile = true
				if f.In != InForm {
					t.Fatalf("%q 的字段 %q 是文件，必须走 form（multipart 才有文件）", k, f.Name)
				}
			}
			// 路径参数的名字必须真的出现在 path 模板里，否则运行面板的
			// 「路径参数」区会显示一个没人使用的输入框。
			if f.In == InPath && !strings.Contains(tr.Path, "{"+f.Name+"}") {
				t.Fatalf("%q 的路径参数 %q 未出现在 path %q 中", k, f.Name, tr.Path)
			}
			// 查询参数只出现在 GET 上（本表没有「POST + query」的端点）。
			if f.In == InQuery && tr.Method != "GET" {
				t.Fatalf("%q 是 %s 端点，却声明了查询参数 %q", k, tr.Method, f.Name)
			}
		}

		// multipart 端点必须有文件字段，且文件字段只能在 multipart 端点上出现。
		if tr.Multipart && !hasFile {
			t.Fatalf("%q 是 multipart 端点，但契约里没有 file 类型字段", k)
		}
		if hasFile && !tr.Multipart {
			t.Fatalf("%q 有 file 字段，但传输契约没标 Multipart（运行面板会按 JSON 发，文件被静默丢掉）", k)
		}

		// 二进制响应的端点，运行面板要么按 blob 渲染、要么干脆不提供试跑。
		if tr.Binary && doc.RunMode != RunBinary && doc.RunMode != RunNone {
			t.Fatalf("%q 的响应是二进制，RunMode 却是 %q", k, doc.RunMode)
		}
		// 标了流式运行模式的端点必须真的支持流式：判据来自契约（Transport.Stream），
		// 不是路径白名单 —— 白名单在新增流式端点时必然漏改（漏加文档报红、
		// 乱加文档骗人），而这条断言的存在意义正是防「文档说能流式、实际不能」。
		if doc.RunMode == RunSSE && !tr.Stream {
			t.Fatalf("%q 标了 SSE 运行模式，但契约未声明 Stream", k)
		}

		for _, r := range doc.Returns {
			if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Desc) == "" {
				t.Fatalf("%q 有返回值字段缺 Name 或 Desc", k)
			}
		}
	}
}

// TestExamplesUseDeclaredFields 是「防幻觉」的第一道闸。
//
// 示例里的每个顶层键都必须是契约声明过的字段。为什么这条重要：示例是运行面板的
// **初值来源**，也是用户直接复制的东西。示例里出现一个契约里没有的键，
// 等于教用户传一个网关不认识的字段——而未知字段会被原样透传给厂商，
// 厂商忽略它、用户以为生效，这类问题最难查。
func TestExamplesUseDeclaredFields(t *testing.T) {
	check := func(k Kind, doc EndpointDoc, label string, ex Obj) {
		if len(ex) == 0 {
			return
		}
		declared := make(map[string]bool, len(doc.Fields))
		for _, f := range doc.Fields {
			declared[f.Name] = true
		}
		for _, kv := range ex {
			if !declared[kv.K] {
				t.Fatalf("%q 的%s用了契约里没有的字段 %q", k, label, kv.K)
			}
		}
	}
	for _, ent := range AllEndpointDocs() {
		k, doc := ent.Doc.Kind, ent.Spec
		check(k, doc, "示例", doc.Example)
		check(k, doc, "第二示例", doc.Example2)
		if len(doc.Example2) > 0 && strings.TrimSpace(doc.ExampleTitle2) == "" {
			t.Fatalf("%q 有第二示例但没给场景名（页面上会显示成没有标题的代码块）", k)
		}
	}
}

// TestExampleModelFirst 守住示例的键序：model 必须在第一位。
//
// 用户复制示例时最先看的是「我要填哪个模型」，把它排在 prompt 后面
// 就等于让每一份文档的示例都以 prompt 开头。Obj 保序就是为了这条能成立。
func TestExampleModelFirst(t *testing.T) {
	for _, ent := range AllEndpointDocs() {
		k, doc := ent.Doc.Kind, ent.Spec
		if len(doc.Example) == 0 {
			continue
		}
		// 没有 model 字段的接口不适用这条：批量查询、取消这类接口根本没有模型可填，
		// 强行要求示例以 model 开头只会逼人写一个上传上去就报错的字段。
		if !declaresModelField(doc.Fields) {
			continue
		}
		if doc.Example[0].K != "model" {
			t.Fatalf("%q 的示例第一个键是 %q，应为 model", k, doc.Example[0].K)
		}
	}
}

// declaresModelField 判断这一页是否真的有 model 参数。
func declaresModelField(fields []Field) bool {
	for _, f := range fields {
		if f.Name == "model" {
			return true
		}
	}
	return false
}

// TestObjRoundTripKeepsOrder 守住 Obj 的序列化契约。
//
// 用 Decoder.Token 逐键读回，而不是先解进 map：解 map 会丢键序，
// 那样「键序没变」这条断言永远为真，等于没测。
func TestObjRoundTripKeepsOrder(t *testing.T) {
	src := Obj{
		{K: "model", V: "m1"},
		{K: "prompt", V: "p"},
		{K: "meta", V: Obj{{K: "a", V: 1}, {K: "b", V: 2}}},
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var back Obj
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(back) != 3 || back[0].K != "model" || back[1].K != "prompt" || back[2].K != "meta" {
		t.Fatalf("键序被打乱：%+v", back)
	}
	sub, ok := back[2].V.(Obj)
	if !ok || len(sub) != 2 || sub[0].K != "a" || sub[1].K != "b" {
		t.Fatalf("嵌套对象键序被打乱：%+v", back[2].V)
	}

	// With 只替换、不改原对象、不改变位置。
	replaced := src.With("model", "m2")
	if got, _ := replaced.Get("model"); got != "m2" {
		t.Fatalf("With 未替换：%v", got)
	}
	if got, _ := src.Get("model"); got != "m1" {
		t.Fatalf("With 修改了原对象：%v", got)
	}
	if replaced[0].K != "model" {
		t.Fatalf("With 改变了键序")
	}
	// 不存在的键插到最前（新增字段时也要保证 model 仍在首位只能靠它自己已存在）
	if got, _ := src.With("brand_new", 1).Get("brand_new"); got != 1 {
		t.Fatalf("With 未插入新键")
	}
}
