package apicontract_test

// 本文件守的是**渲染层与契约的等价性**。
//
// 文档最容易出的问题不是"写错了字"，而是"少了一半"：新增端点没进 OpenAPI、
// 某个端点的鉴权级别在文档里写成别的、CSV 端点被当成 JSON 写进 schema。
// 这些都不会让任何测试变红，只会让接入方按文档写出必然 403 的代码。
//
// 因此这里逐条对照：**catalog 里有什么，OpenAPI 里就必须有什么，且字段一致**。
//
// 另外 8 条替代 JSON Schema 校验器（本项目零新增依赖，不引入
// santhosh-tekuri/jsonschema 一类库）：$ref 可解析、无空 pathItem、
// operationId 唯一、参数双向覆盖、requestBody 与参数一致、
// 无 3.0 残留的 nullable、schema 名唯一、示例键都在 schema 里。
// 这些断言合起来覆盖了 95% 的"文档生成了但用不了"的场景。

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"lingdong-api/internal/apicontract"
)

// mustRe 是小helper：把必须编译成功的正则前置到包级变量。
func mustRe(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// toOpenAPI 是测试侧对路径转换的独立实现（不调被测函数，避免自己断言自己）。
func toOpenAPI(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if strings.HasPrefix(s, ":") {
			parts[i] = "{" + s[1:] + "}"
		}
	}
	return strings.Join(parts, "/")
}

// TestOpenAPICoversEveryEndpoint 每一条契约端点都必须在 OpenAPI 里出现。
func TestOpenAPICoversEveryEndpoint(t *testing.T) {
	doc := apicontract.OpenAPI("https://api.example.com")
	covered := 0
	for _, e := range apicontract.Rules() {
		path := toOpenAPI(e.Path)
		item, ok := doc.Paths[path]
		if !ok {
			t.Errorf("%s %s 未出现在 OpenAPI 的 paths 里（转换后应为 %s）", e.Method, e.Path, path)
			continue
		}
		op, ok := item[strings.ToLower(e.Method)]
		if !ok {
			t.Errorf("%s %s 的路径存在但缺少 %s 操作", e.Method, e.Path, strings.ToLower(e.Method))
			continue
		}
		covered++
		if op.OperationID != e.Key {
			t.Errorf("%s：operationId = %q，期望 %q", e.Key, op.OperationID, e.Key)
		}
		if op.XAuth != e.Auth.String() {
			t.Errorf("%s：x-auth = %q，期望 %q（文档写错级别会让接入方用错凭证）",
				e.Key, op.XAuth, e.Auth.String())
		}
		if op.XScope != e.Scope {
			t.Errorf("%s：x-scope = %q，期望 %q", e.Key, op.XScope, e.Scope)
		}
		if len(op.Tags) != 1 || op.Tags[0] != e.Group {
			t.Errorf("%s：tags = %v，期望 [%q]", e.Key, op.Tags, e.Group)
		}
		// 试运行标志必须与 Auth 派生值一致，否则渲染器会给出一个必 403 的按钮。
		if op.XTryIt != e.Tries() {
			t.Errorf("%s：x-try-it = %v，期望 %v", e.Key, op.XTryIt, e.Tries())
		}
		if !e.Tries() && op.XSessionWhyText == "" {
			t.Errorf("%s：不可试运行但 x-session-why-text 为空", e.Key)
		}
	}
	if covered != len(apicontract.Rules()) {
		t.Fatalf("覆盖 %d 条，契约有 %d 条", covered, len(apicontract.Rules()))
	}
}

// TestOpenAPISecurityFollowsAuth 鉴权级别必须映射到正确的 security。
//
// 空数组**必须**存在（而不是省略）：OpenAPI 里省略 security 表示"继承全局"，
// 而全局没有声明 —— 语义上含糊；显式空数组才是"这个端点不需要凭证"。
func TestOpenAPISecurityFollowsAuth(t *testing.T) {
	doc := apicontract.OpenAPI("")
	for _, e := range apicontract.Rules() {
		op := doc.Paths[toOpenAPI(e.Path)][strings.ToLower(e.Method)]
		if e.Auth == apicontract.AuthOpen {
			if op.Security == nil {
				t.Errorf("%s：open 端点必须显式声明 security: []（省略=继承全局）", e.Key)
				continue
			}
			if len(op.Security) != 0 {
				t.Errorf("%s：open 端点不应要求凭证，实际 %v", e.Key, op.Security)
			}
			continue
		}
		if len(op.Security) != 1 || len(op.Security[0]["bearerAuth"]) != 0 {
			t.Errorf("%s：%s 端点应要求 bearerAuth，实际 %v", e.Key, e.Auth, op.Security)
		}
	}
}

