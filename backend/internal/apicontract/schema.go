package apicontract

// 本文件是**第二层契约**：参数、返回字段、错误码、示例。
//
// 为什么要有第二层，而不是把字段直接塞进 Endpoint：
//
//	catalog.go 一眼能扫完全部端点的权限边界 —— 这是它最重要的
//	属性。把参数一起塞进去会变成 1400 行，review 时"改的是权限还是文案"就分
//	不清了。而权限边界改错是越权，文案改错只是难看，两者的审查强度不该相同。
//
// 两层之间用 Key 关联，由 detail_gate_test.go 的 TestEveryEndpointHasDetail
// 双向差集封死（漏写、写错 Key 都红）。**不用 init 期把 Detail 回填到 catalog**：
// ruleIndex 存的是 Endpoint 的**值拷贝**，回填时序一旦错过就是静默返回零值
// （参数表全空、页面看着完全正常）。改为按需查表，时序问题不存在。
//
// 依赖方向不变：本包仍然**只依赖标准库**。

import (
	"sort"
	"strings"
)

// Type 是字段的 JSON 类型。
//
// 词表与 internal/apidoc 的 Type **完全一致**（string/integer/number/boolean/
// array/object/file/binary/none/any），但代码零耦合：apidoc 传递依赖
// internal/model、service/billing、pkg/money，让契约包 import 它，
// endpoint.go 首部"仅标准库"的依赖方向当场失守。复制的代价约 80 行稳定代码，
// 耦合的代价是依赖倒挂 —— 后者不可接受。
type Type string

const (
	TypeString  Type = "string"
	TypeInteger Type = "integer"
	TypeNumber  Type = "number"
	TypeBoolean Type = "boolean"
	TypeArray   Type = "array"
	TypeObject  Type = "object"
	// TypeFile 是 multipart 上传的文件段。
	TypeFile Type = "file"
	// TypeBinary 是响应侧的原始字节流（不是 JSON 里的字段）。
	TypeBinary Type = "binary"
	// TypeNone 表示"这个端点没有这种字段"（如 204 响应没有 body）。
	TypeNone Type = "none"
	// TypeAny 是 JSON 值不确定（如示例里的嵌套对象）。
	TypeAny Type = "any"
)

// allTypes 是封闭词表，测试用它做越界检查。
//
// 为什么要封闭：前端按 Type 选控件（string -> Input、boolean -> 开关、
// enum -> Select）。一个新的、没登记过的 Type 会让 switch 落空，
// 表现是"这个参数永远填不了"，而文档页看起来毫无异常。
var allTypes = []Type{
	TypeString, TypeInteger, TypeNumber, TypeBoolean, TypeArray,
	TypeObject, TypeFile, TypeBinary, TypeNone, TypeAny,
}

// In 是参数的位置。
type In string

const (
	InBody   In = "body"
	InQuery  In = "query"
	InPath   In = "path"
	InHeader In = "header"
	InForm   In = "form"
)

var allIns = []In{InBody, InQuery, InPath, InHeader, InForm}

// Required 是必填性的**三态**。
//
// 为什么不是 bool：`billing.orders_create` 的 package_id 与 amount 是
// "二选一，传了 A 就不是必填"—— 用 bool 只能二选一（要么谎称必填，
// 要么谎称可选），而谎称可选会让接入方提交后被 400 打回。
type Required string

const (
	ReqRequired Required = "required"
	ReqOptional Required = "optional"
	// ReqConditional 必须同时给出 RequiredNote（条件原文），由门禁强制。
	ReqConditional Required = "conditional"
)

var allRequired = []Required{ReqRequired, ReqOptional, ReqConditional}

// Constraint 是一条取值约束。
//
// Kind 取 {min,max,min_len,max_len,pattern,multiple_of}；Value 用于数值类，
// Text 是给人看的那句话（"≤ 64"、"1..100"）。两者都留：Value 给机器生成
// OpenAPI 的 minimum/maxLength，Text 给页面直出 —— 只留 Value 会让页面出现
// "最小长度 1"这种需要二次解释的文案。
type Constraint struct {
	Kind  string
	Value float64
	Text  string
}

