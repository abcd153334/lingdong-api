package stats

import (
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm/schema"

	"lingdong-api/internal/model"
)

// TestArchiveColumnsMatchModel 是「归档列清单漂移」的回归锁。
//
// 为什么必须有这条测试：归档用 `DELETE ... RETURNING <cols>` + `INSERT <cols>`，
// 两边是同一份字符串常量。model.Usage 新增一列而常量没加时，
// **既不会报错也不会留下任何痕迹** —— 数据就这样在归档那一刻被悄悄丢掉，
// 等运营发现「半年前的记录里费用是 0」时已经无法追回。
//
// 反过来多写一列（模型里已删除）会在归档时直接 SQL 报错，所以这里做双向比对。
func TestArchiveColumnsMatchModel(t *testing.T) {
	got := columnSet(t, archiveColumns)
	want := modelColumns(t, &model.Usage{}, &model.UsageArchive{})

	for name := range want {
		if !got[name] {
			t.Errorf("model.Usage 有列 %q 但 archiveColumns 没有：归档会静默丢值", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("archiveColumns 有 %q 但 model.Usage 没有：归档会 SQL 报错", name)
		}
	}
}

// TestArchiveColumnsQuotedGroup 单独锁住保留字：`group` 不加引号时 PostgreSQL 会解析失败。
func TestArchiveColumnsQuotedGroup(t *testing.T) {
	if !strings.Contains(archiveColumns, `"group"`) {
		t.Fatal(`archiveColumns 里的 group 必须是 "group"（保留字）`)
	}
}

// columnSet 解析列清单字符串（去空白、去引号）为集合。
func columnSet(t *testing.T, raw string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		name = strings.Trim(name, `"`)
		if name == "" {
			continue
		}
		if out[name] {
			t.Fatalf("列清单重复：%q", name)
		}
		out[name] = true
	}
	return out
}

// modelColumns 用 GORM 的命名策略把结构体字段换成真实列名（与 AutoMigrate 完全同源）。
func modelColumns(t *testing.T, models ...any) map[string]bool {
	t.Helper()
	ns := schema.NamingStrategy{}
	out := map[string]bool{}
	for _, m := range models {
		typ := reflect.TypeOf(m).Elem()
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			// 内嵌结构体（UsageArchive 内嵌 Usage）展开递归。
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				inner := reflect.New(f.Type).Interface()
				for k := range modelColumns(t, inner) {
					out[k] = true
				}
				continue
			}
			name := ns.ColumnName("", f.Name)
			if name == "" {
				continue
			}
			out[name] = true
		}
	}
	return out
}