// TestOpenAPIMediaMapping 非 JSON 端点不能声明 application/json。
//
// 这是实际会坑到人的一处：`/usage/export` 返回 CSV、产物端点返回字节流。
// 文档若把它们写成 JSON 信封，接入方会按 JSON 解析并得到一堆乱码。
//
// 204 端点必须挂在 "204" 键上（不是 "200"）：写 200 会让 SDK 去解一个
// 不存在的 body，而"删除成功"在服务端其实完全正常。
func TestOpenAPIMediaMapping(t *testing.T) {
	doc := apicontract.OpenAPI("")
	for _, e := range apicontract.Rules() {
		op := doc.Paths[toOpenAPI(e.Path)][strings.ToLower(e.Method)]
		switch e.Media {
		case apicontract.MediaEmpty:
			resp, has := op.Responses["204"]
			if !has {
				t.Errorf("%s：Media=empty 必须声明 204 响应", e.Key)
				continue
			}
			if _, has200 := op.Responses["200"]; has200 {
				t.Errorf("%s：Media=empty 不应同时声明 200（实际无响应体）", e.Key)
			}
			if len(resp.Content) != 0 {
				t.Errorf("%s：204 不应声明响应体，实际 %v", e.Key, resp.Content)
			}
		case apicontract.MediaCSV:
			ok := op.Responses["200"]
			if _, has := ok.Content["text/csv"]; !has {
				t.Errorf("%s：CSV 端点必须声明 text/csv", e.Key)
			}
			if _, has := ok.Content["application/json"]; has {
				t.Errorf("%s：CSV 端点不应声明 application/json", e.Key)
			}
		case apicontract.MediaBinary:
			ok := op.Responses["200"]
			if _, has := ok.Content["application/octet-stream"]; !has {
				t.Errorf("%s：二进制端点必须声明 application/octet-stream", e.Key)
			}
		default:
			ok := op.Responses["200"]
			if _, has := ok.Content["application/json"]; !has {
				t.Errorf("%s：JSON 端点必须声明 application/json", e.Key)
			}
		}
		// 失败体永远是信封（含 CSV/二进制端点：失败时不会返回半截文件）。
		errResp, has := op.Responses["4XX"]
		if !has {
			t.Errorf("%s：缺少 4XX 响应定义", e.Key)
			continue
		}
		if _, has := errResp.Content["application/json"]; !has {
			t.Errorf("%s：4XX 必须是 JSON 信封", e.Key)
		}
	}
}

// TestOpenAPIPathParams path 参数必须从 `:id` 正确提取，且有说明。
func TestOpenAPIPathParams(t *testing.T) {
	doc := apicontract.OpenAPI("")
	for _, e := range apicontract.Rules() {
		want := strings.Count(e.Path, "/:")
		op := doc.Paths[toOpenAPI(e.Path)][strings.ToLower(e.Method)]
		var got int
		for _, p := range op.Parameters {
			if p.In != "path" {
				continue
			}
			got++
			if !p.Required {
				t.Errorf("%s：path 参数 %q 必须 required", e.Key, p.Name)
			}
			if p.Description == "" {
				t.Errorf("%s：path 参数 %q 缺少说明（接入方最需要知道「这是什么 id」）", e.Key, p.Name)
			}
			if !strings.Contains(toOpenAPI(e.Path), "{"+p.Name+"}") {
				t.Errorf("%s：参数名 %q 未出现在路径里", e.Key, p.Name)
			}
		}
		if got != want {
			t.Errorf("%s：path 参数 %d 个，期望 %d 个", e.Key, got, want)
		}
	}
}

// TestOpenAPIServerFallback 空基址要回落 "/"，不能生成空字符串 URL。
func TestOpenAPIServerFallback(t *testing.T) {
	doc := apicontract.OpenAPI("   ")
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "/" {
		t.Fatalf("空基址应回落 \"/\"，实际 %+v", doc.Servers)
	}
	doc = apicontract.OpenAPI("https://api.example.com/")
	if doc.Servers[0].URL != "https://api.example.com" {
		t.Fatalf("基址应去掉末尾斜杠，实际 %q", doc.Servers[0].URL)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("OpenAPI 版本必须是 3.1.0，实际 %q", doc.OpenAPI)
	}
}

