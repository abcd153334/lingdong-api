// Package appplugins 是「应用插件」的业务语义：目录（manifest）、安装、记录与导出。
//
// 边界（改之前先读）：插件是**纯声明式**的 —— 只描述「能存什么字段、列表怎么显示」，
// 不执行任何第三方代码，manifest 里不存在可执行或可外联的字段。
//
// 为什么是声明式而不是插件进程：本仓库在 2026-10-25 下线过一套完整的「Python 子进程
// + 沙箱 + AST 审计」插件系统（备份 .workbuddy/backup/plugin-removal-20261025/）。
// 结论是实测冷启动 ≈350ms/次、威胁模型不成立（上传口本来就在管理员手里）、
// 而为了安全做的白名单等于重造一门 DSL。声明式是同一需求的另一档：
// 表达力换零执行风险，够了。
//
// 三条不变量：
//  1. manifest 是插件能力的唯一真相；Name/Version/APIVersion 只是投影列。
//  2. 记录共用一张 JSONB 表，**绝不运行期 DDL** —— 每插件建表会让
//     db/indexes.go 的静态索引清单与 verifyIndexes 失效（缺索引没人知道）。
//  3. 记录的对外身份是 ULID（record_key），业务唯一值只进 unique_norm 列：
//     一个字段不能同时当 URL 标识、唯一约束、审计 target 与升级锚点。
package appplugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"lingdong-api/internal/assets/brandicon"
)

// APIVersion 是本基座能解释的 manifest 版本。**精确匹配**，不做模糊兼容：
// 版本不匹配就拒绝导入，比"尽量试试"更容易定位（同 VS Code 插件的 engines 口径）。
const APIVersion = 1

// 字段类型词表（封闭）。新增一种类型意味着校验 / 渲染 / 导出三处都要跟着改，
// 所以这里刻意只有七种，且没有 money（JSON 数字是 float64，金额会掉精度）
// 与 file（uploads 24h 过期，长期引用会静默变空）。
const (
	TypeString = "string"
	TypeText   = "text"
	TypeInt    = "int"
	TypeBool   = "bool"
	TypeEnum   = "enum"
	TypeDate   = "date"
	TypeLink   = "link"
)

// 动作词表（封闭）。四项都必须在 P0 有实现，否则就是"声明了没人读 = 伪造"。
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
	ActionExport = "export"
	// ActionBatchDelete 批量删除。依赖 delete：只是为了少点几次，不额外授予删除权。
	ActionBatchDelete = "batch_delete"
)

// 基座上限。manifest 只能更严，不能更宽（不信任导入方声明的容量）。
const (
	MaxManifestBytes   = 64 << 10
	MaxEntities        = 10
	MaxFieldsPerEntity = 40
	MaxConfigFields    = 20
	MaxColumns         = 8
	MaxStringLen       = 4000
	MaxUniqueLen       = 64
	MaxLinkLen         = 500
	MaxEnumOptions     = 50
	MaxRecordsCap      = 100000
	DefaultMaxRecords  = 20000
	// MaxSortKeys 可排序白名单上限：界面上只表达单列排序，多余条目没有消费者。
	MaxSortKeys = 3
	// MaxFilters 筛选器上限：工具栏一行放得下、SQL 参数可控。
	MaxFilters = 4
	// MaxBatchDelete 单次批量删除上限（一次请求的参数与审计体积都有界）。
	MaxBatchDelete = 100
	// MaxFormSections 表单分段上限：再多就不该塞进一个弹窗了。
	MaxFormSections = 4
	// MaxHelpLen / MaxPlaceholderLen：字段下方的常驻说明与输入框占位。
	// 超过这个长度就不是"提示"而是"正文"，正文该进开发文档。
	MaxHelpLen        = 200
	MaxPlaceholderLen = 32
	// DefaultFormSectionTitle 是"未列进任何分段的字段"被追加到的那一段的标题。
	// 定成常量而不是让前端写死：文案归内容，前端不造字。
	DefaultFormSectionTitle = "其他"
)

// Field 既是记录字段也是插件配置字段（同一套词表，避免两套类型系统）。
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	MaxLen   int      `json:"maxLen"`
	Options  []string `json:"options"`
	// Default 只在表单初值里用，**不写进记录**：否则改默认值会与存量数据打架。
	//
	// 唯一例外：**必填**字段缺失时由服务端取默认值（见 record.Create）。
	// 可选字段不注入 —— 前端"清空"就是不带这个键，注入会让"清空"永久不可达。
	Default any `json:"default"`
	// Help 是字段下方的常驻说明（"填什么、留空什么后果"），不是"为什么"。
	// 纯规则解释（为什么要这么填）应当写进文档，而不是挤进表单。
	Help string `json:"help"`
	// Placeholder 是输入框的占位提示（≤32 字，长了会被截断成噪音）。
	Placeholder string `json:"placeholder"`
	// ReadOnly 只影响表单渲染（禁改），**不是安全边界**：写入判据仍只有
	// manifest 声明与安装状态，脚本直接 PUT 一样能改。
	ReadOnly bool `json:"readOnly"`
	// Min/Max 只对 int 生效（范围提示 + 服务端校验）。
	Min *int64 `json:"min"`
	Max *int64 `json:"max"`
}

