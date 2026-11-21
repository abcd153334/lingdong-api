package appplugins

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// 这一组用例守的是"常见表单模式"的四件事：
//  1. 分段/两列是**声明驱动**的（不是前端猜的）；
//  2. 声明了 form 但漏掉字段，投影会把字段补到末段（不能凭空消失）；
//  3. 必填 + 默认值 ⇒ 服务端在缺键时补值，显式空串仍然报"必填"；
//  4. min/max 在写入侧生效，且不合法的 default 在导入期就被拒。

const miniForm = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
	{"key":"a","name":"A","unique":"no","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16},
		{"key":"title","label":"标题","type":"string","maxLen":40,"placeholder":"一句话概括"},
		{"key":"memo","label":"备注","type":"text","maxLen":200,"help":"写给内部看的处理说明"},
		{"key":"rank","label":"优先级","type":"int","min":1,"max":9}],
	 "form":{"sections":[
		{"title":"基本信息","columns":2,"fields":["no","title"]},
		{"title":"处理","columns":1,"fields":["memo"]}]},
	 "list":{"columns":["no"],"actions":["create","update"]}}]}`

// TestFormSectionsProjection 分段顺序/列数原样下发，漏掉的字段被追加到末段。
func TestFormSectionsProjection(t *testing.T) {
	m, ps := Decode([]byte(miniForm))
	if len(ps) > 0 {
		t.Fatalf("定义应合法: %v", ps)
	}
	if ps := m.Validate(""); len(ps) > 0 {
		t.Fatalf("定义应通过校验: %v", ps)
	}
	ev := m.Describe().Entities[0]
	if len(ev.Form.Sections) != 3 {
		t.Fatalf("应有三段（两段声明 + 一段追加），实际 %d: %+v", len(ev.Form.Sections), ev.Form.Sections)
	}
	if ev.Form.Sections[0].Title != "基本信息" || ev.Form.Sections[0].Columns != 2 {
		t.Fatalf("第一段应按声明原样下发: %+v", ev.Form.Sections[0])
	}
	// memo 在第二段；rank 没被任何段声明 ⇒ 必须出现在末段（"其他"）。
	if got := strings.Join(ev.Form.Sections[1].Fields, ","); got != "memo" {
		t.Fatalf("第二段字段应为 memo，实际 %q", got)
	}
	last := ev.Form.Sections[2]
	if last.Title != DefaultFormSectionTitle || strings.Join(last.Fields, ",") != "rank" {
		t.Fatalf("漏声明的字段应追加到「%s」段，实际 %+v", DefaultFormSectionTitle, last)
	}
	// 字段级提示也要投影出去：前端不解析 manifest，只能从这里拿。
	byKey := map[string]FieldView{}
	for _, f := range ev.Fields {
		byKey[f.Key] = f
	}
	if byKey["title"].Placeholder != "一句话概括" {
		t.Fatalf("placeholder 应下发，实际 %+v", byKey["title"])
	}
	if byKey["memo"].Help == "" {
		t.Fatalf("help 应下发，实际 %+v", byKey["memo"])
	}
	if byKey["rank"].Min == nil || *byKey["rank"].Min != 1 || byKey["rank"].Max == nil || *byKey["rank"].Max != 9 {
		t.Fatalf("min/max 应下发，实际 %+v", byKey["rank"])
	}
}

// TestFormSectionsInvalid 表单声明本身的错误必须一次报全，且路径能定位到段。
func TestFormSectionsInvalid(t *testing.T) {
	base := func(form string) string {
		return `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
			{"key":"a","name":"A","fields":[
				{"key":"no","label":"编号","type":"string","maxLen":16},
				{"key":"memo","label":"备注","type":"text","maxLen":50}],
			 "form":` + form + `,
			 "list":{"columns":["no"],"actions":["create"]}}]}`
	}
	cases := []struct {
		name string
		form string
		want string
	}{
		{"列数非法", `{"sections":[{"title":"A","columns":3,"fields":["no"]}]}`, ".form.sections[0].columns"},
		{"字段不存在", `{"sections":[{"title":"A","columns":1,"fields":["nope"]}]}`, ".form.sections[0].fields[0]"},
		{"跨段重复", `{"sections":[{"title":"A","columns":1,"fields":["no"]},{"title":"B","columns":1,"fields":["no"]}]}`, ".form.sections[1].fields[0]"},
		{"空分段", `{"sections":[{"title":"A","columns":1,"fields":[]}]}`, ".form.sections[0].fields"},
		{"没有分段", `{"sections":[]}`, ".form.sections"},
		{"标题过长", `{"sections":[{"title":"` + strings.Repeat("长", 33) + `","columns":1,"fields":["no"]}]}`, ".form.sections[0].title"},
	}
	for _, c := range cases {
		m, ps := Decode([]byte(base(c.form)))
		if len(ps) > 0 {
			t.Fatalf("%s：解码失败 %v", c.name, ps)
		}
		ps = m.Validate("")
		if len(ps) == 0 {
			t.Fatalf("%s：应报错，实际通过", c.name)
		}
		found := false
		for _, p := range ps {
			if strings.Contains(p.Path, c.want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s：应命中路径 %s，实际 %v", c.name, c.want, ps)
		}
	}
}

// TestFieldFormHintsInvalid help/placeholder 的长度与 min/max 的适用范围要拦下来：
// 这些值会被直接渲染进表单，写错了只有用户会看到。
func TestFieldFormHintsInvalid(t *testing.T) {
	cases := []struct{ name, field, want string }{
		{"说明过长", `{"key":"no","label":"编号","type":"string","maxLen":8,"help":"` + strings.Repeat("字", 201) + `"}`, ".help"},
		{"占位过长", `{"key":"no","label":"编号","type":"string","maxLen":8,"placeholder":"` + strings.Repeat("字", 33) + `"}`, ".placeholder"},
		{"非整数带范围", `{"key":"no","label":"编号","type":"string","maxLen":8,"min":1}`, ".min"},
		{"min 大于 max", `{"key":"n","label":"数","type":"int","min":9,"max":1}`, ".min"},
		{"默认值类型不符", `{"key":"n","label":"数","type":"int","default":"abc"}`, ".default"},
		{"默认值越界", `{"key":"n","label":"数","type":"int","min":1,"max":9,"default":99}`, ".default"},
	}
	for _, c := range cases {
		raw := `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
			{"key":"a","name":"A","fields":[` + c.field + `],
			 "list":{"columns":[],"actions":["create"]}}]}`
		m, ps := Decode([]byte(raw))
		if len(ps) > 0 {
			t.Fatalf("%s：解码失败 %v", c.name, ps)
		}
		ps = m.Validate("")
		found := false
		for _, p := range ps {
			if strings.Contains(p.Path, c.want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s：应命中路径 %s，实际 %v", c.name, c.want, ps)
		}
	}
}

// miniDefaults：必填 + 默认值（必填会被补），可选 + 默认值（不补）。
const miniDefaults = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
	{"key":"a","name":"A","fields":[
		{"key":"no","label":"编号","type":"string","required":true,"maxLen":16,"default":"AUTO-1"},
		{"key":"status","label":"状态","type":"enum","options":["open","done"],"required":true,"default":"open"},
		{"key":"memo","label":"备注","type":"text","maxLen":50,"default":"无"}],
	 "list":{"columns":["no"],"actions":["create"]}}]}`

