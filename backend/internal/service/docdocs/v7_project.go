package docdocs

import (
	"encoding/json"
	"strings"

	"go.uber.org/zap"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
	"lingdong-api/internal/pkg/logger"
	"lingdong-api/internal/service/pricing"
)

// 投影层：**唯一渲染出口**。页面、Markdown、AI 读取全部只认这里的输出。
//
// 为什么不直接序列化 model.*：内部列（origin/version/updated_by）一旦下发，
// 前端就会开始依赖它；等要改语义时才发现删不掉。这里逐字段挑选，加字段要写理由。
//
// 另一条更重要的边界：**这里不造字**。板块名从库里读、示例代码从库里读、
// 字段说明原样搬。投影层每多造一句，后台就多一块改不了的文案。

// ProjectCatalog 生成侧栏目录（shape=7）。
//
// 板块列表来自 `doc_sections`（排序也在库里）；隐藏板块与空板块不出现。
// 读不到板块是致命错误而不是空目录：那意味着播种没跑，静默给一个空侧栏
// 只会让人以为"文档没了"。
func ProjectCatalog(t *Tree, base string) (Catalog, error) {
	out := Catalog{
		ContractVersion: apidoc.ContractVersion,
		Shape:           apidoc.CatalogShape,
		BaseURL:         trimSlash(base),
		Sections:        []CatalogSection{},
	}
	sections, err := LoadSections()
	if err != nil {
		return out, err
	}
	ms := loadModelIndex()
	bySection := map[string][]CatalogNode{}
	for _, n := range t.Nodes {
		eps := t.Endpoints[n.ID]
		bySection[n.Section] = append(bySection[n.Section],
			catalogNodeOf(n, eps, len(ms.ofNode(n, eps))))
	}
	for _, s := range sections {
		if s.Hidden {
			continue
		}
		nodes, ok := bySection[s.Key]
		if !ok || len(nodes) == 0 {
			continue
		}
		out.Sections = append(out.Sections, CatalogSection{
			ID: s.Key, En: s.En, Name: s.Name, Desc: s.Desc, Color: s.Color, Nodes: nodes,
		})
	}
	return out, nil
}

func catalogNodeOf(n model.DocNode, eps []model.DocEndpoint, members int) CatalogNode {
	out := CatalogNode{
		Key: n.Key, Kind: n.Kind, Title: n.Title, Summary: n.Summary,
		Models: members, Status: n.Status, Endpoints: make([]CatalogEndpoint, 0, len(eps)),
	}
	for _, e := range eps {
		out.Endpoints = append(out.Endpoints, catalogEndpointOf(e))
	}
	return out
}

func catalogEndpointOf(e model.DocEndpoint) CatalogEndpoint {
	return CatalogEndpoint{Key: e.Key, Name: e.Name, Method: e.Method, Path: e.Path, Badge: e.Badge}
}

// ProjectNode 生成节点头（面包屑、适用模型、接口清单、说明页正文）。
//
// apiBase 只用于替换说明页里的 `{{base_url}}`（示例代码要打的地址）。
// 传空会保留占位符并记一条警告 —— 宁可在页面上看见 `{{base_url}}`，
// 也不要把它替换成一个猜出来的域名。
func ProjectNode(b *NodeBundle, apiBase string) Node {
	return projectNode(b, apiBase, loadModelIndex())
}

func projectNode(b *NodeBundle, apiBase string, ms modelIndex) Node {
	s := sectionOf(b.Node.Section)
	out := Node{
		Key: b.Node.Key, Kind: b.Node.Kind, Section: b.Node.Section, SectionName: s.Name,
		Title: b.Node.Title, Summary: b.Node.Summary,
		Models:    []NodeModel{},
		Endpoints: []CatalogEndpoint{},
		Blocks:    []GuideBlockView{},
	}
	for _, e := range b.Eps {
		if !visibleEndpoint(b.Node, e) {
			continue
		}
		out.Endpoints = append(out.Endpoints, catalogEndpointOf(e))
	}
	out.Models = ms.ofNode(b.Node, b.Eps)
	if b.Node.Kind == model.DocNodeGuide {
		if trimSlash(apiBase) == "" && len(b.Node.Blocks) > 0 {
			// 只有公开投影会走到这里（管理台/守卫读原文时也传空串，但不该刷日志）。
			logger.L().Warn("说明页投影缺少基址，正文可能残留 {{base_url}}", zap.String("node", b.Node.Key))
		}
		out.Blocks = guideBlocks(b.Node.Blocks, apiBase)
	}
	return out
}

