---
id: 30-spec-dsl
title: 声明式规格 DSL
summary: 骨架与正例、字段表去哪取、非标字段与占位符、高频错位
priority: 30
---

# 规格 DSL

**字段表不在本文**：字段名、类型、上限、枚举一律从
`schema_get(section="spec_dsl")` 取（按 part 取更省上下文：
`spec` / `endpoint` / `auth` / `body` / `array` / `extract` / `errors` / `async` /
`request_field` / `url_selector` / `limits` / `enums`）。
本文只讲**为什么**和**最容易写错的地方** —— 字段表会随代码变，抄在这里迟早过期，
而过期的说明不会报错，只会让你持续产出"看起来对"的配置。

## 骨架（最小可跑）

```json
{
  "slug": "relay-a",
  "label": "中转 A",
  "vendor": "relayco",
  "endpoints": {
    "chat/completions": {
      "method": "POST",
      "path": "/v1/chat/completions",
      "auth": {"in": "header", "name": "authorization", "prefix": "Bearer "},
      "passthrough": true
    }
  }
}
```

三件事先记住：

- `slug` 是规格的主键，格式 `^[a-z][a-z0-9-]{1,31}$`；渠道用 `spec:<slug>` 引用它。
- 端点键（`endpoints` 的键）必须来自 `schema_get(section="endpoints")`，
  自己起的名字能存进去，但平台永远不会调用它。
- 一个端点要么 `passthrough`（只透传对话类），要么用 `body` 重建；
  `drop` 只能在 `passthrough` 上用。

## 六个高频错位（都会在执行期被静默吞掉）

1. **把 `slug` 写到 data 顶层**。提案里规格的标识是 `operations[].key`，
   DSL 内的 `slug` 在 `data.spec.slug`。顶层写 `slug` 会被严格解析拒绝：
   `data 不接受 slug`。
2. **轮询端点路径里没有 `{task_id}`，或写了两个**。必须**恰好一个**，
   否则任务提交得出去、状态永远查不回来。
3. **异步两件套缺一半**：提交端点要给 `extract.task_id`，轮询端点要给
   `extract.status`，并且 `async.poll` 必须**恰好等于** `<端点>@poll`。
   轮询端点不能有 `body`，也不能 `passthrough`。
4. **`status_map` 缺终态**。必须给 `succeeded` 与 `failed`；`queued` / `running` /
   `canceled` 可省略。**上游状态连续 3 次不认识会直接判配置错**（不再空等到超时），
   所以拿不准的状态词宁可先写全。同一上游状态值不能映射到两个态。
5. **把计费维度写成常量**。`n` / `size` / `resolution` / `seconds` / `duration`
   这类键（见 `spec_dsl` 的 `enums.reserved_const_keys`）必须由请求传，
   写进 `const` 会被拦下 —— 常量意味着"所有人按同一价买同一份"。
6. **`errors` 只看 HTTP 码**（缺省 2xx）。上游用 200 包错误体时必须给
   `errors.ok_codes` / `code_path`，否则失败会被当成成功，用户被扣费。
7. **`message_path` 是写给用户看的那句话**。HTTP 通了、业务码说失败，平台判成
   「上游拒绝了本次请求」，以 **400 + 这句话**回给调用方（不重试、不记渠道健康）——
   所以它必须指向上游的人话（通常是 `$.msg`），不要指 `$.errno` 这类内部字段：
   用户会看到一串数字，仍然不知道要改什么。
   反过来写成 `success:"http_2xx"` 才是灾难：业务失败当成功，用户被扣费。

## 非标参数：`request_fields`（不要硬塞进已有字段）

上游多要的参数（参考音频地址、情绪权重、工作流参数）**不要**往平台契约字段上凑 ——
典型事故是把「音色名」当「参考音频 URL」发出去：请求 200、结果全错、没有任何报错。

正确做法是两条都写：

1. `endpoint.request_fields` 声明**调用方能传什么**（名字/类型/必填/说明），
   它会进对外文档的字段表与调试台表单；
2. `body.copy` 或 `path` 占位符决定**它怎么到上游**。

硬规则（保存期会拒）：

- 声明了就必须被消费（出现在 `body.copy` / `body.arrays.from` / `path` 占位符里），
  否则报「声明了但不会发给上游」；
- 不能与平台契约字段同名（`input` / `model` / `stream` …）；
- `required` 与 `default` 互斥；`required` 缺失时平台直接 400，
  文案带 `desc`，所以 `desc` 要写成一句人话。

## 路径占位符：同族多个工作流只写一份规格

提交端点的 `path` 里可以写 `{upstream_model}`（值来自**绑定的 `upstream_model`**），
也可以写 `request_fields` 里的字段名。取值的来源在保存期就定死，运行期不做"猜"。

```json
{"audio/speech":{"path":"/api/v1/comfyui/comfyui_workflow/{upstream_model}"}}
```

换一个 workflow 只改绑定，**不要复制整份规格**：复制出来的渠道/模型/文档都要跟着改，
而漏改一处不会报错。轮询端点的 `path` 仍然只允许一个 `{task_id}`。

## 异步桥接（bridge）

把「提交 + 查询」的非标接口变成同步端点的完整做法（四件套、产物下载、时长口径、混排过滤）见 `skill_read 32-spec-bridge`。

## 轮询端点的两种来源

- `mode=task`：`@poll` 被平台任务队列调用，`extract.status` 必须存在；
- `mode=bridge`：`@poll` 由适配器在本次请求内调用，额外要求提交端点给 `extract.task_id`。

两者都**不接受** `body` / `passthrough`（查询接口没有请求体规则）。

## 端点键的边界（别把入站协议写成端点）

`/v1/messages` 是**平台侧的入站协议**（Anthropic Messages 格式），它不表达新能力：
所有 chat 模型自动可用，因此它**不进 `models.endpoints`、也不进规格的 `endpoints.<key>`**。
写进规格只会得到一个永远匹配不上的键（保存期不报错，运行期表现为"端点不支持"）。

## 写之前的三步

1. `docs_read` 读鉴权、端点、请求体、响应体、错误码五段（见 15-reading-docs）。
2. `schema_get(section="spec_dsl", part="limits")` 确认上限：端点 ≤32、常量键 ≤32、
   数组映射 ≤8 项、扩展字段 ≤16、路径占位符 ≤8、路径 ≤512 字节、整份 ≤64KB。
3. `validate_draft` 预演（与提交同一条管线）。预演通过就不必再改；
   报错会带字段路径（如 `endpoints.audio/speech@poll.extract.result_urls`），照着改那一处。
