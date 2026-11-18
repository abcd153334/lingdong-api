package docdocs

import (
	"encoding/json"
	"strings"

	"lingdong-api/internal/apidoc"
	"lingdong-api/internal/db"
	"lingdong-api/internal/model"
)

// 示例代码：**内容**，不是渲染结果。
//
// 这里只有两件事：把库里的一篇文档渲染成四段代码（生成器），以及把库里的
// 四段代码取出来做一次 `{{base_url}}` 替换（读口）。
//
// 三条边界（改之前先读）：
//  1. 生成期只认**库里的字段表与字段示例值**（`exampleBodyOf`）。示例里的请求体
//     与参数表里那枚「示例」chip 是同一个值 —— 两处各算一份必然分叉。
//  2. 生成期**不许写真实域名**（只写 `{{base_url}}`）。域名属于环境：写进库就等于
//     换域名后所有文档还在教用户调旧地址，而且要逐篇重新生成才修得好。
//  3. 不引入 `{{model}}`：model 是字段示例值的一部分，属于内容。

// SampleBaseURL 是入库示例里的基址占位符（投影期替换成站点地址）。
//
// 与说明页正文用的是同一个记号，替换也走同一个语义：站点没配对外地址时
// 原样保留占位符 —— 宁可在页面上看见 `{{base_url}}`，也不猜一个域名。
const SampleBaseURL = "{{base_url}}"

// sampleEnv 是生成期环境：基址固定占位符，KEY 用默认环境变量名。
//
// KeyEnv 是烘进文本的常量（`$LD_API_KEY`），生成之后它就是内容的一部分，
// 所以照抄 `DefaultSampleEnv` 的值，不另起一个字面量。
func sampleEnv() apidoc.SampleEnv {
	return apidoc.SampleEnv{BaseURL: SampleBaseURL, KeyEnv: apidoc.DefaultSampleEnv().KeyEnv}
}

// exampleBodyOf 由**字段示例值**拼出请求体（唯一实现）。
//
// 只取 body 位置、有 `example` 的请求字段，按字段顺序拼。query/path/form 的
// 值由生成器自己按字段名从同一个 `ep.Example` 里取（`sampleValue`），所以
// 这里不需要分类处理。
//
// 不做「缺值时补占位」：生成器自己会在完全取不到值时用 `<字段名>` 兜底，
// 两处各补一次就会得到两个不同的请求体。
func exampleBodyOf(b *EndpointBundle) apidoc.Obj {
	out := make(apidoc.Obj, 0, len(b.Fields))
	for _, f := range b.Fields {
		if f.VariantID != nil || f.In != "body" || len(f.Example) == 0 {
			continue
		}
		out = append(out, apidoc.KV{K: f.Name, V: jsonAny(f.Example)})
	}
	return out
}

// projectionOf 把库里的接口行投影成示例生成器的输入。
//
// Transport 必须给：生成器靠它决定「拼 JSON 还是 -F 表单」。少了它，
// 文件上传类接口的 curl 会把字段写成 JSON 体 —— 照抄必然 400，
// 而页面上看不出任何异常。
func projectionOf(b *EndpointBundle) apidoc.EndpointProjection {
	ep := apidoc.EndpointProjection{
		Kind:      string(kindOf(b)),
		Method:    b.EP.Method,
		Path:      b.EP.Path,
		Transport: sampleMIMEOf(b),
		RunMode:   b.EP.RunMode,
		Fields:    requestFieldProjections(b),
		Example:   exampleBodyOf(b),
	}
	return ep
}

// BuildSamplesOf 生成主示例的四段代码（请求体取字段示例值）。
func BuildSamplesOf(b *EndpointBundle) (SamplesView, bool) {
	return buildSamples(b, nil)
}

// BuildSamplesWithBody 用指定请求体生成示例（「第二种写法」与预设用）。
//
// body 为空时退回主示例体：预设没写 body 时它就是一个「换个说明的同一请求」，
// 给空白示例比给主示例更误导。
func BuildSamplesWithBody(b *EndpointBundle, body apidoc.Obj) (SamplesView, bool) {
	if len(body) == 0 {
		return BuildSamplesOf(b)
	}
	return buildSamples(b, body)
}

func buildSamples(b *EndpointBundle, body apidoc.Obj) (SamplesView, bool) {
	ep := projectionOf(b)
	// 没有请求字段也照样生成：GET 类接口（模型清单、任务查询）本来就没有参数，
	// 而「页面上没有示例代码」比「示例里只有一个 curl URL」更让人不知道从哪下手。
	s := apidoc.BuildSamplesWithBody("", ep, sampleEnv(), body)
	return SamplesView{Curl: s.Curl, Python: s.Python, Node: s.Node, Go: s.Go}, true
}

// BuildPresetSamples 生成某个「调用方式」的示例（用预设自己的 body）。
func BuildPresetSamples(p *model.DocPreset, b *EndpointBundle) (SamplesView, bool) {
	return BuildSamplesWithBody(b, jsonObj(p.Body))
}

// GenerateEndpointSamples 给编辑器的「生成」按钮用：按当前草稿算，不落库。
//
// 与保存走同一条构造链（`draftOf`），所以「生成」看到的代码与保存后页面上的
// 代码逐字一致 —— 否则按钮就成了第三个真相源。
//
// 只生成**主示例**：第二种写法是另一种请求体的写法，没有可以"照当前字段重算"
// 的输入（它的 body 就是运营手写的那一份），所以它只能手工编辑。
func GenerateEndpointSamples(nodeKey, epKey string, in EndpointInput) (*SamplesView, error) {
	nb, err := LoadNode(nodeKey)
	if err != nil {
		return nil, ErrDocNotFound
	}
	var prev model.DocEndpoint
	if cur, e := loadEndpoint(db.DB, nodeKey, epKey); e == nil {
		prev = cur.EP
	}
	b, err := bundleOfInput(nb.Node, prev, in)
	if err != nil {
		return nil, err
	}
	if s, ok := BuildSamplesOf(b); ok {
		return &s, nil
	}
	return nil, nil
}

