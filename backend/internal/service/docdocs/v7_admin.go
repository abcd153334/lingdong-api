package docdocs

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 管理台视图（v7）：**编辑态**，与对外投影（v7_view.go）刻意分开。
//
// 为什么不能复用对外投影：公开响应只白名单「读者需要的字段」，而编辑器要回传
// 完整内容（含 status / version / transport_doc_id / origin）。把内部列塞进公开
// 响应等于让前端依赖一个随时会改的值；让编辑器读公开投影则会「读回来的东西存不回去」，
// 保存一次就丢字段 —— 这类丢字段的 bug 不会报错，只会慢慢把文档改坏。
//
// 但**内容字段**两边同形（FieldView / VariantView / ExampleView / PresetView /
// ErrorView 共用）：编辑器的输入结构就是这里的输出结构，不做二次转换，
// 否则「读出来 A、存回 B」的映射错误会出现在两处。

// AdminNode 是节点编辑页的全部内容。
type AdminNode struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Section string `json:"section"`
	// SectionName 供下拉展示；保存时只写 section。
	SectionName string    `json:"section_name"`
	Protocol    string    `json:"protocol"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	Sort        int       `json:"sort"`
	Hidden      bool      `json:"hidden"`
	Status      string    `json:"status"`
	Version     int       `json:"version"`
	Origin      string    `json:"origin"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Models 是本节点适用的模型（family 按前缀命中、协议篇按端点能力派生）。
	Models []NodeModel `json:"models"`
	// Blocks 只对 guide 有意义。
	Blocks    []GuideBlockView   `json:"blocks"`
	Endpoints []AdminEndpointRef `json:"endpoints"`
	Problems  []Problem          `json:"problems"`
}

// AdminEndpointRef 是节点下列出的一行接口（编辑态：多带 status/version/hidden）。
type AdminEndpointRef struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Badge   string `json:"badge"`
	Status  string `json:"status"`
	Hidden  bool   `json:"hidden"`
	Sort    int    `json:"sort"`
	Version int    `json:"version"`
	// Bound=false 表示没绑平台端点 ⇒ 只能存草稿（发布会被拦）。
	Bound bool `json:"bound"`
}

// AdminEndpoint 是一篇接口文档的**可编辑全文**。
type AdminEndpoint struct {
	NodeKey     string    `json:"node_key"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Summary     string    `json:"summary"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Aliases     []string  `json:"aliases"`
	TransportID string    `json:"transport_doc_id"`
	MethodCheck string    `json:"method_check"`
	PathCheck   string    `json:"path_check"`
	Phase       string    `json:"phase"`
	RunMode     string    `json:"run_mode"`
	RunHint     string    `json:"run_hint"`
	MaxBody     string    `json:"max_body"`
	Notes       []string  `json:"notes"`
	Badge       string    `json:"badge"`
	Sort        int       `json:"sort"`
	Hidden      bool      `json:"hidden"`
	Status      string    `json:"status"`
	Version     int       `json:"version"`
	Origin      string    `json:"origin"`
	ContractVer string    `json:"contract_ver"`
	UpdatedAt   time.Time `json:"updated_at"`

	Fields   []FieldView   `json:"fields"`
	Response AdminResponse `json:"response"`
	// 示例代码是内容：编辑器**必须**读得到，否则保存一次就把它清空
	// （回传的是整篇，缺字段等于填 NULL）。
	Samples       *SamplesView  `json:"samples"`
	Samples2      *SamplesView  `json:"samples2"`
	ExampleTitle  string        `json:"example_title"`
	ExampleTitle2 string        `json:"example_title2"`
	Presets       []AdminPreset `json:"presets"`
	Errors        []AdminError  `json:"errors"`
	Problems      []Problem     `json:"problems"`
}

// KVItem 是**保序**对象的一项。
//
// 编辑态为什么不直接给 object：Go 的 map 解码不保序，`{model,prompt,system}` 存一次
// 就可能变成 `{prompt,model,system}` —— 而示例是给人抄的，键序跳动会让人以为内容变了。
// 库里本来就存成 `[{k,v}]`，编辑态用同一个形状，读出来什么样就存回什么样。
type KVItem struct {
	K string `json:"k"`
	V any    `json:"v"`
}

