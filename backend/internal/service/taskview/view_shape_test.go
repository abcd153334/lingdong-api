package taskview_test

import (
	"reflect"
	"strings"
	"testing"

	"lingdong-api/internal/pkg/taskfield"
	"lingdong-api/internal/service/taskview"
)

// TestViewMatchesFieldTable 是「运行期任务对象 ↔ 对外字段表」的守卫。
//
// 为什么必须有它：`pkg/taskfield` 的包注释一直写着「apidoc 侧有一条反射测试
// 比对 JSON 标签」，实际上**从来不存在**（写注释时按计划写了，测试没落地）。
// 于是两边可以各自加字段而没人发现：文档里写了一列永远不存在的字段，或者
// 结构体多了一个没人解释的键 —— 两种都不报错，只有接入方踩到。
//
// 这里比的是**完整路径集合**（含嵌套）：`error.message`、`contents[].url`、
// `usage.images`。只比顶层会漏掉嵌套字段的漂移，而嵌套字段恰恰是最容易改名的
// （`content_type` → `mime_type` 这类）。
func TestViewMatchesFieldTable(t *testing.T) {
	got := structPaths(reflect.TypeOf(taskview.View{}), "")
	want := make([]string, 0, len(taskfield.Fields()))
	for _, f := range taskfield.Fields() {
		want = append(want, f.Name)
	}
	if !sameSet(got, want) {
		t.Fatalf("taskview.View 与 taskfield.Fields() 不一致\n"+
			"字段表有、结构体没有: %v\n结构体有、字段表没有: %v",
			missingIn(want, got), missingIn(got, want))
	}
}

// TestViewJSONKeysAreStableFromFieldTable 保证「字段表里的每个叶子键都真的会出现」。
//
// 与上一条的分工：上一条比**声明**，这一条比**序列化结果**。带 omitempty 的字段
// 声明了却可能不出现（`mime_type` 就是这样丢过一次），而字段表不会写「有时缺」。
func TestViewJSONKeysAreStableFromFieldTable(t *testing.T) {
	// 构造一份「全字段有值」的视图：任何一个键被 omitempty 吃掉都会暴露。
	view := taskview.View{
		ID: "task_x", Object: "task", Status: "succeeded", Model: "m", Modality: "video",
		Kind: "video", ModelIconURL: "https://x/i.png", Progress: 100,
		CreatedAt: 1, Contents: []taskview.ViewArtifact{{Index: 0, URL: "u", ContentType: "video/mp4", Kind: "video"}},
		Error: &taskview.ViewError{Message: "x"},
	}
	keys := jsonLeafKeys(reflect.ValueOf(view), "")
	for _, f := range taskfield.Fields() {
		if strings.Contains(f.Name, "[].") {
			continue // 数组元素键在上面已经以 artifact 的形式覆盖
		}
		if !contains(keys, f.Name) {
			t.Fatalf("字段表声明了 %q，但序列化结果里没有（omitempty 吃掉了？）\n实际键: %v", f.Name, keys)
		}
	}
}

// structPaths 递归收集结构体的 JSON 路径。
//
// 嵌套表的唯一声明点是这里：新增嵌套对象时只需在 switch 里补一条，
// 两条门禁同时生效。写死「只展开 error/contents/usage」是有意的 ——
// 自动递归会把 time.Time 之类的实现细节也展开进来。
func structPaths(rt reflect.Type, prefix string) []string {
	if rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := jsonName(f)
		if name == "" {
			continue
		}
		full := prefix + name
		ft := f.Type
		// 切片在字段表里写成 `contents`（本身）+ `contents[].x`（元素）：
		// 这里把「数组本身」和「元素路径」分开记，元素前缀多一个 `[]`。
		childPrefix := full + "."
		if ft.Kind() == reflect.Slice {
			ft = ft.Elem()
			childPrefix = full + "[]."
		}
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		switch {
		case ft == reflect.TypeOf(taskview.ViewError{}):
			out = append(out, full)
			out = append(out, structPaths(ft, childPrefix)...)
		case ft == reflect.TypeOf(taskview.ViewArtifact{}):
			out = append(out, full)
			out = append(out, structPaths(ft, childPrefix)...)
		case ft == reflect.TypeOf(taskview.ViewUsage{}):
			out = append(out, full)
			out = append(out, structPaths(ft, childPrefix)...)
		default:
			out = append(out, full)
		}
	}
	return out
}

// jsonLeafKeys 收集「实际出现」的 JSON 路径（空值也算，只要键在）。
func jsonLeafKeys(v reflect.Value, prefix string) []string {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	out := make([]string, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		name := jsonName(v.Type().Field(i))
		if name == "" {
			continue
		}
		fv := v.Field(i)
		full := prefix + name
		if fv.Kind() == reflect.Slice && fv.Len() > 0 {
			out = append(out, full)
			out = append(out, jsonLeafKeys(fv.Index(0), full+"[].")...)
			continue
		}
		if fv.Kind() == reflect.Struct || (fv.Kind() == reflect.Ptr && fv.Elem().Kind() == reflect.Struct) {
			out = append(out, full)
			out = append(out, jsonLeafKeys(fv, full+".")...)
			continue
		}
		out = append(out, full)
	}
	return out
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

func missingIn(a, b []string) []string {
	out := make([]string, 0)
	for _, x := range a {
		if !contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
