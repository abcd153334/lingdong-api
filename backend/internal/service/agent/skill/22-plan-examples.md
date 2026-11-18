---
id: 22-plan-examples
title: 正例：同步端点 + 上游异步（bridge）
summary: 一份完整的 bridge 接入提案；看分层：每个 endpoint 键就是一个上游接口，@poll 是查询端点
priority: 22
---

# 正例：TTS（上游异步、平台同步）

上游只有「提交 / 查询」两个接口，而平台端点是同步的，所以用 `async.mode=bridge`：
适配器在**本次请求内**等完并把产物作为响应体返回。

看它的**分层**，不要看字段名（字段名以 `schema_get(section="spec_dsl")` 为准）：

- `data.spec.endpoints` 下每个键就是一个上游端点；`audio/speech@poll` 是查询端点。
- `audio/speech` 的 path 用 `{upstream_model}` 占位：一份规格覆盖同族多个 workflow，
  换型号只改绑定的 `upstream_model`，不改规格。
- `result_urls` 带 `where` 过滤：上游 `results` 常混着封面图/字幕，不过滤会拿到图。

<!--example:bridge-tts-->

三点提醒：

1. 这份参数**能直接交给 `validate_draft`**（外层只有 `operations`）。照它的形状改内容，
   不要照它的字段名去写别的模态 —— 每个模态的端点键不同。
2. 桥接只允许"二进制产物 / 图像 URL"两类端点，其余在保存期就会拒绝。
3. 时长口径见 `32-spec-bridge` 的 `duration_unit`：接错字段（比如把"任务耗时"当"音频时长"）
   会让计费差出两个数量级，而且**不会报错**。
