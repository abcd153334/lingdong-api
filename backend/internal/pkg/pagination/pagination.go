package pagination

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 100
	// MaxPage 是页码上界。
	//
	// 没有它时 `(page-1)*page_size` 可以溢出成负数，后果分两种：GORM 会把
	// 负 OFFSET 夹成 0（静默返回第一页，但响应体里的 page 回显天文数字），
	// 而管理台那几处直接绑 `LIMIT ? OFFSET ?` 的原始 SQL 会让 PG 报 2201X，
	// 最终折成 500 —— 一个查询参数就能把管理台列表打成"服务端故障"。
	MaxPage = 100000
)

var sortFieldRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Params 是 T21 分页请求参数：?page&page_size&sort=field:asc|desc。
type Params struct {
	Page     int
	PageSize int
	SortRaw  string
	Field    string
	Desc     bool
}

// Result 是分页响应体：{items,total,page,page_size}。
type Result struct {
	Items    any   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

// Offset 返回 SQL OFFSET。
//
// 用 int64 算再夹紧，是第二道防线：`page` 已被 Parse 用 MaxPage 拦住，
// 但手工构造 Params 的调用方（测试、内部工具、将来新增的入口）不该能把
// 负数塞进 SQL。夹到 0 的语义是"越界即回到第一页"，与 GORM 对负 OFFSET
// 的处理一致，不会让调用方拿到一个静默错误的页。
func (p Params) Offset() int {
	page, size := int64(p.Page), int64(p.PageSize)
	if page < 1 || size < 1 {
		return 0
	}
	off := (page - 1) * size
	if off < 0 || off > int64(MaxPage)*int64(MaxPageSize) {
		return 0
	}
	return int(off)
}

// OffsetOf 是 Offset 的自由函数形态：服务层手里只有两个 int（ListQuery.Page/Size），
// 又必须与 HTTP 层共用同一套夹紧规则 —— 各写一遍 (page-1)*size 就是第二份实现，
// 溢出边界会慢慢漂移。
func OffsetOf(page, size int) int { return Params{Page: page, PageSize: size}.Offset() }

// Limit 返回 SQL LIMIT。
func (p Params) Limit() int { return p.PageSize }

// New 构造分页响应。
func New(items any, total int64, p Params) Result {
	if items == nil {
		items = []any{}
	}
	return Result{Items: items, Total: total, Page: p.Page, PageSize: p.PageSize}
}

// Parse 解析并校验分页参数；非法参数返回 error（调用方应回 400）。
func Parse(c *gin.Context) (Params, error) {
	p := Params{Page: DefaultPage, PageSize: DefaultPageSize}

	if v := c.Query("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPage {
			return p, fmt.Errorf("%w: page 非法", ErrInvalid)
		}
		p.Page = n
	}
	if v := c.Query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPageSize {
			return p, fmt.Errorf("%w: page_size 非法", ErrInvalid)
		}
		p.PageSize = n
	}
	if v := c.Query("sort"); v != "" {
		field, desc, err := ParseSort(v)
		if err != nil {
			return p, fmt.Errorf("%w: %s", ErrInvalid, err.Error())
		}
		p.SortRaw = v
		p.Field = field
		p.Desc = desc
	}
	return p, nil
}

// ParseSort 解析 `field:asc|desc`。
//
// 导出是为了让非 HTTP 调用方（服务层测试、内部工具）用**同一份**实现：
// 各写一遍 split(":") 的话，"什么算合法排序参数"会出现第二个口径。
func ParseSort(v string) (field string, desc bool, err error) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return "", false, fmt.Errorf("sort 非法")
	}
	field, dir := parts[0], strings.ToLower(parts[1])
	if !sortFieldRe.MatchString(field) {
		return "", false, fmt.Errorf("sort 字段非法")
	}
	switch dir {
	case "asc":
		return field, false, nil
	case "desc":
		return field, true, nil
	default:
		return "", false, fmt.Errorf("sort 方向非法")
	}
}

// ErrInvalid 是分页参数非法（调用方应回 400）。用 errors.Is 判定，
// 不靠比较文案 —— 文案会改，判定不能跟着改。
var ErrInvalid = errors.New("分页参数非法")

// ParseOptional 与 Parse 同一套校验，唯一差别：page 缺省或 "0" 表示**全量**。
//
// 为什么要保留「0/缺省 = 全量」：管理台两处依赖它（模型下拉要全量、渠道计数要全量），
// 改成 400 会让配置页直接空掉。但「传了非数字」不能也当全量 —— 那正是
// page=abc 静默退化成全表扫描（最多 5000 行）的来源：慢查询、前端显示"第 1 页"
// 却拿到了全部数据，而两侧都不报错。
//
// all=true 时 Page 归 1（Offset()=0），PageSize 仍按 page_size 校验后的值返回，
// 供调用方拼响应体时保持字段齐全。
func ParseOptional(c *gin.Context) (p Params, all bool, err error) {
	p = Params{Page: DefaultPage, PageSize: DefaultPageSize}
	if v := strings.TrimSpace(c.Query("page")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 || n > MaxPage {
			return p, false, fmt.Errorf("%w: page 必须是 0..%d 的整数", ErrInvalid, MaxPage)
		}
		if n == 0 {
			all = true
		} else {
			p.Page = n
		}
	} else {
		all = true
	}
	if v := strings.TrimSpace(c.Query("page_size")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > MaxPageSize {
			return p, false, fmt.Errorf("%w: page_size 必须是 1..%d 的整数", ErrInvalid, MaxPageSize)
		}
		p.PageSize = n
	}
	if v := c.Query("sort"); v != "" {
		field, desc, serr := ParseSort(v)
		if serr != nil {
			return p, false, fmt.Errorf("%w: %s", ErrInvalid, serr.Error())
		}
		p.SortRaw = v
		p.Field = field
		p.Desc = desc
	}
	return p, all, nil
}
