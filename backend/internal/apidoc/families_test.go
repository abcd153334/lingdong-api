package apidoc

import (
	"regexp"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// 本文件守「模型族 → 接口文档」这条链的不变量。族是文档站的骨架：
// 它错了不会崩，只会「少一块」「多一篇」「字段串味」这类安静的错误。

// ① key / 板块 / 匹配词必须自洽。
func TestFamilyDocsShape(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range AllFamilyDocs() {
		if f.Key == "" || f.Title == "" || f.Summary == "" {
			t.Fatalf("族 %q 缺 key / title / summary", f.Key)
		}
		if seen[f.Key] {
			t.Fatalf("族 key %q 重复", f.Key)
		}
		seen[f.Key] = true
		if !IsAISection(f.Section) {
			t.Fatalf("族 %q 的板块 %q 不是 AI 能力板块", f.Key, f.Section)
		}
		if len(f.Match) == 0 {
			t.Fatalf("族 %q 没有 Match：它永远不会命中任何模型", f.Key)
		}
	}
}

// ② 声明顺序必须与侧栏顺序一致（否则「音频」会跑到「视频」后面）。
func TestFamilyDocsFollowSectionOrder(t *testing.T) {
	prev := -1
	for _, f := range FamilyDocs() {
		if prev > sectionRank(f.Section) {
			t.Fatalf("族 %q 的板块顺序与 sectionOrderList 不一致", f.Key)
		}
		prev = sectionRank(f.Section)
	}
}

// ③ Match 词互不为前缀/子串，且合成 ID 只命中一族。
func TestFamilyMatchDisjoint(t *testing.T) {
	tokens := map[string]string{}
	for _, f := range AllFamilyDocs() {
		for _, m := range f.Match {
			if other, ok := tokens[m]; ok {
				t.Fatalf("Match 词 %q 同时属于 %q 与 %q", m, other, f.Key)
			}
			tokens[m] = f.Key
			got, ok := FamilyDocOfModelID(m + "-probe")
			if !ok || got.Key != f.Key {
				t.Fatalf("族 %q 的 Match 词 %q 未命中自己（命中 %q / %v）", f.Key, m, got.Key, ok)
			}
		}
	}
	for m, owner := range tokens {
		for other := range tokens {
			if m == other {
				continue
			}
			if len(m) < len(other) && strings.Contains(other, m) {
				t.Fatalf("Match 词 %q（%s）是 %q（%s）的子串：族归属会随声明顺序漂移",
					m, owner, other, tokens[other])
			}
		}
	}
}

// ④ 每个**已上架**的族都必须有内容完整的文档，且文档里的端点属于本族板块。
//
// 这是「声明了却没写文档」的唯一守卫：FamilyDoc 不再携带接口清单，
// 页面清单完全来自 familyEndpointTable —— 漏写一族的表现是"这个族的页面空着"，
// 没有任何报错，所以必须由测试兜住。
func TestEveryActiveFamilyHasDocs(t *testing.T) {
	for _, f := range FamilyDocs() {
		eps := FamilyEndpointsOf(f)
		if len(eps) == 0 {
			t.Fatalf("族 %q 已上架但没有接口文档（在 familyEndpointTable 里补，或把它标记为 Deferred）", f.Key)
		}
		inFamily := map[DocumentKey]bool{}
		for _, e := range eps {
			if e.Doc.Kind == "" {
				t.Fatalf("族 %q 有一篇文档缺 Kind", f.Key)
			}
			if inFamily[e.Doc] {
				t.Fatalf("族 %q 里文档 %s 重复", f.Key, e.Doc)
			}
			inFamily[e.Doc] = true
			// 网关自有的读面（`EndpointKey` 为空：查询 / 取消 / 元信息）挂在族节点下是**故意的**：
			// 读者提交完异步任务后要就地能查。它们本属「通用接口」板块，所以这里不比板块。
			if tr, ok := TransportOf(e.Doc.Kind); !ok || tr.EndpointKey != "" {
				if got := SectionOfKind(e.Doc.Kind); got != f.Section {
					t.Fatalf("族 %q 收了 %q 的文档，但它属于板块 %q（跨板块串页）", f.Key, e.Doc, got)
				}
			}
			if strings.TrimSpace(e.Spec.Name) == "" || strings.TrimSpace(e.Spec.Summary) == "" {
				t.Fatalf("族 %q 的 %s 缺名称或摘要", f.Key, e.Doc)
			}
			if len(e.Spec.Fields) == 0 && len(e.Spec.Returns) == 0 {
				t.Fatalf("族 %q 的 %s 既没有请求字段也没有返回字段", f.Key, e.Doc)
			}
		}
	}
}

// ⑤ 两族对**同一个端点**的文档不得是同一份（T1：同文即错误）。
//
// 这正是 2026-11-13 那次事故的核心：三个族的字段表逐字相同，差别只是被删掉了几行。
// 判据只比字段名集合 —— 文案不同但字段相同，仍然是同一份协议，那就该合并成一族，
// 而不是留两页让人挑。
func TestFamilyDocsAreNotClones(t *testing.T) {
	type key struct {
		kind  Kind
		names string
	}
	byKind := map[Kind]map[string]string{} // kind → 字段名指纹 → 族
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			// 网关自有读面不在其列：任务对象只有一份定义，逐字相同是事实。
			if tr, ok := TransportOf(e.Doc.Kind); ok && tr.EndpointKey == "" {
				continue
			}
			names := make([]string, 0, len(e.Spec.Fields))
			for _, fl := range e.Spec.Fields {
				names = append(names, string(fl.In)+"|"+fl.Name)
			}
			sort.Strings(names)
			fp := strings.Join(names, ",")
			if byKind[e.Doc.Kind] == nil {
				byKind[e.Doc.Kind] = map[string]string{}
			}
			if other, ok := byKind[e.Doc.Kind][fp]; ok {
				t.Fatalf("族 %q 与 %q 对 %q 的请求字段完全相同（%d 个）：这不是两篇文档，是同一篇印了两遍",
					f.Key, other, e.Doc.Kind, len(names))
			}
			byKind[e.Doc.Kind][fp] = f.Key
		}
	}
}

