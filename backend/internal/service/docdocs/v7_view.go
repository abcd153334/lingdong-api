package docdocs

// v7 对外视图（**公开响应只允许用这些类型**）。
//
// 为什么必须独立一套 struct 而不是直接序列化 model.DocEndpoint：
// origin / version / updated_by / seed_version 这些内部列一旦下发，就是
// 「前端能读到一个它永远不该依赖的值」，而接口一旦有人用就删不掉。
// 这里每一个字段都是刻意选择的，加字段前先问「读者需不需要」。

// Catalog 是侧栏目录（shape=7）：板块 → 节点 → 接口。
type Catalog struct {
	ContractVersion string           `json:"contract_version"`
	Shape           int              `json:"shape"`
	BaseURL         string           `json:"base_url"`
	Sections        []CatalogSection `json:"sections"`
}

type CatalogSection struct {
	ID    string        `json:"id"`
	En    string        `json:"en"`
	Name  string        `json:"name"`
	Desc  string        `json:"desc"`
	Color string        `json:"color"`
	Nodes []CatalogNode `json:"nodes"`
}

type CatalogNode struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Models 是「适用模型」的数量（侧栏不渲染，AI 与调试用）。
	Models    int               `json:"models"`
	Status    string            `json:"status"`
	Endpoints []CatalogEndpoint `json:"endpoints"`
}

// CatalogEndpoint 是侧栏第三层：一个接口一行，点击进入独立页面。
type CatalogEndpoint struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Badge 是 async/stream 这类提示；空表示普通请求。
	Badge string `json:"badge"`
}

// Node 是节点头（页面面包屑与「本节点有哪些接口」）。
type Node struct {
	Key         string            `json:"key"`
	Kind        string            `json:"kind"`
	Section     string            `json:"section"`
	SectionName string            `json:"section_name"`
	Title       string            `json:"title"`
	Summary     string            `json:"summary"`
	Models      []NodeModel       `json:"models"`
	Endpoints   []CatalogEndpoint `json:"endpoints"`
	// Blocks 说明页正文；非说明页恒为 []（不加 omitempty：键消失会让前端 .map 崩）。
	Blocks []GuideBlockView `json:"blocks"`
}

// NodeModel 是节点适用的模型（族页展示「这页适用于谁」，空数组时前端整段隐藏）。
type NodeModel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Vendor     string `json:"vendor"`
	VendorName string `json:"vendor_name"`
}

// Endpoint 是一篇接口文档的全文（页面与 Markdown 只读它）。
type Endpoint struct {
	Node    NodeRef  `json:"node"`
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Aliases []string `json:"aliases"`
	Phase   string   `json:"phase"`
	// PhaseCode 是契约里的机器码（sync / submit / read）。
	//
	// 与 Phase 并存而不是替换：Phase 是给读者看的中文，文案随时可改；
	// 前端若要判"这条会不会返回任务号"，只能认这个码，不能比中文串。
	PhaseCode string `json:"phase_code"`
	// AcceptsAsync 表示同一路径还能加 `async: true` 转异步任务（图像类）。
	AcceptsAsync bool   `json:"accepts_async"`
	Transport    string `json:"transport"`
	RunMode      string `json:"run_mode"`
	RunHint      string `json:"run_hint,omitempty"`
	MaxBody      string `json:"max_body,omitempty"`
	// Notes 是接口级注意事项（计费口径 / 超时 / 409 语义）。
	// 来自 doc_endpoints.notes 列 —— 后台可编辑，投影期不读契约。
	Notes         []string     `json:"notes"`
	Fields        []FieldView  `json:"fields"`
	Response      ResponseView `json:"response"`
	Presets       []PresetView `json:"presets"`
	Errors        []ErrorView  `json:"errors"`
	Models        []NodeModel  `json:"models"`
	Samples       *SamplesView `json:"samples,omitempty"`
	Samples2      *SamplesView `json:"samples2,omitempty"`
	ExampleTitle  string       `json:"example_title,omitempty"`
	ExampleTitle2 string       `json:"example_title2,omitempty"`
	Published     bool         `json:"published"`
}

// NodeRef 只带面包屑需要的最小信息。
type NodeRef struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Section     string `json:"section"`
	SectionName string `json:"section_name"`
}

// FieldView 是字段表的一行。
//
// 这里**每一个内容列**（说明、只模型标注、层级、约束、可选值、示例…）
// 在管理台都有一一对应的输入框：前台显示的每个字都必须能在后台改到，
// 否则「所见即所得」就是假的。说明只有一列文字（Desc，多行）。
type FieldView struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	In           string `json:"in"`
	Required     string `json:"required"`
	RequiredNote string `json:"required_note,omitempty"`
	Default      string `json:"default"`
	// Desc 是这个字段的全部说明（多行）：页面上是这几段，后台是一个文本框。
	Desc        string           `json:"desc"`
	Enum        []EnumView       `json:"enum,omitempty"`
	ValueSource string           `json:"value_source,omitempty"`
	Constraints []ConstraintView `json:"constraints,omitempty"`
	Tier        string           `json:"tier"`
	Passthrough bool             `json:"passthrough,omitempty"`
	// SketchSkip 表示该字段不进响应骨架（返回字段才有意义）。
	SketchSkip bool `json:"sketch_skip,omitempty"`
	// OnlyModels 是「只对这些模型生效」的展示文本（空 = 全部成员适用）。
	OnlyModels string `json:"only_models,omitempty"`
	Example    any    `json:"example,omitempty"`
}

type EnumView struct {
	Value      string `json:"value"`
	Desc       string `json:"desc,omitempty"`
	Deprecated bool   `json:"deprecated,omitempty"`
}

type ConstraintView struct {
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`
	Text  string  `json:"text"`
}

// ResponseView 是一组状态码变体；返回字段挂在变体上。
type ResponseView struct {
	Kind        string        `json:"kind"`
	Envelope    bool          `json:"envelope"`
	ContentType string        `json:"content_type,omitempty"`
	Variants    []VariantView `json:"variants"`
}

type VariantView struct {
	Status      int         `json:"status"`
	Label       string      `json:"label"`
	ContentType string      `json:"content_type,omitempty"`
	Returns     []FieldView `json:"returns"`
	Example     any         `json:"example,omitempty"`
	Raw         string      `json:"raw,omitempty"`
	Notes       []string    `json:"notes,omitempty"`
}

type PresetView struct {
	Key     string       `json:"key"`
	Label   string       `json:"label"`
	Summary string       `json:"summary"`
	Body    any          `json:"body"`
	Fields  []string     `json:"fields"`
	Uploads []string     `json:"uploads,omitempty"`
	Note    string       `json:"note,omitempty"`
	Samples *SamplesView `json:"samples,omitempty"`
}

type ErrorView struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Desc   string `json:"desc"`
}

// SamplesView 是四语言示例（唯一生成器是 apidoc.BuildSamples）。
type SamplesView struct {
	Curl   string `json:"curl"`
	Python string `json:"python"`
	Node   string `json:"node"`
	Go     string `json:"go"`
}

// GuideBlockView 是说明页的一段正文（结构与 apidoc.GuideBlock 一致）。
type GuideBlockView struct {
	Type  string   `json:"type"`
	Text  string   `json:"text,omitempty"`
	Title string   `json:"title,omitempty"`
	Items []string `json:"items,omitempty"`
	Lang  string   `json:"lang,omitempty"`
	Code  string   `json:"code,omitempty"`
}
