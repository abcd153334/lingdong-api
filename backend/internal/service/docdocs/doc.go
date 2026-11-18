// Package docdocs 是「对外 API 文档」的唯一读写入口。
//
// 五层真相源与职责（改之前先读）：
//
//	apidoc                 端点契约、板块、出厂种子、示例生成  ← 代码单源，不可写
//	models                 模型事实（模态/端点/计费/绑定）      ← 管理台模型页
//	doc_nodes + node_models 节点（板块内的一族接口）与成员模型    ← 本包写
//	doc_endpoints + 子表    接口页（字段/变体/示例/预设/错误码）  ← 本包写
//	doc_endpoints.transport 绑定的平台端点                      ← 发布闸门
//
// 三条边界：
//  1. **可编辑但不许伪造**：内容全部落库（运营与 AI 都能改），但一个接口页只有绑定了
//     `apidoc.TransportSet()` 里真实登记的端点才能发布 —— 否则客户会照着调一个不存在的接口。
//     判据只有一处：`CheckEndpoint` 的 `E_UNBOUND`。别在写层再加一套"看起来像不像"的校验。
//  2. **本包不渲染**：页面与 Markdown 的唯一出口是 `ProjectEndpoint` / `ProjectNode` /
//     `ProjectCatalog`。一旦在这里拼一段文案，就会出现「页面与导出说法不同」这种没人查得出的问题。
//  3. **本包不缓存**：文档站是低频页面，一次请求几条查询远比「改完文案要等缓存过期」划算。
//
// 文件分工：
//
//	v7_seed.go     出厂种子（只插不改）与「按在售模型补草稿族」
//	v7_load.go     目录 / 节点 / 接口三级按需装载
//	v7_view.go     对外投影的数据结构
//	v7_project.go  投影（页面与 Markdown 共用）
//	v7_check.go    体检（发布闸门 + AI 自查，同一套判据）
//	v7_write.go    写层（保存=整篇替换子行 + version 乐观锁）
//	v7_admin.go    编辑态视图（含未发布内容）
//	v7_preview.go  未保存内容的预览（不落库）
//	v7_problems.go 体检码目录（AI 看到的 `blocking_codes` 唯一来源）
package docdocs