// TestRequiredDefaultInjected 必填缺键 ⇒ 补默认值；显式值优先；显式空串仍报必填；
// 可选字段**不**补（否则"清空"永久不可达）。
func TestRequiredDefaultInjected(t *testing.T) {
	gdb := withPluginDB(t)
	importOK(t, gdb, miniDefaults, 0, false)
	ctx := context.Background()
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}

	// ① 只给必填之一也够：另一个必填吃默认值。
	item, err := Create(ctx, gdb, 7, "mini", "a", []byte(`{"no":"X-1"}`))
	if err != nil {
		t.Fatalf("应新建成功: %v", err)
	}
	if item.Data["status"] != "open" {
		t.Fatalf("必填的 status 应补默认值 open，实际 %#v", item.Data)
	}
	if _, ok := item.Data["memo"]; ok {
		t.Fatalf("可选字段不应被补默认值（否则删不掉），实际 %#v", item.Data)
	}

	// ② 显式值优先于默认值。
	item2, err := Create(ctx, gdb, 7, "mini", "a", []byte(`{"no":"X-2","status":"done"}`))
	if err != nil {
		t.Fatalf("应新建成功: %v", err)
	}
	if item2.Data["status"] != "done" {
		t.Fatalf("显式值应覆盖默认值，实际 %#v", item2.Data)
	}

	// ③ 显式空串不算"缺失"：那是"我要清空它"，必填的应当报错而不是悄悄换成默认值。
	if _, err := Create(ctx, gdb, 7, "mini", "a", []byte(`{"no":"X-3","status":""}`)); err == nil {
		t.Fatal("必填字段显式空串应被拒")
	} else if !strings.Contains(err.Error(), "必填") {
		t.Fatalf("错误应说明必填，实际 %v", err)
	}
}

// TestIntRangeEnforcedOnWrite min/max 必须在写入侧拦：只在界面上标范围＝靠自觉。
func TestIntRangeEnforcedOnWrite(t *testing.T) {
	gdb := withPluginDB(t)
	const raw = `{"apiVersion":1,"key":"mini","name":"迷你","version":"1.0.0","entities":[
		{"key":"a","name":"A","fields":[
			{"key":"n","label":"数","type":"int","required":true,"min":1,"max":9}],
		 "list":{"columns":["n"],"actions":["create"]}}]}`
	importOK(t, gdb, raw, 0, false)
	ctx := context.Background()
	if _, err := Install(ctx, gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}

	ok := []string{`{"n":1}`, `{"n":9}`}
	for _, body := range ok {
		if _, err := Create(ctx, gdb, 7, "mini", "a", []byte(body)); err != nil {
			t.Fatalf("%s 应被接受: %v", body, err)
		}
	}
	bad := []string{`{"n":0}`, `{"n":10}`, `{"n":-1}`}
	for _, body := range bad {
		if _, err := Create(ctx, gdb, 7, "mini", "a", []byte(body)); err == nil {
			t.Fatalf("%s 应被拒绝（超出 1-9）", body)
		}
	}
}

// 确认 form 分段不会影响记录读写：两列布局只是呈现，不改变存储形状。
func TestFormDoesNotChangeStorage(t *testing.T) {
	gdb := withPluginDB(t)
	importOK(t, gdb, miniForm, 0, false)
	if _, err := Install(context.Background(), gdb, 7, "mini", map[string]any{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if _, err := Create(context.Background(), gdb, 7, "mini", "a",
		[]byte(`{"no":"F-1","title":"T","memo":"M","rank":3}`)); err != nil {
		t.Fatalf("应新建成功: %v", err)
	}
	items, total, err := listRecords(gdb, 7, "mini", "a", "", 0, 10)
	if err != nil || total != 1 {
		t.Fatalf("应查到 1 条，实际 total=%d err=%v", total, err)
	}
	// 读回来的 int 可能是 float64 也可能是 json.Number（取决于解码路径），
	// 这里只断言"数值是 3"，不依赖具体 Go 类型。
	if got := fmt.Sprint(items[0].Data["rank"]); got != "3" {
		t.Fatalf("int 字段应原样读回，实际 %q", got)
	}
}