// FormSection 是表单的一段（"基本信息""处理信息"），可两列。
//
// Fields 写字段 key；未列进任何分段的字段由 Describe 追加到末尾的默认段 ——
// 校验层面要求"每个字段恰好属于一段"，投影层面保证"没有字段会消失"。
type FormSection struct {
	Title   string   `json:"title"`
	Columns int      `json:"columns"`
	Fields  []string `json:"fields"`
}

// EntityForm 是表单布局声明。nil = 一段单列（前端按字段声明顺序渲染）。
type EntityForm struct {
	Sections []FormSection `json:"sections"`
}

// SortKey 是 `list.sort` 的一项：字段 + 方向。首项同时是**默认排序**。
type SortKey struct {
	Field string `json:"field"`
	// Order 只有 asc / desc；缺省按 desc（"最新/最大在前"是列表的默认期待）。
	Order string `json:"order"`
}

// EntityList 是实体的列表视图。刻意不做独立的 views[]：
// views[].entity 与 entities[].key 是同一个事实的两次声明，必然出现
// "声明了视图却没有实体"的伪能力；一实体一份列表已经覆盖真实需求。
type EntityList struct {
	Columns []string `json:"columns"`
	Actions []string `json:"actions"`
	// Sort 是可排序**白名单**（≤3，首项为默认排序）。
	//
	// 为什么是白名单而不是"任意列都能排"：允许按 data->>'x' 排序意味着任意
	// JSONB 表达式参与 Sort，代价与注入面都不可控；白名单让"能排哪几列"成为
	// 作者显式声明的能力，也让前端知道该给哪列表头加排序箭头。
	Sort []SortKey `json:"sort"`
	// Filters 是可筛选**白名单**（≤4）。操作符由字段类型决定（见 types_meta.go），
	// 不在这里声明 —— 否则同一个事实会有两份定义。
	Filters []string `json:"filters"`
}

// Entity 是一种业务对象（工单、地址、白名单…）。
type Entity struct {
	Key    string     `json:"key"`
	Name   string     `json:"name"`
	Unique string     `json:"unique"`
	Fields []Field    `json:"fields"`
	List   EntityList `json:"list"`
	// Form 是表单布局（分段 + 列数）。不声明 = 单段单列。
	Form *EntityForm `json:"form"`
	// Public 是**对外只读的字段白名单**：只有列在这里的字段会被免凭证端点
	// （GET /api/apps/:slug/p/:plugin/:entity）下发。空 = 这个对象不对外。
	//
	// 为什么不是 `public: true`：布尔开关等于"整个对象公开"，以后加一个字段就
	// 悄悄多暴露一列，而没人会回头再看这个开关。白名单让每次新增字段都必须
	// 再决定一次"它能不能给外面看"。
	Public []string `json:"public"`
}

// Limits 是插件请求的容量，实际生效值 = min(声明, 基座上限)。
type Limits struct {
	MaxRecords int `json:"maxRecords"`
}