// Param 是一个入参。
type Param struct {
	// Name 是线上真实的参数名（query/body/header 用原名，path 用 `:id` 里的 id）。
	Name string
	In   In
	Type Type
	// Required 三态；ReqConditional 时 RequiredNote 必填。
	Required     Required
	RequiredNote string
	// Desc 是一句话说明（中文，动词或名词短语，不带句号）。
	Desc string
	// Note 是"会踩的坑"（如"不传就不参与过滤"、"必须是 25+4n"）。可空。
	Note string
	// Default 是人话默认值，只用于展示与 OpenAPI default，**不参与表单预填**。
	// 预填走 Example：默认值是"服务端不传时怎么算"，示例值是"你该填什么"，
	// 两者在 orders_create 这种端点上恰好相反（method 无默认值但示例是 alipay）。
	Default string
	// Enum 只在能从代码坐实时才写。凭印象写的枚举比没有更糟 ——
	// 接入方会照着它写分支，而服务端多一个取值就会漏判。
	Enum []string
	// Example 同时是试运行的预填值与 OpenAPI 的 example，单一来源。
	Example any
	// Constraints 是取值约束。
	Constraints []Constraint
}

// WithNote 追加一句坑。
func (p Param) WithNote(s string) Param { p.Note = s; return p }

// WithReq 给 conditional 参数补条件原文。
func (p Param) WithReq(s string) Param { p.RequiredNote = s; return p }

// WithDefault 设置人话默认值。
func (p Param) WithDefault(s string) Param { p.Default = s; return p }

// WithEnum 设置封闭取值。
func (p Param) WithEnum(v ...string) Param { p.Enum = v; return p }

// WithExample 设置示例值（试运行预填）。
func (p Param) WithExample(v any) Param { p.Example = v; return p }

// WithLimit 追加一条约束。kind 取 min/max/min_len/max_len/pattern/multiple_of。
func (p Param) WithLimit(kind string, value float64, text string) Param {
	p.Constraints = append(p.Constraints, Constraint{Kind: kind, Value: value, Text: text})
	return p
}

// Return 是一个响应字段。
//
// Name 支持**一层**数组折叠：`items[].key_name` 表示 items 是数组、
// 其元素对象的 key_name 字段。只允许一层：两层嵌套（`a[].b[].c`）在
// 本项目的对外响应里不存在，支持它只会让 buildObjectSchema 长出无人验证的
// 递归分支。门禁 TestReturnNameShape 会拒绝两层。
type Return struct {
	Name string
	Type Type
	Desc string
	Note string
	Enum []string
}

// WithNote 追加一句坑（如"为 0 表示不限"）。
func (r Return) WithNote(s string) Return { r.Note = s; return r }

// WithEnum 设置该字段的封闭取值。
func (r Return) WithEnum(v ...string) Return { r.Enum = v; return r }

// ErrCode 是"这个端点可能返回的失败码"。
//
// Code 必须真实存在于 errs.All()（门禁 TestErrCodesExist 逐条核对）。
// 为什么必须核对：文档写了 invald_param 这种拼错的码，接入方会照着它写
// 重试分支，而它永远不会命中 —— 表现是"错误处理代码永远走 default"。
type ErrCode struct {
	Code string
	// When 是触发条件（一句话，说明"什么情况下会遇到"，不重复码本身的含义）。
	When string
}

// SessionWhy 是"为什么这条端点不能在文档页试运行"的分类。
//
// 为什么分类而不是 18 条手写理由：手写必然被复制成 18 句近义废话，而且
// 没有任何测试能断言"这句话是否准确"。四类各有确切边界，文案只需写一遍。
type SessionWhy string

const (
	// WhyNone 表示不需要解释（可试运行的端点）。
	WhyNone SessionWhy = ""
	// WhyCredential 覆盖改密、二次验证 5 条：会改变账号凭证。
	WhyCredential SessionWhy = "credential"
	// WhySession 覆盖退出、设备管理 3 条：操作对象是当前浏览器会话。
	WhySession SessionWhy = "session"
	// WhyKeyManage 覆盖密钥 7 条：API 密钥不能管理 API 密钥。
	WhyKeyManage SessionWhy = "key_manage"
	// WhyKeyIssue 覆盖 apps.hosted_key：签发须账号本人在门户确认。
	WhyKeyIssue SessionWhy = "key_issue"
	// WhyHostedKey 覆盖 keys.self_rotate / keys.health：
	// 这两条只认托管密钥，而文档页的浏览器只有会话 Cookie —— 试运行必然 401。
	// 给一个点了就失败的按钮，比不给更糟：它会让人以为自己的凭证出了问题。
	WhyHostedKey SessionWhy = "hosted_key"
)

