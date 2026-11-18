package apicontract

// 本文件把契约渲染成 **OpenAPI 3.1**。
//
// 为什么不是"手写一份 openapi.yaml 再对照契约"：那会立刻变成第二份真相源
// （改契约忘改 yaml，接入方按 yaml 联调、按 403 调试）。这里只从 catalog +
// details 派生，契约改了文档必然跟着改，不需要任何对账。
//
// 输出刻意保持**稳定顺序**：分组顺序 → 组内声明顺序 → 路径字典序。
// 文档要能进版本库 diff，随机顺序会让每次导出都"全文件变更"。
//
// 3.1 而不是 3.0：3.1 用 JSON Schema 2020-12 描述 schema，`type: ["string","null"]`
// 可以表达可空，不需要 3.0 的 `nullable` 扩展。混用两种写法时 Swagger UI
// 会"部分字段不显示"，而校验器不会报错（扩展字段被忽略），排查成本极高。
// 因此 `TestNoNullableKeyword` 直接断言序列化结果里不含 `"nullable"`。

import (
	"sort"
	"strconv"
	"strings"

	"lingdong-api/internal/pkg/errs"
)

// openAPIVersion 是生成的 OpenAPI 版本号。
const openAPIVersion = "3.1.0"

// OpenAPIDoc 是渲染结果（顶层结构固定，便于 json.Marshal 稳定输出）。
type OpenAPIDoc struct {
	OpenAPI string          `json:"openapi"`
	Info    OpenAPIInfo     `json:"info"`
	Servers []OpenAPIServer `json:"servers"`
	// Security 为空：具体安全要求写在每个 operation 上（本 API 同时存在
	// "无凭证""任一种 Bearer""仅会话票"三种，用全局 security 表达不了）。
	Tags       []OpenAPITag               `json:"tags"`
	Paths      map[string]OpenAPIPathItem `json:"paths"`
	Components OpenAPIComponents          `json:"components"`
}

// OpenAPIInfo 是文档元信息。
type OpenAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// OpenAPIServer 是服务地址。
type OpenAPIServer struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// OpenAPITag 是分组（对应文档左侧导航）。
type OpenAPITag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// OpenAPIPathItem 是单个路径下的方法集合。
type OpenAPIPathItem map[string]OpenAPIOperation

// OpenAPIOperation 是一个操作。
type OpenAPIOperation struct {
	Tags        []string              `json:"tags"`
	Summary     string                `json:"summary"`
	OperationID string                `json:"operationId"`
	Description string                `json:"description,omitempty"`
	Security    []map[string][]string `json:"security"`
	Parameters  []OpenAPIParameter    `json:"parameters,omitempty"`
	// RequestBody 只在有 body/form 参数时出现。
	RequestBody *OpenAPIRequestBody    `json:"requestBody,omitempty"`
	Responses   map[string]OpenAPIResp `json:"responses"`

	// 以下是契约能表达、OpenAPI 标准字段表达不了的信息。
	// 放进扩展而不是塞进 description，是为了让生成的 SDK 与文档站能**结构化**读取：
	// 从一段中文里正则抽"是否需要会话票"必然出错。
	XAuth          string `json:"x-auth"`
	XScope         string `json:"x-scope,omitempty"`
	XScopeEnforced bool   `json:"x-scope-enforced"`
	XIdempotency   string `json:"x-idempotency,omitempty"`
	// XTryIt 表示能否在文档页试运行（= Auth != session）。
	XTryIt bool `json:"x-try-it"`
	// XSessionWhy 是不可试运行的原因分类（credential/session/key_manage/key_issue）。
	XSessionWhy string `json:"x-session-why,omitempty"`
	// XSessionWhyText 是该分类的完整文案，避免渲染器各自维护一份翻译。
	XSessionWhyText string `json:"x-session-why-text,omitempty"`
	XTrialRisk      string `json:"x-trial-risk"`
	// XNoData 表示成功时 data 恒为 null。
	XNoData bool `json:"x-no-data,omitempty"`
}

