package docdocs

import (
	"strings"
	"time"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/model"
)

// 编辑态 → 行结构的**唯一构造点**。
//
// 保存与预览都从这里出发：预览之所以会和发布结果不一样，从来不是渲染层的错，
// 而是两条构造链各补各的默认值（预览漏 aliases、把顶层 kind/envelope 广播给
// 所有变体，保存却逐变体）。合并成一条之后，"预览看到什么"就是"保存后是什么"。
//
// 规则很少，但每一条都必须在两处同时成立，所以只能写在这里：
//   - 方法/路径/试跑模式的兜底；
//   - phase/badge 由**绑定的契约**派生（不接受手填，否则侧栏会承诺不存在的能力）；
//   - 变体的 status/label/kind 逐条兜底（202 是 JSON 任务对象，200 二进制是 png，
//     不能共用一个 kind）；
//   - 保存保持原状态：已发布的仍是发布态，草稿仍是草稿。

// endpointDraft 是规范化之后的接口行结构（尚未落库）。
type endpointDraft struct {
	EP       model.DocEndpoint
	Fields   []model.DocEndpointField
	Enums    [][]model.DocFieldEnum
	Variants []draftVariant
	Presets  []model.DocPreset
	Errors   []model.DocEndpointError
}

type draftVariant struct {
	Row    model.DocResponseVariant
	Fields []model.DocEndpointField
	Enums  [][]model.DocFieldEnum
}

// draftOf 把编辑态输入规范成行结构。
//
// prev 是库里现存的接口行（新建时传零值）：它提供状态、发布时间与版本。
// 只读它、不改它 —— 副作用全部留给 writeDraft。
func draftOf(prev model.DocEndpoint, in EndpointInput) (*endpointDraft, error) {
	if err := limit(in.Name, 160, "接口名"); err != nil {
		return nil, err
	}
	if err := limit(in.Path, 200, "路径"); err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = "POST"
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		path = "/v1/endpoint"
	}

	var docID *string
	phase, badge := "sync", ""
	if s := strings.TrimSpace(in.TransportID); s != "" {
		id := s
		docID = &id
		if tr, ok := apidoc.TransportOfDocID(s); ok {
			phase, badge = phaseOf(tr), badgeOf(tr)
		}
	}
	status := prev.Status
	if status == "" {
		status = model.DocDraft
	}
	// 状态与发布时间的对应关系只在这里定一次：发布态必须有发布时间（否则前台
	// 排序与「最近更新」没有依据），草稿态必须没有（否则看起来像上过线）。
	publishedAt := prev.PublishedAt
	if status == model.DocPublished {
		if publishedAt == nil {
			now := time.Now()
			publishedAt = &now
		}
	} else {
		publishedAt = nil
	}

	row := prev
	row.Name = strings.TrimSpace(in.Name)
	row.Summary = strings.TrimSpace(in.Summary)
	row.Method, row.Path, row.TransportDocID = method, path, docID
	row.Phase, row.Badge = phase, badge
	row.RunMode = def(in.RunMode, "json")
	row.RunHint, row.MaxBody = in.RunHint, strings.TrimSpace(in.MaxBody)
	row.Aliases = strJSON(in.Aliases)
	row.Notes = strJSON(cleanLines(in.Notes))
	row.Hidden, row.Sort = in.Hidden, in.Sort
	row.Status, row.PublishedAt = status, publishedAt
	row.Samples = samplesJSON(in.Samples)
	row.Samples2 = samplesJSON(in.Samples2)
	row.ExampleTitle = strings.TrimSpace(in.ExampleTitle)
	row.ExampleTitle2 = strings.TrimSpace(in.ExampleTitle2)
	// 绑定成功时把契约的方法/路径冻结下来：漂移检测与「绑定后契约变了」都靠它。
	if docID != nil {
		if tr, ok := apidoc.TransportOfDocID(*docID); ok {
			row.MethodCheck, row.PathCheck = tr.Method, tr.Path
		}
	} else {
		row.MethodCheck, row.PathCheck = "", ""
	}

	d := &endpointDraft{EP: row}

	for i, f := range in.Fields {
		d.Fields = append(d.Fields, fieldRowOf(f, i))
		d.Enums = append(d.Enums, enumRowsOf(f))
	}

	defKind := def(in.Response.Kind, "json")
	for vi, v := range in.Response.Variants {
		status := v.Status
		if status == 0 {
			status = 200
		}
		label := strings.TrimSpace(v.Label)
		if label == "" {
			label = "成功"
		}
		dv := draftVariant{Row: model.DocResponseVariant{
			StatusCode: status, Label: label,
			// 逐变体：封套与格式都是「这一条响应」的属性，不是整篇的。
			Kind: def(v.Kind, defKind), ContentType: v.ContentType, Envelope: v.Envelope,
			Example: kvsJSON(v.Example), Raw: v.Raw,
			Notes: strJSON(cleanLines(v.Notes)), Sort: vi * 10,
		}}
		for i, f := range v.Returns {
			dv.Fields = append(dv.Fields, fieldRowOf(f, i))
			dv.Enums = append(dv.Enums, enumRowsOf(f))
		}
		d.Variants = append(d.Variants, dv)
	}

	for i, p := range in.Presets {
		if strings.TrimSpace(p.Key) == "" {
			return nil, inputErrf("第 %d 个调用方式缺少标识", i+1)
		}
		d.Presets = append(d.Presets, model.DocPreset{
			Key: strings.TrimSpace(p.Key), Label: strings.TrimSpace(p.Label),
			Summary: p.Summary, Body: kvsJSON(p.Body),
			Fields: strJSON(p.Fields), Uploads: strJSON(p.Uploads),
			Note: p.Note, Samples: samplesJSON(p.Samples), Sort: i * 10,
		})
	}

	for i, e := range in.Errors {
		if strings.TrimSpace(e.Code) == "" {
			return nil, inputErrf("第 %d 条错误码缺少 code", i+1)
		}
		status := e.Status
		if status == 0 {
			status = 400
		}
		d.Errors = append(d.Errors, model.DocEndpointError{
			StatusCode: status, Code: strings.TrimSpace(e.Code), Message: e.Desc,
			// 来源原样保留：seed/ai/manual 的区分只在这一列，写死会把出厂错误码
			// 都变成"人写的"，之后再也分不出哪些该跟着契约走。
			Source: e.Source, Sort: i * 10,
		})
	}
	return d, nil
}

