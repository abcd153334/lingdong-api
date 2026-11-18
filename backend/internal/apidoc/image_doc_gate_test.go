package apidoc

import (
	"strings"
	"testing"
)

// 图片三族的文档门禁。
//
// 为什么图片侧必须单独有一组：视频侧早就有「字段预算 / 骨架字段 / 示例模型是真实 slug /
// 兼容说明与字段一致」这些门禁，图片侧此前**一条都没有**。结果是三个图片页可以
// 自由地多一个字段、少一句说明、示例里写别的族的模型，而全部测试仍然绿 ——
// 发现的渠道只有"有人照着文档调一次"。

// imageFamilies 是图片侧三族（顺序即页面顺序）。
var imageFamilies = []string{FamilyImagesGptImage, FamilyImagesNanoBanana, FamilyImagesSeedream}

// 每族提交页的**请求字段预算**（改这个数字必须同时改文档与这条断言）。
//
// 数字是"读者能在页面上数出来的行数"，包括平台本地字段（aspect_ratio / async /
// response_format）。它的作用是拦住"顺手多塞一个字段"：字段表是对外承诺，
// 多一行就意味着多一个别人会去传的参数。
var imageSubmitFieldBudget = map[string]int{
	FamilyImagesGptImage:   11,
	FamilyImagesNanoBanana: 8,
	FamilyImagesSeedream:   18,
}

// imageDocRef 是一篇图片文档 + 它所属的族（FamilyEndpoint 本身不带族名）。
type imageDocRef struct {
	Family string
	Doc    DocumentKey
	Spec   EndpointDoc
	Resp   ResponseSpec
	Preset []Preset
}

// imageSubmitDocs 取三族的提交页（不含查询任务状态页）。
func imageSubmitDocs() []imageDocRef {
	var out []imageDocRef
	for _, key := range imageFamilies {
		f, ok := FamilyOfKey(key)
		if !ok {
			continue
		}
		for _, e := range FamilyEndpointsOf(f) {
			if e.Doc.Kind == KindImagesGenerations {
				out = append(out, imageDocRef{Family: f.Key, Doc: e.Doc, Spec: e.Spec, Resp: e.Spec.Response, Preset: e.Presets})
			}
		}
	}
	return out
}

// pageFields 是"读者在字段表里能数到的行"。
//
// 不按 In 过滤：multipart 端点的字段是 InForm，JSON 端点是 InBody，
// 但对读者来说都是同一张表。只数 InBody 会让编辑/变体两页看起来"没有字段"，
// 于是那条 b64_json 门禁永远发现不了它们缺 response_format。
func pageFields(d EndpointDoc) []Field { return d.Fields }

func hasFieldNamed(d EndpointDoc, name string) bool {
	for _, f := range pageFields(d) {
		if f.Name == name {
			return true
		}
	}
	return false
}

// TestImageSubmitFieldBudget 守住字段预算：多一个少一个都红。
func TestImageSubmitFieldBudget(t *testing.T) {
	docs := imageSubmitDocs()
	if len(docs) != len(imageFamilies) {
		t.Fatalf("应有三族提交页，实际 %d", len(docs))
	}
	for _, e := range docs {
		want, ok := imageSubmitFieldBudget[e.Family]
		if !ok {
			t.Fatalf("族 %q 没有登记字段预算", e.Family)
		}
		t.Logf("族 %s 实际请求字段数 = %d（预算 %d）", e.Family, len(pageFields(e.Spec)), want)
		if got := len(pageFields(e.Spec)); got != want {
			names := make([]string, 0, got)
			for _, f := range pageFields(e.Spec) {
				names = append(names, f.Name)
			}
			t.Errorf("族 %q 请求字段数 %d，预算 %d：%s（改字段必须同时改预算与文档）",
				e.Family, got, want, strings.Join(names, ", "))
		}
	}
}

// TestImageSubmitHasSkeleton 守住骨架：每篇提交页都要有 model 与 prompt。
//
// 这两行是"这篇文档能不能用"的最低条件；漏掉 prompt 的页面曾经真实存在过
// （族页从模板派生，模板改字段时被连带删掉）。
func TestImageSubmitHasSkeleton(t *testing.T) {
	for _, e := range imageSubmitDocs() {
		for _, name := range []string{"model", "prompt"} {
			if !hasFieldNamed(e.Spec, name) {
				t.Errorf("族 %q 的提交页缺骨架字段 %q", e.Family, name)
			}
		}
	}
}

// TestImageAsyncFieldMatchesFamilyDeclaration 是**双向**门禁：
// 声明 AsyncSubmit 的族必须有 async 字段，没声明的必须没有。
//
// 单向（只查"有声明就得有字段"）抓不住 Seedream 那种情况 —— 它的页面曾经
// 有一个谁都不接的 async 字段，而当时的断言正好也是"必须有 async"。
func TestImageAsyncFieldMatchesFamilyDeclaration(t *testing.T) {
	for _, e := range imageSubmitDocs() {
		f, ok := FamilyOfKey(e.Family)
		if !ok {
			t.Fatalf("族 %q 未登记", e.Family)
		}
		has := hasFieldNamed(e.Spec, "async")
		if f.AsyncSubmit && !has {
			t.Errorf("族 %q 声明支持 async，但提交页没有 async 字段", e.Family)
		}
		if !f.AsyncSubmit && has {
			t.Errorf("族 %q 的协议不支持 async，提交页却写了 async 字段（读者会照它发）", e.Family)
		}
	}
}