// OpenAPIParameter 是一个入参（path / query / header）。
type OpenAPIParameter struct {
	Name        string         `json:"name"`
	In          string         `json:"in"`
	Required    bool           `json:"required"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema"`
	Example     any            `json:"example,omitempty"`
}

// OpenAPIRequestBody 是请求体。
type OpenAPIRequestBody struct {
	Description string                  `json:"description,omitempty"`
	Required    bool                    `json:"required"`
	Content     map[string]OpenAPIMedia `json:"content"`
}

// OpenAPIMedia 是请求/响应体描述。
type OpenAPIMedia struct {
	Schema map[string]any `json:"schema"`
	// Examples 用 media 级 examples（而不是 schema 级 example）：
	// Swagger UI 对 3.1 的 schema 级 example 支持不稳，media 级是兼容性最好的写法。
	Examples map[string]OpenAPIExample `json:"examples,omitempty"`
}

// OpenAPIExample 是一个具名示例。
type OpenAPIExample struct {
	Summary string `json:"summary,omitempty"`
	Value   any    `json:"value"`
}

// OpenAPIResp 是响应定义。
type OpenAPIResp struct {
	Description string                  `json:"description"`
	Content     map[string]OpenAPIMedia `json:"content,omitempty"`
}

// OpenAPIComponents 是复用组件。
type OpenAPIComponents struct {
	SecuritySchemes map[string]any `json:"securitySchemes"`
	// Schemas 存每个端点 data 的对象 schema，由 `returns` 折叠而来。
	// 用 $ref 而不是内联：同一结构（如订单）在列表与详情里重复出现，
	// 内联会让整份文档膨胀，也让 Swagger UI 的模型面板无法复用。
	Schemas map[string]any `json:"schemas"`
}

// OpenAPI 渲染整份文档。
//
// serverURL 取站点对外地址（调用方传入，不在这里读配置 —— 契约包只依赖标准库）。
// 为空时回落 `/`，避免生成 `"servers":[{"url":""}]` 这种非法值。
func OpenAPI(serverURL string) OpenAPIDoc {
	server := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if server == "" {
		server = "/"
	}
	doc := OpenAPIDoc{
		OpenAPI: openAPIVersion,
		Info: OpenAPIInfo{
			Title:   "灵动开放接口",
			Version: "1.0.0",
			Description: "面向接入方的用户侧接口：账号、密钥、用量、账单、任务、文件、充值。\n\n" +
				"鉴权统一使用 `Authorization: Bearer <凭证>`。凭证有两种：会话票（用户登录后下发）" +
				"与 API 密钥（`ld-` 前缀）。写操作只接受会话票或应用托管密钥。\n\n" +
				"响应信封统一为 `{code,message,error_code,data}`，`code=0` 表示成功；" +
				"失败时 `error_code` 用于分支判断（HTTP 状态码分不出「换凭证」与「没权限」）。",
		},
		Servers: []OpenAPIServer{{URL: server, Description: "生产环境"}},
		Paths:   map[string]OpenAPIPathItem{},
		Components: OpenAPIComponents{
			SecuritySchemes: map[string]any{
				// 同一个 HTTP Bearer 头承载两种凭证，因此在 OpenAPI 里只能描述一种方案；
				// 区别写在每个 operation 的 x-auth 上（open/reader/writer/session）。
				"bearerAuth": map[string]any{
					"type":        "http",
					"scheme":      "bearer",
					"description": "会话票（lds_ 前缀）或 API 密钥（ld- 前缀）",
				},
			},
			Schemas: map[string]any{},
		},
	}
	for _, g := range Groups() {
		doc.Tags = append(doc.Tags, OpenAPITag{Name: g})
	}
	for _, e := range catalogOrdered() {
		d := e.Doc()

		// data 的 schema：登记进 components 再 $ref。
		var dataRef map[string]any
		switch {
		case e.Media != MediaJSON:
			dataRef = nil // 不是 JSON 信封，不需要 data schema
		case d.NoData:
			dataRef = map[string]any{"type": "null"}
		default:
			name := SchemaNameOf(e.Key)
			doc.Components.Schemas[name] = buildObjectSchema(d.Returns)
			dataRef = map[string]any{"$ref": "#/components/schemas/" + name}
		}

		path := toOpenAPIPath(e.Path)
		item, ok := doc.Paths[path]
		if !ok {
			item = OpenAPIPathItem{}
			doc.Paths[path] = item
		}
		op := OpenAPIOperation{
			Tags:            []string{e.Group},
			Summary:         e.Title,
			OperationID:     e.Key,
			Description:     descriptionOf(e),
			Security:        securityOf(e.Auth),
			Parameters:      parametersOf(e, d),
			RequestBody:     requestBodyOf(e, d),
			Responses:       responsesOf(e, d.Errors, dataRef),
			XAuth:           e.Auth.String(),
			XScope:          e.Scope,
			XScopeEnforced:  d.ScopeEnforced,
			XIdempotency:    e.Idempotency,
			XTryIt:          e.Tries(),
			XSessionWhy:     string(d.SessionWhy),
			XSessionWhyText: d.SessionWhy.Text(),
			XTrialRisk:      string(e.TrialRiskOf()),
			XNoData:         d.NoData,
		}
		item[strings.ToLower(e.Method)] = op
	}
	return doc
}