var allSessionWhys = []SessionWhy{WhyNone, WhyCredential, WhySession, WhyKeyManage, WhyKeyIssue, WhyHostedKey}

// Text 返回给接入方看的那句话。
func (w SessionWhy) Text() string {
	switch w {
	case WhyCredential:
		return "会改变账号凭证（密码 / 二次验证），只接受浏览器登录会话"
	case WhySession:
		return "操作对象就是你当前这个浏览器会话，不能由外部密钥代理"
	case WhyKeyManage:
		return "API 密钥不能管理 API 密钥 —— 否则一次泄露就能自我续期"
	case WhyKeyIssue:
		return "签发须账号本人在门户确认，密钥明文只在那一次响应里出现"
	case WhyHostedKey:
		return "只接受应用托管密钥，浏览器（会话 Cookie）无法提供，请在应用后端调用"
	default:
		return ""
	}
}

// TrialRisk 是试运行的风险级别。
//
// 这 23 条可试端点打的是**生产**：orders_create 会真建一笔待付订单、
// files.delete 真删对象存储且不可恢复、reset_contact_code 真发一条短信。
// 只给一个"发送"按钮 = 把生产写操作做成一次误点。
type TrialRisk string

const (
	// RiskSafe 是只读、或重复执行无副作用的端点。
	RiskSafe TrialRisk = "safe"
	// RiskConfirm 必须在发送前弹二次确认，文案写具体后果。
	RiskConfirm TrialRisk = "confirm"
)

var allRisks = []TrialRisk{RiskSafe, RiskConfirm}

// Detail 是一条端点的第二层契约。
type Detail struct {
	// Params 是全部入参（path/query/body/header/form 混合，按 In 分区渲染）。
	Params []Param
	// Returns 是 data 内的字段。Media 为 empty/binary/csv 时为空。
	Returns []Return
	// NoData 表示成功时 data 恒为 null（`response.OK(c, nil)`）。
	//
	// 不用"Returns 为空"来表达这件事：那样就分不清
	//   「这个端点成功时没有数据」（如 logout）
	// 与「忘了写返回字段」（正是本轮要消灭的缺陷）。
	// 两者在页面上必须长得不一样：前者写"成功即可，无返回数据"，
	// 后者由 TestJSONHasReturns 直接判红。
	NoData bool
	// Errors 是该端点可能返回的失败码。
	Errors []ErrCode
	// ReqExample 是请求体示例（仅 body/form 端点）。
	// 用 map 而不是保序结构：encoding/json 对 map 键按字典序稳定输出，
	// 不存在"示例 diff 每天抖"的问题，保序类型是多余设计。
	ReqExample map[string]any
	// RespExample 是成功时 data 的示例。
	RespExample map[string]any
	// ScopeEnforced 表示该端点的 Scope 是否真的在运行期被校验。
	//
	// 必须如实标注：ScopeGuard 只对 AuthWriter 校验 scope（writerAllowed），
	// AuthReader 与 AuthSession 都只判身份不判 scope。文档若给 14 条读接口
	// 标"需要 usage:read"，是在让接入方去申请一个不存在也永远用不到的权限点。
	ScopeEnforced bool
	// SessionWhy 是不可试运行的原因分类（Tries()==false 时必填）。
	SessionWhy SessionWhy
	// TrialRisk 是试运行风险（Tries()==true 时生效）。
	TrialRisk TrialRisk
	// Sortable 是本端点支持的排序字段（空表示不支持 sort 参数）。
	//
	// 它是**唯一**的白名单来源：后端 pkg/sorting 据此校验、文档据此渲染
	// 可选值、OpenAPI 据此生成 enum，三处不会漂移。
	Sortable []string
}

