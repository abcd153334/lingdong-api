---
id: 10-identify-level
title: 判断接入层级
summary: L1/L2/L3 的判据、adapter_config 的边界、什么时候该用 submit_unsupported
priority: 10
---

# 怎么判断用哪一层

先看上游文档的「鉴权」与「请求形状」，再看「响应怎么拿结果」。不要凭供应商名字猜。

## L1：内置适配器 + 渠道配置

上游与 OpenAI 协议兼容，差异只在**路径**或**鉴权头**。你可以写 `adapter_config`，但只有三个字段：

- `auth_header`：密钥放在哪个头（必须在平台白名单内，如 `authorization` / `x-api-key`）
- `auth_prefix`：前缀，如 `Bearer `
- `path_overrides`：把某个标准路径换成上游的路径（键必须是平台已有的端点路径）

`adapter_options` 不能写：那是适配器私有配置，形状由适配器自己定义，写错只能在第一次真实请求时暴露。

## L2：声明式规格

差异在路径、鉴权、请求字段名、静态常量、响应取值位置。典型特征：

- 请求参数名不一样（`prompt` 要叫 `text`、`size` 要叫 `resolution`、音色要叫 `speaker`）
- 结果在 `data[0].url` 或 `output.task_id` 这类位置（含多层信封、数组）
- 上游状态值不是平台五态（queued/running/succeeded/failed/canceled，如 `{"state":"SUCCESS"}`）→ `status_map` 映射
- 静态常量（版本号、格式、是否需要水印）→ `body.const`

**异步也在 L2，而且是两种形态**（这是最容易误判成 L3 的一处）：

| 上游形态 | 平台怎么写 | 适用 |
|---|---|---|
| 提交拿 task_id，之后查状态（分钟级：视频/音乐） | `async.mode=task` + `@poll` 端点 | 平台任务队列轮询 |
| 提交拿 task_id，但平台这个端点是**同步**的（秒级：TTS/生图） | `async.mode=bridge` + `@poll` 端点，且 `@poll` 必须给出 `extract.result` 或 `extract.result_urls`（产物链接来源） | 适配器在本次请求内等完并下载产物 |

判据只看两件事：平台端点是不是同步的（`schema_get(section="endpoints")` 的 `async` 字段），
以及上游要不要轮询。**只要"提交 + 查询"两个 HTTP 接口就能拿到结果，就一定能用规格表达**，
不需要写代码。典型例子：AutoDL IndexTTS（`/api/v1/comfyui/comfyui_workflow/{workflow_id}` →
`/api/v1/comfyui/comfyui_workflow/result/{task_id}` → `data.results[].url`，按 `type=audio` 过滤）。

写 `adapter_spec`（entity）→ 渠道用 `spec:<slug>`。规格是**可校验**的，优先选它。
DSL 的字段与上限从 `schema_get(section="spec_dsl")` 取（见 30-spec-dsl）。

## L3：必须写内置适配器（代码）

出现任一特征就不要硬凑：

- 自研签名（HMAC / RSA / 时间戳 + nonce）
- 多步调用（先换 token 再调用）
- WebSocket 长连接
- 依赖回调（平台没有对外回调接收）
- 需要真正的结构变换（例如把一张图切成九宫格分别推理，或请求体要按业务规则递归重排）
- 需要读取并复用上游返回的**中间令牌**再发起第二次业务调用（多步链路）

此时用 `submit_unsupported` 把结论沉淀下来：`vendor` + `reasons`（每条要能让工程师复现）
+ `required_features`。**不要**编一份看起来能用的规格，也不要因为"异步"就判 L3。

## 判断顺序

1. `docs_read` 读上游文档（鉴权 → 端点 → 请求体 → 响应体 → 错误码）
2. `spec_support_check` 给出层级建议（关键词判断，最终以你的阅读为准；它说 L3 时也要自己复核是不是"其实是异步/改名/状态映射"）
3. `schema_get(section="limits")` 与 `schema_get(section="spec_dsl", part="limits")`
   确认规格能力上限（端点数量、数组映射、路径长度）
4. 判层本身**不需要问**：读文档 + 查 schema 就能定。
   只有"文档没写清鉴权方式/是否异步"这种事实缺失才 `ask_user`（判据见 05-when-to-ask）