// ⑥ 示例里的 model 必须是本族模型（T3）。
func TestFamilyExampleModelBelongsToFamily(t *testing.T) {
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			for _, ex := range []struct {
				name string
				obj  Obj
			}{{"Example", e.Spec.Example}, {"Example2", e.Spec.Example2}} {
				v, ok := ex.obj.Get("model")
				if !ok {
					continue
				}
				id, _ := v.(string)
				if id == "" || id == PresetModelToken {
					continue
				}
				got, hit := FamilyDocOfModelID(id)
				if !hit || got.Key != f.Key {
					t.Fatalf("族 %q 的 %s 示例 model 是 %q（属于 %q）：读者照着抄会调错模型",
						f.Key, ex.name, id, got.Key)
				}
			}
		}
	}
}

// ⑦ 一族的文案里不得出现别家的名字（T4：跨族串味）。
func TestFamilyCopyDoesNotMentionOtherFamilies(t *testing.T) {
	all := AllFamilyDocs()
	for _, f := range FamilyDocs() {
		var sb strings.Builder
		for _, e := range FamilyEndpointsOf(f) {
			sb.WriteString(e.Spec.Summary)
			for _, n := range e.Spec.Notes {
				sb.WriteString(n)
			}
			for _, fl := range e.Spec.Fields {
				sb.WriteString(fl.Desc)
			}
		}
		text := strings.ToLower(sb.String())
		for _, other := range all {
			if other.Key == f.Key || other.Deferred {
				continue
			}
			if strings.Contains(text, strings.ToLower(other.Title)) {
				t.Fatalf("族 %q 的文案里出现了别家 %q（%q）：读者会以为这一页讲的是那一家",
					f.Key, other.Title, other.Key)
			}
		}
	}
}

