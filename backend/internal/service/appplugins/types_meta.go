package appplugins

/**
 * 字段类型的**能力矩阵**（唯一真相源）。
 *
 * 为什么单独一张表：同一件事原来散落在三处 —— 类型词表在 `fieldTypes`、
 * 长度上限在 `validateFields` 的 switch、可筛选/可排序的能力只存在于文档里。
 * 文档里的人话表格没有任何门禁，实测已经漂过一次（文档说"不提供对外读端点"，
 * 而同一篇文档的 §3.4 就在讲对外只读）。现在校验与文档生成都读这张表，
 * 加一种类型/改一个能力只改这里。
 *
 * 字段顺序即文档里的表格顺序（也即作者读到的顺序）：先常用标量，再长文本，最后链接。
 */

/** 筛选算子。前端按它选控件，后端按它拼 WHERE，两边同源。 */
const (
	// FilterOpEq 等值：enum / bool
	FilterOpEq = "eq"
	// FilterOpRange 区间（`a..b`，任一侧可空）：int / date
	FilterOpRange = "range"
	// FilterOpContains 包含（大小写不敏感，`%_\` 已转义）：string
	FilterOpContains = "contains"
)

// TypeSpec 描述一种字段类型在基座里的全部能力。
type TypeSpec struct {
	Type string
	// Control 是表单控件（前端按它渲染；文档里也用它说明"填什么"）。
	Control string
	// TableCell 是表格里的呈现方式。
	TableCell string
	// FilterOp 为空表示**不能**作为筛选器（text/link）。
	FilterOp string
	// MaxLenCap 是该类型 maxLen 的上限（0 = 长度不适用）。
	MaxLenCap int
	// CanSort 能否作为排序键。
	//
	// text/link 不可排序：text 无界（排序即整表 Sort + TOAST 读放大），
	// link 是长 URL；两者排出来的顺序对使用者没有意义。
	CanSort bool
	// SortNeedsShort 表示可排序但要求 maxLen ≤ MaxUniqueLen（string 走文本比较，
	// 过长只会在 SQL 里做大字符串排序，收益为零）。
	SortNeedsShort bool
}

// TypeSpecs 是封闭词表：新增一种类型意味着校验、渲染、导出、文档四处都要跟着动。
var TypeSpecs = []TypeSpec{
	{
		Type: TypeString, Control: "input", TableCell: "text",
		FilterOp: FilterOpContains, MaxLenCap: MaxStringLen,
		CanSort: true, SortNeedsShort: true,
	},
	{
		Type: TypeEnum, Control: "select", TableCell: "text",
		FilterOp: FilterOpEq, CanSort: true,
	},
	{
		Type: TypeInt, Control: "number", TableCell: "number",
		FilterOp: FilterOpRange, CanSort: true,
	},
	{
		Type: TypeDate, Control: "date", TableCell: "date",
		FilterOp: FilterOpRange, CanSort: true,
	},
	{
		Type: TypeBool, Control: "switch", TableCell: "bool",
		FilterOp: FilterOpEq, CanSort: true,
	},
	{
		Type: TypeText, Control: "textarea", TableCell: "text",
		MaxLenCap: MaxStringLen,
	},
	{
		Type: TypeLink, Control: "url", TableCell: "link",
		MaxLenCap: MaxLinkLen,
	},
}

// typeSpec 按类型取能力；未知类型返回 false（校验据此报"不支持的类型"）。
func typeSpec(t string) (TypeSpec, bool) {
	for i := range TypeSpecs {
		if TypeSpecs[i].Type == t {
			return TypeSpecs[i], true
		}
	}
	return TypeSpec{}, false
}

// typeSpecOK 只回答"这个词表里有吗"（校验用，读起来比二元组解构清楚）。
func typeSpecOK(t string) bool {
	_, ok := typeSpec(t)
	return ok
}

// typeLabel 给文档用的中文名（与前端控件文案一致）。
func typeLabel(t string) string {
	switch t {
	case TypeString:
		return "短文本"
	case TypeText:
		return "长文本"
	case TypeInt:
		return "整数"
	case TypeBool:
		return "开关"
	case TypeEnum:
		return "枚举"
	case TypeDate:
		return "日期"
	case TypeLink:
		return "链接"
	default:
		return t
	}
}

// SortField* 是**内置排序键**：不进 manifest 的字段表，但可以写进 `list.sort`。
//
// 为什么需要：表格最常见的默认排序是"最近更新"，而 id DESC 只能近似创建顺序；
// 让作者为此多声明一个冗余字段（还得自己维护）是本末倒置。
// 这三个键是记录外壳列（不是 JSONB 里的数据），排序走真列、代价可忽略。
const (
	SortFieldRecordKey = "record_key"
	SortFieldCreatedAt = "created_at"
	SortFieldUpdatedAt = "updated_at"
)

/** 内置排序键的展示类型（descriptor 用；前端只按它决定列对齐）。 */
const (
	SortTypeString   = "string"
	SortTypeDatetime = "datetime"
)

var builtinSortFields = map[string]string{
	SortFieldRecordKey: SortTypeString,
	SortFieldCreatedAt: SortTypeDatetime,
	SortFieldUpdatedAt: SortTypeDatetime,
}

// builtinSortable 判断是否内置排序键，并返回它的展示类型。
func builtinSortable(key string) (string, bool) {
	t, ok := builtinSortFields[key]
	return t, ok
}