// modelIndex 是投影期的「在售模型快照」：一次加载，全部节点与接口共用。
//
// 「在售」判据走 pricing（与模型广场同源）：模型下架后文档里不该继续列它 ——
// 用户照着文档写完代码才发现没有权限。
type modelIndex struct {
	rows        []model.Model
	vendorNames map[string]string
}

func loadModelIndex() modelIndex {
	// 纯单元测试（没连库）等价于「0 个在售模型」：适用模型为空，
	// 页面照常渲染 —— 这正是文档与模型解耦要保证的行为。
	if db.DB == nil {
		return modelIndex{}
	}
	rows, err := pricing.OnSaleModels()
	if err != nil {
		logger.L().Warn("读取在售模型失败（适用模型降级为空）", zap.Error(err))
		return modelIndex{}
	}
	return modelIndex{rows: rows, vendorNames: vendorNamesOf(rows)}
}

// ofNode 返回「这个节点适用于哪些模型」。
//
// 两条判据、一处实现：
//   - 模型族：`apidoc.FamilyDocOfModelID`（模型 ID 子串命中）—— 与字段收窄同一套词；
//   - 协议篇：`apidoc.AppliesDoc`（模型声明的端点能对上这篇讲的能力）。
//
// 通用接口与说明页没有「适用模型」概念，返回空数组（不是 nil）。
func (ms modelIndex) ofNode(n model.DocNode, eps []model.DocEndpoint) []NodeModel {
	out := make([]NodeModel, 0, 4)
	switch n.Kind {
	case model.DocNodeFamily:
		f, ok := apidoc.FamilyDocOf(n.Key)
		if !ok {
			return out
		}
		for i := range ms.rows {
			if got, ok := apidoc.FamilyDocOfModelID(ms.rows[i].ID); ok && got.Key == f.Key {
				out = append(out, ms.view(ms.rows[i]))
			}
		}
	case model.DocNodeProtocol:
		kinds := nodeKinds(eps)
		if len(kinds) == 0 {
			return out
		}
		for i := range ms.rows {
			facts := apidoc.FactsOf(&ms.rows[i])
			for _, k := range kinds {
				if apidoc.AppliesDoc(facts, k) {
					out = append(out, ms.view(ms.rows[i]))
					break
				}
			}
		}
	}
	return out
}

// ofEndpointNode 是接口页用的版本：接口 bundle 只带节点行，协议篇要判「适用模型」
// 还需要该节点下已发布的接口种类，所以这里按需再查一次（一个节点通常 1–6 行）。
func (ms modelIndex) ofEndpointNode(n model.DocNode) []NodeModel {
	if n.Kind != model.DocNodeProtocol {
		return ms.ofNode(n, nil)
	}
	var eps []model.DocEndpoint
	if err := db.DB.Where("node_id = ?", n.ID).Find(&eps).Error; err != nil {
		logger.L().Warn("读取节点接口失败（适用模型降级为空）", zap.String("node", n.Key), zap.Error(err))
		return []NodeModel{}
	}
	return ms.ofNode(n, eps)
}

func (ms modelIndex) view(m model.Model) NodeModel {
	name := ms.vendorNames[m.Vendor]
	if name == "" {
		name = m.Vendor
	}
	return NodeModel{ID: m.ID, Name: displayNameOf(m), Vendor: m.Vendor, VendorName: name}
}

// nodeKinds 返回节点下已发布接口绑定的端点种类（协议篇的适用模型判据）。
func nodeKinds(eps []model.DocEndpoint) []apidoc.Kind {
	seen := map[apidoc.Kind]bool{}
	out := make([]apidoc.Kind, 0, len(eps))
	for _, e := range eps {
		if e.TransportDocID == nil || e.Status != model.DocPublished || e.Hidden {
			continue
		}
		dk, ok := apidoc.DocumentKeyOf(*e.TransportDocID)
		if !ok || seen[dk.Kind] {
			continue
		}
		seen[dk.Kind] = true
		out = append(out, dk.Kind)
	}
	return out
}