// ⑧ 方言篇必须被某族拥有，且被拥有的方言必须在 TransportSet 里登记（T7）。
func TestVariantOwnership(t *testing.T) {
	registered := map[DocumentKey]bool{}
	for _, d := range variantDocTable {
		registered[d.Doc] = true
	}
	owned := map[DocumentKey]string{}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			if e.Doc.Variant == "" {
				continue
			}
			if !registered[e.Doc] {
				t.Fatalf("族 %q 收了方言 %s，但它没在 variantDocTable 登记（发布闸门会拒绝它）", f.Key, e.Doc)
			}
			if prev, ok := owned[e.Doc]; ok {
				t.Fatalf("方言 %s 同时被 %q 与 %q 拥有", e.Doc, prev, f.Key)
			}
			owned[e.Doc] = f.Key
		}
	}
	for d := range registered {
		if _, ok := owned[d]; !ok {
			t.Fatalf("方言 %s 登记了但没有任何族写它那篇（孤儿文档）", d)
		}
	}
}

// ⑨ 播种自洽：节点内不重复、每页都指向真实登记过的端点、每条路由都有页。
//
// 判据是**篇**（族 + 端点 + 方言），不是端点：三族的「生成图片」打同一个网关路径，
// 但那是三篇文档。拿端点做判据时它们会塌成一条 —— 少一族也数不出来。
func TestEveryContractDocIsSeeded(t *testing.T) {
	seeded := map[PageKey]bool{}
	for _, n := range SeedNodes() {
		inNode := map[PageKey]bool{}
		for _, ep := range n.Endpoints {
			if ep.Page.Kind == "" {
				t.Fatalf("节点 %q 的 %q 没有页面身份", n.Key, ep.Key)
			}
			if ep.DocID != ep.Page.Transport().String() {
				t.Fatalf("节点 %q 的 %q：页面身份指向 %s，绑定写的却是 %q（两套身份不能各写一遍）",
					n.Key, ep.Key, ep.Page.Transport(), ep.DocID)
			}
			if inNode[ep.Page] {
				t.Fatalf("节点 %q 里页面 %s 重复", n.Key, ep.Page)
			}
			inNode[ep.Page] = true
			seeded[ep.Page] = true
		}
	}

	// 方向一：页面的端点必须是登记过的路由 —— 否则发布闸门会拒绝它。
	registered := map[DocumentKey]bool{}
	for _, d := range TransportSet() {
		registered[d] = true
	}
	for pk := range seeded {
		if !registered[pk.Transport()] {
			t.Fatalf("节点收录了未登记的端点 %s（页面 %s）", pk.Transport(), pk)
		}
	}

	// 方向二：登记过的每条路由都得有页（暂不上架的族除外）。
	//
	// 这一条才是「少了一族」的真正守卫：只看节点内部一致性的话，
	// 整个族没被收录也能全绿。
	pending := map[Kind]bool{}
	for _, f := range AllFamilyDocs() {
		for _, k := range f.PendingKinds {
			pending[k] = true
		}
	}
	covered := map[DocumentKey]bool{}
	for pk := range seeded {
		covered[pk.Transport()] = true
	}
	for d := range registered {
		if covered[d] || pending[d.Kind] {
			continue
		}
		t.Fatalf("端点 %s 登记了，但没有任何节点收录它：这一页永远不会出现在站点上", d)
	}
}

