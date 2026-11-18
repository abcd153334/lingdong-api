package apidoc

import "strings"

// 本文件定义**文档篇的身份**。
//
// 为什么需要它：`images/generations` 在 GPT Image / Nano Banana / Seedream 下是
// 三篇不同的文档（字段表、示例、预设、默认值都不是同一套）。如果只拿端点身份
// 当标识，这三篇在系统里就是「同一篇」：
//
//   - 播种集合会把三篇塌成一条 → 「少了一族」的门禁数不出来；
//   - 页面 URL 段完全相同（`ep-images-generations`）→ 复制出来的链接看不出指向谁；
//   - 「两族内容不得逐字相同」这类门禁没有可索引的主键，只能比较总数。
//
// 所以身份必须带族。族是**文档的身份维度**，不是筛选条件 —— 这正是
// 「一份共享模板 + 按族删字段」那个结构最根上的错误：它把族当成了过滤条件。
//
// 端点身份（DocumentKey）仍然只表示路由与线格式，全族共享：三族页面确实打的是
// 同一个网关路径，这是事实，不该被文档结构掩盖。

// PageKey 是一篇接口文档的身份：族 + 端点 + 方言。
//
// Family 为空表示**平台自有端点**（协议篇 / 通用接口 / 任务读面）：
// 它们由网关自己实现，与厂商协议无关，全站只有一篇。
type PageKey struct {
	// Family 是族 key（`images-gpt-image`）；平台自有端点为空串。
	Family string
	// Kind 是端点身份（路由 + 线格式）。
	Kind Kind
	// Variant 是同一路由的第二套报文（目前只有 Seedance 的 `jimeng`）。
	Variant string
}

// String 是落库与日志用的稳定写法：`images-gpt-image:images/generations`。
//
// 分隔符用 `:`：它不在 kind、族 key、variant 的字符集里，不会误解析；
// 平台自有端点不带前缀（`tasks/get`），与改造前逐字节相同。
func (p PageKey) String() string {
	body := p.Transport().String()
	if p.Family == "" {
		return body
	}
	return p.Family + ":" + body
}

// Transport 是这篇文档绑定的端点身份（发布闸门、路由校验用它，不用 PageKey）。
func (p PageKey) Transport() DocumentKey {
	return DocumentKey{Kind: p.Kind, Variant: p.Variant}
}

// PageSlugOf 把文档身份换算成 URL 段（节点内唯一，且全站唯一）。
//
//	平台自有 `tasks/get`                    → `ep-tasks-get`
//	族 `images-gpt-image:images/generations` → `ep-images-gpt-image-images-generations`
//	方言 `videos-seedance:video/submit@jimeng` → `ep-videos-seedance-video-submit--jimeng`
//
// 三条规则：
//  1. 前缀 `ep-` 只服务可读性，不参与判定；
//  2. 族在前、端点在後：标识本身就说清「这是哪一族的哪一页」；
//  3. 方言用 `--`：`-` 是 kind 与族 key 里都有的字符，单横线会与真实 kind 撞车
//     （`TestKindNeverContainsDoubleDash` 守「kind 里不出现双横线」）。
func PageSlugOf(p PageKey) string {
	base := EndpointDocSlugPrefix + strings.ReplaceAll(string(p.Kind), "/", "-")
	if p.Family != "" {
		base = EndpointDocSlugPrefix + p.Family + "-" + strings.ReplaceAll(string(p.Kind), "/", "-")
	}
	if p.Variant == "" {
		return base
	}
	return base + "--" + p.Variant
}

// PageSet 返回站点上**真实存在**的全部页。
//
// 由播种表直接算出，而不是按「端点 × 拥有它的族」猜：同一个端点可能既有通用节点
// 那一篇（`tasks/get` 的契约全集），又有各异步族各一篇 —— 猜出来的集合会漏掉前者，
// 于是「通用节点收录了一篇未登记的页」这种假报警告会一直挂着。
func PageSet() []PageKey {
	out := make([]PageKey, 0, 64)
	for _, n := range SeedNodes() {
		for _, ep := range n.Endpoints {
			out = append(out, ep.Page)
		}
	}
	return out
}