// SchemaNameOf 把端点 Key 归一成 components.schemas 里的名字。
//
// `billing.orders_create` -> `BillingOrdersCreateData`
//
// 后缀 `Data` 不是装饰：它保证永不与 `Error` / `Envelope` 这类保留名相撞，
// 也为将来给同一端点加 `XxxParams` 留出位置（那时只改"data 是哪个"，
// 不必重命名已有的引用）。
//
// 唯一性不靠推理，靠 TestSchemaNamesUnique 断言 41 个名字两两不等。
func SchemaNameOf(key string) string {
	var b strings.Builder
	up := true
	for _, r := range key {
		switch r {
		case '.', '_', '-':
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
			continue
		}
		b.WriteRune(r)
	}
	b.WriteString("Data")
	return b.String()
}

// catalogOrdered 返回按 (分组顺序, 组内声明顺序) 排序的端点。
func catalogOrdered() []Endpoint {
	out := make([]Endpoint, 0, len(catalog))
	for _, g := range Groups() {
		for _, e := range catalog {
			if e.Group == g {
				out = append(out, e)
			}
		}
	}
	return out
}

// toOpenAPIPath 把 gin 形式（`:id`）转成 OpenAPI 形式（`{id}`）。
//
// 两种写法的差异只有一个字符，但写错就是"路径参数不被识别"：
// 文档站会把它渲染成固定路径，接入方照抄就去掉了真实 id。
func toOpenAPIPath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if strings.HasPrefix(seg, ":") {
			parts[i] = "{" + strings.TrimPrefix(seg, ":") + "}"
		}
	}
	return strings.Join(parts, "/")
}

// securityOf 把鉴权级别翻成 OpenAPI security。
//
//	open    -> []（空数组 = 不需要凭证；不能省略该字段，省略等于"继承全局"）
//	reader  -> bearerAuth
//	writer  -> bearerAuth（额外要求见 x-scope + x-scope-enforced）
//	session -> bearerAuth（额外要求见 x-auth）
func securityOf(a Auth) []map[string][]string {
	if a == AuthOpen {
		return []map[string][]string{}
	}
	return []map[string][]string{{"bearerAuth": {}}}
}

