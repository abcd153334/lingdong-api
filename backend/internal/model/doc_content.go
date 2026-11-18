package model

import "time"

// 文档内容模型（v7）：**文档内容只在库里**。
//
// 与重构前的根本区别（改之前先读这一段）：
//
//	旧：doc_groups/doc_group_members/doc_group_texts 存结构与文案，字段表/示例/响应/错误码
//	    全在 internal/apidoc 的 Go 常量里 —— 所以文档天然不可编辑，改一个字段要发版。
//	新：板块、节点、接口、字段、枚举、响应变体、预设、错误码、示例代码全部落库（8 张表），
//	    Go 常量只剩「出厂种子」（apidoc/seed_docs.go）与「平台真实路由登记表」
//	    （apidoc/endpointTable，relay 依赖，禁止入库）。
//
// 三条边界：
//  1. **key 是身份，入库后不可改**（改了旧链接 404，且文档与模型的绑定会断）。
//  2. **不用软删**：墓碑占主键/唯一索引，会让「同名节点重新建」建不回来。
//  3. **对外 JSON 绝不直接用这些结构体**：投影层（docdocs/v7_view.go）负责白名单，
//     origin/version/updated_by 这类内部列不得出现在公开响应里。

// DocSection 是文档站侧栏的 L1 板块（语言 / 图片 / 视频 …）。
//
// 为什么必须落库：它是侧栏第一层，也是读者看到的第一句话。名字、英文名、
// 描述、颜色、顺序以前写在 apidoc/section.go 里，运营改不了 —— 那是"文档站
// 有一块内容属于代码"的最后一个入口。
//
// Key 是身份（节点用 `doc_nodes.section` 指向它），创建后不可改；不提供新建与
// 删除：新 key 接不到契约侧任何传输，删 key 会让其下节点失去归属。
type DocSection struct {
	ID   uint64 `gorm:"primaryKey;autoIncrement"`
	Key  string `gorm:"size:32;not null"`
	Name string `gorm:"size:32;not null"`
	// En 是标题括号里的英文名（"语言（Protocols）"）；空则不显示括号。
	En   string `gorm:"size:32;not null;default:''"`
	Desc string `gorm:"type:text;not null;default:''"`
	// Color 是 #RRGGBB，前台直接当颜色用，保存时校验格式。
	Color  string `gorm:"size:16;not null;default:''"`
	Sort   int    `gorm:"not null;default:0"`
	Hidden bool   `gorm:"not null;default:false"`
	// Version 是乐观锁（与节点/接口同一套语义）。
	Version   int `gorm:"not null;default:1"`
	UpdatedBy uint64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (DocSection) TableName() string { return "doc_sections" }

// DocNodeKind 是节点的四种形态。
const (
	DocNodeProtocol = "protocol" // 固定协议文档（OpenAI 兼容 / Claude 原生 / Embeddings）
	DocNodeFamily   = "family"   // 模型族（按厂商 + 板块归组）
	DocNodeGeneral  = "general"  // 通用接口分组（文件与产物 / 任务 / 账户）
	DocNodeGuide    = "guide"    // 说明页（快速开始 / 鉴权 / 错误码）
)

// DocStatus 是草稿/发布两态。
const (
	DocDraft     = "draft"
	DocPublished = "published"
)

// DocOrigin 记录一行是谁写的：种子 / 人工 / AI。种子刷新只允许覆盖 seed 行。
const (
	DocOriginSeed   = "seed"
	DocOriginManual = "manual"
	DocOriginAI     = "ai"
)

// DocNode 是一个文档节点（侧栏第二层）。
//
// 为什么把说明页也塞进来（kind=guide）：它们与节点是同一件事——「侧栏里的一行、
// 点开是一篇正文」。分成两张表会让侧栏、路由、渲染各写两套分支。
type DocNode struct {
	ID      uint64 `gorm:"primaryKey;autoIncrement"`
	Key     string `gorm:"size:96;not null"`
	Kind    string `gorm:"size:16;not null"`
	Section string `gorm:"size:16;not null"`
	// Protocol 只在 kind=protocol 时有值（openai/claude/embeddings）。
	Protocol string `gorm:"size:32;not null;default:''"`
	Title    string `gorm:"size:160;not null"`
	Summary  string `gorm:"type:text;not null;default:''"`
	// Blocks 只在 kind=guide 时有值（[]apidoc.GuideBlock），其余恒为 []。
	Blocks JSON   `gorm:"type:jsonb;not null;default:'[]'"`
	Sort   int    `gorm:"not null;default:0"`
	Hidden bool   `gorm:"not null;default:false"`
	Status string `gorm:"size:16;not null;default:'draft'"`
	// PublishedAt 为空 = 从未发布。下架时置空，内容不动。
	PublishedAt *time.Time `gorm:"type:timestamptz"`
	// Version 是乐观锁：保存必须带旧 version，不匹配返回 409，避免两端互相覆盖。
	Version int `gorm:"not null;default:1"`
	// ContractVer 是发布那一刻的契约版本，只用于管理台「契约已升级，请复核」。
	ContractVer string `gorm:"size:16;not null;default:''"`
	// Origin 记录这一行是谁写的（seed/manual/ai）：种子刷新只覆盖 seed 行。
	Origin    string `gorm:"size:16;not null;default:'seed'"`
	UpdatedBy uint64 `gorm:"not null;default:0"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (DocNode) TableName() string { return "doc_nodes" }

// DocEndpoint 是一篇接口文档 = 对外的一个独立页面。
//
// TransportDocID 是它与平台真实路由的唯一连接：
//   - 有值 = 绑定到 apidoc 已登记端点（发布时必须与方法/路径一致）；
//   - 为空 = 未绑定（只能是草稿，发布被 E_UNBOUND 拒绝）——这是"不许伪造能力"的闸门。
type DocEndpoint struct {
	ID     uint64 `gorm:"primaryKey;autoIncrement"`
	NodeID uint64 `gorm:"not null"`
	Key    string `gorm:"size:64;not null"`

	Name    string `gorm:"size:160;not null"`
	Summary string `gorm:"type:text;not null;default:''"`

	Method  string `gorm:"size:8;not null"`
	Path    string `gorm:"size:200;not null"`
	Aliases JSON   `gorm:"type:jsonb;not null;default:'[]'"`

	// TransportDocID 形如 `images/generations` / `images/generations@jimeng`。
	TransportDocID *string `gorm:"size:128"`
	// MethodCheck/PathCheck 是绑定时冻结的契约值，用于漂移比对（比 hash 好：能说出哪里错）。
	MethodCheck string `gorm:"size:8;not null;default:''"`
	PathCheck   string `gorm:"size:200;not null;default:''"`

	Phase   string `gorm:"size:16;not null;default:'sync'"`
	RunMode string `gorm:"size:16;not null;default:'json'"`
	RunHint string `gorm:"type:text;not null;default:''"`
	MaxBody string `gorm:"size:32;not null;default:''"`
	// Badge 是侧栏徽标（async/stream/binary），空则前端按契约派生。
	Badge string `gorm:"size:16;not null;default:''"`

	// Notes 是接口级注意事项（计费口径 / 超时 / 409 语义），一行一条。
	//
	// 为什么必须是列而不是从契约读：这几句恰恰是最常变的部分（口径调整、超时改了）。
	// 从契约读意味着运营改不了它，只能改代码 + 重启 —— 那不是文档系统，是发版。
	Notes JSON `gorm:"type:jsonb;not null;default:'[]'"`

	// 示例代码（四语言）是**内容**，不是渲染结果：运营能改、能重生成。
	//
	// 存 `{{base_url}}` 占位符而不是当时域名 —— 域名属于环境，写进库里就等于
	// 换域名后所有文档还在教用户调旧地址。NULL 表示没有示例代码（页面不显示）。
	Samples  JSON `gorm:"type:jsonb"`
	Samples2 JSON `gorm:"type:jsonb"`
	// 两段示例的标题（"流式输出（打字机效果）"），空则不显示。
	ExampleTitle  string `gorm:"size:96;not null;default:''"`
	ExampleTitle2 string `gorm:"size:96;not null;default:''"`

	Sort        int        `gorm:"not null;default:0"`
	Hidden      bool       `gorm:"not null;default:false"`
	Status      string     `gorm:"size:16;not null;default:'draft'"`
	PublishedAt *time.Time `gorm:"type:timestamptz"`
	Version     int        `gorm:"not null;default:1"`
	Origin      string     `gorm:"size:16;not null;default:'seed'"`
	ContractVer string     `gorm:"size:16;not null;default:''"`
	UpdatedBy   uint64     `gorm:"not null;default:0"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (DocEndpoint) TableName() string { return "doc_endpoints" }

// DocEndpointField 是一个请求字段（VariantID 为空）或某个响应变体的返回字段。
//
// 为什么独立成行而不是 jsonb 数组：要逐行增删/上下移/按 in 分组/体检查重；
// jsonb 每次改一行都要整段重写，人工与 AI 并发必丢更新。
type DocEndpointField struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement"`
	EndpointID uint64 `gorm:"not null"`
	// VariantID 为空 = 请求字段；非空 = 该状态码变体的返回字段。
	VariantID *uint64
	// In 只对请求字段有意义：body/query/path/form/header。
	// 列名必须叫 location：`in` 是 PostgreSQL 保留字，建索引时 `ON (... , in, ...)`
	// 直接语法错误（SQLSTATE 42601），而报错只会说「索引建不起来」。
	In   string `gorm:"column:location;size:16;not null;default:'body'"`
	Name string `gorm:"size:160;not null"`

	Type         string `gorm:"size:32;not null;default:'string'"`
	Required     string `gorm:"size:16;not null;default:'optional'"`
	RequiredNote string `gorm:"size:200;not null;default:''"`
	DefaultValue string `gorm:"size:160;not null;default:''"`
	// Desc 是这个字段的**全部说明文字**（多行，空行分段）。
	//
	// 曾经的 Note（第二段）与 Groups（按模型分组的取值参考表）已合并进来：
	// 页面上读者看到的是一段连续说明，后台就是一个多行文本框。
	// 三列并存的代价不是冗余，而是「哪句话存在哪一列」在后台根本看不出来 ——
	// 像素参考表就这么当过一段没有输入框的内容。
	Desc string `gorm:"type:text;not null;default:''"`

	EnumSource  string `gorm:"size:24;not null;default:'static'"`
	Constraints JSON   `gorm:"type:jsonb;not null;default:'[]'"`
	// OnlyModels 是「哪些模型才认这个字段」的人话标注（如 "gpt-image 系列"）。
	//
	// 与 Groups 同理：这是给人看的一句话，运营会因为上/下线一个型号而改它。
	// 曾经由投影期从契约的 OnlyModels 覆盖，结果是后台只读、改不动。
	OnlyModels  string `gorm:"size:64;not null;default:''"`
	Tier        string `gorm:"size:16;not null;default:'standard'"`
	Passthrough bool   `gorm:"not null;default:false"`
	SketchSkip  bool   `gorm:"not null;default:false"`
	// Example 是该字段在示例里的真值（nil = 用类型占位）。
	Example JSON `gorm:"type:jsonb"`
	Sort    int  `gorm:"not null;default:0"`
}

func (DocEndpointField) TableName() string { return "doc_endpoint_fields" }

// DocFieldEnum 是枚举值的逐值说明（参考图里每个候选值后面那一句）。
type DocFieldEnum struct {
	FieldID    uint64 `gorm:"primaryKey"`
	Value      string `gorm:"size:160;primaryKey"`
	Desc       string `gorm:"size:300;not null;default:''"`
	Deprecated bool   `gorm:"not null;default:false"`
	Sort       int    `gorm:"not null;default:0"`
}

func (DocFieldEnum) TableName() string { return "doc_field_enums" }

// DocResponseVariant 是一个状态码下的响应形态（200 同步 / 202 已受理 / 流式 …）。
type DocResponseVariant struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement"`
	EndpointID  uint64 `gorm:"not null"`
	StatusCode  int    `gorm:"not null"`
	Label       string `gorm:"size:64;not null"`
	Kind        string `gorm:"size:16;not null;default:'json'"` // json|binary|none|sse
	ContentType string `gorm:"size:96;not null;default:''"`
	Envelope    bool   `gorm:"not null;default:false"`
	// Example 是有序 KV 数组（对象在 jsonb 里不保序，必须用 [{k,v}] 形状）。
	Example JSON `gorm:"type:jsonb;not null;default:'[]'"`
	// Raw 是 SSE 等非 JSON 响应的原文。
	Raw   string `gorm:"type:text;not null;default:''"`
	Notes JSON   `gorm:"type:jsonb;not null;default:'[]'"`
	Sort  int    `gorm:"not null;default:0"`
}