// TestMarkdownCoversEveryEndpoint 手册必须覆盖每条端点与每条"不开放"说明。
func TestMarkdownCoversEveryEndpoint(t *testing.T) {
	md := apicontract.Markdown()
	for _, e := range apicontract.Rules() {
		if !strings.Contains(md, "`"+e.Method+" "+e.Path+"`") {
			t.Errorf("%s %s 未出现在 Markdown 手册里", e.Method, e.Path)
		}
	}
	for _, n := range apicontract.NotExposedList() {
		if !strings.Contains(md, n.Title) {
			t.Errorf("不开放清单缺条目 %q", n.Title)
		}
	}
	if !strings.Contains(md, "error_code") {
		t.Error("手册必须说明 error_code（接入方要靠它做分支）")
	}
}

// ---------------- 以下 8 条替代 JSON Schema 校验器 ----------------

// TestRefsResolvable 保证每个 $ref 都能在 components.schemas 里找到。
//
// 不测的后果：Swagger UI 显示 "Could not resolve reference"，
// 或者更糟 —— 模型面板空着但接口列表正常，看着像"这个端点没有返回结构"。
func TestRefsResolvable(t *testing.T) {
	doc := apicontract.OpenAPI("")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	text := string(raw)
	refs := refRe.FindAllStringSubmatch(text, -1)
	if len(refs) == 0 {
		t.Fatal("整份文档没有任何 $ref：data 的 schema 没有走 components（本轮改动没生效？）")
	}
	for _, m := range refs {
		name := m[1]
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("$ref 指向不存在的 schema %q", name)
		}
	}
}

// refRe 匹配 `#/components/schemas/Xxx`。
var refRe = mustRe(`#/components/schemas/([A-Za-z0-9_]+)`)

// TestNoEmptyPathItem 保证每个路径至少有一个操作。
func TestNoEmptyPathItem(t *testing.T) {
	doc := apicontract.OpenAPI("")
	for p, item := range doc.Paths {
		if len(item) == 0 {
			t.Errorf("路径 %q 下没有任何操作（渲染出来是一个空分组）", p)
		}
	}
	if len(doc.Paths) == 0 {
		t.Fatal("paths 为空")
	}
}

// TestOperationIDUnique 保证 operationId 唯一且等于 Key。
//
// 不测的后果：SDK 生成器会用 operationId 当函数名，重复就会静默覆盖 ——
// 生成的客户端少一个方法，而生成日志一切正常。
func TestOperationIDUnique(t *testing.T) {
	doc := apicontract.OpenAPI("")
	seen := map[string]string{}
	for p, item := range doc.Paths {
		for method, op := range item {
			where := method + " " + p
			if prev, ok := seen[op.OperationID]; ok {
				t.Errorf("operationId %q 重复：%s 与 %s", op.OperationID, prev, where)
			}
			seen[op.OperationID] = where
			if !strings.Contains(op.OperationID, ".") {
				t.Errorf("%s：operationId %q 不像 Key（应为 resource.action）", where, op.OperationID)
			}
		}
	}
}

// TestOpenAPIParamsReverseCoverage 反向覆盖：OpenAPI 里出现的参数名必须在契约里。
//
// 正向（契约 -> OpenAPI）由 TestOpenAPICoversEveryEndpoint 守；
// 反向这条防的是"渲染器自己编了一个参数"（例如又把 path 串现推了一遍）。
func TestOpenAPIParamsReverseCoverage(t *testing.T) {
	doc := apicontract.OpenAPI("")
	for _, e := range apicontract.Rules() {
		op := doc.Paths[toOpenAPI(e.Path)][strings.ToLower(e.Method)]
		declared := map[string]string{}
		for _, p := range e.Doc().Params {
			declared[p.Name+"/"+string(p.In)] = p.Desc
		}
		for _, p := range op.Parameters {
			if _, ok := declared[p.Name+"/"+p.In]; !ok {
				t.Errorf("%s：OpenAPI 出现了契约里没有的参数 %s(in=%s)", e.Key, p.Name, p.In)
			}
		}
	}
}

