---
id: 32-spec-bridge
title: 异步桥接（提交 + 查询 → 同步响应）
summary: 四件套、产物分类、时长口径、混排过滤、状态五态；含一份可运行示例
priority: 32
---

# 异步桥接（bridge）

平台端点是同步的（秒级：TTS / 生图），而上游只有「提交 + 查询」两个接口时，
用 `async.mode=bridge`：适配器在**本次请求内**等完并把产物作为响应体返回。
分钟级的视频/音乐用 `async.mode=task`（平台任务队列），两者的判据见 `10-identify-level`。

适用：平台端点是**同步**的，而上游只有提交与查询两个接口。
**桥接只支持两类端点**：二进制响应端点（`audio/speech`）与图像生成端点
（`images/generations`，回 OpenAI 的 `data[].url`）。其余 JSON 契约（rerank / moderation /
embeddings）承载不了产物，保存期会拒。

四件套缺一不可：

1. 提交端点：`extract.task_id`（从提交响应里取上游任务号）；
2. `async: {mode:"bridge", poll:"<端点>@poll", timeout_secs:600, interval_ms:1000}`；
3. `@poll` 端点：`path` 含 `{task_id}`、`extract.status`、`status_map`、`extract.result`
   或 `extract.result_urls`；
4. 提交端点上的 `response_content_type`（下载体没有 Content-Type 时的兜底，如 `audio/wav`）。

<!--example:bridge-tts-->


五个容易踩的点：

- **`omit_model`**：上游请求体里没有 `model` 字段时必须写 `true`。缺省行为是一律覆盖
  `model`（那是对的：上游只认自己的型号名），但多塞一个未知键会被严格校验的上游 400。
- **产物混排必须过滤**：上游的 `results` 常常混着封面图/字幕/多段音轨。
  不写 `where` 时 `result_urls` 会把**所有**匹配链接都取回来：桥接下载只用第一个
  （音频请求拿到封面图就是这么来的），图像桥接还会把它们全部写进 `data[].url`。
  混排上游一律用 `where` 过滤（如 `{"type":"audio"}`）；过滤后为空会明确报「没有匹配的产物」。
- **`duration` 只写产物时长，不写任务耗时**。这在真实上游上踩过：
  AutoDL 查询响应里的 `data.duration` 是**任务执行秒数**（RUNNING 时就已经是 196），
  写进 `extract.duration` 会把 4 秒音频按 196 秒计费。
  分不清时宁可不写 —— 桥接回的是完整音频，平台会自己探测时长。
  单位不确定时用 `duration_unit: "ms"`（毫秒），比"猜它是秒"安全。
- **数字状态码不要抄进 `status_map`**：`"1"/"2"/"3"` 是各家自己定的，
  平台别名表故意不收数字；直接写英文状态词，或让对应的内置适配器去映射。
- **上游会校验什么，要写进 `request_fields.desc`**。AutoDL 的 ComfyUI 工作流只收
  `audio/mpeg` / `audio/wav`，而有些 URL 返回 `audio/wave`（同样是 WAV）—— 上游直接拒。
  这类要求是用户在调试台**唯一**的线索：写进 desc（例：「URL 或 base64；URL 的
  Content-Type 必须是 audio/wav 或 audio/mpeg」），别指望他从「服务不可用」里猜。