func (DocResponseVariant) TableName() string { return "doc_response_variants" }

// DocPreset 是右栏「调用方式」的一个预设（同一路径的几种用法）。
//
// 每个预设带自己的示例代码：选中一个用法，右栏要给的是**这个用法**的可执行示例，
// 而不是主示例。代码与 body 一样是内容（可编辑），但来源是同一个 body。
type DocPreset struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement"`
	EndpointID uint64 `gorm:"not null"`
	Key        string `gorm:"size:48;not null"`
	Label      string `gorm:"size:48;not null"`
	Summary    string `gorm:"size:200;not null;default:''"`
	Body       JSON   `gorm:"type:jsonb;not null;default:'[]'"`
	Fields     JSON   `gorm:"type:jsonb;not null;default:'[]'"`
	Uploads    JSON   `gorm:"type:jsonb;not null;default:'[]'"`
	Note       string `gorm:"size:300;not null;default:''"`
	Samples    JSON   `gorm:"type:jsonb"`
	Sort       int    `gorm:"not null;default:0"`
}

func (DocPreset) TableName() string { return "doc_presets" }

// DocEndpointError 是一条错误码。Source 区分「种子派生」与「人工补写」：
// 种子刷新只补 source=seed 的缺失行，人工行永不被覆盖。
type DocEndpointError struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement"`
	EndpointID uint64 `gorm:"not null"`
	StatusCode int    `gorm:"not null"`
	Code       string `gorm:"size:80;not null"`
	Message    string `gorm:"size:300;not null"`
	Source     string `gorm:"size:8;not null;default:'manual'"`
	Sort       int    `gorm:"not null;default:0"`
}

func (DocEndpointError) TableName() string { return "doc_endpoint_errors" }
