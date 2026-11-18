package apidoc

import (
	"testing"
)

// TestDocTextPassesGuard 把「文案守卫」从发布期搬到 `go test`。
//
// 为什么必须有这条：守卫（`CheckText`）原来只在发布闸门上跑，而播种路径的表现是
// **页面悄悄变成草稿**——不报错、接口 200、测试全绿，只有去查 `doc_endpoints.status`
// 才发现。本轮真实踩到：给 gpt-image 的 size 加了一句「上游接受比例」，
// 六个图片页里两个直接掉草稿，而当时所有测试都是绿的。
//
// 只查视角/泄漏两类（forbidden_word / forwarder_name）：
// external_host 依赖运行期站点域名（sitecfg），在单测里拿不到全量白名单，
// 查它会把"引用平台自己的地址"误判成第三方域名。
func TestDocTextPassesGuard(t *testing.T) {
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			for _, it := range docTexts(e.Spec) {
				for _, v := range CheckText(it.text) {
					if v.Kind == "external_host" {
						continue
					}
					t.Errorf("族 %q 的 %s 命中%s用词 %q：%s",
						f.Key, it.path, v.Kind, v.Text, v.Hint)
				}
			}
		}
		for _, p := range presetsOfFamily(f.Key) {
			for _, s := range []string{p.Label, p.Summary, p.Note} {
				for _, v := range CheckText(s) {
					if v.Kind == "external_host" {
						continue
					}
					t.Errorf("族 %q 的预设「%s」命中%s用词 %q", f.Key, p.Label, v.Kind, v.Text)
				}
			}
		}
	}
}

type docText struct{ path, text string }

// docTexts 收集一篇接口文档里**读者会读到的**全部文字（与 v7_guard 的取法同构）。
func docTexts(d EndpointDoc) []docText {
	out := []docText{
		{"name", d.Name},
		{"summary", d.Summary},
		{"run_hint", d.RunHint},
	}
	for _, n := range d.Notes {
		out = append(out, docText{"notes", n})
	}
	for _, f := range d.Fields {
		out = append(out, docText{"fields." + f.Name + ".desc", f.Desc})
		out = append(out, docText{"fields." + f.Name + ".default", f.Default})
		for _, c := range f.Constraints {
			out = append(out, docText{"fields." + f.Name + ".constraint", c.Text})
		}
	}
	for _, r := range d.Returns {
		out = append(out, docText{"returns." + r.Name, r.Desc})
	}
	for _, v := range d.Response.Variants {
		out = append(out, docText{"variants.label", v.Label})
		for _, n := range v.Notes {
			out = append(out, docText{"variants.notes", n})
		}
	}
	return out
}

func presetsOfFamily(key string) []Preset {
	var out []Preset
	f, ok := FamilyOfKey(key)
	if !ok {
		return nil
	}
	for _, e := range FamilyEndpointsOf(f) {
		out = append(out, e.Presets...)
	}
	return out
}