// parametersOf 生成 path / query / header 参数。
//
// path 参数**只**来自 details（`In: InPath`），不再从 Path 串现推：
// 现推的结果没有 description，而"这个 id 是什么的 id"恰恰是文档最该回答的问题。
// 两者的集合一致性由 TestPathParamsMatchPathTemplate 保证。
//
// 顺序按 PathParams() 声明顺序（而非 map 遍历），否则每次导出 diff 都会错位。
func parametersOf(e Endpoint, d Detail) []OpenAPIParameter {
	out := make([]OpenAPIParameter, 0, len(d.Params))
	// 先按 Path 里的顺序输出 path 参数。
	byName := map[string]Param{}
	for _, p := range d.Params {
		if p.In == InPath {
			byName[p.Name] = p
		}
	}
	for _, name := range e.PathParams() {
		if p, ok := byName[name]; ok {
			out = append(out, toOpenAPIParameter(p))
		}
	}
	// 再输出 query 与 header（按声明顺序）。
	for _, p := range d.Params {
		if p.In == InQuery || p.In == InHeader {
			out = append(out, toOpenAPIParameter(p))
		}
	}
	return out
}

func toOpenAPIParameter(p Param) OpenAPIParameter {
	return OpenAPIParameter{
		Name:        p.Name,
		In:          string(p.In),
		Required:    p.Required == ReqRequired,
		Description: paramDescription(p),
		Schema:      paramSchema(p),
		Example:     p.Example,
	}
}

// paramDescription 把 Desc 与 Note 合成一句话（OpenAPI 只有 description 一个位置）。
func paramDescription(p Param) string {
	if p.Note == "" {
		return p.Desc
	}
	if p.RequiredNote != "" {
		return p.Desc + "。" + p.RequiredNote + "。" + p.Note
	}
	return p.Desc + "。" + p.Note
}

// paramSchema 生成参数的 JSON Schema 片段。
//
// Constraint.Kind=pattern 时用 Text 当正则原文（那一条既是给人看的、也是机器要的），
// 其余 kind 用 Value。
func paramSchema(p Param) map[string]any {
	out := baseSchema(p.Type, p.Enum)
	for _, c := range p.Constraints {
		switch c.Kind {
		case "min":
			out["minimum"] = c.Value
		case "max":
			out["maximum"] = c.Value
		case "min_len":
			out["minLength"] = int(c.Value)
		case "max_len":
			out["maxLength"] = int(c.Value)
		case "pattern":
			out["pattern"] = c.Text
		case "multiple_of":
			out["multipleOf"] = c.Value
		}
	}
	if p.Default != "" {
		out["default"] = p.Default
	}
	return out
}

// baseSchema 只描述类型与枚举（示例由 description 之外的 example 字段承载）。
func baseSchema(t Type, enum []string) map[string]any {
	out := map[string]any{}
	switch t {
	case TypeString:
		out["type"] = "string"
	case TypeInteger:
		out["type"] = "integer"
	case TypeNumber:
		out["type"] = "number"
	case TypeBoolean:
		out["type"] = "boolean"
	case TypeArray:
		// items 未知时不写 items（3.1 允许），写成 {"type":"string"} 会是假精度。
		out["type"] = "array"
	case TypeObject:
		out["type"] = "object"
	case TypeFile:
		out["type"] = "string"
		out["format"] = "binary"
	case TypeBinary:
		out["type"] = "string"
		out["contentMediaType"] = "application/octet-stream"
	case TypeNone, TypeAny:
		// 不写 type = 任意值（3.1 合法；3.0 必须写 type，所以这是 3.1 才有的写法）
	}
	if len(enum) > 0 {
		out["enum"] = enum
	}
	return out
}

