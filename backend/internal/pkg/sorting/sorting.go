// Package sorting 把对外接口的 `sort` 查询参数解析成 SQL ORDER BY 片段。
//
// 为什么单独成包（而不是每个列表 handler 里 `c.Query("sort")` 各写一份）：
//
//   - 排序字段是**白名单**。散落实现迟早有一处忘了校验，于是 `sort=password_hash`
//     直接进 ORDER BY —— 而它不会报错，只会慢，或者按一个不该暴露的列排序。
//   - 「解析语法」与「列名映射」必须分开：语法是平台的（`-created_at` 表示倒序），
//     映射是每个表的（`charge` → `usages.charge_micro`）。混在一起，
//     换一个字段就要重写一遍解析。
//
// 三条硬约定：
//
//  1. **只允许单字段**。多字段（`a,-b`）需要为每个字段各配一个方向与一个白名单项，
//     而对外契约里 Sortable 是一维字符串列表 —— 支持多字段就等于让契约与实现
//     各有一套语法。需要时先扩契约。
//  2. **永远追加 `id` 作次序键**。Postgres 不保证同值行的相对顺序，
//     而分页是 OFFSET/LIMIT：同值不 tie-break 时，翻页会重复或漏行，
//     且只在数据量大、有同值时才出现。
//  3. **未知字段报错，不静默回落**。静默回落会让接入方以为排序生效了
//     （尤其当字段拼错但结果"看起来也对"时）。
package sorting

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 解析失败的两种原因。用哨兵错误而不是裸 error：handler 要区分
// "参数拼错"（400，能把白名单回给调用方）与"没有这个能力"（也是 400，
// 但话术不同），并且测试要能断言具体是哪一种。
var (
	// ErrUnknownField 表示字段不在白名单内。
	ErrUnknownField = errors.New("sorting: 不支持的排序字段")
	// ErrTooManyFields 表示传了多个字段。
	ErrTooManyFields = errors.New("sorting: 只支持按一个字段排序")
)

// Order 是一次解析结果。
//
// 零值表示"没传 sort"：Clause() 会给出默认的 `id DESC`，
// 与引入本包之前的行为完全一致（默认顺序不能变，否则前端列表会莫名其妙倒过来）。
type Order struct {
	// Column 是映射后的 SQL 列名（来自调用方白名单，不含用户输入）。
	Column string
	// Desc 表示倒序。
	Desc bool
}

// IsZero 表示没有显式排序。
func (o Order) IsZero() bool { return o.Column == "" }

// Clause 返回可安全拼进 ORDER BY 的片段。
//
// 空 Order → `id DESC`（历史默认，保持所有列表的既有顺序）；
// 非空 → `<col> <dir>, id <dir>`（id 作次序键，见包注释第 2 条）。
func (o Order) Clause() string {
	if o.Column == "" {
		return "id DESC"
	}
	dir := "ASC"
	if o.Desc {
		dir = "DESC"
	}
	return fmt.Sprintf("%s %s, id %s", o.Column, dir, dir)
}

// ClauseOr 在"没传 sort"时返回调用方自己的默认片段。
//
// 为什么需要它：各列表的历史默认并不统一（多数是 `id DESC`，
// 上传件列表是 `created_at DESC, id DESC`）。若一律用 Clause() 的 `id DESC`，
// 加了这个参数就会**悄悄改掉**若干列表的默认顺序 —— 而这种改动不会报错，
// 只会让用户觉得"顺序好像变了"。
//
// 传默认值而不是写死，是把"保持原样"的责任放在调用点上，一眼能审。
func (o Order) ClauseOr(def string) string {
	if o.Column == "" {
		if def == "" {
			return "id DESC"
		}
		return def
	}
	return o.Clause()
}

// FieldError 携带"为什么不行 + 可以传什么"，供 handler 直接给调用方看。
//
// 只把 allowed 排序后输出：白名单是 map，遍历顺序随机，
// 错误文案每次不同会让接入方以为参数在变。
type FieldError struct {
	Raw     string
	Allowed []string
	Reason  error
}

func (e *FieldError) Error() string {
	allowed := strings.Join(e.Allowed, "、")
	if e.Reason == ErrTooManyFields {
		return "sort 只支持按一个字段排序（可选：" + allowed + "）"
	}
	return "sort 不支持字段「" + e.Raw + "」（可选：" + allowed + "）"
}

// Unwrap 让 errors.Is 能用哨兵判断。
func (e *FieldError) Unwrap() error { return e.Reason }

// Parse 解析 `sort` 参数。
//
// 语法：`field`（升序）/ `-field`（降序）/ `+field`（升序，等价于 field）。
// 空串返回零值 Order 与 nil —— "没传"是合法状态，不是错误。
//
// columns 是**契约字段名 → SQL 列名**的映射，由调用方提供（本包不猜列名，
// 也不知道任何一张表）。它的键集合必须与 apicontract 里该端点的 Sortable 一致，
// 由 `internal/apicontract` 的门禁测试双向比对。
func Parse(raw string, columns map[string]string) (Order, error) {
	field := strings.TrimSpace(raw)
	if field == "" {
		return Order{}, nil
	}
	if strings.Contains(field, ",") {
		return Order{}, &FieldError{Raw: field, Allowed: allowedOf(columns), Reason: ErrTooManyFields}
	}
	desc := false
	switch field[0] {
	case '-':
		desc = true
		field = field[1:]
	case '+':
		field = field[1:]
	}
	col, ok := columns[field]
	if !ok || col == "" {
		return Order{}, &FieldError{Raw: field, Allowed: allowedOf(columns), Reason: ErrUnknownField}
	}
	return Order{Column: col, Desc: desc}, nil
}

// allowedOf 把白名单键排序后返回（稳定文案）。
func allowedOf(columns map[string]string) []string {
	out := make([]string, 0, len(columns))
	for k := range columns {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
