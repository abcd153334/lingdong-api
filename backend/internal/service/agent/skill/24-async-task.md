---
id: 24-async-task
title: 正例：分钟级异步任务（mode=task）
summary: 视频/音乐这类分钟级上游用平台任务队列；与 bridge 的判据差别只有一条：谁在等
priority: 24
---

# 正例：视频任务（平台排队轮询）

判断"用 `mode=task` 还是 `mode=bridge`"只看一件事：**这次请求里等不等得到结果**。

- 秒级（TTS、生图）→ `bridge`：请求内等完，同步返回（见 `22-plan-examples`）。
- 分钟级（视频、音乐）→ `task`：平台建任务、后台轮询，用户拿 `task_id` 来查。

例外只有一个：**图像（`images/generations`）两面同路径**。同一个端点既有同步出图，
也能被调用方用 `async: true` 转成任务。所以在这里声明 `mode=task` 时，同步那一面
仍由适配器在请求内等（同一份 `async.poll` 支撑两条路），`timeout_secs` / `interval_ms`
照样可以声明；声明 `mode=bridge` 则只有同步面，`async: true` 的请求会被判成渠道配置错误。

<!--example:video-bridge-task-->

三点提醒：

1. `status_map` 至少要有 `succeeded` 与 `failed`；其余状态缺省按"运行中"处理。
   上游状态值写错的表现是**一直转圈到超时**（不是报错），所以别名表要用平台已收敛的那套。
2. `extract.result_urls` 混排时一律给 `where`；不给就只取第一个（可能是封面图）。
3. 结果是多段产物（视频 + 封面 + 字幕）时，运营要的是主产物 —— 用 `where` 精确到它。

## 任务读面（全站一套，不用自己设计）

异步任务建好后，查状态与取产物**不是**每个上游一套，全站只有一个入口：

- `GET /v1/tasks/{id}` —— 状态、进度、产物清单、用量
- `GET /v1/tasks/{id}/contents/{index}` —— 按序号下载产物（序号从 0 连续编号）
- `DELETE /v1/tasks/{id}` —— 只对「已受理但还没提交到上游」的任务生效，其余 409

## 平台已承接的异步端点（按端点判定，不是按模态）

| 端点键 | 模态 | 谁能承接 |
| --- | --- | --- |
| `video/generations` | video | 全部异步适配器 |
| `video/regenerations` | video | 仅 metaso |
| `images/generations` | image | 同步面：全部图像适配器；`async: true` 面：多米、规格声明 |
| `audio/music/generations` | music | 多米、规格声明 |
| `audio/lyrics` | music | 多米、规格声明 |

给模型声明 `endpoints` 时只能写上面这些键；写成别的（或声明了而没有任何启用绑定能承接）
会在保存期被拒。**判据是端点不是模态**：一个渠道能做视频，不代表它能做音乐。