// AdminResponse 与 ResponseView 的唯一区别：变体示例是 KVItem 数组。
type AdminResponse struct {
	Kind        string         `json:"kind"`
	Envelope    bool           `json:"envelope"`
	ContentType string         `json:"content_type"`
	Variants    []AdminVariant `json:"variants"`
}

type AdminVariant struct {
	Status int    `json:"status"`
	Label  string `json:"label"`
	// ContentType 只在「这个状态码的响应体不是 JSON」时才需要写。
	ContentType string `json:"content_type"`
	// Kind / Envelope 是**逐变体**的线格式：202 变体是 JSON 任务对象，
	// 200 二进制变体是 image/png。旧实现把它们折叠成一个节点级值再广播给所有变体，
	// 于是「下载产物」那篇的 200 变体被写成 json/无信封。
	Kind     string      `json:"kind"`
	Envelope bool        `json:"envelope"`
	Returns  []FieldView `json:"returns"`
	Example  []KVItem    `json:"example"`
	Raw      string      `json:"raw"`
	Notes    []string    `json:"notes"`
}

// UnmarshalJSON 让预设体同时接受 `{"k":v}` 与 `[{"k","v"}]`。
func (p *AdminPreset) UnmarshalJSON(b []byte) error {
	type plain AdminPreset
	aux := struct {
		*plain
		Body json.RawMessage `json:"body"`
	}{plain: (*plain)(p)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	kv, err := ParseKVJSON(aux.Body)
	if err != nil {
		return err
	}
	p.Body = kv
	return nil
}

// UnmarshalJSON 同 AdminExample：example 允许对象或 KV 数组。
func (v *AdminVariant) UnmarshalJSON(b []byte) error {
	type plain AdminVariant
	aux := struct {
		*plain
		Example json.RawMessage `json:"example"`
	}{plain: (*plain)(v)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	kv, err := ParseKVJSON(aux.Example)
	if err != nil {
		return err
	}
	v.Example = kv
	return nil
}

type AdminPreset struct {
	Key     string       `json:"key"`
	Label   string       `json:"label"`
	Summary string       `json:"summary"`
	Body    []KVItem     `json:"body"`
	Fields  []string     `json:"fields"`
	Uploads []string     `json:"uploads"`
	Note    string       `json:"note"`
	Samples *SamplesView `json:"samples"`
}

// AdminTree 是整个工作台的目录（含草稿与隐藏项）。
type AdminTree struct {
	Sections []AdminSection `json:"sections"`
	Shape    int            `json:"shape"`
	Contract string         `json:"contract_version"`
	// BaseURL 是站点对外地址：预览要把示例里的 {{base_url}} 换成它。
	// 放在树里是为了让编辑器只拉一次接口就能渲染预览，而不是再请求一次公开目录。
	BaseURL string          `json:"base_url"`
	Totals  AdminTreeTotals `json:"totals"`
}

// AdminSection 是侧栏第一层（内容来自 `doc_sections`，可改名/改序/改色/隐藏）。
//
// 节点按 `section` 归位；库里出现未知板块（table 里没有这一行）时**不隐藏**，
// 单独成组并标 `unknown` —— 那是数据坏了，要么补板块、要么把节点挪走，
// 静默丢掉只会让人以为"这篇文档没了"。
type AdminSection struct {
	Key     string          `json:"key"`
	Name    string          `json:"name"`
	En      string          `json:"en"`
	Desc    string          `json:"desc"`
	Color   string          `json:"color"`
	Sort    int             `json:"sort"`
	Hidden  bool            `json:"hidden"`
	Version int             `json:"version"`
	Unknown bool            `json:"unknown"`
	Nodes   []AdminTreeNode `json:"nodes"`
}

// AdminTreeNode 是侧栏第二层；Endpoints 只有一行行摘要，点开才拉全文。
type AdminTreeNode struct {
	Key       string             `json:"key"`
	Kind      string             `json:"kind"`
	Title     string             `json:"title"`
	Status    string             `json:"status"`
	Hidden    bool               `json:"hidden"`
	Sort      int                `json:"sort"`
	Version   int                `json:"version"`
	Models    int                `json:"models"`
	Endpoints []AdminEndpointRef `json:"endpoints"`
}

// AdminTreeTotals 给工作台顶部一句「多少篇草稿 / 多少篇发布」。
type AdminTreeTotals struct {
	Nodes          int `json:"nodes"`
	Endpoints      int `json:"endpoints"`
	DraftNodes     int `json:"draft_nodes"`
	DraftEndpoints int `json:"draft_endpoints"`
}

// LoadAdminTree 装载管理台目录（**不走缓存**：编辑器必须看到刚保存的状态）。
func LoadAdminTree(base string) (*AdminTree, error) {
	var nodes []model.DocNode
	if err := db.DB.Order("section, sort, key").Find(&nodes).Error; err != nil {
		return nil, err
	}
	sections, err := LoadSections()
	if err != nil {
		return nil, err
	}
	out := &AdminTree{
		Shape: apidoc.CatalogShape, Contract: apidoc.ContractVersion,
		BaseURL:  trimSlash(base),
		Sections: make([]AdminSection, 0, len(sections)+2),
	}
	byID := make(map[uint64]*AdminTreeNode, len(nodes))
	order := make([]*AdminTreeNode, 0, len(nodes))
	for _, n := range nodes {
		tn := &AdminTreeNode{
			Key: n.Key, Kind: n.Kind, Title: n.Title, Status: n.Status,
			Hidden: n.Hidden, Sort: n.Sort, Version: n.Version,
			Endpoints: make([]AdminEndpointRef, 0, 4),
		}
		byID[n.ID] = tn
		order = append(order, tn)
		out.Totals.Nodes++
		if n.Status != model.DocPublished {
			out.Totals.DraftNodes++
		}
	}
	if len(nodes) > 0 {
		ids := make([]uint64, 0, len(nodes))
		for _, n := range nodes {
			ids = append(ids, n.ID)
		}
		var eps []model.DocEndpoint
		if err := db.DB.Where("node_id IN ?", ids).Order("node_id, sort, key").Find(&eps).Error; err != nil {
			return nil, err
		}
		epsByNode := make(map[uint64][]model.DocEndpoint, len(nodes))
		for _, e := range eps {
			epsByNode[e.NodeID] = append(epsByNode[e.NodeID], e)
			tn := byID[e.NodeID]
			if tn == nil {
				continue
			}
			tn.Endpoints = append(tn.Endpoints, endpointRefOf(e))
			out.Totals.Endpoints++
			if e.Status != model.DocPublished {
				out.Totals.DraftEndpoints++
			}
		}
		// 适用模型是派生量（族按前缀、协议按端点能力）：一次模型快照，逐节点算一遍。
		ms := loadModelIndex()
		for _, n := range nodes {
			if tn := byID[n.ID]; tn != nil {
				tn.Models = len(ms.ofNode(n, epsByNode[n.ID]))
			}
		}
	}

	// 按板块归位：先建好全部板块（含隐藏与空板块 —— 运营要能看到并往空板块里加节点）。
	pos := map[string]int{}
	add := func(sec AdminSection) {
		pos[sec.Key] = len(out.Sections)
		out.Sections = append(out.Sections, sec)
	}
	for _, sv := range sections {
		add(AdminSection{
			Key: sv.Key, Name: sv.Name, En: sv.En, Desc: sv.Desc, Color: sv.Color,
			Sort: sv.Sort, Hidden: sv.Hidden, Version: sv.Version,
			Nodes: make([]AdminTreeNode, 0, 4),
		})
	}
	for i := range nodes {
		sec := nodes[i].Section
		if _, ok := pos[sec]; !ok {
			// 未知板块：单独成组，标 unknown，绝不静默丢节点。
			add(AdminSection{Key: sec, Name: sec, Unknown: true, Nodes: make([]AdminTreeNode, 0, 4)})
		}
		out.Sections[pos[sec]].Nodes = append(out.Sections[pos[sec]].Nodes, *order[i])
	}
	return out, nil
}

// LoadAdminNode 装载节点编辑页。
func LoadAdminNode(key string) (*AdminNode, error) {
	b, err := LoadNode(key)
	if err != nil {
		return nil, err
	}
	out := &AdminNode{
		Key: b.Node.Key, Kind: b.Node.Kind, Section: b.Node.Section,
		SectionName: sectionOf(b.Node.Section).Name, Protocol: b.Node.Protocol,
		Title: b.Node.Title, Summary: b.Node.Summary,
		Sort:   b.Node.Sort,
		Hidden: b.Node.Hidden, Status: b.Node.Status, Version: b.Node.Version,
		Origin: b.Node.Origin, UpdatedAt: b.Node.UpdatedAt,
		Models:    loadModelIndex().ofNode(b.Node, b.Eps),
		Blocks:    guideBlocks(b.Node.Blocks, ""),
		Endpoints: make([]AdminEndpointRef, 0, len(b.Eps)),
		Problems:  CheckNode(b),
	}
	for _, e := range b.Eps {
		out.Endpoints = append(out.Endpoints, endpointRefOf(e))
	}
	return out, nil
}

// LoadAdminEndpoint 装载接口编辑页（含体检结论，编辑器直接挂红点）。
func LoadAdminEndpoint(nodeKey, epKey string) (*AdminEndpoint, error) {
	b, err := LoadEndpoint(nodeKey, epKey)
	if err != nil {
		return nil, err
	}
	return adminEndpointOf(b), nil
}

func adminEndpointOf(b *EndpointBundle) *AdminEndpoint {
	out := &AdminEndpoint{
		NodeKey: b.Node.Key, Key: b.EP.Key, Name: b.EP.Name, Summary: b.EP.Summary,
		Method: b.EP.Method, Path: b.EP.Path, Aliases: jsonStrings(b.EP.Aliases),
		Phase: b.EP.Phase, RunMode: b.EP.RunMode, RunHint: b.EP.RunHint,
		MaxBody: b.EP.MaxBody, Notes: jsonStrings(b.EP.Notes), Badge: b.EP.Badge, Sort: b.EP.Sort,
		Hidden: b.EP.Hidden, Status: b.EP.Status, Version: b.EP.Version,
		Origin: b.EP.Origin, ContractVer: b.EP.ContractVer, UpdatedAt: b.EP.UpdatedAt,
		Fields:        make([]FieldView, 0, len(b.Fields)),
		Samples:       samplesViewOf(b.EP.Samples, ""),
		Samples2:      samplesViewOf(b.EP.Samples2, ""),
		ExampleTitle:  b.EP.ExampleTitle,
		ExampleTitle2: b.EP.ExampleTitle2,
		Presets:       make([]AdminPreset, 0, len(b.Presets)),
		Errors:        make([]AdminError, 0, len(b.Errors)),
		Problems:      CheckEndpoint(b),
	}
	if b.EP.TransportDocID != nil {
		out.TransportID = *b.EP.TransportDocID
	}
	out.MethodCheck, out.PathCheck = b.EP.MethodCheck, b.EP.PathCheck

	// 字段/返回字段一律走 fieldView：编辑页看到的措辞必须与对外页面逐字相同，
	// 这样「右边预览」不需要第二套渲染规则。
	for _, f := range b.Fields {
		if f.VariantID != nil {
			continue
		}
		out.Fields = append(out.Fields, fieldView(f, b.Enums[f.ID], f.OnlyModels))
	}
	out.Response = AdminResponse{
		Kind: "json", ContentType: "", Variants: make([]AdminVariant, 0, len(b.Variants)),
	}
	for _, v := range b.Variants {
		if out.Response.ContentType == "" && v.ContentType != "" {
			out.Response.ContentType = v.ContentType
		}
		av := AdminVariant{
			Status: v.StatusCode, Label: v.Label, ContentType: v.ContentType,
			// 逐变体读写：线格式是「这一条响应」的属性，不是整篇的属性。
			Kind: def(v.Kind, "json"), Envelope: v.Envelope,
			Returns: make([]FieldView, 0, 4), Example: kvItems(v.Example),
			Raw: v.Raw, Notes: jsonStrings(v.Notes),
		}
		for _, f := range b.Fields {
			if f.VariantID == nil || *f.VariantID != v.ID {
				continue
			}
			av.Returns = append(av.Returns, fieldView(f, b.Enums[f.ID], ""))
		}
		out.Response.Variants = append(out.Response.Variants, av)
	}
	// 不再「没有变体就合成一条 200」。
	//
	// 合成出来的那条会被原样存回库：运营什么都没改，库里就多了一条本来不存在的
	// 响应。新建接口时**由编辑器显式给一条 200**（用户看得见的数据），
	// 读口只如实返回库里的东西。
	if len(out.Response.Variants) > 0 {
		// 顶层的 kind/envelope 只当「新增一条变体时的默认值」，不广播给已有变体。
		out.Response.Kind = out.Response.Variants[0].Kind
		out.Response.Envelope = out.Response.Variants[0].Envelope
	}
	for _, p := range b.Presets {
		out.Presets = append(out.Presets, AdminPreset{
			Key: p.Key, Label: p.Label, Summary: p.Summary, Body: kvItems(p.Body),
			Fields: jsonStrings(p.Fields), Uploads: jsonStrings(p.Uploads), Note: p.Note,
			Samples: samplesViewOf(p.Samples, ""),
		})
	}
	for _, e := range b.Errors {
		out.Errors = append(out.Errors, AdminError{
			Status: e.StatusCode, Code: e.Code, Desc: e.Message, Source: e.Source,
		})
	}
	return out
}

func endpointRefOf(e model.DocEndpoint) AdminEndpointRef {
	return AdminEndpointRef{
		Key: e.Key, Name: e.Name, Method: e.Method, Path: e.Path, Badge: e.Badge,
		Status: e.Status, Hidden: e.Hidden, Sort: e.Sort, Version: e.Version,
		Bound: e.TransportDocID != nil && strings.TrimSpace(*e.TransportDocID) != "",
	}
}

// ── 可绑定的平台端点（下拉清单） ──

// TransportOption 是「绑定平台端点」下拉里的一项。
//
// 清单只来自 apidoc.TransportSet()（**平台真实登记的端点**），不查库、不接受前端自填：
// 这是「不许伪造能力」的物理边界 —— 下拉里没有的 ID，绑定校验也一定拒绝。
//
// **不带文档名称与摘要**：同一端点在各族是不同的协议（`images/generations`
// 在 GPT Image 与 Seedream 下是两套字段表），端点没有唯一的"名字"。
// 这个下拉只回答"绑定哪条真实路由"，标识就是 doc_id 本身。
type TransportOption struct {
	DocID        string `json:"doc_id"`
	Section      string `json:"section"`
	SectionName  string `json:"section_name"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Phase        string `json:"phase"`
	Badge        string `json:"badge"`
	AcceptsAsync bool   `json:"accepts_async"`
	// UsedBy 是被哪些节点用了（同端点可在多族各有一篇，所以不是"只能选一次"）。
	UsedBy []string `json:"used_by"`
}

// TransportOptions 列出全部可绑定端点，并标注已被哪些节点占用。
func TransportOptions() []TransportOption {
	used := map[string][]string{}
	var eps []model.DocEndpoint
	if err := db.DB.Where("transport_doc_id IS NOT NULL").Find(&eps).Error; err == nil && len(eps) > 0 {
		byNode := map[uint64]string{}
		var nodes []model.DocNode
		_ = db.DB.Find(&nodes).Error
		for _, n := range nodes {
			byNode[n.ID] = n.Title
		}
		for _, e := range eps {
			id := strings.TrimSpace(*e.TransportDocID)
			used[id] = append(used[id], byNode[e.NodeID])
		}
	}
	keys := apidoc.TransportSet()
	out := make([]TransportOption, 0, len(keys))
	for _, d := range keys {
		tr, ok := apidoc.TransportOf(d.Kind)
		if !ok {
			continue
		}
		sec := apidoc.SectionOfKind(d.Kind)
		o := TransportOption{
			DocID:   d.String(),
			Section: sec, SectionName: sectionOf(sec).Name,
			Method: tr.Method, Path: tr.Path, Phase: phaseOf(tr), Badge: badgeOf(tr),
			AcceptsAsync: tr.AcceptsAsync, UsedBy: used[d.String()],
		}
		if o.UsedBy == nil {
			o.UsedBy = []string{}
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Section != out[j].Section {
			return out[i].Section < out[j].Section
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].DocID < out[j].DocID
	})
	return out
}
