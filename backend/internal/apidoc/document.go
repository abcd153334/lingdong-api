package apidoc

import "strings"

// EndpointDocSlugPrefix 是接口文档 slug 的前缀。
//
// 加前缀是为了让 URL 空间一眼可辨：`/docs/nodes/images-kling/ep-images-generations`
// 里那一段不是节点 key、也不是 kind，而是「契约端点的 URL 形态」。
// 前缀只服务可读性，不参与任何判定 —— 反查一律走 `PageOfSlug` 遍历现算。
const EndpointDocSlugPrefix = "ep-"

// DocumentKey 是一篇**接口文档**的身份。
//
// 两个维度分工（混用会让整条链路失效）：
//   - Kind 只表示**路由 / 线格式**身份（`images/generations`），它决定 relay 怎么转发、
//     进不进 `models.endpoints`、参不参与渠道能力校验；
//   - Variant 只表示「同一路由的**第二套报文**」（`jimeng` = 即梦原生报文）。
//
// 为什么不把方言做进 Kind：`transportIndex` / `kindByPath` / `endpointKeyByPath`
// 全是「Kind 唯一、先登记者优先」的 map（kind.go:383/452/508）。同一个 Kind 登记
// 两条 Transport 时，后者会被静默吞掉——症状是"方言篇的路径指向主篇"或
// "中间件拿错线格式"，而两处代码单看都对。
//
// 反过来，Variant 不参与任何运行期判定：它只是"这篇文档讲的是哪套字段"。
type DocumentKey struct {
	Kind    Kind
	Variant string
}

// String 是落库与日志用的稳定写法：主篇 `video/submit`，方言 `images/generations@jimeng`。
//
// `@` 不在 slug 字符集里、也不在字段名里，所以不会被误解析；
// 主篇不带后缀是刻意的——它让「字段说明键」`<doc>.<field>` 与今天逐字节相同（零迁移）。
func (d DocumentKey) String() string {
	if d.Variant == "" {
		return string(d.Kind)
	}
	return string(d.Kind) + "@" + d.Variant
}

// DocumentKeyOf 把 `images/generations` / `images/generations@jimeng` 解析成文档身份。
//
// 这是「库里存的身份」与「契约里的身份」之间唯一的转换点：文档表存字符串，
// 校验与投影必须还原成 DocumentKey 才能查 Transport / 字段表。
// 解析失败（未知 Kind、空 variant 后缀）一律 false —— 绝不做前缀猜测。
func DocumentKeyOf(id string) (DocumentKey, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return DocumentKey{}, false
	}
	kind, variant, has := strings.Cut(id, "@")
	if has && variant == "" {
		return DocumentKey{}, false
	}
	dk := DocumentKey{Kind: Kind(kind), Variant: variant}
	if _, ok := TransportOf(dk.Kind); !ok {
		return DocumentKey{}, false
	}
	return dk, true
}

// TransportOfDocID 是 DocumentKeyOf + TransportOf 的组合（校验器与播种共用）。
func TransportOfDocID(id string) (Transport, bool) {
	dk, ok := DocumentKeyOf(id)
	if !ok {
		return Transport{}, false
	}
	return TransportOf(dk.Kind)
}

// docEntry 是方言文档的登记项。
//
// v6 里它还带「格式组 / 挂载板块 / 独有能力」三个字段，那三件事只服务
// 侧栏分组与页内指路句；v7 的目录由库里的节点树决定，指路句没了，
// 于是这里只剩「这一条是哪篇方言文档」。
type docEntry struct {
	Doc DocumentKey
}

// TransportSet 返回**全部端点身份**（= 可被绑定的路由，声明顺序 = 声明顺序）。
//
// 它回答的是「哪些路由能挂文档」，不是「站点上有哪些页」——后者是 `PageSet()`。
// 三族页面共用 `images/generations` 这一个端点身份，所以这个集合里它只有一条；
// 拿它数页面数量必然少数两篇（见 page.go 的说明）。
func TransportSet() []DocumentKey {
	variants := make(map[Kind][]DocumentKey, len(variantDocTable))
	for _, e := range variantDocTable {
		variants[e.Doc.Kind] = append(variants[e.Doc.Kind], e.Doc)
	}
	out := make([]DocumentKey, 0, len(Kinds())+len(variantDocTable))
	for _, k := range Kinds() {
		out = append(out, DocumentKey{Kind: k})
		out = append(out, variants[k]...)
	}
	return out
}
