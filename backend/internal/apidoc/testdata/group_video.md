# 视频生成

> 同一个接口覆盖标准版与快速版，切换只改 model。
>
> 自动生成 · 契约版本 2 · 事实指纹 `06fec6e1014d`

## 0. 快速开始

- **Base URL**：`https://api.example.com`（`example.com` 是占位域名，请换成平台实际地址）
- **鉴权**：请求头 `Authorization: Bearer <你的 API KEY>`，KEY 在控制台「API KEY」页创建
- **模型 ID**：`vid-a`、`vid-b`、`vid-c`（本组共用同一套参数，切换模型只改 `model`）
- **第一个请求**：`POST /v1/video/generations`

```bash
curl -X POST \
  "https://api.example.com/v1/video/generations" \
  -H "Authorization: Bearer $LD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
  "model": "vid-b",
  "prompt": "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近",
  "seconds": 5,
  "resolution": "720p",
  "input_reference": [
    {
      "type": "image",
      "image_url": "https://cdn.example.com/cat.png"
    },
    {
      "type": "audio",
      "audio_url": "https://cdn.example.com/piano.mp3"
    }
  ]
}'
```

## 1. 适用模型

| 模型 ID | 名称 | 说明 |
| --- | --- | --- |
| `vid-a` | 视频 A | 参数与本组其他模型一致 |
| `vid-b` | 视频 B（代表） | 出片更快，单价更高 |
| `vid-c` | 视频 C | 参数与本组其他模型一致 |

## 2. 本组怎么用

先调 `POST /v1/video/generations` 提交，拿到任务号后用 `GET /v1/tasks/{id}` 查进度、`GET /v1/tasks/{id}/contents/{index}` 取产物。

每个接口一节：字段表、响应示例、可运行示例都在那一节里（第 5 节）。

## 3. 鉴权

所有接口都用一个 Bearer Token 鉴权：

```
Authorization: Bearer <你的 API KEY>
```

- KEY 只放服务端；不要写进前端代码、不要提交进代码仓库。
- KEY 无效、过期或被禁用 → `401 invalid_api_key`。
- KEY 没有该模型权限 → `403 model_not_allowed`。

```bash
curl "https://api.example.com/v1/models" -H "Authorization: Bearer $LD_API_KEY"
```

## 4. 接口清单

本族共 4 个接口。路径里的 `{...}` 是占位符，请求时替换成实际值；参数与示例见对应章节。

| 接口 | 方法与路径 | 说明 |
| --- | --- | --- |
| 创建视频任务 | `POST /v1/video/generations` | 提交视频生成任务，立即返回任务号。文字生视频 / 图片生视频 / 配音 / 续写统一入口。 |
| 任务详情 | `GET /v1/tasks/{id}` | 查单个任务的状态、产物清单与用量，建议 5~10 秒轮询一次。 |
| 下载任务产物 | `GET /v1/tasks/{id}/contents/{index}` | 按序号下载产物（图片 / 音频 / 歌词文本）。序号从 0 开始且连续。 |
| 取消任务 | `DELETE /v1/tasks/{id}` | 取消**仍在排队、尚未开始生成**的任务（其余一律 409），费用自动退回。终态任务不会被物理删除（对账需要）。 |

## 5. 参数约束（本组专属）

- 档位取值见上方参数表的 `size` / `resolution` 取值列；像素写法按面积就近吸附到该模型已开放的档位。
- 单次时长上限 10 秒（超出提交即报错，不截断、不自动降档）。

## 6. 异步任务流程

本族是异步的：一次请求不会立刻出结果，标准流程是三步。

1. **提交**：`POST /v1/video/generations`，拿到 `id`（任务号）与 `status=queued`。
2. **轮询**：每隔 5~10 秒查一次状态，直到 `status` 变成 `succeeded` / `failed` / `expired` / `canceled`。
3. **取产物**：`status=succeeded` 后用产物接口下载，产物保留 7 天。

```python
import os, time, requests

BASE = "https://api.example.com"
H = {"Authorization": "Bearer " + os.environ["LD_API_KEY"]}

payload = {
    "model": "vid-b",
    "prompt": "一只橘猫在钢琴前专注弹奏，镜头从中景缓缓推近",
    "seconds": 5,
    "resolution": "720p",
    "input_reference": [
        {
            "type": "image",
            "image_url": "https://cdn.example.com/cat.png",
        },
        {
            "type": "audio",
            "audio_url": "https://cdn.example.com/piano.mp3",
        },
    ],
}

task = requests.post(BASE + "/v1/video/generations", headers=H, json=payload).json()
tid = task["id"]

while True:
    info = requests.get(BASE + "/v1/tasks/{id}".format(id=tid), headers=H).json()
    if info["status"] in ("succeeded", "failed", "expired", "canceled"):
        break
    time.sleep(5)

if info["status"] == "succeeded":
    data = requests.get(BASE + "/v1/tasks/{id}/contents/{index}".format(id=tid, index=0), headers=H)
    open("out.bin", "wb").write(data.content)
```

提交时会冻结预估额度，结束后按实际结算（多退少补）；失败、取消、超时全额释放，不扣费。

## 7. 计费口径

按规格计费：先选档位，按「档位单价 × 数量（张 / 秒）」计费；提交时冻结，结算多退少补。

- `resolution`：按「档位秒单价 × 秒数」计费。
- 提交时冻结预估额度，结束后按实际结算；失败、取消、超时全额释放。
- 本节只写口径，**不写金额**——实时单价见控制台「价格」页，避免文档与账单不一致。

## 8. 错误码

| HTTP | code | 含义 | 处理建议 |
| --- | --- | --- | --- |
| 400 | `invalid_param` | 参数不合法（字段缺失、取值越界、素材地址不可访问）。 | 对照字段表核对参数，错误信息会指出具体字段。 |
| 401 | `invalid_api_key` | KEY 无效、已过期或被禁用。 | 检查 KEY 是否完整、是否已在控制台删除或过期。 |
| 403 | `model_not_allowed` | 该 KEY 没有这个模型的权限。 | 确认这个 KEY 的可用模型包含当前模型。 |
| 404 | `model_not_found` | 模型不存在或已下线。 | 核对模型 ID 拼写；模型可能已下线，见模型广场。 |
| 409 | `task_not_cancelable` | 任务已开始生成或已结束，不能取消（只有排队中可取消）。 | 只有排队中（queued）且尚未开始生成的任务可取消；已开始生成请等它结束。 |
| 410 | `file_gone` | 产物已过保留期（7 天），无法再下载。 | 产物只保留 7 天，请重新生成。 |
| 429 | `rate_limited` | 请求过于频繁，稍后重试。 | 按退避重试；持续触发请申请更高并发。 |
| 503 | `service_unavailable` | 本次调用未能完成，未产生费用。 | 稍后重试，本次未扣费。 |
| 504 | `gateway_timeout` | 等待超时，已全额释放额度；调用记录里可查到任务号。 | 缩短时长或稍后重试；已全额释放额度。 |

## 9. 限制与配额

- 每个 API KEY 有每分钟请求数上限与在途任务数上限，超出返回 `429 rate_limited`。
- 产物保留 7 天，过期下载返回 `410 file_gone`，请及时转存。
- 模型专属的上限（时长、张数、参考素材免费额度）见第 5 节。

## 10. 注意事项

- 产物保留 7 天，请及时转存。

---

> 契约版本 2 · 文档组 `video-a` · 3 个模型 · 事实指纹 `06fec6e1014d`
