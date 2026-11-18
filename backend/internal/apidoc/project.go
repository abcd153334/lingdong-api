package apidoc

// 本文件只剩**投影的数据结构**：字段与端点的对外形状。
//
// v6 时代这里还有一整套构造函数（projectEndpoint / projectField / projectBilling /
// errorCodesFor / specFieldProjection …），把「模型事实 + 契约 + 文案」拼成页面内容。
// v7 之后内容全部落库（doc_endpoint_fields / doc_response_variants / doc_presets），
// 投影由 `docdocs.ProjectEndpoint` 从库里读，于是那套构造函数整体删除。
//
// 保留下来的两个类型仍然是**活契约**：
//   - `docdocs` 用它拼「示例代码生成器的输入」（EndpointProjection —— 只填
//     samples 需要的字段，其余留空）；
//   - 页面与 AI 工具复用它表达字段表（FieldProjection）。
//
// 为什么不把这些字段直接搬进 docdocs：示例生成器（`samples.go`）是 apidoc 的
// 纯函数，它只认这两个结构；搬走会让 samples 反向依赖 service 层。

// FieldProjection 是投影后的字段（对外白名单）。
//
// 字段名与 JSON tag 是**对外契约**：页面、AI 工具参数、导出都按它读，
// 改名等于改对外协议（前端会静默显示空白列，而不是报错）。
type FieldProjection struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// In 是参数位置：body / query / path / form / header。
	//
	// 页面的请求参数表按它分块；写成同一张表会让读者把 query 参数塞进 JSON body。
	In       string `json:"in"`
	Required string `json:"required"`
	// RequiredNote 是「条件必填」的条件（如「与 image 至少给一个」）。
	RequiredNote string `json:"required_note,omitempty"`
	Default      string `json:"default"`
	Desc         string `json:"desc"`
	Note         string `json:"note,omitempty"`
	// ValuesByModel 是「随模型变化」的取值：模型 ID → 该模型真实可用的取值。
	ValuesByModel map[string][]string `json:"values_by_model,omitempty"`
	// Values 是可用取值（静态枚举或模型驱动的并集）。
	Values []string `json:"values,omitempty"`
	// ValueSource 是 values 的来源：static / model / 空（无枚举）。
	//
	// 前端据此决定徽标文案（「可选值」vs「本模型档位」）—— 两者不能混：
	// 模型驱动的取值改价后可能变化，用户需要知道它跟模型绑定。
	ValueSource string   `json:"value_source,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
	Tier        string   `json:"tier"`
	// Source 标明字段来自哪一层契约：空 = 平台契约，spec = 本接入（规格 DSL）声明。
	Source     string `json:"source,omitempty"`
	OnlyModels string `json:"only_models,omitempty"`
	// Passthrough 为真时，页面补一句「平台按原样转发，不校验」。
	Passthrough bool `json:"passthrough,omitempty"`
	// ForbidReason 非空表示该字段在当前模型上不可用（控件禁用 + 显示原因）。
	ForbidReason string `json:"forbid_reason,omitempty"`
	// DocNote 是编辑针对这个字段补的说明。
	DocNote string `json:"doc_note,omitempty"`
}

// EndpointProjection 是**示例代码生成器**的输入，不是页面契约。
//
// 页面契约是 `docdocs.EndpointView`（从库里读、可编辑）；这里只保留
// `samples.go` 真正读取的字段。两件事分开之后，「给示例加一个模板」
// 不会被迫给页面也加一个字段。
type EndpointProjection struct {
	Kind   string `json:"kind"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Transport 是传输方式（`application/json` / `multipart/form-data`）。
	//
	// 示例生成器靠它决定「字段拼成 JSON 还是 -F 表单」；留空会退化成 JSON。
	Transport string            `json:"transport"`
	RunMode   string            `json:"run_mode"`
	Fields    []FieldProjection `json:"fields"`
	Returns   []FieldProjection `json:"returns"`
	// Example 是请求体示例（保序）。生成器只读它，不改它。
	Example       Obj    `json:"example,omitempty"`
	ExampleTitle  string `json:"example_title,omitempty"`
	Example2      Obj    `json:"example2,omitempty"`
	ExampleTitle2 string `json:"example_title2,omitempty"`
}

// ErrorCode 是「这个端点会返回哪些错误」的一行（种子的 doc_endpoint_errors 由它填充）。
type ErrorCode struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Desc   string `json:"desc"`
}