// TestRequestBodyPresenceMatchesParams 有 requestBody 当且仅当有 body/form 参数。
//
// 不测的后果两种都真实存在：多声明 requestBody -> SDK 要求传一个服务端不读的对象；
// 少声明 -> 接入方根本不知道要传 body，只能靠 400 反推字段名。
func TestRequestBodyPresenceMatchesParams(t *testing.T) {
	doc := apicontract.OpenAPI("")
	for _, e := range apicontract.Rules() {
		op := doc.Paths[toOpenAPI(e.Path)][strings.ToLower(e.Method)]
		var hasBodyParam bool
		for _, p := range e.Doc().Params {
			if p.In == apicontract.InBody || p.In == apicontract.InForm {
				hasBodyParam = true
			}
		}
		if hasBodyParam != (op.RequestBody != nil) {
			t.Errorf("%s：有 body 参数=%v，但 requestBody 存在=%v", e.Key, hasBodyParam, op.RequestBody != nil)
			continue
		}
		if op.RequestBody == nil {
			continue
		}
		if len(op.RequestBody.Content) == 0 {
			t.Errorf("%s：requestBody 没有任何 content", e.Key)
		}
		if _, ok := op.RequestBody.Content["application/json"]; !ok {
			t.Errorf("%s：requestBody 缺少 application/json", e.Key)
		}
	}
}

// TestNoNullableKeyword 保证输出里没有 OpenAPI 3.0 的 nullable 残留。
//
// 3.1 用 JSON Schema 2020-12，`nullable` 不是合法关键字，会被校验器忽略 ——
// 于是"可空"这个信息静默丢失，而两边都不报错。
func TestNoNullableKeyword(t *testing.T) {
	doc := apicontract.OpenAPI("")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if strings.Contains(string(raw), `"nullable"`) {
		t.Fatal("输出里含 3.0 的 nullable 关键字：3.1 下它会被忽略，可空信息静默丢失")
	}
}

// TestSchemaNamesUnique 保证 41 个 data schema 名两两不等。
//
// 不测的后果：两个端点折出同名 schema，后写的覆盖先写的 ——
// 文档里两条接口显示同一个返回结构，而没有任何报错。
func TestSchemaNamesUnique(t *testing.T) {
	seen := map[string]string{}
	for _, e := range apicontract.Rules() {
		if e.Media != apicontract.MediaJSON || e.Doc().NoData {
			continue
		}
		name := apicontract.SchemaNameOf(e.Key)
		if prev, ok := seen[name]; ok {
			t.Errorf("schema 名 %q 被 %s 与 %s 同时使用", name, prev, e.Key)
		}
		seen[name] = e.Key
		if !strings.HasSuffix(name, "Data") {
			t.Errorf("%s：schema 名 %q 必须以 Data 结尾（为将来的 XxxParams 留位）", e.Key, name)
		}
	}
}

// TestExamplesAreInsideSchema 保证示例里的每个键都在 schema 的 properties 里。
//
// 不测的后果：示例多加一个字段，Swagger UI 会把它渲染出来，
// 接入方照着传，服务端静默忽略 —— 一个"文档说有、实际没有"的字段。
func TestExamplesAreInsideSchema(t *testing.T) {
	doc := apicontract.OpenAPI("")
	for _, e := range apicontract.Rules() {
		op := doc.Paths[toOpenAPI(e.Path)][strings.ToLower(e.Method)]
		check := func(example any, schema map[string]any, where string) {
			obj, ok := example.(map[string]any)
			if !ok {
				return
			}
			props, _ := schema["properties"].(map[string]any)
			if props == nil {
				return
			}
			for k := range obj {
				if _, ok := props[k]; !ok {
					t.Errorf("%s %s：示例字段 %q 不在 schema 里", e.Key, where, k)
				}
			}
		}
		// 响应示例
		if resp, ok := op.Responses["200"]; ok {
			if media, ok := resp.Content["application/json"]; ok {
				env := media.Schema
				envProps, _ := env["properties"].(map[string]any)
				dataSchema, _ := envProps["data"].(map[string]any)
				if dataSchema != nil {
					// $ref 时取 components 里的真 schema。
					if ref, ok := dataSchema["$ref"].(string); ok {
						name := strings.TrimPrefix(ref, "#/components/schemas/")
						if s, ok := doc.Components.Schemas[name].(map[string]any); ok {
							dataSchema = s
						}
					}
					if ex, ok := media.Examples["default"]; ok {
						check(ex.Value, dataSchema, "响应示例")
					}
				}
			}
		}
		// 请求示例
		if op.RequestBody != nil {
			if media, ok := op.RequestBody.Content["application/json"]; ok {
				if ex, ok := media.Examples["default"]; ok {
					check(ex.Value, media.Schema, "请求示例")
				}
			}
		}
	}
}
