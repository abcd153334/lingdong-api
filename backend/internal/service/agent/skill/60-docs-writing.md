---
id: 60-docs-writing
title: 对外文档怎么写
summary: 文档节点与接口页的写法判据、发布闸门、草稿与发布
priority: 60
---

# 对外文档

文档站是**一棵树**：板块 → 节点 → 接口页。一个节点是一组接口（如某厂商的图片接口），
一个接口页是一篇完整文档（请求参数 / 响应 / 示例 / 错误码）。

## 先读再写

`doc_tree` 找 node/key → `doc_endpoint_get` 读现有内容（含可绑定端点与体检结论）→
`doc_endpoint_update` 只提交要改的键。

- 参数与形状：见工具参数里的 JSON Schema，**不要凭记忆编字段名**。
- 数组（fields / response / examples / presets / errors）是**整体替换**，不是合并。
  只想改请求字段时用 `doc_fields_write`，不必把整篇回传。
- 改节点元信息（标题、成员、板块）用 `doc_node_update`（增量语义，只提交要改的键）。

## 三条硬规矩

1. **只能写平台真实登记过的端点**。`transport_doc_id` 只取自 `doc_endpoint_get`
   返回的 `bindable_transports`；没绑定的文档**只能存草稿**，发布会一直被拒（E_UNBOUND）。
   不要为了"让页面完整"编一个端点 —— 那会让客户照着调一个不存在的接口。
2. **异步端点必须成套**：端点支持 `async:true` 时，请求字段里要有 `async`，
   响应里要有 202 变体。少哪一半，发布都会被拦（E_ASYNC_FIELD_MISSING / E_ASYNC_VARIANT_MISSING）。
3. **模型族必须有成员**，且一个模型只能属于一个族。空族发布不了（E_NODE_EMPTY）。

## 写完必须自查

`doc_check`（单篇给 node+key，整族只给 node）。`blocking` 必须为 0，
否则把 problems 里的 `field` 逐条改掉 —— 不要整篇重写，整篇重写会覆盖别人刚改的内容。

体检码的含义与改法：`schema_get(section="docs_text")` 的 `blocking_codes`
（唯一来源是后端 `docdocs.ProblemCatalog`，不要凭印象解释一个码）。

## 你写的是草稿

所有 doc_* 写工具都只落**草稿**，对外不可见；发布由运营在管理台文档工作台点击。
你没有发布能力，也不要建议"先发出去看看" —— 草稿预览（管理台右栏）就能看效果。

## 写作判据

- 读者是接入方：写"传什么、返回什么、什么时候会失败"，不写内部实现与运营口径。
- 不写金额：价格随时会变，写死必然失真。
- 示例地址一律 `https://api.example.com`，不允许第三方域名。
- 不要出现上游名、渠道、内部代号（见 `forbidden_words`）。