// ⑪ 页面标识**全站唯一**，且能区分族（T9）。
//
// 这条是「三族同文」那次事故的直接守卫：过去三族的 URL 段都是
// `ep-images-generations`，标识完全相同 ⇒ 复制出去的链接看不出指向哪一族，
// 而系统里也没有任何东西能证明它们原本是三篇。
func TestPageSlugUniqueAndFamilyQualified(t *testing.T) {
	seen := map[string]PageKey{}
	for _, pk := range PageSet() {
		slug := PageSlugOf(pk)
		if prev, ok := seen[slug]; ok {
			t.Fatalf("页面标识 %q 同时属于 %s 与 %s：标识必须全站唯一", slug, prev, pk)
		}
		seen[slug] = pk
		if len(slug) > 64 {
			t.Fatalf("页面标识 %q 超过 64 字符（doc_endpoints.key 的列宽）", slug)
		}
		if pk.Family != "" && !strings.HasPrefix(slug, EndpointDocSlugPrefix+pk.Family+"-") {
			t.Fatalf("族页面 %s 的标识 %q 没带族前缀：三族会撞成同一个 URL 段", pk, slug)
		}
	}
}

// ⑫ 同一个端点在不同族里必须是**两篇不同的文档**（T1）。
//
// 只说"名称不同"不够：字段表、示例、预设逐字相同而名字不同，仍然是复制品。
// 这里逐项比内容，任一项相同就报错 —— 它拦的正是"改个名字交差"。
func TestSameKindAcrossFamiliesDiffers(t *testing.T) {
	type shape struct {
		family  string
		fields  string
		example string
		presets string
	}
	byKind := map[Kind][]shape{}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			// 只比**厂商协议端点**（`EndpointKey` 非空）。网关自有的读面
			// （查询/取消/元信息）返回的是同一份任务对象，逐字相同是事实：
			// 让它们各不相同才是伪造。族间差异写在各族导语里。
			if tr, ok := TransportOf(e.Doc.Kind); ok && tr.EndpointKey == "" {
				continue
			}
			names := make([]string, 0, len(e.Spec.Fields))
			for _, fl := range e.Spec.Fields {
				names = append(names, fl.Name+":"+string(fl.Type))
			}
			sort.Strings(names)
			pre := make([]string, 0, len(e.Presets))
			for _, p := range e.Presets {
				pre = append(pre, p.Key)
			}
			sort.Strings(pre)
			ex := ""
			if len(e.Spec.Example) > 0 {
				ex = fmt.Sprint(e.Spec.Example)
			}
			byKind[e.Doc.Kind] = append(byKind[e.Doc.Kind], shape{
				family:  f.Key,
				fields:  strings.Join(names, ","),
				example: ex,
				presets: strings.Join(pre, ","),
			})
		}
	}
	for k, list := range byKind {
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				a, b := list[i], list[j]
				switch {
				case a.fields == b.fields:
					t.Fatalf("%s 与 %s 在端点 %s 上字段表完全相同（%d 个字段）：这是同一篇文档被写了两遍",
						a.family, b.family, k, len(strings.Split(a.fields, ",")))
				case a.example != "" && a.example == b.example:
					t.Fatalf("%s 与 %s 在端点 %s 上请求示例逐字相同", a.family, b.family, k)
				case a.presets == b.presets:
					t.Fatalf("%s 与 %s 在端点 %s 上调用方式完全相同（%s）", a.family, b.family, k, a.presets)
				}
			}
		}
	}
}

