package apidoc

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 本文件是**视频字段收敛的守门人**。
//
// 它存在的理由不是"多几条断言"，而是把一件容易反复的事变成不可回退的约束：
// 视频提交的字段曾经在三篇文档里各写一遍（82 条定义、去重 47 个名字），
// 其中 30 个在反复表达 11 组语义。一次人工收敛很容易，但下一个人加一个
// "顺手"的字段就会回到原点 —— 而且没人会注意到。
//
// 五条线：
//  1. **平台字段预算**：视频提交的 Tier=P 字段去重后 ≤ 13。
//  2. **骨架字段必须在场**：每个视频族页都要声明那 10 个（由 VideoCoreFields 生成）。
//  3. **厂商字段必须标记**：非骨架字段一律 Tier=V 且写明适用模型。
//  4. **素材只能有一套名字**：不许再冒出 images / image_urls / first-frame 等同义顶层字段。
//  5. **文档写的字段必须真有人读**：平台字段要落在适配器可读键集合里。

// videoDocRef 是一篇视频提交文档 + 它属于哪个族。
type videoDocRef struct {
	Family string
	Spec   EndpointDoc
}

// videoSubmitDocs 返回所有**非方言**的视频提交文档。
//
// 不用 AllEndpointDocs()：它返回的 Doc 是 DocumentKey（只有 Kind 与 Variant），
// 族信息在装配时丢了 —— 而这几条门禁恰恰要按族分组。
//
// 排除方言（`@jimeng` 那篇）：它讲的是厂商原生报文（content[]），字段表与
// 平台骨架是两套完全不同的东西，混进来会让"骨架字段必须在场"恒假。
func videoSubmitDocs() []videoDocRef {
	var out []videoDocRef
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			if e.Doc.Kind == KindVideoSubmit && e.Doc.Variant == "" {
				out = append(out, videoDocRef{Family: f.Key, Spec: e.Spec})
			}
		}
	}
	return out
}

// videoCoreFieldNames 是骨架字段名（与 VideoCoreFields 的产出一一对应）。
//
// 手写清单而不是从生成器反推：门禁要能发现"生成器被改了、少了一个字段"，
// 用它自己的产出当期望值就永远不会失败。
//
// 这 10 个就是"一个请求体覆盖全部能力"的全部：5 个描述/规格 + 5 个素材。
// 素材这 5 个之间没有枚举、没有 mode、没有 type —— 生成方式由"哪些有值"决定。
var videoCoreFieldNames = []string{
	"model",
	"prompt",
	"seconds",
	"resolution",
	"aspect_ratio",
	"image",
	"first_frame",
	"last_frame",
	"audio",
	"video",
}

// videoOptionalFieldNames 是允许按族裁掉的字段。
//
//   - input_reference：兼容承载（老客户端用数组写法），新接入不用它；
//   - watermark / callback_url：只有部分通道实现，族页可裁（H3 两个都不读）。
var videoOptionalFieldNames = []string{"input_reference", "watermark", "callback_url"}

// videoAllCoreNames 是骨架 + 可选，用于"哪些字段算平台字段"的判断。
func videoAllCoreNames() map[string]bool {
	out := make(map[string]bool, len(videoCoreFieldNames)+len(videoOptionalFieldNames))
	for _, n := range videoCoreFieldNames {
		out[n] = true
	}
	for _, n := range videoOptionalFieldNames {
		out[n] = true
	}
	return out
}