// requestBodyOf 生成请求体（只有 body/form 参数时才有）。
func requestBodyOf(e Endpoint, d Detail) *OpenAPIRequestBody {
	var bodyParams, formParams []Param
	for _, p := range d.Params {
		switch p.In {
		case InBody:
			bodyParams = append(bodyParams, p)
		case InForm:
			formParams = append(formParams, p)
		}
	}
	if len(bodyParams) == 0 && len(formParams) == 0 {
		return nil
	}
	required := false
	for _, p := range bodyParams {
		if p.Required == ReqRequired {
			required = true
			break
		}
	}
	out := &OpenAPIRequestBody{Required: required, Content: map[string]OpenAPIMedia{}}
	if len(bodyParams) > 0 {
		media := OpenAPIMedia{Schema: objectSchemaOf(bodyParams)}
		if len(d.ReqExample) > 0 {
			media.Examples = map[string]OpenAPIExample{
				"default": {Summary: "请求示例", Value: d.ReqExample},
			}
		}
		out.Content["application/json"] = media
	}
	if len(formParams) > 0 {
		out.Content["multipart/form-data"] = OpenAPIMedia{Schema: objectSchemaOf(formParams)}
	}
	// 契约里写了 body 参数却一个必填也没有时，required=false 是准确描述；
	// 不强行改成 true —— 那会让 SDK 生成器要求一个其实可省略的对象。
	return out
}

// objectSchemaOf 把一组 Param 折成一个 object schema。
func objectSchemaOf(params []Param) map[string]any {
	props := map[string]any{}
	required := make([]string, 0, len(params))
	for _, p := range params {
		s := paramSchema(p)
		if p.Desc != "" || p.Note != "" {
			s["description"] = paramDescription(p)
		}
		props[p.Name] = s
		if p.Required == ReqRequired {
			required = append(required, p.Name)
		}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

// buildObjectSchema 把扁平的 Return 列表折成嵌套 JSON Schema。
//
// `items[].key_name` 折叠成：
//
//	{"type":"object","properties":{"items":{"type":"array","items":{"type":"object",
//	  "properties":{"key_name":{"type":"string"}}}}}}
//
// 只支持一层 `[]`（门禁 TestReturnNameShape 拒绝两层）：本项目对外响应里
// 没有两层数组，支持它只会让这里长出一条无人验证的递归分支。
func buildObjectSchema(rets []Return) map[string]any {
	props := map[string]any{}
	nested := map[string]map[string]any{}

	for _, r := range rets {
		top, rest := splitReturnName(r.Name)
		if rest == "" {
			s := baseSchema(r.Type, r.Enum)
			if d := joinDesc(r.Desc, r.Note); d != "" {
				s["description"] = d
			}
			props[top] = s
			continue
		}
		if nested[top] == nil {
			nested[top] = map[string]any{}
		}
		s := baseSchema(r.Type, r.Enum)
		if d := joinDesc(r.Desc, r.Note); d != "" {
			s["description"] = d
		}
		nested[top][rest] = s
	}

	// 有子字段的顶层名统一变成"对象数组"。
	// 键序由 encoding/json 保证（字典序），这里不需要额外维护顺序。
	for top, sub := range nested {
		props[top] = map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "object", "properties": sub},
			"description": "列表",
		}
	}
	return map[string]any{"type": "object", "properties": props}
}

// splitReturnName 拆 `a[].b` -> ("a", "b")；无 `[]` 时返回 (name, "")。
func splitReturnName(name string) (string, string) {
	if i := strings.Index(name, "[]."); i >= 0 {
		return name[:i], name[i+3:]
	}
	return name, ""
}

func joinDesc(desc, note string) string {
	switch {
	case desc == "":
		return note
	case note == "":
		return desc
	default:
		return desc + "。" + note
	}
}