// ⑩ 任务读面的位置（T6）：列表 / 产物 / 取消全站只有一篇，查询每族各一篇。
//
// 这条规则改过一次，改的原因是**读者链路**：
//   - 过去四个读面全放在「通用 → 异步任务」，结果族节点里提交完异步任务，
//     下一步去哪个接口查状态要自己跨板块找 —— 链路断在半路；
//   - 现在「查询任务状态」在**每个异步族节点下**各挂一篇（内容是本族导语 + 同一份任务对象），
//     其余三个仍是全站唯一：它们与"哪一族"无关，多挂一份就是重复页。
func TestTaskReadFacesPlacement(t *testing.T) {
	count := map[DocumentKey][]string{} // 文档身份 → 出现在哪些节点
	for _, n := range SeedNodes() {
		for _, ep := range n.Endpoints {
			count[ep.Page.Transport()] = append(count[ep.Page.Transport()], n.Key)
		}
	}
	// 三个"与族无关"的读面：恰好一次，且在异步任务节点。
	for _, k := range []Kind{KindTasksList, KindTasksArtifacts, KindTasksCancel} {
		got := count[DocumentKey{Kind: k}]
		if len(got) != 1 || got[0] != "tasks" {
			t.Fatalf("任务读面 %s 出现在 %v（应恰好 1 次，放在「异步任务」节点）", k, got)
		}
	}

	// 查询：通用节点一份（契约全集）+ 每个异步族一份（本族怎么用）。
	got := count[DocumentKey{Kind: KindTasksGet}]
	in := map[string]bool{}
	for _, n := range got {
		in[n] = true
	}
	if !in["tasks"] {
		t.Fatalf("「异步任务」节点缺少查询接口（契约全集只有这一篇）")
	}
	want := map[string]bool{"tasks": true}
	for _, f := range FamilyDocs() {
		if familyEmitsTasks(f) {
			want[f.Key] = true
		}
	}
	for k := range want {
		if !in[k] {
			t.Fatalf("节点 %q 有异步提交端点，却没有「查询任务状态」", k)
		}
	}
	for k := range in {
		if !want[k] {
			t.Fatalf("节点 %q 不该有查询任务状态（它没有异步提交端点，或它是同步族）", k)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("查询任务状态出现了 %d 次，应为 %d 次（节点清单 %v）", len(got), len(want), got)
	}
}

func TestAsyncFamilyHasPollNote(t *testing.T) {
	for _, f := range FamilyDocs() {
		if !familyEmitsTasks(f) {
			continue
		}
		if _, ok := familyPollNotes[f.Key]; !ok {
			t.Fatalf("族 %q 有异步提交端点，但没写「查询任务状态」的导语（family_tasks.go 的 familyPollNotes）", f.Key)
		}
		ep, ok := taskReadFaceOf(f)
		if !ok {
			t.Fatalf("族 %q 应该挂得上一篇查询任务状态", f.Key)
		}
		if ep.Doc.Kind != KindTasksGet {
			t.Fatalf("族 %q 的查询页绑定的是 %s（应为 tasks/get）", f.Key, ep.Doc.Kind)
		}
		if len(ep.Spec.Returns) == 0 {
			t.Fatalf("族 %q 的查询页没有返回字段", f.Key)
		}
		// 同步族不许有：给它挂一篇就是伪造一个不存在的流程。
	}

	for _, f := range FamilyDocs() {
		if familyEmitsTasks(f) {
			continue
		}
		if _, ok := taskReadFaceOf(f); ok {
			t.Fatalf("族 %q 没有异步端点，却有「查询任务状态」", f.Key)
		}
	}
}

// TestFamilyExampleModelIsSlug 守住「示例里的 model 必须是合法模型 id 的形态」。
//
// 为什么需要这条：TestFamilyExampleModelBelongsToFamily 用的是**名称模式**匹配
// （`minimax`/`hailuo`），所以 `MiniMax-H3` 这种大写写法照样通过 —— 而对外合法 id
// 全是全小写连字符 slug（`minimax-h3`）。真机测试里照抄文档示例直接 404 model_not_found，
// 这条门禁就是为了让那种文案在 CI 阶段就红。
//
// 只校验形态不校验存在性：apidoc 是离线包，不认识数据库里的模型注册表。
// 形态是这里能守住的、也是历史上真正出过错的那一层。
func TestFamilyExampleModelIsSlug(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			for _, ex := range []struct {
				name string
				obj  Obj
			}{{"Example", e.Spec.Example}, {"Example2", e.Spec.Example2}} {
				v, ok := ex.obj.Get("model")
				if !ok {
					continue
				}
				id, _ := v.(string)
				if id == "" || id == PresetModelToken {
					continue
				}
				if !slug.MatchString(id) {
					t.Fatalf("族 %q 的 %s 示例 model=%q 不是合法 id 形态（应全小写连字符，如 minimax-h3）",
						f.Key, ex.name, id)
				}
			}
		}
	}
}