// TestVideoPlatformFieldBudget 守住「平台字段不超过 13 个」。
//
// 为什么要一个数：读者抱怨的不是"字段多"，是"同一件事有好几种写法"。
// 预算会逼着下一个想加字段的人先回答"能不能并进已有的字段里"。
//
// 13 = 10 个骨架 + 3 个可选，已经是当前并集，没有余量。
func TestVideoPlatformFieldBudget(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range videoSubmitDocs() {
		for _, f := range d.Spec.Fields {
			if f.Tier == TierPlatform {
				seen[f.Name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)

	// 三个数打成一行（-v 可见）：它是本测试的输入，也是"收敛到底省了多少"的账本。
	docs := videoSubmitDocs()
	defs := 0
	all := map[string]bool{}
	for _, d := range docs {
		defs += len(d.Spec.Fields)
		for _, f := range d.Spec.Fields {
			all[f.Name] = true
		}
	}
	t.Logf("视频提交文档 %d 篇：字段定义 %d 条，去重后 %d 个名字，其中平台字段 %d 个（预算 13）",
		len(docs), defs, len(all), len(names))
	if len(names) > 13 {
		t.Fatalf("视频提交的平台字段去重后有 %d 个（预算 13）：%s\n"+
			"新增前先问：能不能并进现有字段？如果确实需要，请同步调高预算并说明理由。",
			len(names), strings.Join(names, " / "))
	}
}

// videoCoreOmissions 是「某族按协议裁掉骨架字段」的白名单。
//
// 只有**协议层就不存在**的字段才允许进来，并且每条必须写理由：
// 没有理由的豁免等于把这条门禁变成后门，下一个人会照着加一行图省事。
//
// 为什么不把骨架字段拆成「必须 / 可选」两组：那会让"可选"对**所有族**生效，
// 等于给可灵 / Seedance 的 resolution 也开了口子 —— 而它们是真正在读这个字段的。
var videoCoreOmissions = map[string]map[string]string{
	FamilyVideosMuse: {
		"resolution":  "上游协议没有分辨率参数（duomiMuseLocalOnly 丢弃），价目固定 720p",
		"first_frame": "上游只有 image_urls，首帧会被当普通参考图展开，写成首帧是伪造语义",
		"last_frame":  "同上",
		"audio":       "适配器直接 400（muse 只接受图片参考）",
		"video":       "同上",
	},
}

// TestVideoFamilyDeclaresCoreFields 守住「每个视频族页都声明了骨架字段」。
//
// 这些字段由 VideoCoreFields 生成，正常写不可能缺。这条门禁防的是
// **绕开生成器手写字段表** —— 那正是改造前每族写法逐渐漂移的起点。
//
// 允许缺的只有 videoCoreOmissions 里登记过理由的（目前只有 muse 一家）。
func TestVideoFamilyDeclaresCoreFields(t *testing.T) {
	for _, d := range videoSubmitDocs() {
		have := map[string]bool{}
		for _, f := range d.Spec.Fields {
			have[f.Name] = true
		}
		omit := videoCoreOmissions[d.Family]
		for _, want := range videoCoreFieldNames {
			if have[want] {
				continue
			}
			if strings.TrimSpace(omit[want]) != "" {
				continue // 已在豁免表里登记并写明理由
			}
			t.Errorf("族 %q 的视频提交页缺骨架字段 %q（应用 VideoCoreFields 生成，而不是手写）",
				d.Family, want)
		}
	}
}

// TestVideoCoreOmissionsAreNotBackdoor 守住豁免表本身不腐化。
//
// 三个方向都会让这张表变成后门：
//   - 族被删/改名了，条目还留着 → 下一个人以为那族还在用；
//   - 字段名写错（比如写成 images）→ 豁免了一个不存在的字段，看着像在管，其实没管；
//   - 理由为空 → 等于「我说了算」，门禁失去讨论成本。
func TestVideoCoreOmissionsAreNotBackdoor(t *testing.T) {
	active := map[string]bool{}
	for _, f := range FamilyDocs() {
		active[f.Key] = true
	}
	core := map[string]bool{}
	for _, n := range videoCoreFieldNames {
		core[n] = true
	}
	for family, fields := range videoCoreOmissions {
		if !active[family] {
			t.Errorf("豁免表里的族 %q 不在已上架族里：族已删或改名，条目应一起删", family)
		}
		for name, reason := range fields {
			if !core[name] {
				t.Errorf("族 %q 豁免了 %q，但它不是骨架字段（写错名字 = 豁免了一个不存在的字段）", family, name)
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("族 %q 豁免 %q 但没写理由：没有理由的豁免等于把门禁变成后门", family, name)
			}
		}
	}
}

// TestVideoVendorFieldsAreMarked 守住「非骨架字段必须标成厂商扩展并写明适用模型」。
//
// 不标的后果：读者看到 `service_tier` / `camera_control` 会以为所有模型都认，
// 换一个模型就静默失效（或直接 400）。
func TestVideoVendorFieldsAreMarked(t *testing.T) {
	core := videoAllCoreNames()
	for _, d := range videoSubmitDocs() {
		for _, f := range d.Spec.Fields {
			if core[f.Name] {
				continue
			}
			if f.Tier != TierVendor {
				t.Errorf("族 %q 的字段 %q 不是骨架字段，必须标 Tier=V（厂商扩展），实际 %q",
					d.Family, f.Name, f.Tier)
			}
			if strings.TrimSpace(f.OnlyModels) == "" {
				t.Errorf("族 %q 的字段 %q 标了厂商扩展却没写适用模型", d.Family, f.Name)
			}
		}
	}
}

// TestVideoAliasFieldsPointToCanonical 守住「别名不再逐页声明」。
//
// 改造前族页有 8 个别名字段（duration/image/first_frame/last_frame 各两族），
// 说明文字各不相同，读者无法判断它们是不是同一件事。现在别名统一由
// VideoCompatNote 说明；这条门禁守住"万一有人又加回来"的情况。
func TestVideoAliasFieldsPointToCanonical(t *testing.T) {
	for _, d := range videoSubmitDocs() {
		for _, f := range d.Spec.Fields {
			if f.Tier != TierAlias {
				continue
			}
			t.Errorf("族 %q 又出现了别名字段 %q：别名统一由 VideoCompatNote 说明，"+
				"不再逐页声明（见 video_common.go）", d.Family, f.Name)
		}
	}
}

// TestVideoCompatNoteCoversAllAliases 守住「兼容写法说明不漏项」。
//
// 代码认的每个别名都必须出现在那段人话里，否则就是"文档没说、但平台确实会改写"，
// 读者会以为自己写的字段被吞了。
func TestVideoCompatNoteCoversAllAliases(t *testing.T) {
	note := VideoCompatNote()
	for _, a := range VideoCompatAliases() {
		if !strings.Contains(note, "`"+a.Legacy+"`") {
			t.Errorf("兼容写法说明里漏了别名 %q：%s", a.Legacy, note)
		}
		if !strings.Contains(note, "`"+a.Canonical+"`") {
			t.Errorf("别名 %q 没写清等价于什么（%q）", a.Legacy, a.Canonical)
		}
	}
}

// TestVideoCompatNotePresentInEveryFamily 守住「每族都说了兼容写法」。
//
// 别名是网关级行为，对每一族都生效。少写一页的后果是那一页的读者以为
// 自己写的 `duration` 被吞了 —— 而实际上平台改写成了 seconds。
//
// 不再要求每页一字不差地复述全表：允许按族裁剪（见 VideoCompatNoteOf），
// 裁剪得对不对由下一条 TestVideoCompatNoteMatchesFamilyFields 双向核对。
func TestVideoCompatNotePresentInEveryFamily(t *testing.T) {
	const prefix = "旧写法仍然可用"
	for _, d := range videoSubmitDocs() {
		found := false
		for _, n := range d.Spec.Notes {
			if strings.Contains(n, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("族 %q 的视频提交页没有「兼容写法」说明（应包含 VideoCompatNote 系列）", d.Family)
		}
	}
}

// compatPairRe 抽「旧写法 → 目标」对：`duration` → `seconds`。
var compatPairRe = regexp.MustCompile("`([^`]+)` → `([^`]+)`")

// compatPairsOf 返回一篇文档兼容段里的全部映射（Legacy → 目标字段名列表）。
func compatPairsOf(spec EndpointDoc) map[string][]string {
	out := map[string][]string{}
	for _, n := range spec.Notes {
		for _, m := range compatPairRe.FindAllStringSubmatch(n, -1) {
			var targets []string
			for _, part := range strings.Split(m[2], "/") {
				if part = strings.TrimSpace(part); part != "" {
					targets = append(targets, part)
				}
			}
			out[m[1]] = targets
		}
	}
	return out
}

// TestVideoCompatNoteMatchesFamilyFields 守住「兼容段与字段表严格对齐」。
//
// 两个方向都要查，只查一边都会留下假承诺：
//   - **该说的没说**：本页明明有 `seconds` / `aspect_ratio` / `image`，却漏了
//     `duration` / `ratio` / `image_urls` —— 读者以为旧写法被吞了。
//   - **不该说的说了**：本页没有 `resolution`（Muse 上游就没有这个参数），
//     却写「`size` → `resolution`，旧写法仍然可用」—— 读者照着传会得到
//     「传了没反应」，比不写更糟。这条正是 Muse 页第一版犯的错。
func TestVideoCompatNoteMatchesFamilyFields(t *testing.T) {
	// knownFields 是「视频族里真实存在的字段名」全集：只有落在这里的目标
	// 才需要在本页存在。`720p` 这类**取值**不在其中，靠 AppliesTo 判定。
	knownFields := map[string]bool{}
	for _, d := range videoSubmitDocs() {
		for _, f := range d.Spec.Fields {
			knownFields[f.Name] = true
		}
	}
	if len(knownFields) == 0 {
		t.Fatal("没有采集到任何视频字段：videoSubmitDocs 失效了")
	}

	for _, d := range videoSubmitDocs() {
		have := map[string]bool{}
		for _, f := range d.Spec.Fields {
			have[f.Name] = true
		}
		pairs := compatPairsOf(d.Spec)

		// ① 该说的都说了
		for _, a := range VideoCompatAliases() {
			if a.AppliesTo != "" && !have[a.AppliesTo] {
				continue // 本页没有它落的字段（如 Muse 没有 resolution）
			}
			applies := true
			for _, tg := range strings.Split(a.Canonical, "/") {
				tg = strings.TrimSpace(tg)
				if knownFields[tg] && !have[tg] {
					applies = false
					break
				}
			}
			if !applies {
				continue
			}
			if _, ok := pairs[a.Legacy]; !ok {
				t.Errorf("族 %q 的字段表里有这条别名要落的目标，兼容段却没提 `%s`：读者会以为旧写法被吞了",
					d.Family, a.Legacy)
			}
		}

		// ② 不该说的没说
		for legacy, targets := range pairs {
			for _, tg := range targets {
				if knownFields[tg] && !have[tg] {
					t.Errorf("族 %q 的兼容段写了 `%s` → `%s`，但本页没有 %q 这个字段："+
						"照抄会得到「传了没反应」", d.Family, legacy, tg, tg)
				}
			}
		}
	}
}

// TestVideoCoreFieldsShape 守住生成器本身：产出与门禁清单一致、全部 Tier=P。
//
// 生成器是"一处维护、多页引用"的那个"一处"。它的产出变了而清单没变，
// 所有族页会同时漂移 —— 这条断言把两者钉在一起。
func TestVideoCoreFieldsShape(t *testing.T) {
	got := VideoCoreFields(VideoCoreOpts{})
	names := make([]string, 0, len(got))
	for _, f := range got {
		names = append(names, f.Name)
		if f.Tier != TierPlatform {
			t.Errorf("骨架字段 %q 必须是 Tier=P，实际 %q", f.Name, f.Tier)
		}
	}
	want := append(append([]string{}, videoCoreFieldNames...), videoOptionalFieldNames...)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("骨架字段与门禁清单不一致：\n实际 %v\n期望 %v", names, want)
	}
}

// TestVideoCoreFieldsOverrides 守住按族覆盖点真的生效。
//
// Omit / Require 是"同一个生成器服务多个族"的唯一出口。它们失效的表现很隐蔽：
// 页面照常渲染，只是把"这一族根本不读的字段"或"必填写成可选"留在那里。
func TestVideoCoreFieldsOverrides(t *testing.T) {
	got := VideoCoreFields(VideoCoreOpts{
		Omit:    []string{"input_reference", "watermark", "callback_url"},
		Require: map[string]Required{"prompt": ReqRequired, "seconds": ReqRequired},
	})
	have := map[string]Field{}
	for _, f := range got {
		have[f.Name] = f
	}
	for _, omitted := range []string{"input_reference", "watermark", "callback_url"} {
		if _, ok := have[omitted]; ok {
			t.Errorf("Omit 没生效：%q 仍在字段表里", omitted)
		}
	}
	for _, name := range []string{"prompt", "seconds"} {
		f, ok := have[name]
		if !ok {
			t.Fatalf("Omit 误删了 %q", name)
		}
		if f.Required != ReqRequired {
			t.Errorf("%q 的必填性覆盖失败：%q", name, f.Required)
		}
		if strings.TrimSpace(f.RequiredNote) != "" {
			t.Errorf("%q 覆盖成必填后仍留着条件说明 %q：读者会看到「必填 + 条件是…」", name, f.RequiredNote)
		}
	}
}

// 视频提交的字段名里不允许再出现这两类概念。
//
// role 是"第二套同义词表"（对 type 的再包装），mode 是"让调用方自己声明生成方式"——
// 后者正是本平台刻意不做的事：方式由素材推断，读者不需要选。
var (
	videoBannedFieldName = regexp.MustCompile(`\b(role|mode)\b`)
	videoBannedFieldDesc = regexp.MustCompile(`\b(role|mode)\b`)
)

// TestVideoNoRoleOrMode 守住「不再引入 role / mode」。
//
// 用词边界匹配：`model` 含 "mode" 四个字母，直接子串匹配会误伤每个族的第一行。
func TestVideoNoRoleOrMode(t *testing.T) {
	for _, d := range videoSubmitDocs() {
		for _, f := range d.Spec.Fields {
			if videoBannedFieldName.MatchString(f.Name) {
				t.Errorf("族 %q 的字段名 %q 出现了 role/mode：生成方式应由素材推断，"+
					"用途由字段名本身表达（first_frame / last_frame）", d.Family, f.Name)
			}
			if videoBannedFieldDesc.MatchString(f.Desc) {
				t.Errorf("族 %q 的字段 %q 的说明里出现了 role/mode：%q", d.Family, f.Name, f.Desc)
			}
		}
	}
}

// videoBannedAliasNames 是与骨架字段同义的其它写法。
//
// 它们曾经都存在过（image_urls / image_url / first-frame…），每一次都让读者
// 多学一个名字。列成表而不是靠人记：新加一个字段时先来这里看一眼。
var videoBannedAliasNames = []string{
	"images", "image_urls", "image_url", "audio_urls", "audio_url",
	"video_urls", "video_url", "first-frame", "last-frame", "firstFrame", "lastFrame",
	"type", "material", "materials",
}

// TestVideoMaterialFieldsAreCanonical 守住「素材只能有一套名字」。
func TestVideoMaterialFieldsAreCanonical(t *testing.T) {
	banned := map[string]bool{}
	for _, n := range videoBannedAliasNames {
		banned[n] = true
	}
	for _, d := range videoSubmitDocs() {
		for _, f := range d.Spec.Fields {
			// 子路径（`input_reference[].image_url`）是兼容承载内部的键，不算顶层字段。
			if strings.ContainsAny(f.Name, ".[") {
				continue
			}
			if banned[f.Name] {
				t.Errorf("族 %q 出现了同义顶层字段 %q：素材只应有 image / first_frame / "+
					"last_frame / audio / video 五个名字", d.Family, f.Name)
			}
		}
	}
}

// videoConsumedTopKeys 是**适配器真的会读**的顶层键（两家合并）。
//
// 依据：`metasoBody` 的字段 + `metasoLocalOnly` 之外原样下发的部分，
// 以及 `autodlBody` 的字段与 `autodlKnownTopKeys`（由 json tag 反射得出）。
//
// 手写而不是从结构体反射：这条门禁的意义就是"文档与适配器对账"，
// 用适配器的结构体当期望值会让它变成自证。
var videoConsumedTopKeys = map[string]bool{
	"model": true, "prompt": true, "seconds": true, "resolution": true, "size": true,
	"aspect_ratio": true, "input_reference": true, "metadata": true, "seed": true,
	"image": true, "audio": true, "video": true,
	"first_frame": true, "last_frame": true,
	"watermark": true, "callback_url": true,
}

// TestVideoDocFieldsAreConsumed 守住「文档写的字段真的有人读」。
//
// 反例就是本轮改掉的那个：H3 页面原本写着 `watermark` 与 `callback_url`，
// 而 H3 的两个适配器一个只认 `metadata.aigc_watermark`、另一个顶层白名单里
// 根本没有这两个键 —— 读者照文档传，一边静默无效、一边直接 400。
func TestVideoDocFieldsAreConsumed(t *testing.T) {
	for _, d := range videoSubmitDocs() {
		for _, f := range d.Spec.Fields {
			if f.Tier != TierPlatform {
				continue
			}
			if strings.ContainsAny(f.Name, ".[") {
				continue // 子路径由父字段的读取方负责
			}
			if !videoConsumedTopKeys[f.Name] {
				t.Errorf("族 %q 的字段 %q 没有任何适配器会读：要么给它接上，"+
					"要么从页面删掉（文档声明了却没人读 = 伪造）", d.Family, f.Name)
			}
		}
	}
}

// docLinkPattern 匹配文档正文里的站内文档链接。
var docLinkPattern = regexp.MustCompile(`\]\((/docs/nodes/[^)\s]+)\)`)

// TestFamilyLinksResolve 守住「文档里的站内链接不会 404」。
//
// 这条是真实事故的守卫：删掉「视频生成」通用节点时，可灵与 Seedance 的摘要里
// 还留着指过去的链接 —— 页面照常渲染，点下去 404，而没有任何测试会红。
func TestFamilyLinksResolve(t *testing.T) {
	// 节点 key → 该节点下所有页面的 slug。
	slugs := map[string]map[string]bool{}
	for _, n := range SeedNodes() {
		set := map[string]bool{}
		for _, ep := range n.Endpoints {
			set[ep.Key] = true
			set[PageSlugOf(ep.Page)] = true
		}
		slugs[n.Key] = set
	}
	check := func(owner, text string) {
		for _, m := range docLinkPattern.FindAllStringSubmatch(text, -1) {
			parts := strings.Split(strings.TrimSuffix(m[1], "/"), "/")
			// 期望形态：["", "docs", "nodes", node, "endpoints", slug]
			if len(parts) != 6 {
				t.Errorf("%s 的链接 %q 形态不对（应为 /docs/nodes/{节点}/endpoints/{页面}）", owner, m[1])
				continue
			}
			node, slug := parts[3], parts[5]
			set, ok := slugs[node]
			if !ok {
				t.Errorf("%s 的链接 %q 指向不存在的节点 %q", owner, m[1], node)
				continue
			}
			if !set[slug] {
				t.Errorf("%s 的链接 %q 指向不存在的页面 %q（节点 %q 下没有它）", owner, m[1], slug, node)
			}
		}
	}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			check(f.Key+"/"+e.Doc.String(), e.Spec.Summary)
			for _, n := range e.Spec.Notes {
				check(f.Key+"/"+e.Doc.String(), n)
			}
		}
	}
}