// Manifest 是插件的全部能力声明。
type Manifest struct {
	APIVersion  int      `json:"apiVersion"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Icon        string   `json:"icon"`
	Config      []Field  `json:"config"`
	Entities    []Entity `json:"entities"`
	Limits      *Limits  `json:"limits"`
}

// Problem 是**可批量收集**的校验结论（不 fail-fast）。
//
// 为什么批量：一次导入可能同时有 5 个字段类型写错，逐个报会让 AI/运营改 5 轮。
// Path 形如 entities[0].fields[2].type，前端直接定位到输入框。
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func problem(path, format string, args ...any) Problem {
	return Problem{Path: path, Message: fmt.Sprintf(format, args...)}
}

var (
	keyRe     = regexp.MustCompile(`^[a-z][a-zA-Z0-9_-]{0,31}$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// 保留键：它们会与下发的记录外壳（record_key / version / 时间戳）撞名，
// 撞了之后前端取值取到的是外壳字段，而作者以为是自己的数据。
var reservedFieldKeys = map[string]bool{
	"id": true, "record_key": true, "version": true,
	"created_at": true, "updated_at": true,
}

var actionSet = map[string]bool{
	ActionCreate: true, ActionUpdate: true, ActionDelete: true,
	ActionExport: true, ActionBatchDelete: true,
}

// Decode 严格解析 manifest：未知字段直接拒（先例 planspec.go:76-79 —— 宽容解析
// 曾让提案 100% 被自己的解码器拒掉，且"多写的字段被静默忽略"比报错更危险）。
//
// 解析成功后立即 normalize（补默认值并回写），所以落库的字节是**完整**的：
// 不存在"库里没写 maxLen、读取时各自猜一个默认值"的两份语义。
func Decode(raw []byte) (*Manifest, []Problem) {
	if len(raw) > MaxManifestBytes {
		return nil, []Problem{problem("", "插件定义超过 %d KB，请精简字段或拆成两个插件", MaxManifestBytes/1024)}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, []Problem{decodeProblem(err)}
	}
	// 允许多余的空白，但不允许多个 JSON 值：那说明前端把两个 manifest 拼在了一起。
	if dec.More() {
		return nil, []Problem{problem("", "只允许一个 JSON 对象")}
	}
	m.normalize()
	return &m, nil
}

// decodeProblem 把 encoding/json 的报错翻译成能定位的问题。
//
// 不直出 Go 的英文原文：它会带 `json: cannot unmarshal ...` 这类前缀，
// 而运营看到的是"插件定义不合法 + 具体位置"才有意义。
func decodeProblem(err error) Problem {
	msg := err.Error()
	if i := strings.Index(msg, "unknown field "); i >= 0 {
		name := strings.Trim(strings.TrimSpace(msg[i+len("unknown field "):]), `"`)
		return problem(name, "不支持的字段 %q（基础平台没有这个能力）", name)
	}
	var te *json.UnmarshalTypeError
	if ok := asTypeErr(err, &te); ok && te != nil {
		path := te.Field
		if path == "" {
			path = te.Type.String()
		}
		return problem(path, "类型不正确：期望 %s", te.Type.String())
	}
	if i := strings.Index(msg, "invalid character"); i >= 0 {
		return problem("", "不是合法的 JSON（%s）", msg[i:])
	}
	return problem("", "不是合法的 JSON")
}

// asTypeErr 用一个窄接口避开 errors.As 的泛型样板（这里只需要一个分支）。
func asTypeErr(err error, out **json.UnmarshalTypeError) bool {
	for err != nil {
		if te, ok := err.(*json.UnmarshalTypeError); ok {
			*out = te
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// normalize 补默认值并去掉首尾空白。**只做"填缺省"，不改语义**：
// 把 maxLen 缺省填成具体数字是为了让校验与渲染看到同一份值。
func (m *Manifest) normalize() {
	m.Key = strings.TrimSpace(m.Key)
	m.Name = strings.TrimSpace(m.Name)
	m.Version = strings.TrimSpace(m.Version)
	m.Description = strings.TrimSpace(m.Description)
	m.Icon = strings.TrimSpace(m.Icon)
	if m.Limits == nil {
		m.Limits = &Limits{}
	}
	if m.Limits.MaxRecords <= 0 {
		m.Limits.MaxRecords = DefaultMaxRecords
	}
	for i := range m.Config {
		m.Config[i].normalize()
	}
	for i := range m.Entities {
		e := &m.Entities[i]
		e.Key = strings.TrimSpace(e.Key)
		e.Name = strings.TrimSpace(e.Name)
		e.Unique = strings.TrimSpace(e.Unique)
		for j := range e.Fields {
			e.Fields[j].normalize()
		}
		for j := range e.List.Columns {
			e.List.Columns[j] = strings.TrimSpace(e.List.Columns[j])
		}
		for j := range e.List.Sort {
			e.List.Sort[j].Field = strings.TrimSpace(e.List.Sort[j].Field)
			o := strings.ToLower(strings.TrimSpace(e.List.Sort[j].Order))
			if o == "" {
				o = "desc" // 缺省方向：最新/最大在前
			}
			e.List.Sort[j].Order = o
		}
		for j := range e.List.Filters {
			e.List.Filters[j] = strings.TrimSpace(e.List.Filters[j])
		}
		if e.Form != nil {
			for j := range e.Form.Sections {
				sec := &e.Form.Sections[j]
				sec.Title = strings.TrimSpace(sec.Title)
				if sec.Columns <= 0 {
					sec.Columns = 1
				}
				for k := range sec.Fields {
					sec.Fields[k] = strings.TrimSpace(sec.Fields[k])
				}
			}
		}
	}
}

func (f *Field) normalize() {
	f.Key = strings.TrimSpace(f.Key)
	f.Label = strings.TrimSpace(f.Label)
	f.Type = strings.ToLower(strings.TrimSpace(f.Type))
	if f.MaxLen <= 0 {
		switch f.Type {
		case TypeString:
			f.MaxLen = 64
		case TypeText:
			f.MaxLen = 1000
		case TypeLink:
			f.MaxLen = MaxLinkLen
		}
	}
	for i := range f.Options {
		f.Options[i] = strings.TrimSpace(f.Options[i])
	}
}

// Validate 一次收全所有问题。wantKey 非空时要求 m.Key 与 URL 里的 :key 一致
// （否则同一份定义可以挂在两个地址上，安装记录会指向哪个？）。
func (m *Manifest) Validate(wantKey string) []Problem {
	var ps []Problem
	add := func(p Problem) { ps = append(ps, p) }

	if m.APIVersion != APIVersion {
		add(problem("apiVersion", "只支持 apiVersion=%d，收到 %d", APIVersion, m.APIVersion))
	}
	if !keyRe.MatchString(m.Key) {
		add(problem("key", "标识需要小写字母开头，只含字母/数字/下划线/连字符，最长 32 位"))
	} else if wantKey != "" && m.Key != wantKey {
		add(problem("key", "标识必须与地址里的 %q 一致", wantKey))
	}
	if n := utf8.RuneCountInString(m.Name); n == 0 || n > 64 {
		add(problem("name", "名称需要 1-64 个字符"))
	}
	if !versionRe.MatchString(m.Version) {
		add(problem("version", "版本号需要形如 1.0.0"))
	}
	if utf8.RuneCountInString(m.Description) > 200 {
		add(problem("description", "说明最多 200 个字符"))
	}
	if p, ok := iconProblem(m.Icon); !ok {
		add(p)
	}
	if len(m.Config) > MaxConfigFields {
		add(problem("config", "配置项最多 %d 个", MaxConfigFields))
	}
	ps = append(ps, validateFields(m.Config, "config")...)

	if len(m.Entities) == 0 {
		add(problem("entities", "至少需要一个数据对象"))
	}
	if len(m.Entities) > MaxEntities {
		add(problem("entities", "数据对象最多 %d 个", MaxEntities))
	}
	seenEntity := map[string]bool{}
	for i := range m.Entities {
		e := &m.Entities[i]
		base := fmt.Sprintf("entities[%d]", i)
		if !keyRe.MatchString(e.Key) {
			add(problem(base+".key", "标识需要小写字母开头，只含字母/数字/下划线/连字符，最长 32 位"))
		} else if seenEntity[e.Key] {
			add(problem(base+".key", "对象标识 %q 重复", e.Key))
		}
		seenEntity[e.Key] = true
		if n := utf8.RuneCountInString(e.Name); n == 0 || n > 64 {
			add(problem(base+".name", "名称需要 1-64 个字符"))
		}
		if len(e.Fields) == 0 {
			add(problem(base+".fields", "至少需要一个字段"))
		}
		if len(e.Fields) > MaxFieldsPerEntity {
			add(problem(base+".fields", "字段最多 %d 个", MaxFieldsPerEntity))
		}
		ps = append(ps, validateFields(e.Fields, base+".fields")...)

		// list
		if len(e.List.Columns) == 0 {
			add(problem(base+".list.columns", "至少选择一列用于列表展示"))
		}
		if len(e.List.Columns) > MaxColumns {
			add(problem(base+".list.columns", "列表列最多 %d 个", MaxColumns))
		}
		seenCol := map[string]bool{}
		for j, c := range e.List.Columns {
			if _, ok := e.field(c); !ok {
				add(problem(fmt.Sprintf("%s.list.columns[%d]", base, j), "字段 %q 不存在", c))
			}
			if seenCol[c] {
				add(problem(fmt.Sprintf("%s.list.columns[%d]", base, j), "列 %q 重复", c))
			}
			seenCol[c] = true
		}
		if len(e.List.Actions) == 0 {
			add(problem(base+".list.actions", "至少声明一个操作"))
		}
		seenAct := map[string]bool{}
		for j, a := range e.List.Actions {
			if !actionSet[a] {
				add(problem(fmt.Sprintf("%s.list.actions[%d]", base, j), "不支持的操作 %q", a))
			}
			if seenAct[a] {
				add(problem(fmt.Sprintf("%s.list.actions[%d]", base, j), "操作 %q 重复", a))
			}
			seenAct[a] = true
		}
		// 批量删除依赖删除：它只是"少点几次"，不额外授予删除权。
		if seenAct[ActionBatchDelete] && !seenAct[ActionDelete] {
			add(problem(base+".list.actions", "批量删除依赖删除操作，请同时声明 %q", ActionDelete))
		}

		// sort：白名单。字段存在 + 类型可排 + 不重复 + 方向合法 + 数量上限。
		if len(e.List.Sort) > MaxSortKeys {
			add(problem(base+".list.sort", "可排序字段最多 %d 个", MaxSortKeys))
		}
		seenSort := map[string]bool{}
		for j := range e.List.Sort {
			sk := &e.List.Sort[j]
			p := fmt.Sprintf("%s.list.sort[%d]", base, j)
			if sk.Order != "asc" && sk.Order != "desc" {
				add(problem(p+".order", "排序方向只能是 asc 或 desc"))
			}
			if seenSort[sk.Field] {
				add(problem(p+".field", "字段 %q 重复", sk.Field))
			}
			seenSort[sk.Field] = true
			if _, ok := builtinSortable(sk.Field); ok {
				continue // 内置键（编号/创建时间/更新时间）不走字段表
			}
			f, ok := e.field(sk.Field)
			if !ok {
				add(problem(p+".field", "字段 %q 不存在", sk.Field))
				continue
			}
			spec, _ := typeSpec(f.Type)
			switch {
			case !spec.CanSort:
				add(problem(p+".field", "%s 类型不能排序", f.Type))
			case spec.SortNeedsShort && f.MaxLen > MaxUniqueLen:
				add(problem(p+".field", "排序字段最长 %d 个字符（当前 %d）", MaxUniqueLen, f.MaxLen))
			}
		}

		// filters：白名单。操作符由字段类型决定，不在这里重复声明。
		if len(e.List.Filters) > MaxFilters {
			add(problem(base+".list.filters", "可筛选字段最多 %d 个", MaxFilters))
		}
		seenFilter := map[string]bool{}
		for j, k := range e.List.Filters {
			p := fmt.Sprintf("%s.list.filters[%d]", base, j)
			if seenFilter[k] {
				add(problem(p, "字段 %q 重复", k))
			}
			seenFilter[k] = true
			f, ok := e.field(k)
			if !ok {
				add(problem(p, "字段 %q 不存在", k))
				continue
			}
			if spec, _ := typeSpec(f.Type); spec.FilterOp == "" {
				add(problem(p, "%s 类型不支持筛选", f.Type))
			}
		}

		// form：分段布局。分段的字段必须存在、不许跨段重复、段内不能为空。
		// 「未列出的字段」不报错：投影期会被追加到末段（保证"声明了却看不见"不会发生）。
		if e.Form != nil {
			if len(e.Form.Sections) > MaxFormSections {
				add(problem(base+".form.sections", "表单分段最多 %d 个", MaxFormSections))
			}
			if len(e.Form.Sections) == 0 {
				add(problem(base+".form.sections", "声明了 form 就至少要有一个分段（不声明即为单段单列）"))
			}
			seenForm := map[string]bool{}
			for j := range e.Form.Sections {
				sec := &e.Form.Sections[j]
				p := fmt.Sprintf("%s.form.sections[%d]", base, j)
				if n := utf8.RuneCountInString(sec.Title); n == 0 || n > 32 {
					add(problem(p+".title", "分段标题需要 1-32 个字符"))
				}
				if sec.Columns != 1 && sec.Columns != 2 {
					add(problem(p+".columns", "列数只能是 1 或 2"))
				}
				if len(sec.Fields) == 0 {
					add(problem(p+".fields", "分段里至少要有一个字段"))
				}
				for k, fk := range sec.Fields {
					fp := fmt.Sprintf("%s.fields[%d]", p, k)
					if _, ok := e.field(fk); !ok {
						add(problem(fp, "字段 %q 不存在", fk))
					}
					if seenForm[fk] {
						add(problem(fp, "字段 %q 在多个分段里重复", fk))
					}
					seenForm[fk] = true
				}
			}
		}

		// unique 只能指向可比较、进得了 unique_norm(≤64) 的字段。
		if e.Unique != "" {
			f, ok := e.field(e.Unique)
			switch {
			case !ok:
				add(problem(base+".unique", "字段 %q 不存在", e.Unique))
			case f.Type == TypeText || f.Type == TypeBool || f.Type == TypeLink:
				add(problem(base+".unique", "%s 类型不能作为唯一字段", f.Type))
			case f.MaxLen > MaxUniqueLen:
				add(problem(base+".unique", "唯一字段最长 %d 个字符（当前 %d）", MaxUniqueLen, f.MaxLen))
			}
		}

		// public 白名单：只接受本对象声明过的字段，且不许重复。
		// 不校验"必须包含 unique"——对外列表不必带业务唯一值，那是作者的选择。
		seenPub := map[string]bool{}
		for j, k := range e.Public {
			p := fmt.Sprintf("%s.public[%d]", base, j)
			if _, ok := e.field(k); !ok {
				add(problem(p, "字段 %q 不存在", k))
			}
			if seenPub[k] {
				add(problem(p, "字段 %q 重复", k))
			}
			seenPub[k] = true
		}
	}

	if m.Limits.MaxRecords > MaxRecordsCap {
		add(problem("limits.maxRecords", "最多 %d 条（当前声明 %d）", MaxRecordsCap, m.Limits.MaxRecords))
	}
	return ps
}

func validateFields(fields []Field, base string) []Problem {
	var ps []Problem
	seen := map[string]bool{}
	for i := range fields {
		f := &fields[i]
		p := fmt.Sprintf("%s[%d]", base, i)
		if !keyRe.MatchString(f.Key) {
			ps = append(ps, problem(p+".key", "标识需要小写字母开头，只含字母/数字/下划线/连字符，最长 32 位"))
		} else if reservedFieldKeys[f.Key] {
			ps = append(ps, problem(p+".key", "%q 是系统保留字段名", f.Key))
		} else if seen[f.Key] {
			ps = append(ps, problem(p+".key", "字段 %q 重复", f.Key))
		}
		seen[f.Key] = true
		if n := utf8.RuneCountInString(f.Label); n == 0 || n > 32 {
			ps = append(ps, problem(p+".label", "显示名需要 1-32 个字符"))
		}
		if !typeSpecOK(f.Type) {
			ps = append(ps, problem(p+".type", "不支持的类型 %q（可用 string/text/int/bool/enum/date/link）", f.Type))
			continue
		}
		switch f.Type {
		case TypeString, TypeText:
			if f.MaxLen <= 0 || f.MaxLen > MaxStringLen {
				ps = append(ps, problem(p+".maxLen", "长度需要在 1-%d 之间", MaxStringLen))
			}
		case TypeLink:
			if f.MaxLen <= 0 || f.MaxLen > MaxLinkLen {
				ps = append(ps, problem(p+".maxLen", "长度需要在 1-%d 之间", MaxLinkLen))
			}
		case TypeEnum:
			if len(f.Options) == 0 {
				ps = append(ps, problem(p+".options", "枚举至少需要一个取值"))
			}
			if len(f.Options) > MaxEnumOptions {
				ps = append(ps, problem(p+".options", "枚举最多 %d 个取值", MaxEnumOptions))
			}
			seenOpt := map[string]bool{}
			for j, o := range f.Options {
				if o == "" || utf8.RuneCountInString(o) > 32 {
					ps = append(ps, problem(fmt.Sprintf("%s.options[%d]", p, j), "取值需要 1-32 个字符"))
				}
				if seenOpt[o] {
					ps = append(ps, problem(fmt.Sprintf("%s.options[%d]", p, j), "取值 %q 重复", o))
				}
				seenOpt[o] = true
			}
		}

		// 表单提示：只校验形状（长度/范围），文案内容不在这里评判。
		if n := utf8.RuneCountInString(f.Help); n > MaxHelpLen {
			ps = append(ps, problem(p+".help", "说明最多 %d 个字符（当前 %d），长文请写进开发文档", MaxHelpLen, n))
		}
		if n := utf8.RuneCountInString(f.Placeholder); n > MaxPlaceholderLen {
			ps = append(ps, problem(p+".placeholder", "占位提示最多 %d 个字符", MaxPlaceholderLen))
		}
		if f.Min != nil || f.Max != nil {
			switch {
			case f.Type != TypeInt:
				ps = append(ps, problem(p+".min", "只有整数类型支持 min/max"))
			case f.Min != nil && f.Max != nil && *f.Min > *f.Max:
				ps = append(ps, problem(p+".min", "最小值不能大于最大值"))
			}
		}
		// 默认值本身必须合法：必填字段缺失时服务端会**拿它入账**，
		// 一个不合法的 default 等于把脏数据写进记录（且从表单上看不出是它干的）。
		if f.Default != nil {
			if _, msg := normalizeField(f, f.Default, true); msg != "" {
				ps = append(ps, problem(p+".default", "默认值不合法：%s", msg))
			}
		}
	}
	return ps
}

// iconProblem 校验图标令牌：空可以；builtin: 必须在品牌图标目录里存在
// （否则 iconref 会返回空串，页面留一个洞）；upload: 只校验形状 ——
// 图标行是否存在属于运行期事实，不该让"上传的图被清理"阻塞导入。
func iconProblem(token string) (Problem, bool) {
	if token == "" {
		return Problem{}, true
	}
	if key, ok := strings.CutPrefix(token, brandicon.BuiltinPrefix); ok {
		if _, exists := brandicon.Lookup(key); !exists {
			return problem("icon", "内置图标 %q 不存在", key), false
		}
		return Problem{}, true
	}
	if _, ok := strings.CutPrefix(token, brandicon.UploadPrefix); !ok {
		return problem("icon", "图标需要以 builtin: 或 upload: 开头"), false
	}
	if _, err := brandicon.Normalize(token); err != nil {
		return problem("icon", "上传图标的标识不合法"), false
	}
	return Problem{}, true
}

// field 按 key 找字段（唯一实现：列表列、unique 校验、写入投影都用它）。
func (e *Entity) field(key string) (*Field, bool) {
	for i := range e.Fields {
		if e.Fields[i].Key == key {
			return &e.Fields[i], true
		}
	}
	return nil, false
}

// FieldByKey 是 field 的导出形态：controller 的导出要按列取字段定义（label/type），
// 而 field 本身不导出（写路径只应该在服务层）。
func (e *Entity) FieldByKey(key string) (*Field, bool) { return e.field(key) }

// EntityByKey 按 key 找实体；供记录读写与导出定位。
func (m *Manifest) EntityByKey(key string) (*Entity, bool) {
	for i := range m.Entities {
		if m.Entities[i].Key == key {
			return &m.Entities[i], true
		}
	}
	return nil, false
}

// ---- 编译产物（前端只认它，不解析 manifest）----

type FieldView struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	MaxLen   int      `json:"maxLen"`
	Options  []string `json:"options,omitempty"`
	Default  any      `json:"default,omitempty"`
	// Help/Placeholder/ReadOnly/Min/Max 是表单呈现的全部依据：前端不再读 manifest。
	Help        string `json:"help,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
	Min         *int64 `json:"min,omitempty"`
	Max         *int64 `json:"max,omitempty"`
}

// FormSectionView 是**投影后**的表单分段：字段 key 已补全为"本对象全部字段恰好出现一次"。
// 前端不需要再算"哪些字段没排进去"，也不需要自己造一个默认段标题。
type FormSectionView struct {
	Title   string   `json:"title"`
	Columns int      `json:"columns"`
	Fields  []string `json:"fields"`
}

// FormView 恒至少有一段（未声明 form 时就是"单段单列、按字段声明顺序"），
// 因此前端直接 for 循环即可，不做空值判断、不写兜底分支。
type FormView struct {
	Sections []FormSectionView `json:"sections"`
}

type ColumnView struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

// SortView 是排序白名单的一项（含默认方向）。前端据此决定哪列可点、初始箭头朝哪。
type SortView struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
	Order string `json:"order"`
}

// FilterView 是筛选白名单的一项。Op 是**行为声明**（eq/range/contains），
// 前端按它选控件，服务端按它拼 WHERE —— 两边同源，不做类型推断的第二份实现。
type FilterView struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"`
	Op    string `json:"op"`
}

type ListView struct {
	Columns []ColumnView `json:"columns"`
	Actions []string     `json:"actions"`
	// Sort/Filters 恒为数组（空也是 []）：前端直接 .length，不做空值判断。
	Sort    []SortView   `json:"sort"`
	Filters []FilterView `json:"filters"`
}

type EntityView struct {
	Key    string      `json:"key"`
	Name   string      `json:"name"`
	Unique string      `json:"unique"`
	Fields []FieldView `json:"fields"`
	List   ListView    `json:"list"`
	// Form 是表单布局（恒有值，见 FormView）。
	Form FormView `json:"form"`
	// Public 是可直接对外读取的字段 key（顺序按 manifest）。空数组 = 不对外，
	// 前端据此决定"对外只读"开关是否有效、复制哪个地址。
	Public []string `json:"public"`
}

// Descriptor 是 manifest 的**投影**（只多带 label/type 便于渲染），
// 不是第二份定义：列顺序、字段集合都直接来自 manifest 的同一份数据。
type Descriptor struct {
	Key        string       `json:"key"`
	Name       string       `json:"name"`
	Version    string       `json:"version"`
	APIVersion int          `json:"api_version"`
	Icon       string       `json:"icon"`
	IconURL    string       `json:"icon_url"`
	Config     []FieldView  `json:"config"`
	Entities   []EntityView `json:"entities"`
	Limits     Limits       `json:"limits"`
}

func (f *Field) view() FieldView {
	return FieldView{
		Key: f.Key, Label: f.Label, Type: f.Type,
		Required: f.Required, MaxLen: f.MaxLen,
		Options: f.Options, Default: f.Default,
		Help: f.Help, Placeholder: f.Placeholder, ReadOnly: f.ReadOnly,
		Min: f.Min, Max: f.Max,
	}
}

// Describe 把 manifest 编译成前端描述。列在编译期就解析成 {key,label,type}，
// 前端因此不需要知道"columns 里写的是字段名"这件事。
func (m *Manifest) Describe() *Descriptor {
	d := &Descriptor{
		Key: m.Key, Name: m.Name, Version: m.Version,
		APIVersion: m.APIVersion, Icon: m.Icon,
		Config:   make([]FieldView, 0, len(m.Config)),
		Entities: make([]EntityView, 0, len(m.Entities)),
	}
	if m.Limits != nil {
		d.Limits = *m.Limits
	}
	for i := range m.Config {
		d.Config = append(d.Config, m.Config[i].view())
	}
	for i := range m.Entities {
		e := &m.Entities[i]
		ev := EntityView{Key: e.Key, Name: e.Name, Unique: e.Unique}
		// 恒为数组（空也是 []，不是 null）：前端直接 .length，不做空值判断。
		ev.Public = append(make([]string, 0, len(e.Public)), e.Public...)
		ev.Fields = make([]FieldView, 0, len(e.Fields))
		for j := range e.Fields {
			ev.Fields = append(ev.Fields, e.Fields[j].view())
		}
		ev.List.Actions = append(make([]string, 0, len(e.List.Actions)), e.List.Actions...)
		for _, c := range e.List.Columns {
			if f, ok := e.field(c); ok {
				ev.List.Columns = append(ev.List.Columns, ColumnView{Key: f.Key, Label: f.Label, Type: f.Type})
			}
		}
		ev.List.Sort = make([]SortView, 0, len(e.List.Sort))
		for j := range e.List.Sort {
			sk := &e.List.Sort[j]
			if t, ok := builtinSortable(sk.Field); ok {
				ev.List.Sort = append(ev.List.Sort, SortView{
					Key: sk.Field, Label: builtinSortLabel(sk.Field), Type: t, Order: sk.Order,
				})
				continue
			}
			if f, ok := e.field(sk.Field); ok {
				ev.List.Sort = append(ev.List.Sort, SortView{Key: f.Key, Label: f.Label, Type: f.Type, Order: sk.Order})
			}
		}
		ev.List.Filters = make([]FilterView, 0, len(e.List.Filters))
		for _, k := range e.List.Filters {
			if f, ok := e.field(k); ok {
				spec, _ := typeSpec(f.Type)
				ev.List.Filters = append(ev.List.Filters, FilterView{
					Key: f.Key, Label: f.Label, Type: f.Type, Op: spec.FilterOp,
				})
			}
		}
		ev.Form = e.formView()
		d.Entities = append(d.Entities, ev)
	}
	return d
}

// formView 把表单声明投影成"每个字段恰好出现一次"的分段列表。
//
// 两条保证（前端因此不需要兜底分支）：
//  1. 至少有一段；
//  2. 声明过、但 form 里漏掉的字段会被追加到末段 —— 否则"加了字段却没有输入框"
//     会变成一类只有用户才会撞上的 bug（后台表单必须覆盖前台每个字，反过来也不行）。
func (e *Entity) formView() FormView {
	listed := map[string]bool{}
	out := FormView{Sections: make([]FormSectionView, 0, 2)}
	if e.Form != nil {
		for _, sec := range e.Form.Sections {
			fields := make([]string, 0, len(sec.Fields))
			for _, k := range sec.Fields {
				if _, ok := e.field(k); !ok {
					continue // 校验期已报错，投影期跳过：不把不存在的 key 抛给前端
				}
				if listed[k] {
					continue
				}
				listed[k] = true
				fields = append(fields, k)
			}
			if len(fields) == 0 {
				continue
			}
			cols := sec.Columns
			if cols != 2 {
				cols = 1
			}
			out.Sections = append(out.Sections, FormSectionView{Title: sec.Title, Columns: cols, Fields: fields})
		}
	}
	rest := make([]string, 0, len(e.Fields))
	for i := range e.Fields {
		if !listed[e.Fields[i].Key] {
			rest = append(rest, e.Fields[i].Key)
		}
	}
	if len(rest) > 0 {
		// 未声明 form 时标题留空：前端不渲染段头，与"没有分段"看起来完全一样。
		title := ""
		if e.Form != nil {
			title = DefaultFormSectionTitle
		}
		out.Sections = append(out.Sections, FormSectionView{Title: title, Columns: 1, Fields: rest})
	}
	return out
}

// ActionAllowed 判断实体是否声明了某个操作（导出/建/改/删的入口闸）。
func (e *Entity) ActionAllowed(action string) bool {
	for _, a := range e.List.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// builtinSortLabel 是内置排序键的列名（进 descriptor 的表头）。
func builtinSortLabel(key string) string {
	switch key {
	case SortFieldRecordKey:
		return "编号"
	case SortFieldCreatedAt:
		return "创建时间"
	case SortFieldUpdatedAt:
		return "更新时间"
	default:
		return key
	}
}

// ConfigField 按 key 找配置项（Patch 裁剪存量键、启用前必填校验共用一处判据）。
func (m *Manifest) ConfigField(key string) (*Field, bool) {
	for i := range m.Config {
		if m.Config[i].Key == key {
			return &m.Config[i], true
		}
	}
	return nil, false
}

// SortAllowed 判断字段能否排序，返回展示类型。
//
// 与 Validate 的白名单**同一判据**：所以"能导入的"与"能排的"不会各说一套。
func (e *Entity) SortAllowed(key string) (string, bool) {
	if t, ok := builtinSortable(key); ok {
		return t, true
	}
	f, ok := e.field(key)
	if !ok {
		return "", false
	}
	spec, ok := typeSpec(f.Type)
	if !ok || !spec.CanSort {
		return "", false
	}
	if spec.SortNeedsShort && f.MaxLen > MaxUniqueLen {
		return "", false
	}
	return f.Type, true
}

// FilterAllowed 判断字段能否筛选，返回算子。理由同 SortAllowed。
func (e *Entity) FilterAllowed(key string) (string, bool) {
	f, ok := e.field(key)
	if !ok {
		return "", false
	}
	spec, ok := typeSpec(f.Type)
	if !ok || spec.FilterOp == "" {
		return "", false
	}
	return spec.FilterOp, true
}

// dateLayout 是 date 类型的唯一格式：不带时区、不带时分秒。
// 带时区会让"同一天"在跨时区下变成两天，而插件里没有时区配置这个概念。
const dateLayout = "2006-01-02"

func parseDate(s string) (time.Time, error) { return time.Parse(dateLayout, s) }