// normalize 把 nil 切片换成空切片。
//
// 项目铁律：对外 JSON 数组必须是 `[]` 不能是 `null` —— 前端 `.length` 在
// null 上会抛错，整页白屏而 HTTP 仍是 200。这里一次性规范化全部数组，
// 而不是让每个调用方各自 `?? []`（那必然会漏一处）。
func (d Detail) normalize() Detail {
	if d.Params == nil {
		d.Params = []Param{}
	}
	if d.Returns == nil {
		d.Returns = []Return{}
	}
	if d.Errors == nil {
		d.Errors = []ErrCode{}
	}
	if d.Sortable == nil {
		d.Sortable = []string{}
	}
	for i := range d.Params {
		if d.Params[i].Enum == nil {
			d.Params[i].Enum = []string{}
		}
		if d.Params[i].Constraints == nil {
			d.Params[i].Constraints = []Constraint{}
		}
	}
	for i := range d.Returns {
		if d.Returns[i].Enum == nil {
			d.Returns[i].Enum = []string{}
		}
	}
	// sort 参数由 Sortable 派生（唯一真相源），这里统一追加而不是各处手写。
	// 手写过的端点若已声明 sort，就不再追加 —— 重复声明会让表格出现两行，
	// 而门禁 TestSortParamIsGenerated 会直接判红（禁止手写）。
	if len(d.Sortable) > 0 && !hasParam(d.Params, sortParamName) {
		// 生成的参数自己就是"已规范化"的（Enum / Constraints 都非 nil），
		// 不需要再过一遍 normalize（那会在末尾再追加一次，无限递归）。
		d.Params = append(d.Params, sortParamOf(d.Sortable))
	}
	return d
}