// responsesOf 生成响应定义：成功体（按 Media）+ 统一的失败信封。
//
// 成功码不是恒 200：`MediaEmpty` 是 **204**（真的没有响应体）。
// 统一写 200 的后果是生成的 SDK 去解一个不存在的 body，拿到空字符串后
// JSON 解析失败 —— 而"删除成功"在服务端其实完全正常。
func responsesOf(e Endpoint, declared []ErrCode, dataRef map[string]any) map[string]OpenAPIResp {
	out := map[string]OpenAPIResp{}
	switch e.Media {
	case MediaEmpty:
		out["204"] = OpenAPIResp{Description: "成功（无响应体）"}
	case MediaCSV:
		out["200"] = OpenAPIResp{
			Description: "成功（CSV 文件流，UTF-8 BOM）",
			Content: map[string]OpenAPIMedia{
				"text/csv": {Schema: map[string]any{"type": "string", "contentMediaType": "text/csv"}},
			},
		}
	case MediaBinary:
		out["200"] = OpenAPIResp{
			Description: "成功（字节流，支持 Range/206）",
			Content: map[string]OpenAPIMedia{
				"application/octet-stream": {Schema: map[string]any{
					"type": "string", "contentMediaType": "application/octet-stream",
				}},
			},
		}
	default:
		out["200"] = OpenAPIResp{
			Description: "成功",
			Content: map[string]OpenAPIMedia{
				"application/json": {Schema: envelopeSchema(dataRef)},
			},
		}
	}
	out["4XX"] = OpenAPIResp{
		Description: "失败（统一信封，code 非 0）",
		Content: map[string]OpenAPIMedia{
			"application/json": {Schema: errorSchema()},
		},
	}
	// 按契约里声明的错误码再展开具体状态码：`4XX` 是"任意 4xx 都可能发生"的
	// 通配兜底，接入方真正要写分支的是 400/403/409 这些**确切**的状态。
	//
	// 只写 description 不写 examples：每个码的"何时出现"本来就写在契约的 When 里，
	// 直接拼进描述即可；再造一份示例体等于第二真相源（message 文案会漂）。
	for st, list := range errorsByStatus(declared) {
		parts := make([]string, 0, len(list))
		for _, ec := range list {
			parts = append(parts, "`"+ec.Code+"`（"+ec.When+"）")
		}
		out[strconv.Itoa(st)] = OpenAPIResp{
			Description: "失败码：" + strings.Join(parts, "；"),
			Content: map[string]OpenAPIMedia{
				"application/json": {Schema: errorSchema()},
			},
		}
	}
	return out
}

// errorsByStatus 把契约声明的错误码按 HTTP 状态分组（未知码跳过：它由
// TestErrCodesExist 保证存在，这里只是不让 OpenAPI 因一个笔误崩掉）。
func errorsByStatus(declared []ErrCode) map[int][]ErrCode {
	out := map[int][]ErrCode{}
	for _, ec := range declared {
		def, ok := errs.Get(ec.Code)
		if !ok {
			continue
		}
		out[def.HTTP] = append(out[def.HTTP], ec)
	}
	return out
}

// envelopeSchema 是成功信封的结构。dataRef 为 nil 时 data 不出现。
func envelopeSchema(dataRef map[string]any) map[string]any {
	props := map[string]any{
		"code":    map[string]any{"type": "integer", "example": 0},
		"message": map[string]any{"type": "string", "example": "ok"},
	}
	if dataRef != nil {
		props["data"] = dataRef
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []string{"code", "message"},
	}
}

// errorSchema 是失败信封的结构。
//
// error_code 必填：接入方要靠它做分支（session_required 要重新登录、
// scope_denied 要换凭证、rate_limited 要退避），只看 HTTP 状态码分不出来。
//
// data 不写 `"nullable": true` —— 那是 3.0 的写法，3.1 里必须删掉 type
// 才能表达"任意值"。混用会让校验器与渲染器的行为不一致。
func errorSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code":       map[string]any{"type": "integer", "example": 400},
			"message":    map[string]any{"type": "string"},
			"error_code": map[string]any{"type": "string", "example": "invalid_param"},
			"data":       map[string]any{"description": "失败时通常为 null"},
		},
		"required": []string{"code", "message", "error_code"},
	}
}

// descriptionOf 拼接 operation 描述（标题已进 summary，这里放注意事项）。
func descriptionOf(e Endpoint) string {
	var b strings.Builder
	if e.Idempotency != "" {
		b.WriteString("幂等：")
		b.WriteString(e.Idempotency)
	}
	if e.Notes != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("注意：")
		b.WriteString(e.Notes)
	}
	if !e.Tries() {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("不可在此调试：")
		b.WriteString(e.TryHint())
	}
	return b.String()
}