// bundle 把草稿渲染成投影层的输入（合成主键，仅供「这条属于哪个变体」的连接）。
//
// 合成的 id 必须是正数且唯一：用 0 会让所有返回字段都挂到第一个变体上；
// 这些 id 只在这里存在，落库时由 writeDraft 换成真实主键。
func (d *endpointDraft) bundle(node model.DocNode) *EndpointBundle {
	b := &EndpointBundle{
		Node: node, EP: d.EP,
		Enums: make(map[uint64][]model.DocFieldEnum, len(d.Fields)),
	}
	next := uint64(1)
	appendField := func(row model.DocEndpointField, enums []model.DocFieldEnum) {
		row.ID = next
		next++
		for i := range enums {
			enums[i].FieldID = row.ID
		}
		b.Fields = append(b.Fields, row)
		b.Enums[row.ID] = enums
	}
	for i, f := range d.Fields {
		appendField(f, d.Enums[i])
	}
	for vi, v := range d.Variants {
		row := v.Row
		row.ID = 1000 + uint64(vi)
		b.Variants = append(b.Variants, row)
		for i, f := range v.Fields {
			f.VariantID = &row.ID
			appendField(f, v.Enums[i])
		}
	}
	b.Presets = d.Presets
	b.Errors = d.Errors
	return b
}

// bundleOfInput 用编辑态输入构造一个完整 bundle（预览与「生成示例」用，不落库）。
func bundleOfInput(node model.DocNode, prev model.DocEndpoint, in EndpointInput) (*EndpointBundle, error) {
	d, err := draftOf(prev, in)
	if err != nil {
		return nil, err
	}
	return d.bundle(node), nil
}

// fieldRowOf 把编辑态字段映射成行结构（唯一映射，保存与预览共用）。
func fieldRowOf(f FieldView, sort int) model.DocEndpointField {
	return model.DocEndpointField{
		In: def(f.In, "body"), Name: strings.TrimSpace(f.Name),
		Type: def(f.Type, "string"), Required: def(f.Required, "optional"),
		RequiredNote: strings.TrimSpace(f.RequiredNote), DefaultValue: f.Default,
		Desc:       f.Desc,
		EnumSource: enumSourceFor(f), Constraints: constraintsOf(f.Constraints),
		OnlyModels: strings.TrimSpace(f.OnlyModels),
		Tier:       def(f.Tier, "standard"), Passthrough: f.Passthrough, SketchSkip: f.SketchSkip,
		Example: anyJSON(f.Example), Sort: sort,
	}
}

// enumRowsOf 是字段枚举的行结构（重复值在落库前就被拒，见 writeDraft）。
func enumRowsOf(f FieldView) []model.DocFieldEnum {
	out := make([]model.DocFieldEnum, 0, len(f.Enum))
	for i, e := range f.Enum {
		v := strings.TrimSpace(e.Value)
		if v == "" {
			continue
		}
		out = append(out, model.DocFieldEnum{
			Value: v, Desc: e.Desc, Deprecated: e.Deprecated, Sort: i * 10,
		})
	}
	return out
}

// kvObj 把有序 KV 转成生成器用的 Obj（保留顺序与数值原样）。
func kvObj(items []KVItem) apidoc.Obj {
	out := make(apidoc.Obj, 0, len(items))
	for _, it := range items {
		out = append(out, apidoc.KV{K: it.K, V: it.V})
	}
	return out
}