// hasParam 判断是否已声明过某个参数名（跨位置一起看）。
func hasParam(ps []Param, name string) bool {
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Doc 返回该端点的第二层契约（缺失时返回规范化过的零值）。
//
// 为什么是方法查表而不是字段：ruleIndex 存的是 Endpoint 的**值拷贝**，
// 如果在 init 期把 Detail 回填进 catalog 再由 ruleIndex 复制，回填与构建的
// 时序就成了隐性依赖 —— 顺序错了不会报错，只会让参数表全空。
// 按需查表没有时序概念。
func (e Endpoint) Doc() Detail {
	d, ok := details[e.Key]
	if !ok {
		return Detail{}.normalize()
	}
	return d.normalize()
}

// TrialRiskOf 返回归一化后的试运行风险级别：未声明视为 safe。
//
// 为什么需要归一化：`TrialRisk` 是闭合枚举（safe / confirm），空串会让
// 前端 `risk === "confirm"` 之外再多一种隐式状态，也让 OpenAPI 的
// `x-trial-risk` 出现一个非法值。
//
// 为什么只在这里兜底、而不在 normalize() 里改字段：`TestTrialRiskIsExplicit`
// 要能看到**原始声明的空串**，否则"可试端点忘写 TrialRisk"会被静默兜成 safe。
// 因此归一化放在出口（两个投影点共用这个方法），字段本身保持原样。
func (e Endpoint) TrialRiskOf() TrialRisk {
	if r := e.Doc().TrialRisk; r != "" {
		return r
	}
	return RiskSafe
}

// HasDoc 表示该端点是否已声明第二层契约。
// 与 `len(Doc().Params) > 0` 不等价：`files.delete` 只有 path 参数、
// 无返回、无示例；用长度判断会把它误判成"未声明"。
func (e Endpoint) HasDoc() bool {
	_, ok := details[e.Key]
	return ok
}

// DetailKeys 返回已声明第二层契约的 Key（字典序）。
//
// 给门禁做双向差集用：catalog 有而 detail 没有 = 漏写；
// detail 有而 catalog 没有 = Key 打错字（静默失效）。
func DetailKeys() []string {
	out := make([]string, 0, len(details))
	for k := range details {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Tries 表示该端点能否在文档页试运行。
//
// 从 Auth 派生而不是存一个 bool：存下来就有两个真相源，而它们打架的表现
// 正是"文档给了发送按钮、真实调用被守卫 403 session_required"。
// Tries 表示文档页是否提供"试运行"按钮。
//
// 排除两类：
//   - AuthSession：需要真人会话，试运行等于替用户执行敏感操作
//   - AuthHostedKey：只认托管密钥，而浏览器手里只有会话 Cookie —— 试运行必然 401，
//     按钮点一次失败一次，用户会以为是自己的密钥坏了
func (e Endpoint) Tries() bool { return e.Auth != AuthSession && e.Auth != AuthHostedKey }

// TryHint 返回不可试运行的原因（可试时为空串）。
func (e Endpoint) TryHint() string {
	if e.Tries() {
		return ""
	}
	return e.Doc().SessionWhy.Text()
}

// ---------------- 声明助手 ----------------

// body 声明一个请求体字段。
func body(name string, t Type, req Required, desc string) Param {
	return Param{Name: name, In: InBody, Type: t, Required: req, Desc: desc}
}

// query 声明一个查询参数。
func query(name string, t Type, req Required, desc string) Param {
	return Param{Name: name, In: InQuery, Type: t, Required: req, Desc: desc}
}

// path 声明一个路径参数（恒 string + required）。
func path(name, desc string) Param {
	return Param{Name: name, In: InPath, Type: TypeString, Required: ReqRequired, Desc: desc}
}

// header 声明一个请求头。
func header(name string, req Required, desc string) Param {
	return Param{Name: name, In: InHeader, Type: TypeString, Required: req, Desc: desc}
}

// form 声明一个 multipart 表单字段。
func form(name string, t Type, req Required, desc string) Param {
	return Param{Name: name, In: InForm, Type: t, Required: req, Desc: desc}
}

// ret 声明一个响应字段。
func ret(name string, t Type, desc string) Return {
	return Return{Name: name, Type: t, Desc: desc}
}

// sortParamName 是排序参数名。
//
// 抽成常量而不是到处写字面量：有三处要它（自动生成的 Param、门禁的重复检查、
// 后端 handler 读 query），三处各写一遍迟早有一处拼错，而拼错的表现是
// "文档里有 sort，实际不生效"。
const sortParamName = "sort"

// SortParamName 是对外暴露的参数名（给 handler / 测试用，避免各处硬编码）。
func SortParamName() string { return sortParamName }

// sortParamOf 由 Sortable 生成 `sort` 查询参数。
//
// 为什么自动生成而不是在 detail.go 里手写：
//
//	手写就多一份能写错的列表 —— Sortable 改了、参数说明里的枚举没改，
//	于是文档承诺 `sort=-amount` 而服务端白名单里没有 amount。自动生成后，
//	Sortable 是唯一真相源，枚举、OpenAPI 的 enum、试运行下拉框全部同源。
//
// 枚举取 `field` 与 `-field` 两种形态（升/降各一个值）：只列字段名会让
// "怎么倒序"变成必须读说明才知道的事，而试运行的下拉框只有字段名可选时，
// 用户会以为倒序不支持。
func sortParamOf(fields []string) Param {
	enum := make([]string, 0, len(fields)*2)
	for _, f := range fields {
		enum = append(enum, f, "-"+f)
	}
	return Param{
		Name:     sortParamName,
		In:       InQuery,
		Type:     TypeString,
		Required: ReqOptional,
		Desc:     "排序字段，前缀 - 表示倒序",
		// 默认顺序故意不写具体列：各列表历史默认不统一（多数 `id DESC`，
		// 上传件是 `created_at DESC`），而这个参数是五个端点共用生成的 ——
		// 写死一个具体列就等于在四个端点上说错话。
		Note: "只支持一个字段；不传按默认顺序；同值行按 id 次序，翻页不会重复或漏行",
		Enum: enum,
		// 显式给空切片而不是留 nil：normalize() 之后不允许出现 null 数组，
		// 而这个参数是**绕过** normalize 直接追加进去的。
		Constraints: []Constraint{},
	}
}

// errCode 声明一个失败码。
func errCode(code, when string) ErrCode {
	return ErrCode{Code: code, When: when}
}

// PathParams 返回该端点 Path 里的参数名（按出现顺序）。
//
// 不只给测试用：openapi.go 生成 parameters 时也要它来保证顺序稳定
// （map 遍历顺序随机，会让导出文件每次 diff 全变）。
func (e Endpoint) PathParams() []string {
	var out []string
	for _, seg := range strings.Split(e.Path, "/") {
		if strings.HasPrefix(seg, ":") {
			out = append(out, strings.TrimPrefix(seg, ":"))
		}
	}
	return out
}