// GeneratePresetSamples 按预设当前的 body 生成示例（不落库）。
//
// 用**库里已保存**的字段表：预设的示例只换请求体，其余（方法/路径/字段/环境）
// 与主示例同一份 —— 传一份草稿进来会让两段代码来自两篇不同的文档。
func GeneratePresetSamples(nodeKey, epKey string, body []KVItem) (*SamplesView, error) {
	b, err := LoadEndpoint(nodeKey, epKey)
	if err != nil {
		return nil, err
	}
	if s, ok := BuildSamplesWithBody(b, kvObj(body)); ok {
		return &s, nil
	}
	return nil, nil
}

// samplesViewOf 把库里的四段代码转成视图，并替换 `{{base_url}}`。
//
// 这是**唯一**的替换点：生成器只写占位符，读口只做替换。除此之外任何地方
// 拼示例代码，都会绕开这条规则（换域名后不生效，或页面上漏出占位符）。
func samplesViewOf(j model.JSON, base string) *SamplesView {
	if len(j) == 0 {
		return nil
	}
	var s SamplesView
	if err := json.Unmarshal(j, &s); err != nil {
		return nil
	}
	if s.Curl == "" && s.Python == "" && s.Node == "" && s.Go == "" {
		return nil
	}
	if b := trimSlash(base); b != "" {
		s = SamplesView{
			Curl:   strings.ReplaceAll(s.Curl, SampleBaseURL, b),
			Python: strings.ReplaceAll(s.Python, SampleBaseURL, b),
			Node:   strings.ReplaceAll(s.Node, SampleBaseURL, b),
			Go:     strings.ReplaceAll(s.Go, SampleBaseURL, b),
		}
	}
	return &s
}

// samplesJSON 是写库侧的同一件事（入参是视图）。
//
// 四段全空 → nil：页面据此判断「这篇没有示例代码」，不显示空代码框。
func samplesJSON(s *SamplesView) model.JSON {
	if s == nil || (s.Curl == "" && s.Python == "" && s.Node == "" && s.Go == "") {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return model.JSON(b)
}

// sampleMIMEOf 是示例生成器要的传输方式（标准 MIME，不是展示用的短名）。
func sampleMIMEOf(b *EndpointBundle) string {
	if transportOf(b) == "multipart" {
		return "multipart/form-data"
	}
	return "application/json"
}

// requestFieldProjections 只投影**请求**字段（返回字段不进示例）。
func requestFieldProjections(b *EndpointBundle) []apidoc.FieldProjection {
	out := make([]apidoc.FieldProjection, 0, len(b.Fields))
	for _, f := range b.Fields {
		if f.VariantID != nil {
			continue
		}
		fp := apidoc.FieldProjection{
			Name: f.Name, Type: f.Type, In: f.In, Required: f.Required,
			RequiredNote: f.RequiredNote, Default: f.DefaultValue,
			Desc: f.Desc, Tier: f.Tier, Passthrough: f.Passthrough,
		}
		for _, e := range b.Enums[f.ID] {
			fp.Values = append(fp.Values, e.Value)
		}
		if len(fp.Values) > 0 {
			fp.ValueSource = f.EnumSource
		}
		out = append(out, fp)
	}
	return out
}

// kindOf 是接口绑定的契约端点种类（未绑定返回空）。
func kindOf(b *EndpointBundle) apidoc.Kind {
	if b.EP.TransportDocID != nil {
		if dk, ok := apidoc.DocumentKeyOf(*b.EP.TransportDocID); ok {
			return dk.Kind
		}
	}
	return ""
}

// transportOf 是展示用的传输短名（json / multipart）。
func transportOf(b *EndpointBundle) string {
	if b.EP.TransportDocID != nil {
		if dk, ok := apidoc.DocumentKeyOf(*b.EP.TransportDocID); ok {
			if tr, ok := apidoc.TransportOf(dk.Kind); ok && tr.Multipart {
				return "multipart"
			}
		}
	}
	return "json"
}

// acceptsAsyncOf 是「同一路径还能加 `async: true` 转任务」的事实。
//
// 与 transportOf 同源：**只从绑定的契约端点取**，不看库里手填的 badge
// （badge 是侧栏的展示文本，可能被运营改；二值能力不该靠字符串反推）。
// 未绑定一律 false —— 草稿页不能凭空承诺异步。
func acceptsAsyncOf(b *EndpointBundle) bool {
	if b.EP.TransportDocID == nil {
		return false
	}
	tr, ok := apidoc.TransportOfDocID(*b.EP.TransportDocID)
	if !ok {
		return false
	}
	return tr.AcceptsAsync && familyAllowsAsync(b.Node.Key, tr)
}

// familyAllowsAsync 是「这一族的协议到底支不支持 async」的唯一包装。
//
// 端点契约里的 AcceptsAsync 说的是「这个 URL 长得像能异步」，它盖不住族差异：
// /v1/images/generations 在多米有原生异步任务，在方舟只有同步。
// 族声明（FamilyDoc.AsyncSubmit）才是协议事实；**未知族回落端点事实**，
// 通用节点（tasks / models / 说明页）不受这条影响。
func familyAllowsAsync(nodeKey string, tr apidoc.Transport) bool {
	if f, ok := apidoc.FamilyOfKey(nodeKey); ok {
		return f.AsyncSubmit
	}
	return tr.AcceptsAsync
}
