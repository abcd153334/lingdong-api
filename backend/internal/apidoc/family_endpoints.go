package apidoc

// 本文件是**接口文档内容的唯一声明处**：每一族自己那几篇，完整写在各自文件里。
//
// 为什么是「每族一份完整文档」而不是「一份共享模板 + 按族删字段」：
//
//	旧结构下族只能在模板上做减法（`NarrowDocToFamily + NarrowDocByCaps`），
//	族无法新增自己的参数、无法改一句话、无法换示例 —— 于是 GPT Image、
//	Nano Banana、Seedream 三个页面的字段表与说明文字必然逐字相同，
//	差别只剩「最后几行被删掉了」，而被删剩的那几行仍然在讲别家的语义。
//	这不是"文案没写好"，是结构决定了它们不可能不同。
//
// 三条纪律（写内容前先读）：
//  1. **每个字段都要能回答「出自哪份官方文档的哪一节」**。答不上来的字段删掉，
//     不要用"部分模型可能支持"这类免责句掩盖 —— 那句话对读者等于没有信息。
//  2. **平台自己的行为**（网关强校验、平台归一、异步切换）写在字段说明里并说明
//     是谁的行为；厂商参数照抄厂商文档的默认值与取值范围。
//  3. **示例、预设、返回字段都属于这一族**：示例里的 model 必须是本族模型，
//     预设的 body 必须只含本族字段。这三样过去是 Kind 级共享的，
//     于是 Seedream 的页面上能抄到 `doubao-seedream-5-0` 的 model 却在讲 Nano Banana。
//
// 新增一族 = 在本目录加一个文件 + 在 familyEndpointTable 里登记一行。

// FamilyEndpoint 是一族里一篇接口文档的全部内容。
//
// 内容与「身份」分开：`Doc` 是身份（绑定校验、URL、文档表主键），
// `Spec` 是内容（名称/摘要/字段/返回/示例/响应变体）。
type FamilyEndpoint struct {
	// Doc 是平台已登记端点身份（`images/generations` / `images/generations@jimeng`）。
	Doc DocumentKey
	// Spec 是这篇文档的全部内容。`Spec.Kind` 必须等于 `Doc.Kind`。
	Spec EndpointDoc
	// Presets 是这篇文档右栏的「调用方式」，只含这篇文档自己的字段。
	Presets []Preset
}

// familyEndpointTable 把族 key 映射到它的接口文档（顺序即页面顺序）。
//
// 只登记**已上架**的族：`FamilyDocs()` 遍历的就是这份表的键；
// `TestEveryActiveFamilyHasDocs` 守「声明了却忘了写文档」与「写了却没登记」两个方向。
var familyEndpointTable = map[string][]FamilyEndpoint{
	FamilyImagesGptImage:   docsImagesGptImage,
	FamilyImagesNanoBanana: docsImagesNanoBanana,
	FamilyImagesSeedream:   docsImagesSeedream,
	FamilyVideosKling:      docsVideosKling,
	FamilyVideosSeedance:   docsVideosSeedance,
	FamilyVideosMiniMax:    docsVideosMiniMax,
	FamilyVideosMuse:       docsVideosMuse,
	FamilyMusicSuno:        docsMusicSuno,
	FamilyAudioWhisper:     docsAudioWhisper,
}

// FamilyEndpointsOf 返回某族的接口文档（顺序即页面顺序）。
//
// 顺序 = 本族自己的清单 + 末尾追加「查询任务状态」（异步族才有，见 family_tasks.go）。
// 追加是**派生**的而不是在每族文件里各写一行：漏写的表现是"这个节点没有查询接口"，
// 不报错、也不好发现；而它完全由传输契约的 `AcceptsAsync` 决定，不该再抄一遍。
//
// 暂未上架的族返回空切片：调用方（播种）不需要为它建节点。
func FamilyEndpointsOf(f FamilyDoc) []FamilyEndpoint {
	out := familyEndpointTable[f.Key]
	if len(out) == 0 {
		return out
	}
	if ep, ok := taskReadFaceOf(f); ok {
		// 拷贝再追加：直接 append 可能写进表底层的 spare capacity，
		// 让下一次调用拿到重复项（表里长度恰好等于容量时才会踩到，极难复现）。
		out = append(append([]FamilyEndpoint(nil), out...), ep)
	}
	return out
}

// FamilyEndpointOf 取一族里的一篇文档；未登记返回 false。
//
// 这是「这篇文档归属哪一族」的反查入口：同一端点身份（`images/generations`）
// 在三族各有一篇，只按 Kind 查必然拿到其中一篇而不知道是哪一篇的语义。
func FamilyEndpointOf(family string, d DocumentKey) (FamilyEndpoint, bool) {
	// 必须走 FamilyEndpointsOf：异步族末尾的「查询任务状态」是派生出来的，
	// 直接翻表会漏掉它，于是那一页会被判成「未登记的页面」。
	f, ok := FamilyDocOf(family)
	if !ok {
		return FamilyEndpoint{}, false
	}
	for _, e := range FamilyEndpointsOf(f) {
		if e.Doc == d {
			return e, true
		}
	}
	return FamilyEndpoint{}, false
}

// AllEndpointDocs 返回全部字段契约：平台自有端点 + 各族接口文档。
//
// 顺序：先平台自有（endpointTable 顺序），再各族（FamilyDocs 顺序）。
// 只给测试与自检用 —— 运行期不得读文档内容（那会让"后台改文案"影响运行判定）。
func AllEndpointDocs() []FamilyEndpoint {
	out := make([]FamilyEndpoint, 0, len(endpointDocs)+16)
	for _, k := range Kinds() {
		if doc, ok := endpointDocs[k]; ok {
			out = append(out, FamilyEndpoint{Doc: DocumentKey{Kind: k}, Spec: doc})
		}
	}
	for _, f := range FamilyDocs() {
		for _, e := range FamilyEndpointsOf(f) {
			// Kind 由身份决定：内容里不重复写一遍（写错就会按错误的线格式渲染）。
			e.Spec.Kind = e.Doc.Kind
			out = append(out, e)
		}
	}
	return out
}

// FamiliesOfDocument 返回某篇文档身份被哪些族拥有（用于「这个端点有文档吗」）。
func FamiliesOfDocument(d DocumentKey) []string {
	out := make([]string, 0, 2)
	for _, f := range FamilyDocs() {
		if _, ok := FamilyEndpointOf(f.Key, d); ok {
			out = append(out, f.Key)
		}
	}
	return out
}