// vendorNamesOf 一次查齐厂商显示名（缺行回落空串，由调用方回落到 id）。
func vendorNamesOf(rows []model.Model) map[string]string {
	seen := map[string]bool{}
	ids := make([]string, 0, len(rows))
	for _, m := range rows {
		if m.Vendor == "" || seen[m.Vendor] {
			continue
		}
		seen[m.Vendor] = true
		ids = append(ids, m.Vendor)
	}
	if len(ids) == 0 {
		return map[string]string{}
	}
	var vs []model.Vendor
	if err := db.DB.Where("id IN ?", ids).Find(&vs).Error; err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(vs))
	for _, v := range vs {
		if n := strings.TrimSpace(v.Name); n != "" {
			out[v.ID] = n
		}
	}
	return out
}

func displayNameOf(m model.Model) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.ID
}

// ProjectEndpoint 生成一篇接口文档的全文。
func ProjectEndpoint(b *EndpointBundle, base string) Endpoint {
	return projectEndpoint(b, base, loadModelIndex())
}

func projectEndpoint(b *EndpointBundle, base string, ms modelIndex) Endpoint {
	s := sectionOf(b.Node.Section)
	kind := kindOf(b)
	out := Endpoint{
		Node:         NodeRef{Key: b.Node.Key, Title: b.Node.Title, Section: b.Node.Section, SectionName: s.Name},
		Key:          b.EP.Key,
		Kind:         string(kind),
		Name:         b.EP.Name,
		Summary:      b.EP.Summary,
		Method:       b.EP.Method,
		Path:         b.EP.Path,
		Aliases:      jsonStrings(b.EP.Aliases),
		Phase:        phaseText(b.EP.Phase),
		PhaseCode:    b.EP.Phase,
		AcceptsAsync: acceptsAsyncOf(b),
		Transport:    transportOf(b),
		RunMode:      b.EP.RunMode,
		RunHint:      b.EP.RunHint,
		MaxBody:      b.EP.MaxBody,
		Notes:        jsonStrings(b.EP.Notes),
		Fields:       []FieldView{},
		Presets:      []PresetView{},
		Errors:       []ErrorView{},
		// 示例代码是**内容**：库里写什么就下发什么，只把 `{{base_url}}` 换成
		// 站点地址。这里不再现算 —— 现算意味着"页面上的代码"不属于任何一篇文档，
		// 运营在后台无从修改。
		Samples:       samplesViewOf(b.EP.Samples, base),
		Samples2:      samplesViewOf(b.EP.Samples2, base),
		ExampleTitle:  b.EP.ExampleTitle,
		ExampleTitle2: b.EP.ExampleTitle2,
		// 适用模型是**节点级**概念：族本身就是「这一族模型共用这套协议」，
		// 逐接口再切一次等于把族的定义重新猜一遍。空数组在前端整段隐藏。
		Models:    ms.ofEndpointNode(b.Node),
		Published: b.EP.Status == model.DocPublished,
	}
	// 字段说明**原样投影**，不按「本篇有哪些模型」删任何东西。
	//
	// 这里曾按成员/族归属过滤分组说明（为了不让共享模板把别族的模型名印过来）。
	// 但那个判据的输入是库里现在有哪些模型，于是模型下架 ⇒ 说明整块消失。
	// 文档是契约：字段在不在由契约决定，模型在不在由货架决定，两者没有因果关系。
	for _, f := range b.Fields {
		if f.VariantID != nil {
			continue
		}
		out.Fields = append(out.Fields, fieldView(f, b.Enums[f.ID], f.OnlyModels))
	}
	out.Response = responseView(b)
	for _, e := range b.Errors {
		out.Errors = append(out.Errors, ErrorView{Status: e.StatusCode, Code: e.Code, Desc: e.Message})
	}
	for _, p := range b.Presets {
		out.Presets = append(out.Presets, PresetView{
			Key: p.Key, Label: p.Label, Summary: p.Summary,
			Body: jsonObj(p.Body), Fields: jsonStrings(p.Fields),
			Uploads: jsonStrings(p.Uploads), Note: p.Note,
			Samples: samplesViewOf(p.Samples, base),
		})
	}
	return out
}

