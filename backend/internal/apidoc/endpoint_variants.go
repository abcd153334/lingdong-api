package apidoc

// 本文件只剩**方言文档的路由登记**：哪条路由存在第二套报文。
//
// 内容（字段表 / 示例 / 返回）不在这里 —— 方言篇同样属于某一族，
// 写在 family_*.go 里那篇文档旁边。这里只回答「`video/submit@jimeng`
// 这个身份是不是平台真实登记过的端点」，`TransportSet()` 与绑定校验都靠它。
//
// 为什么"身份"与"内容"要分开：身份是**发布闸门**的判据（没登记就不许发布），
// 内容是运营可改的文案；两者混在一起时，运营删一个字段就会改变"这个端点能不能发布"。

// VariantJimeng 是「即梦（Seedream / Seedance）原生报文」变体名。
//
// 它是**报文方言**的名字，不是渠道名：同一个变体可以走不同渠道，
// 渠道是部署细节，改名会让文档 URL 与运营文案一起漂。
const VariantJimeng = "jimeng"

// variantDocTable 是方言文档的登记（主篇由 Kinds() 自动派生，不写在这里）。
//
// 一条记录 = 「同一路由的第二套报文」。登记了却没有族写它那篇，由
// `TestEveryVariantIsDocumented` 拦住 —— 否则它会是一篇没有归属、
// 永远不出现在任何节点下的孤儿文档。
var variantDocTable = []docEntry{
	{Doc: DocumentKey{Kind: KindVideoSubmit, Variant: VariantJimeng}},
}
