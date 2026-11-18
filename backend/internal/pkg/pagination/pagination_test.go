package pagination

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func ctxWithQuery(raw string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/?"+raw, nil)
	return c
}

func TestParseDefaults(t *testing.T) {
	p, err := Parse(ctxWithQuery(""))
	if err != nil {
		t.Fatalf("默认参数不应报错: %v", err)
	}
	if p.Page != DefaultPage || p.PageSize != DefaultPageSize {
		t.Fatalf("默认应为 page=%d page_size=%d, got %d/%d", DefaultPage, DefaultPageSize, p.Page, p.PageSize)
	}
}

func TestParseValid(t *testing.T) {
	p, err := Parse(ctxWithQuery("page=3&page_size=50&sort=created_at:desc"))
	if err != nil {
		t.Fatalf("合法参数报错: %v", err)
	}
	if p.Page != 3 || p.PageSize != 50 || p.Field != "created_at" || !p.Desc {
		t.Fatalf("解析结果不符: %+v", p)
	}
	if p.Offset() != 100 || p.Limit() != 50 {
		t.Fatalf("Offset/Limit 不符: %d/%d", p.Offset(), p.Limit())
	}
}

func TestParseSortAsc(t *testing.T) {
	p, err := Parse(ctxWithQuery("sort=id:asc"))
	if err != nil {
		t.Fatalf("报错: %v", err)
	}
	if p.Field != "id" || p.Desc {
		t.Fatalf("解析结果不符: %+v", p)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []string{
		"page=0",
		"page=-1",
		"page=abc",
		"page_size=0",
		"page_size=101", // 超上限 100
		"page_size=abc",
		"sort=created_at",    // 缺方向
		"sort=created_at:up", // 非法方向
		"sort=1bad:asc",      // 非法字段
	}
	for _, q := range cases {
		if _, err := Parse(ctxWithQuery(q)); err == nil {
			t.Errorf("非法参数 %q 应报错", q)
		}
	}
}

func TestNew(t *testing.T) {
	p := Params{Page: 1, PageSize: 20}
	r := New(nil, 0, p)
	if r.Items == nil {
		t.Fatal("items 不应为 nil（应为空数组）")
	}
	if r.Total != 0 || r.Page != 1 || r.PageSize != 20 {
		t.Fatalf("分页响应不符: %+v", r)
	}
}

// 页码上界：没有它时 (page-1)*page_size 会溢出成负数。
//
// 两种后果都很糟：GORM 把负 OFFSET 夹成 0（静默返回第一页，但响应体回显
// 天文数字的 page），管理台那几处直接绑 `LIMIT ? OFFSET ?` 的原始 SQL 则
// 让 PG 报 2201X，最终折成 500 —— 一个查询参数就能把列表打成"服务端故障"。
func TestPageUpperBound(t *testing.T) {
	for _, raw := range []string{
		"page=1000000000",
		"page=92233720368547760", // 恰好会让 int64 乘法回绕
		"page=99999999999999999999",
	} {
		if _, err := Parse(ctxWithQuery(raw)); err == nil {
			t.Fatalf("%s 应被拒绝", raw)
		}
	}
	if _, err := Parse(ctxWithQuery("page=100000")); err != nil {
		t.Fatalf("上界本身应通过: %v", err)
	}
}

// Offset 永远是 [0, MaxPage*MaxPageSize]，不允许把负数交给 SQL。
//
// 这条守的是"绕过 Parse 直接构造 Params"的调用方（测试、内部工具、
// 将来新增的入口）：它们不该能把负数或回绕值塞进 OFFSET。
func TestOffsetNeverNegative(t *testing.T) {
	cases := []Params{
		{Page: 0, PageSize: 20},
		{Page: -1, PageSize: 20},
		{Page: 1, PageSize: 0},
		{Page: 1 << 60, PageSize: 100}, // 回绕成负
		{Page: 1 << 40, PageSize: 100}, // 未回绕但远超上界
	}
	for _, p := range cases {
		if got := p.Offset(); got < 0 {
			t.Fatalf("page=%d size=%d 得到负 OFFSET %d", p.Page, p.PageSize, got)
		} else if got > MaxPage*MaxPageSize {
			t.Fatalf("page=%d size=%d 得到越界 OFFSET %d", p.Page, p.PageSize, got)
		}
	}
	// 正常路径仍然是精确值
	if got := (Params{Page: 3, PageSize: 20}).Offset(); got != 40 {
		t.Fatalf("正常分页偏移错误: %d", got)
	}
}

// TestParseOptional 覆盖「全量」与「严格」两条口径的交界。
//
// 这是本轮真正的事故点：改造前 model/agent 两处用 `Atoi` 忽略错误，
// 于是 `page=abc` 与「没传 page」都得到 0 → 静默走全量分支（最多 5000 行）。
// 慢查询、前端显示"第 1 页"、两侧都不报错。
func TestParseOptional(t *testing.T) {
	cases := []struct {
		q        string
		wantAll  bool
		wantErr  bool
		wantPage int
	}{
		{"", true, false, DefaultPage},      // 缺省 = 全量
		{"page=0", true, false, DefaultPage}, // 显式 0 = 全量（管理台模型下拉/渠道计数依赖）
		{"page=2", false, false, 2},
		{"page=2&page_size=50", false, false, 2},
		{"page=abc", false, true, 0},  // 笔误必须 400，不能当全量
		{"page=-1", false, true, 0},
		{"page=1x", false, true, 0},
		{"page=100001", false, true, 0},
		{"page_size=0", false, true, 0},
		{"page_size=101", false, true, 0},
		{"page_size=abc", false, true, 0},
		{"sort=created_at", false, true, 0},
	}
	for _, c := range cases {
		p, all, err := ParseOptional(ctxWithQuery(c.q))
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报错", c.q)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不应报错: %v", c.q, err)
			continue
		}
		if all != c.wantAll {
			t.Errorf("%q all=%v，期望 %v", c.q, all, c.wantAll)
		}
		if p.Page != c.wantPage {
			t.Errorf("%q page=%d，期望 %d", c.q, p.Page, c.wantPage)
		}
	}
	// 全量分支的 Offset 必须是 0：调用方据此不加 OFFSET 子句。
	p, all, _ := ParseOptional(ctxWithQuery(""))
	if !all || p.Offset() != 0 {
		t.Fatalf("全量分支 offset 应为 0，实际 all=%v offset=%d", all, p.Offset())
	}
}

// TestParseErrorsAreSentinel 保证调用方能靠 errors.Is 判定「参数错」而不是比文案。
func TestParseErrorsAreSentinel(t *testing.T) {
	for _, q := range []string{"page=abc", "page_size=999", "sort=x:up"} {
		if _, err := Parse(ctxWithQuery(q)); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) 的错误应包裹 ErrInvalid，实际 %v", q, err)
		}
		if _, _, err := ParseOptional(ctxWithQuery(q)); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseOptional(%q) 的错误应包裹 ErrInvalid，实际 %v", q, err)
		}
	}
}

// TestOffsetOfMatchesOffset 保证服务层用的自由函数与 HTTP 层同一套夹紧。
func TestOffsetOfMatchesOffset(t *testing.T) {
	for _, c := range []struct{ page, size int }{{3, 20}, {0, 20}, {-1, 20}, {1, 0}, {1 << 60, 100}} {
		want := Params{Page: c.page, PageSize: c.size}.Offset()
		if got := OffsetOf(c.page, c.size); got != want {
			t.Fatalf("OffsetOf(%d,%d)=%d，期望 %d", c.page, c.size, got, want)
		}
	}
}