func phaseText(p string) string {
	switch p {
	case "submit":
		return "异步提交"
	case "read":
		return "任务查询"
	}
	return "同步"
}

func fieldView(f model.DocEndpointField, enums []model.DocFieldEnum, only string) FieldView {
	out := FieldView{
		Name: f.Name, Type: f.Type, In: f.In, Required: f.Required,
		RequiredNote: f.RequiredNote, Default: f.DefaultValue,
		Desc: f.Desc, ValueSource: f.EnumSource,
		Constraints: jsonConstraints(f.Constraints), Tier: f.Tier,
		Passthrough: f.Passthrough, SketchSkip: f.SketchSkip, OnlyModels: only,
		Example: jsonAny(f.Example),
	}
	for _, e := range enums {
		out.Enum = append(out.Enum, EnumView{Value: e.Value, Desc: e.Desc, Deprecated: e.Deprecated})
	}
	// 注意：不能因为「没有静态枚举」就把 value_source 清空。
	// `image_tier` 这类来源本来就没有静态列表（值由模型决定），清空等于
	// 告诉前端「这个字段没有取值范围」，而它其实有。
	return out
}

func responseView(b *EndpointBundle) ResponseView {
	out := ResponseView{Kind: "json", Variants: []VariantView{}}
	for _, v := range b.Variants {
		if v.Kind != "" {
			out.Kind = v.Kind
		}
		out.Envelope = out.Envelope || v.Envelope
		if v.ContentType != "" && out.ContentType == "" {
			out.ContentType = v.ContentType
		}
		vv := VariantView{
			Status: v.StatusCode, Label: v.Label, ContentType: v.ContentType,
			Returns: []FieldView{}, Example: jsonObjPreserve(v.Example),
			Raw: v.Raw, Notes: jsonStrings(v.Notes),
		}
		for _, f := range b.Fields {
			if f.VariantID == nil || *f.VariantID != v.ID {
				continue
			}
			vv.Returns = append(vv.Returns, fieldView(f, b.Enums[f.ID], ""))
		}
		out.Variants = append(out.Variants, vv)
	}
	if len(out.Variants) == 0 {
		// 没写响应变体时给一个空的成功形态：页面上「没有任何响应」比「空表」更误导。
		// 这是只读兜底（不写库、不产生数据），新建接口时编辑器会显式给一条 200。
		out.Variants = append(out.Variants, VariantView{Status: 200, Label: "成功", Returns: []FieldView{}})
	}
	return out
}

// jsonObjPreserve 把有序 KV 还原成对象；空值返回 nil（省掉一个 `null`）。
func jsonObjPreserve(j model.JSON) any {
	o := jsonObj(j)
	if len(o) == 0 {
		return nil
	}
	return o
}

// guideBlocks 把库里的 JSON 转成视图，并把 {{base_url}} 换成真实入口地址。
//
// 替换函数是 apidoc.ResolveGuideBlocks（说明页占位符的唯一实现）。
// 这里只做「类型边界」的转换：库里是 model.JSON，对外是 GuideBlockView。
func guideBlocks(j model.JSON, apiBase string) []GuideBlockView {
	if len(j) == 0 {
		return []GuideBlockView{}
	}
	var raw []apidoc.GuideBlock
	if err := json.Unmarshal(j, &raw); err != nil {
		logger.L().Warn("说明页正文解析失败（按空正文下发）", zap.Error(err))
		return []GuideBlockView{}
	}
	if base := trimSlash(apiBase); base != "" {
		raw = apidoc.ResolveGuideBlocks(raw, apidoc.GuideVars{BaseURL: base})
	}
	out := make([]GuideBlockView, 0, len(raw))
	for _, b := range raw {
		out = append(out, GuideBlockView{
			Type: b.Type, Text: b.Text, Title: b.Title, Items: b.Items, Lang: b.Lang, Code: b.Code,
		})
	}
	return out
}
