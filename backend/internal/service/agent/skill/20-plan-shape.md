---
id: 20-plan-shape
title: 提案的形状
summary: op / entity / key / id / expect_version / data 的语义、三条最易错规则、四个字段放错层、一份最小正例
priority: 20
---

# 提案（plan）怎么写

一条提案是「一次自洽的配置变更」，服务端按依赖顺序执行：
**供应商 → 规格 → 渠道 → 模型 → 绑定**。你不要自己排序。

## 一条操作的字段

`op` / `entity` 的合法取值、每个实体允许哪些 op、`data` 里每个字段的含义与上限，
**全部**在 `schema_get(section="plan")` 里（可按 `entity` 只取一个实体）。
不要凭字段名猜，也不要从示例里类比出字段名——示例教的是"形状"，字段表才是权威。

只有四条语义不在字段表里，记这几条：

- `key`：提案内的引用名。`create` 必填；`model`/`vendor` 的 key 就是主键；`replace model_bindings` 时是模型 ID
- `id`：已存在实体的主键，只有 `channel` / `adapter_spec` 用
- `expect_version`：`update` 必填，取 `*_get` 返回的 `version`
- 同一实体在一条提案里只能出现一次

## 三条最容易写错的规则

1. **新建模型必须同时给出 `model_bindings`**，否则模型不进目录（门户看不到）。
   这条是跨操作的：分块校验（`mode="shape"`）会跳过它，所以最后一趟 `full` 必须跑。
2. **`update` 只提交要改的字段**：省略 = 保持库内现值。不要为了"安全"把整份配置重述一遍——
   重述时漏掉限流/并发就会把它们重置。
3. `disable` 与 `replace` 不是删除：`disable` 停用、`replace` 整体替换绑定集合。平台没有 delete。

## 四个最常见的"字段放错层"

被拒时会返回 `code=plan_invalid` + `index`（第几条操作）+ `field` + `message`。
`message` 里既有「允许的字段」，也有「提示：这个值应该写在哪」。**只改那一处，不要整份重写。**

| 写错的 | 正确的 | 说明 |
| --- | --- | --- |
| `data.slug` | `operations[].key`（+ `data.spec.slug`） | 规格的标识在 op.key；DSL 内的 slug 在 data.spec 里 |
| `data.is_free` / `data.is_default` | 不写 | 派生字段，由价格/绑定唯一决定，写了会被拒 |
| `data.adapter_options` | 不写 | 适配器私有配置，L1 只能改 `adapter_config` 的三个字段 |
| `data.endpoints: ["chat"]` | `["chat/completions"]` | 端点键必须来自 `schema_get(section="endpoints")` |

## 正例：接入一家中转的对话模型（五个实体全齐）

这就是一份**可以直接交给 `validate_draft` 的参数**：外层只有 `operations`，字段全填、无占位符。
价格一律是**微**（1 货币单位 = 1000000 微）：`500000` = 0.5 元 / 百万 tokens。

<!--example:minimal-chat-->

`validate_draft` 只收 `operations`；`propose_change` 要在同一份内容外面再加一个 `summary`
（一句话说清这次改什么）。

## 只改名怎么写

```json
{"op":"update","entity":"channel","id":12,"expect_version":3,"data":{"name":"中转A-电信"}}
```

这一条是 `operations` 数组里的**一项**。`data` 里只有 `name` —— 其余字段保持库内现值。
"最小更新"是提案的基本功：多写一个字段，就多一个覆盖线上值的风险。

## 另外两份示例（按需读，别一次性全读）

- 上游「提交 + 查询」但平台端点要同步返回（TTS/生图）：`skill_read 22-plan-examples`
- 分钟级异步任务（视频/音乐，平台排队轮询）：`skill_read 24-async-task`

## 被拒绝时怎么办

返回里会带 `index` 与 `field`，例如 `index:2` + `field:"data.name"`。**只改那一个字段再提交**。
整份重写会让已经正确的部分重新冒风险；如果同一处连错两次，按 25-writing-json 的办法分块校验。