// TestImageExamplesUseOwnFamilyModel 守住「示例里的 model 必须是本族模型」。
//
// 判据用族的 Match 前缀（gpt-image / nano-banana / seedream），不查库：
// 文档内容必须在"模型是否在售"之外成立（下架不该让文档变红），
// 但"图片页的示例里出现视频模型"是结构错误，必须拦住。
func TestImageExamplesUseOwnFamilyModel(t *testing.T) {
	for _, e := range imageSubmitDocs() {
		f, _ := FamilyOfKey(e.Family)
		check := func(where string, o Obj) {
			v, ok := o.Get("model")
			if !ok {
				return
			}
			s, _ := v.(string)
			if s == "" || strings.Contains(s, "$") {
				return // 预设里的 $model 占位合法
			}
			matched := false
			for _, m := range f.Match {
				if strings.Contains(s, m) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("族 %q 的 %s 示例用了不属于本族的模型 %q（本族匹配 %v）",
					e.Family, where, s, f.Match)
			}
		}
		check("Example", e.Spec.Example)
		check("Example2", e.Spec.Example2)
	}
}

// TestImageAcceptedExampleUsesOwnFamilyModel 守住「已受理」变体的示例模型。
//
// 这一条是单独的门禁：`TaskExampleFlat` 曾经把 model 写死成 `kling-v2`，
// 于是三个图片页的「已受理」示例里都显示一个视频模型。读者第一眼就会怀疑整页。
func TestImageAcceptedExampleUsesOwnFamilyModel(t *testing.T) {
	for _, e := range imageSubmitDocs() {
		f, _ := FamilyOfKey(e.Family)
		for _, v := range e.Resp.Variants {
			raw, ok := v.Example.Get("model")
			if !ok {
				continue
			}
			s, _ := raw.(string)
			matched := false
			for _, m := range f.Match {
				if strings.Contains(s, m) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("族 %q 的响应变体「%s」示例模型是 %q，不属于本族 %v",
					e.Family, v.Label, s, f.Match)
			}
		}
	}
}

// TestGPTImageSizeNoteMatchesFacts 守住「文档那张换算表与代码同一份数据」。
//
// 判据是逐行包含：GPTImageTierPixels 里每一行（画幅 + 三个像素）都必须出现在
// size 字段的说明里。手抄时代这条一定会红 —— 当时页面挂着 gpt-image-2 的
// 另一套像素（16:9 → 1672x941），而适配器从来不发它。
func TestGPTImageSizeNoteMatchesFacts(t *testing.T) {
	fam, ok := FamilyOfKey(FamilyImagesGptImage)
	if !ok {
		t.Fatal("GPT Image 族未登记")
	}
	var e FamilyEndpoint
	found := false
	for _, cand := range FamilyEndpointsOf(fam) {
		if cand.Doc.Kind == KindImagesGenerations {
			e, found = cand, true
			break
		}
	}
	if !found {
		t.Fatal("GPT Image 的生成页不存在")
	}
	var desc string
	for _, f := range e.Spec.Fields {
		if f.Name == "size" {
			desc = f.Desc
		}
	}
	if desc == "" {
		t.Fatal("生成页没有 size 字段说明")
	}
	for ratio, cols := range GPTImageTierPixels {
		if !strings.Contains(desc, ratio+"　") && !strings.Contains(desc, ratio+" ") {
			t.Errorf("size 说明里没有画幅 %s 这一行", ratio)
			continue
		}
		for tier, px := range cols {
			if !strings.Contains(desc, px) {
				t.Errorf("size 说明里缺 %s 的 %s 像素 %s", ratio, tier, px)
			}
		}
	}
}

// TestPromptLimitMatchesSharedConstant 守住「页面上写的上限 = 适配器用的上限」。
//
// 两处曾经分叉：适配器按 OpenAI 契约放行 32000，页面写 5000。
// 代码比文档宽松同样是文档不准 —— 用户按文档算好的长描述被上游拒绝时，
// 平台给的是上游原话，看不出真正原因。
func TestPromptLimitMatchesSharedConstant(t *testing.T) {
	want := map[string]float64{
		FamilyImagesGptImage:   float64(GPTImagePromptMaxRunes),
		FamilyImagesNanoBanana: float64(NanoPromptMaxRunes),
	}
	for _, e := range imageSubmitDocs() {
		w, ok := want[e.Family]
		if !ok {
			continue
		}
		found := false
		for _, f := range e.Spec.Fields {
			if f.Name != "prompt" {
				continue
			}
			for _, c := range f.Constraints {
				if c.Kind == "max_len" {
					found = true
					if c.Value != w {
						t.Errorf("族 %q 的 prompt 上限写 %v，代码是 %v", e.Family, c.Value, w)
					}
				}
			}
		}
		if !found {
			t.Errorf("族 %q 的 prompt 没有声明 max_len（读者不知道上限）", e.Family)
		}
	}
}

// TestImageReturnsMentionOnlyDeclaredFields 守住「返回里提到的字段必须在请求字段里存在」。
//
// 判据只针对平台本地开关：`data[].b64_json` 只有在页面声明了 `response_format`
// 时才该出现。曾经 edits 两页的返回里写着 b64_json，而字段表里没有
// response_format —— 读者会去找一个不存在的开关。
func TestImageReturnsMentionOnlyDeclaredFields(t *testing.T) {
	for _, key := range imageFamilies {
		f, _ := FamilyOfKey(key)
		for _, e := range FamilyEndpointsOf(f) {
			hasB64 := false
			for _, r := range e.Spec.Returns {
				if strings.Contains(r.Name, "b64_json") {
					hasB64 = true
				}
			}
			if !hasB64 {
				continue
			}
			if !hasFieldNamed(e.Spec, "response_format") {
				t.Errorf("族 %q 的「%s」返回里有 b64_json，但请求字段里没有 response_format",
					key, e.Spec.Name)
			}
		}
	}
}
